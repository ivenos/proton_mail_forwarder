package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/gluon/rfc822"
	gosmtp "github.com/emersion/go-smtp"
)

type rawSMTP struct {
	t *testing.T
	c *textproto.Conn
}

func dialRaw(t *testing.T, addr string) *rawSMTP {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	r := &rawSMTP{t: t, c: textproto.NewConn(conn)}
	t.Cleanup(func() { r.c.Close() })
	if code, msg := r.reply(); code != 220 {
		t.Fatalf("greeting %d %s", code, msg)
	}
	return r
}

func (r *rawSMTP) reply() (int, string) {
	r.t.Helper()
	code, msg, err := r.c.ReadResponse(0)
	if code == 0 {
		r.t.Fatal(err)
	}
	return code, msg
}

func (r *rawSMTP) cmd(format string, args ...any) (int, string) {
	r.t.Helper()
	if err := r.c.PrintfLine(format, args...); err != nil {
		r.t.Fatal(err)
	}
	return r.reply()
}

func (r *rawSMTP) bdat(chunk string, last bool) (int, string) {
	r.t.Helper()
	suffix := ""
	if last {
		suffix = " LAST"
	}
	fmt.Fprintf(r.c.W, "BDAT %d%s\r\n%s", len(chunk), suffix, chunk)
	if err := r.c.W.Flush(); err != nil {
		r.t.Fatal(err)
	}
	return r.reply()
}

func (r *rawSMTP) data(literal string) (int, string) {
	r.t.Helper()
	if code, msg := r.cmd("DATA"); code != 354 {
		r.t.Fatalf("DATA gave %d %s", code, msg)
	}
	fmt.Fprintf(r.c.W, "%s.\r\n", literal)
	if err := r.c.W.Flush(); err != nil {
		r.t.Fatal(err)
	}
	return r.reply()
}

func (r *rawSMTP) envelope() {
	r.t.Helper()
	if code, msg := r.cmd("EHLO client"); code != 250 {
		r.t.Fatalf("EHLO gave %d %s", code, msg)
	}
	if code, msg := r.cmd("MAIL FROM:<shop@example.org>"); code != 250 {
		r.t.Fatalf("MAIL gave %d %s", code, msg)
	}
	if code, msg := r.cmd("RCPT TO:<hello@example.com>"); code != 250 {
		r.t.Fatalf("RCPT gave %d %s", code, msg)
	}
}

func literalOfSize(size int) string {
	header := "Subject: Big\r\n\r\n"
	return header + strings.Repeat("x", size-len(header)-2) + "\r\n"
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	previous := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	return buf
}

func sendLiteral(addr, literal string) error {
	return smtp.SendMail(addr, nil, "juergen@example.org", []string{"hello@example.com"}, []byte(literal))
}

func TestSlowProtonDefersTheMessageAtTheDeliveryTimeout(t *testing.T) {
	e := newTestEnv(t)
	e.cfg.deliveryTimeout = time.Second
	e.submit.delay = 3 * time.Second
	e.login()
	addr := e.serve()

	start := time.Now()
	err := send(addr, nil, "hello@example.com")
	if took := time.Since(start); err == nil || !strings.HasPrefix(err.Error(), "451") || took > 2500*time.Millisecond {
		t.Fatalf("got %v after %v, want a 451 reply at the delivery timeout", err, took.Round(time.Millisecond))
	}
}

func TestMessageThatWaitedInLineGetsTheWholeDeliveryTime(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.deliveryTimeout = 3 * time.Second
	e.submit.delay, e.submit.lag = 1500*time.Millisecond, time.Millisecond
	e.login()
	addr := e.serve()

	sent := make(chan error, 2)
	for range 2 {
		go func() { sent <- send(addr, nil, "hello@example.com") }()
	}
	for range 2 {
		if err := <-sent; err != nil {
			t.Errorf("the sender saw %v, so it would send again", err)
		}
	}
}

func TestNextCheck(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want time.Duration
	}{
		{nil, time.Hour},
		{errTokenRefused, time.Hour},
		{errNotSignedIn, time.Minute},
		{errors.New("connection refused"), time.Minute},
	} {
		if got := nextCheck(tc.err, config{checkEvery: time.Hour, retryEvery: time.Minute}); got != tc.want {
			t.Errorf("after %v: %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestRefusalAtMailFromIsTemporaryAndReported(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.submit.mailErr.Store(&gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 7, 1}, Message: "Sender address rejected"})
	e.login()
	cfg := e.cfg
	cfg.listen = e.serve()

	if err := send(cfg.listen, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
		t.Errorf("got %v, want a 451 reply", err)
	}
	if err := healthcheck(cfg); err == nil {
		t.Error("the healthcheck passes while Proton Mail refuses every sender")
	}
}

func TestRefusalAtRcptIsTemporaryAndReported(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.submit.rcptErr.Store(&gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 1, 1}, Message: "Mailbox unavailable"})
	e.login()
	cfg := e.cfg
	cfg.listen = e.serve()

	if err := send(cfg.listen, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
		t.Errorf("got %v, want a 451 reply", err)
	}
	if err := healthcheck(cfg); err == nil {
		t.Error("the healthcheck passes while Proton Mail refuses the recipient")
	}
}

func TestProtonReplyTextIsNotLogged(t *testing.T) {
	for _, stage := range []string{"MAIL FROM", "RCPT TO"} {
		e := newTestEnv(t)
		reply := &gosmtp.SMTPError{Code: 450, EnhancedCode: gosmtp.EnhancedCode{4, 2, 1}, Message: "<" + e.submit.sender + ">: Address rejected: try again later"}
		if stage == "MAIL FROM" {
			e.submit.mailErr.Store(reply)
		} else {
			e.submit.rcptErr.Store(reply)
		}
		e.login()
		addr := e.serve()
		logged := captureLog(t)

		if err := send(addr, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
			t.Fatalf("%s: got %v, want a 451 reply", stage, err)
		}
		if strings.Contains(logged.String(), e.submit.sender) {
			t.Errorf("%s: the log has the address:\n%s", stage, logged)
		}
		if status, _ := os.ReadFile(statusPath(e.cfg)); strings.Contains(string(status), e.submit.sender) {
			t.Errorf("%s: the status has the address: %s", stage, status)
		}
	}
}

func TestUnsupportedAuthMechanismIsRefused(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.username, e.cfg.password = "app", "secret"
	e.login()
	r := dialRaw(t, e.serve())

	if code, msg := r.cmd("EHLO client"); code != 250 || !strings.Contains(msg, "AUTH PLAIN") || strings.Contains(msg, "LOGIN") {
		t.Fatalf("EHLO gave %d %s", code, msg)
	}
	if code, msg := r.cmd("AUTH LOGIN"); code != 504 {
		t.Errorf("AUTH LOGIN gave %d %q, want 504", code, msg)
	}
}

func TestSenderNameInOtherCharsetsIsShown(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	addr := e.serve()

	literal := "From: =?windows-1252?Q?J=FCrgen_M=FCller?= <juergen@example.org>\r\n" +
		"Subject: Contact form\r\n" +
		"\r\n" +
		"Hello\r\n"
	if err := sendLiteral(addr, literal); err != nil {
		t.Fatal(err)
	}

	messages := e.inbox()
	if len(messages) != 1 {
		t.Fatalf("inbox has %d messages, want 1", len(messages))
	}
	if got := messages[0].Sender.Name; got != "Jürgen Müller" {
		t.Errorf("sender name is %q", got)
	}
}

func TestBodyArrivesUnchanged(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	addr := e.serve()

	header := "From: Shop <shop@example.org>\r\n" +
		"To: hello@example.com\r\n" +
		"Subject: Body\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"\r\n"
	body := ".leading dot\r\n" +
		"..two dots\r\n" +
		".\r\n" +
		"8-bit Grüße\r\n" +
		"\x00\x01\x7f\xfe\xff binary\r\n" +
		strings.Repeat("x", 998) + "\r\n" +
		"last line\r\n"
	if err := sendLiteral(addr, header+body); err != nil {
		t.Fatal(err)
	}

	sent := e.submit.messages()
	if len(sent) != 1 {
		t.Fatalf("Proton Mail got %d messages, want 1", len(sent))
	}
	gotHeader, gotBody := rfc822.Split(sent[0])
	if string(gotBody) != body {
		t.Errorf("body changed:\n%q\nwant\n%q", gotBody, body)
	}
	for _, line := range []string{"To: hello@example.com\r\n", "Subject: Body\r\n", "Content-Type: application/octet-stream\r\n"} {
		if !bytes.Contains(gotHeader, []byte(line)) {
			t.Errorf("header lost %q:\n%s", line, gotHeader)
		}
	}
}

func TestChunkedMessageArrivesUnchanged(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	r := dialRaw(t, e.serve())

	if code, msg := r.cmd("EHLO client"); code != 250 || !strings.Contains(msg, "CHUNKING") {
		t.Fatalf("EHLO gave %d %s", code, msg)
	}
	if code, msg := r.cmd("MAIL FROM:<shop@example.org>"); code != 250 {
		t.Fatalf("MAIL gave %d %s", code, msg)
	}
	if code, msg := r.cmd("RCPT TO:<hello@example.com>"); code != 250 {
		t.Fatalf("RCPT gave %d %s", code, msg)
	}

	header := "From: Shop <shop@example.org>\r\nSubject: Chunks\r\n\r\n"
	body := "first\r\n.\r\n.dot\r\nQUIT\r\nno line end"
	if code, msg := r.bdat(header+body[:9], false); code != 250 {
		t.Fatalf("first BDAT gave %d %s", code, msg)
	}
	if code, msg := r.bdat(body[9:], true); code != 250 {
		t.Fatalf("last BDAT gave %d %s", code, msg)
	}

	sent := e.submit.messages()
	if len(sent) != 1 {
		t.Fatalf("Proton Mail got %d messages, want 1", len(sent))
	}
	if _, got := rfc822.Split(sent[0]); string(got) != body+"\r\n" {
		t.Errorf("body is %q", got)
	}
}

func TestEachMessageOfASessionHasItsOwnRecipients(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.username, e.cfg.password = "app", "secret"
	alias, err := e.server.CreateAddress(e.userID, "alias@proton.local", []byte(testPassword), true)
	if err != nil {
		t.Fatal(err)
	}
	e.login()
	addr := e.serve()
	host, _, _ := net.SplitHostPort(addr)

	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Auth(smtp.PlainAuth("", "app", "secret", host)); err != nil {
		t.Fatal(err)
	}
	for _, rcpt := range []string{"alias@proton.local", "someone@example.com"} {
		if err := c.Mail("juergen@example.org"); err != nil {
			t.Fatal(err)
		}
		if err := c.Rcpt(rcpt); err != nil {
			t.Fatal(err)
		}
		w, err := c.Data()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(testMessage)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := c.Reset(); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Quit(); err != nil {
		t.Fatal(err)
	}

	byAddress := map[string]int{}
	for _, m := range e.inbox() {
		byAddress[m.AddressID]++
	}
	if byAddress[alias] != 1 || byAddress[e.addrID] != 1 {
		t.Errorf("messages per address: %v, want one for %s and one for %s", byAddress, alias, e.addrID)
	}
}

func TestMessageWithoutHeaderIsDelivered(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	addr := e.serve()

	if err := sendLiteral(addr, "Backup failed: disk full\r\n"); err != nil {
		t.Fatal(err)
	}
	sent := e.submit.messages()
	if len(sent) != 1 || !bytes.Contains(sent[0], []byte("\r\n\r\nBackup failed: disk full\r\n")) {
		t.Fatalf("Proton Mail got %q", sent)
	}
}

func TestMailDuringShutdownIsDeferred(t *testing.T) {
	e := newTestEnv(t)
	e.submit.delay = 2 * time.Second
	e.login()
	addr, stop := e.serveStoppable()

	sent := make(chan error, 1)
	go func() { sent <- send(addr, nil, "hello@example.com") }()
	time.Sleep(500 * time.Millisecond)
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	time.Sleep(200 * time.Millisecond)

	r := dialRaw(t, addr)
	if code, msg := r.cmd("EHLO client"); code != 250 {
		t.Fatalf("EHLO gave %d %s", code, msg)
	}
	if code, msg := r.cmd("MAIL FROM:<shop@example.org>"); code != 421 {
		t.Errorf("MAIL while stopping gave %d %q, want 421", code, msg)
	}
	if err := <-sent; err != nil {
		t.Errorf("the delivery in progress failed: %v", err)
	}
	<-stopped
}

func TestStoppingIsQuickWhenAClientStaysConnectedAfterItsMessage(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	addr, stop := e.serveStoppable()

	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Mail("juergen@example.org"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("hello@example.com"); err != nil {
		t.Fatal(err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(testMessage)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	e.waitForNoSentCopy()

	start := time.Now()
	stop()
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("stopping took %s with a client that is done with its message", took.Round(time.Millisecond))
	}
}

func TestMoreThanFiftyRecipientsAreRefused(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	r := dialRaw(t, e.serve())

	if code, msg := r.cmd("EHLO client"); code != 250 {
		t.Fatalf("EHLO gave %d %s", code, msg)
	}
	if code, msg := r.cmd("MAIL FROM:<shop@example.org>"); code != 250 {
		t.Fatalf("MAIL gave %d %s", code, msg)
	}
	for i := range 50 {
		if code, msg := r.cmd("RCPT TO:<r%d@example.com>", i); code != 250 {
			t.Fatalf("recipient %d gave %d %s", i+1, code, msg)
		}
	}
	if code, msg := r.cmd("RCPT TO:<r50@example.com>"); code != 452 {
		t.Errorf("recipient 51 gave %d %q, want 452", code, msg)
	}
}

func TestTheEleventhConnectionWaitsForAFreeOne(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	addr := e.serve()

	var conns []net.Conn
	for range maxConnections {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}

	extra, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	r := bufio.NewReader(extra)
	extra.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := r.ReadString('\n'); err == nil {
		t.Fatal("an eleventh connection was served")
	}

	conns[0].Close()
	extra.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := r.ReadString('\n'); err != nil {
		t.Errorf("the connection was not served after one was freed: %v", err)
	}
}

func TestLinesLongerThanACommandArriveUnchanged(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	addr := e.serve()

	long := strings.Repeat("x", 10000)
	literal := "From: Shop <shop@example.org>\r\nSubject: Long\r\nX-Long: " + long + "\r\n\r\n" + long + "\r\n"
	if err := sendLiteral(addr, literal); err != nil {
		t.Fatal(err)
	}
	sent := e.submit.messages()
	if len(sent) != 1 || !bytes.Contains(sent[0], []byte("\r\nX-Long: "+long+"\r\n")) || !bytes.HasSuffix(sent[0], []byte("\r\n\r\n"+long+"\r\n")) {
		t.Fatalf("Proton Mail got %d messages, the first of %d bytes", len(sent), len(bytes.Join(sent, nil)))
	}
}

func TestTooLargeChunkedMessageIsRefused(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.maxSize = 100
	e.login()
	r := dialRaw(t, e.serve())

	if code, msg := r.cmd("EHLO client"); code != 250 {
		t.Fatalf("EHLO gave %d %s", code, msg)
	}
	if code, msg := r.cmd("MAIL FROM:<shop@example.org>"); code != 250 {
		t.Fatalf("MAIL gave %d %s", code, msg)
	}
	if code, msg := r.cmd("RCPT TO:<hello@example.com>"); code != 250 {
		t.Fatalf("RCPT gave %d %s", code, msg)
	}
	chunk := "Subject: Big\r\n\r\n" + strings.Repeat("x", 60) + "\r\n"
	if code, msg := r.bdat(chunk, false); code != 250 {
		t.Fatalf("first BDAT gave %d %s", code, msg)
	}
	if code, msg := r.bdat(chunk, true); code != 552 {
		t.Errorf("last BDAT gave %d %q, want 552", code, msg)
	}
	if got := len(e.submit.messages()); got != 0 {
		t.Errorf("Proton Mail got %d messages", got)
	}
}

func TestServeListensOnTheConfiguredAddress(t *testing.T) {
	e := newTestEnv(t)
	e.cfg.listen = "nonsense"
	if err := serve(context.Background(), e.cfg, e.manager()); err == nil {
		t.Error("serve accepted a listen address without a port")
	}

	e.cfg.listen = "127.0.0.1:0"
	logged := captureLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, e.cfg, e.manager()) }()

	deadline := time.Now().Add(10 * time.Second)
	var addr string
	for addr == "" {
		if _, after, ok := strings.Cut(logged.String(), " listening on "); ok {
			addr, _, _ = strings.Cut(after, "\n")
		} else if time.Now().After(deadline) {
			t.Fatal("serve did not start")
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("listening on %s", addr)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
}

func TestServeReturnsWhenTheListenerBreaks(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	listener, err := net.Listen("tcp", e.cfg.listen)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serveOn(context.Background(), e.cfg, e.manager(), listener) }()
	time.Sleep(300 * time.Millisecond)

	listener.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("serving went on without a listener")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveOn did not return after the listener broke")
	}
}

func TestASessionThatComesBackIsNoticedByTheNextCheck(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.retryEvery = 200 * time.Millisecond
	cfg := e.cfg
	cfg.listen = e.serve()
	if err := healthcheck(cfg); err == nil {
		t.Fatal("healthy without a session")
	}

	e.login()
	deadline := time.Now().Add(10 * time.Second)
	for healthcheck(cfg) != nil {
		if time.Now().After(deadline) {
			t.Fatal("no check ran after the login")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestChunkedMessageExactlyAtTheSizeLimitIsAccepted(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.maxSize = 200
	e.login()
	r := dialRaw(t, e.serve())
	r.envelope()

	if code, msg := r.bdat(literalOfSize(200), true); code != 250 {
		t.Errorf("BDAT gave %d %q, want 250", code, msg)
	}
	if got := len(e.submit.messages()); got != 1 {
		t.Errorf("Proton Mail got %d messages, want 1", got)
	}
}

func TestMessageOneByteOverTheSizeLimitIsRefused(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.maxSize = 200
	e.login()
	r := dialRaw(t, e.serve())
	r.envelope()

	if code, msg := r.data(literalOfSize(201)); code != 552 {
		t.Errorf("got %d %q, want 552", code, msg)
	}
	if got := len(e.submit.messages()); got != 0 {
		t.Errorf("Proton Mail got %d messages", got)
	}
	if code, msg := r.cmd("NOOP"); code != 250 {
		t.Errorf("the connection is unusable after a refused message: %d %q", code, msg)
	}
}

func TestNullSenderIsForwarded(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	r := dialRaw(t, e.serve())

	if code, msg := r.cmd("EHLO client"); code != 250 {
		t.Fatalf("EHLO gave %d %s", code, msg)
	}
	if code, msg := r.cmd("MAIL FROM:<>"); code != 250 {
		t.Fatalf("MAIL FROM:<> gave %d %s", code, msg)
	}
	if code, msg := r.cmd("RCPT TO:<hello@example.com>"); code != 250 {
		t.Fatalf("RCPT gave %d %s", code, msg)
	}
	if code, msg := r.data(testMessage); code != 250 {
		t.Fatalf("DATA gave %d %s", code, msg)
	}
	if got := len(e.inbox()); got != 1 {
		t.Errorf("inbox has %d messages, want 1", got)
	}
}

func TestEmptyMessageIsForwarded(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	r := dialRaw(t, e.serve())
	r.envelope()

	if code, msg := r.data(""); code != 250 {
		t.Fatalf("DATA gave %d %s", code, msg)
	}
	sent := e.submit.messages()
	if len(sent) != 1 {
		t.Fatalf("Proton Mail got %d messages, want 1", len(sent))
	}
	if from, _ := rfc822.GetHeaderValue(sent[0], "From"); from != "<"+e.submit.sender+">" {
		t.Errorf("From is %q", from)
	}
}

func TestPipelinedSessionIsServed(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	r := dialRaw(t, e.serve())

	fmt.Fprintf(r.c.W, "EHLO client\r\nMAIL FROM:<shop@example.org>\r\nRCPT TO:<hello@example.com>\r\nDATA\r\n%s.\r\nQUIT\r\n", testMessage)
	if err := r.c.W.Flush(); err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{250, 250, 250, 354, 250, 221} {
		if code, msg := r.reply(); code != want {
			t.Fatalf("reply %d is %d %q, want %d", i+1, code, msg, want)
		}
	}
	if got := len(e.inbox()); got != 1 {
		t.Errorf("inbox has %d messages, want 1", got)
	}
}

func TestSecondMailWithoutResetCountsOnce(t *testing.T) {
	b := &backend{}
	s, err := b.NewSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Mail("shop@example.org", nil); err != nil {
			t.Fatal(err)
		}
	}
	if b.busy != 1 {
		t.Errorf("%d sessions are busy after two MAIL commands, want 1", b.busy)
	}
	s.Reset()
	if b.busy != 0 {
		t.Errorf("%d sessions are busy after RSET, want 0", b.busy)
	}
	if err := s.Mail("shop@example.org", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Logout(); err != nil {
		t.Fatal(err)
	}
	if b.busy != 0 {
		t.Errorf("%d sessions are busy after QUIT, want 0", b.busy)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !b.drain(ctx) {
		t.Error("drain waits although nothing is busy")
	}
}

func TestClientThatHangsUpDuringDataFreesTheServer(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	addr, stop := e.serveStoppable()
	r := dialRaw(t, addr)
	r.envelope()

	if code, msg := r.cmd("DATA"); code != 354 {
		t.Fatalf("DATA gave %d %s", code, msg)
	}
	fmt.Fprint(r.c.W, testMessage[:20])
	if err := r.c.W.Flush(); err != nil {
		t.Fatal(err)
	}
	r.c.Close()
	time.Sleep(200 * time.Millisecond)

	start := time.Now()
	stop()
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("stopping took %s after a client hung up during DATA", took.Round(time.Millisecond))
	}
	if got := len(e.submit.messages()); got != 0 {
		t.Errorf("Proton Mail got %d messages from a client that hung up", got)
	}
}

func TestProtonReplyTextAtDataIsNotLogged(t *testing.T) {
	for _, code := range []int{450, 554} {
		e := newTestEnv(t)
		e.submit.dataErr.Store(&gosmtp.SMTPError{Code: code, EnhancedCode: gosmtp.EnhancedCode{5, 7, 1}, Message: "<" + e.submit.sender + ">: Message refused"})
		e.login()
		addr := e.serve()
		logged := captureLog(t)

		if err := send(addr, nil, "hello@example.com"); err == nil {
			t.Fatalf("%d: the message was accepted", code)
		}
		if strings.Contains(logged.String(), e.submit.sender) {
			t.Errorf("%d: the log has the address:\n%s", code, logged)
		}
		if status, _ := os.ReadFile(statusPath(e.cfg)); strings.Contains(string(status), e.submit.sender) {
			t.Errorf("%d: the status has the address: %s", code, status)
		}
	}
}

func TestMessageWithBareLineFeedsArrivesWithCRLF(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	r := dialRaw(t, e.serve())
	r.envelope()

	if code, msg := r.data("From: A <a@example.org>\nSubject: Bare\n\nline one\nline two\r\n"); code != 250 {
		t.Fatalf("DATA gave %d %s", code, msg)
	}
	sent := e.submit.messages()
	if len(sent) != 1 {
		t.Fatalf("Proton Mail got %d messages, want 1", len(sent))
	}
	if bytes.Contains(bytes.ReplaceAll(sent[0], []byte("\r\n"), nil), []byte("\n")) {
		t.Errorf("Proton Mail got bare line feeds:\n%q", sent[0])
	}
	if !bytes.HasSuffix(sent[0], []byte("\r\n\r\nline one\r\nline two\r\n")) {
		t.Errorf("the body changed:\n%q", sent[0])
	}
}

func TestClientThatStallsDuringDataIsDeferred(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.readTimeout = time.Second
	r := dialRaw(t, e.serve())
	r.envelope()

	if code, msg := r.cmd("DATA"); code != 354 {
		t.Fatalf("DATA gave %d %s", code, msg)
	}
	fmt.Fprint(r.c.W, testMessage[:20])
	if err := r.c.W.Flush(); err != nil {
		t.Fatal(err)
	}
	if code, msg := r.reply(); code != 451 {
		t.Errorf("a stalled upload gave %d %q, want 451", code, msg)
	}
	if got := len(e.submit.messages()); got != 0 {
		t.Errorf("Proton Mail got %d messages from a stalled upload", got)
	}
}

func TestSenderWithUTF8AddressIsForwarded(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	r := dialRaw(t, e.serve())

	if code, msg := r.cmd("EHLO client"); code != 250 || !strings.Contains(msg, "SMTPUTF8") {
		t.Fatalf("EHLO gave %d %q, want SMTPUTF8", code, msg)
	}
	if code, msg := r.cmd("MAIL FROM:<jürgen@bücher.example> SMTPUTF8"); code != 250 {
		t.Fatalf("MAIL with SMTPUTF8 gave %d %s", code, msg)
	}
	if code, msg := r.cmd("RCPT TO:<hello@bücher.example>"); code != 250 {
		t.Fatalf("RCPT gave %d %s", code, msg)
	}
	if code, msg := r.data("From: Jürgen Müller <jürgen@bücher.example>\r\nSubject: Grüße\r\n\r\nHallo\r\n"); code != 250 {
		t.Fatalf("DATA gave %d %s", code, msg)
	}

	sent := e.submit.messages()
	if len(sent) != 1 {
		t.Fatalf("Proton Mail got %d messages, want 1", len(sent))
	}
	h, err := rfc822.NewHeader(rfc822.Parse(sent[0]).Header())
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get("Reply-To"); got != "Jürgen Müller <jürgen@bücher.example>" {
		t.Errorf("Reply-To is %q", got)
	}
	if got := h.Get("From"); !strings.HasSuffix(got, "<"+e.submit.sender+">") || !strings.Contains(got, "=?utf-8?") {
		t.Errorf("From is %q, want the encoded name and the own address", got)
	}
}

func TestSlowButSteadyUploadIsAccepted(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.readTimeout = time.Second
	e.login()
	r := dialRaw(t, e.serve())
	r.envelope()

	if code, msg := r.cmd("DATA"); code != 354 {
		t.Fatalf("DATA gave %d %s", code, msg)
	}
	literal := literalOfSize(64<<10) + ".\r\n"
	for len(literal) > 0 {
		n := min(4096, len(literal))
		fmt.Fprint(r.c.W, literal[:n])
		if err := r.c.W.Flush(); err != nil {
			t.Fatalf("the server hung up during a steady upload: %v", err)
		}
		literal = literal[n:]
		time.Sleep(150 * time.Millisecond)
	}
	if code, msg := r.reply(); code != 250 {
		t.Errorf("an upload that never pauses for a second gave %d %q, want 250", code, msg)
	}
}
