package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"net/mail"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/gluon/rfc822"
	"github.com/ProtonMail/go-proton-api"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

const testToken = "smtp token"

// fakeSubmission plays Proton Mail's SMTP server: it keeps a copy under Sent and delivers to the recipient through the fake API.
type fakeSubmission struct {
	e       *testEnv
	addr    string
	tls     *tls.Config
	sender  string
	delay   time.Duration
	lag     time.Duration
	refuse  atomic.Bool
	mailErr atomic.Pointer[smtp.SMTPError]
	rcptErr atomic.Pointer[smtp.SMTPError]
	dataErr atomic.Pointer[smtp.SMTPError]
	hangUp  atomic.Bool

	mu       sync.Mutex
	received [][]byte
	copies   sync.WaitGroup
}

func newFakeSubmission(e *testEnv, sender string) *fakeSubmission {
	e.t.Helper()
	serverTLS, clientTLS := testTLS(e.t)
	f := &fakeSubmission{e: e, tls: clientTLS, sender: sender}

	s := smtp.NewServer(f)
	s.Domain = "127.0.0.1"
	s.TLSConfig = serverTLS
	s.MaxLineLength = 1 << 20
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	go s.Serve(l)
	e.t.Cleanup(func() {
		s.Close()
		f.copies.Wait()
	})

	f.addr = l.Addr().String()
	return f
}

func (f *fakeSubmission) submission() submission {
	return submission{addr: f.addr, sender: f.sender, token: testToken, tls: f.tls}
}

func (f *fakeSubmission) messages() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.received...)
}

func (f *fakeSubmission) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &fakeSubmissionSession{fake: f, conn: c}, nil
}

type fakeSubmissionSession struct {
	fake   *fakeSubmission
	conn   *smtp.Conn
	authed bool
	to     []string
}

func (s *fakeSubmissionSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *fakeSubmissionSession) Auth(_ string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, username, password string) error {
		if username != s.fake.sender || password != testToken {
			return &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "Authentication failed"}
		}
		s.authed = true
		return nil
	}), nil
}

func (s *fakeSubmissionSession) Mail(from string, _ *smtp.MailOptions) error {
	if !s.authed || from != s.fake.sender {
		return &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "Authentication required"}
	}
	if err := s.fake.mailErr.Load(); err != nil {
		return err
	}
	return nil
}

func (s *fakeSubmissionSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	if err := s.fake.rcptErr.Load(); err != nil {
		return err
	}
	s.to = append(s.to, to)
	return nil
}

func (s *fakeSubmissionSession) Data(r io.Reader) error {
	f := s.fake
	f.copies.Add(1)
	defer f.copies.Done()

	literal, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if f.hangUp.Load() {
		return s.conn.Conn().Close()
	}
	if f.refuse.Load() {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Message refused"}
	}
	if err := f.dataErr.Load(); err != nil {
		return err
	}
	time.Sleep(f.delay)

	f.mu.Lock()
	f.received = append(f.received, literal)
	f.mu.Unlock()

	if f.lag == 0 {
		return f.deliver(s.to, literal)
	}
	to := s.to
	f.copies.Go(func() {
		time.Sleep(f.lag)
		if err := f.deliver(to, literal); err != nil {
			f.e.t.Error(err)
		}
	})
	return nil
}

func (s *fakeSubmissionSession) Reset() { s.to = nil }

func (s *fakeSubmissionSession) Logout() error { return nil }

func (f *fakeSubmission) deliver(to []string, literal []byte) error {
	ctx := context.Background()
	c, addrKR := f.e.unlock(ctx, f.sender)
	defer c.Close()

	header, body := rfc822.Split(literal)
	h, err := rfc822.NewHeader(header)
	if err != nil {
		return err
	}
	sender, err := mail.ParseAddress(h.Get("From"))
	if err != nil {
		return err
	}

	template := proton.DraftTemplate{
		Subject:    h.Get("Subject"),
		Sender:     sender,
		Body:       string(body),
		MIMEType:   rfc822.TextPlain,
		ExternalID: strings.Trim(h.Get("Message-Id"), "<>"),
	}
	prefs := make(map[string]proton.SendPreferences)
	for _, rcpt := range to {
		template.ToList = append(template.ToList, &mail.Address{Address: rcpt})
		keys, _, err := c.GetPublicKeys(ctx, rcpt)
		if err != nil {
			return err
		}
		kr, err := keys.GetKeyRing()
		if err != nil {
			return err
		}
		prefs[rcpt] = proton.SendPreferences{
			Encrypt:          true,
			PubKey:           kr,
			SignatureType:    proton.DetachedSignature,
			EncryptionScheme: proton.InternalScheme,
			MIMEType:         rfc822.TextPlain,
		}
	}

	draft, err := c.CreateDraft(ctx, addrKR, proton.CreateDraftReq{Message: template})
	if err != nil {
		return err
	}
	var req proton.SendDraftReq
	if err := req.AddTextPackage(addrKR, string(body), rfc822.TextPlain, prefs, nil); err != nil {
		return err
	}
	_, err = c.SendDraft(ctx, draft.ID, req)
	return err
}

func testTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}},
		&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}
}

func TestSubmissionNeedsTLS(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	s := e.cfg.submission
	s.tls = nil
	if err := s.send(context.Background(), "", nil); err == nil {
		t.Error("signed in to a server whose certificate is not trusted")
	}
}

func TestSubmissionReportsARefusedToken(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	s := e.cfg.submission
	s.token = "wrong"
	if err := s.send(context.Background(), "", nil); !errors.Is(err, errTokenRefused) {
		t.Errorf("got %v, want errTokenRefused", err)
	}
}

func TestSubmissionGivesUpAtTheDeadline(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := l.Accept(); err == nil {
			accepted <- c
		}
	}()
	t.Cleanup(func() {
		l.Close()
		select {
		case c := <-accepted:
			c.Close()
		default:
		}
	})

	s := submission{addr: l.Addr().String(), sender: "hello@example.com", token: testToken}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.send(ctx, "", nil) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a server that never answers took the token")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("send waits for a silent server past its deadline")
	}
}

func TestTemporaryRefusalOfTheMessageIsTemporary(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.submit.dataErr.Store(&smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Try again later"})
	e.login()
	cfg := e.cfg
	cfg.listen = e.serve()

	if err := send(cfg.listen, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
		t.Fatalf("got %v, want a 451 reply", err)
	}
	if err := healthcheck(cfg); err == nil {
		t.Error("the healthcheck passes while Proton Mail defers every message")
	}
}

func TestSubmissionReportsAnUnreachableServer(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := submission{addr: l.Addr().String(), sender: "hello@example.com", token: testToken}
	l.Close()
	if err := s.send(context.Background(), "", nil); err == nil {
		t.Error("signed in to a server that is not there")
	}
}
