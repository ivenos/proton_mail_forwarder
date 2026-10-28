package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/ProtonMail/go-proton-api"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"golang.org/x/net/netutil"
)

const (
	checkInterval  = time.Hour
	maxConnections = 10
)

var (
	errAuthRequired = &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "Authentication required"}
	errStopping     = &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 3, 2}, Message: "Shutting down, try again later"}
)

func serve(ctx context.Context, cfg config, m *proton.Manager) error {
	defer m.Close()

	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return err
	}
	return serveOn(ctx, cfg, m, listener)
}

func serveOn(ctx context.Context, cfg config, m *proton.Manager, listener net.Listener) error {
	mb := newMailbox(m, cfg)
	mb.setStatus(errors.New("starting"))
	defer func() {
		close(mb.stopping)
		closing, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if mb.acquire(closing) == nil {
			mb.disconnect()
			mb.release()
		}
	}()

	b := &backend{mb: mb, cfg: cfg}
	server := smtp.NewServer(b)
	server.Domain, _ = os.Hostname()
	server.MaxMessageBytes = cfg.maxSize
	server.MaxLineLength = int(cfg.maxSize)
	server.MaxRecipients = 50
	server.ReadTimeout = 5 * time.Minute
	server.WriteTimeout = time.Minute
	server.AllowInsecureAuth = true

	background, stop := context.WithCancel(ctx)
	defer stop()
	go keepSignedIn(background, mb, cfg)

	served := make(chan error, 1)
	go func() { served <- server.Serve(netutil.LimitListener(listener, maxConnections)) }()
	log.Printf("proton_mail_forwarder %s listening on %s", version, listener.Addr())

	select {
	case <-ctx.Done():
	case err := <-served:
		if errors.Is(err, smtp.ErrServerClosed) {
			return nil
		}
		return err
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	drained := b.drain(shutdown)
	server.Close()
	<-served

	if drained {
		removed := make(chan struct{})
		go func() { mb.removals.Wait(); close(removed) }()
		select {
		case <-removed:
		case <-shutdown.Done():
		}
	}
	return nil
}

func keepSignedIn(ctx context.Context, mb *mailbox, cfg config) {
	for {
		err := mb.check(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("check session: %v", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(nextCheck(err, cfg)):
		}
	}
}

func nextCheck(err error, cfg config) time.Duration {
	if err == nil || errors.Is(err, errTokenRefused) {
		return cfg.checkEvery
	}
	return cfg.retryEvery
}

type backend struct {
	mb  *mailbox
	cfg config

	mu       sync.Mutex
	busy     int
	stopping bool
}

func (b *backend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	s := &smtpSession{backend: b}
	if b.cfg.username == "" {
		return s, nil
	}
	return &smtpAuthSession{smtpSession: s}, nil
}

// drain refuses new messages and waits until the sessions in the middle of one have replied.
func (b *backend) drain(ctx context.Context) bool {
	b.mu.Lock()
	b.stopping = true
	b.mu.Unlock()

	for {
		b.mu.Lock()
		busy := b.busy
		b.mu.Unlock()
		if busy == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

type smtpSession struct {
	backend    *backend
	authed     bool
	busy       bool
	recipients []string
}

func (s *smtpSession) Mail(_ string, _ *smtp.MailOptions) error {
	if s.backend.cfg.username != "" && !s.authed {
		return errAuthRequired
	}
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	if s.backend.stopping {
		return errStopping
	}
	if !s.busy {
		s.busy = true
		s.backend.busy++
	}
	return nil
}

func (s *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.recipients = append(s.recipients, to)
	return nil
}

func (s *smtpSession) Data(r io.Reader) error {
	literal, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.backend.cfg.deliveryTimeout)
	defer cancel()

	id, err := s.backend.mb.deliver(ctx, s.recipients, literal)
	if err != nil {
		log.Printf("deliver: %v", err)
		return smtpError(err)
	}

	log.Printf("sent message %s", id)
	return nil
}

func (s *smtpSession) Reset() {
	s.idle()
	s.recipients = nil
}

func (s *smtpSession) Logout() error {
	s.idle()
	return nil
}

func (s *smtpSession) idle() {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	if s.busy {
		s.busy = false
		s.backend.busy--
	}
}

type smtpAuthSession struct {
	*smtpSession
}

func (s *smtpAuthSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *smtpAuthSession) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(func(_, username, password string) error {
		userOK := subtle.ConstantTimeCompare([]byte(username), []byte(s.backend.cfg.username))
		passOK := subtle.ConstantTimeCompare([]byte(password), []byte(s.backend.cfg.password))
		if userOK&passOK != 1 {
			return &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "Invalid credentials"}
		}
		s.authed = true
		return nil
	}), nil
}

func smtpError(err error) error {
	switch {
	case errors.Is(err, errRefused):
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "Proton Mail refused the message"}
	case errors.Is(err, errNotSignedIn):
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Not signed in to Proton Mail"}
	case errors.Is(err, errTokenRefused):
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Proton Mail refused the SMTP token"}
	case errors.Is(err, errEnvelopeRefused):
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Proton Mail refused the sender or the recipient"}
	default:
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Delivery failed, try again later"}
	}
}
