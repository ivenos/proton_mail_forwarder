package main

import (
	"context"
	"errors"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/gluon/rfc822"
	"github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/go-proton-api/server"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
)

const (
	testUser     = "user"
	testPassword = "password"
)

type scriptedPrompter struct {
	answers []string
	told    []string
}

func (p *scriptedPrompter) ask(question string) (string, error) {
	if len(p.answers) == 0 {
		return "", errors.New("unexpected question: " + question)
	}
	answer := p.answers[0]
	p.answers = p.answers[1:]
	return answer, nil
}

func (p *scriptedPrompter) askSecret(question string) ([]byte, error) {
	answer, err := p.ask(question)
	return []byte(answer), err
}

func (p *scriptedPrompter) tell(text string) { p.told = append(p.told, text) }

type testEnv struct {
	t       *testing.T
	server  *server.Server
	userID  string
	addrID  string
	cfg     config
	serveBy *proton.Manager
	submit  *fakeSubmission
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	s := server.New()
	t.Cleanup(s.Close)

	userID, addrID, err := s.CreateUser(testUser, []byte(testPassword))
	if err != nil {
		t.Fatal(err)
	}

	e := &testEnv{
		t:      t,
		server: s,
		userID: userID,
		addrID: addrID,
		cfg: config{
			listen:          "127.0.0.1:0",
			dataDir:         t.TempDir(),
			maxSize:         1 << 20,
			checkTimeout:    time.Minute,
			deliveryTimeout: 2 * time.Minute,
			removalWaits:    []time.Duration{50 * time.Millisecond, 200 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second},
			checkEvery:      time.Hour,
			retryEvery:      time.Minute,
		},
	}
	e.submit = newFakeSubmission(e, testUser+"@proton.local")
	e.cfg.submission = e.submit.submission()
	return e
}

func (e *testEnv) manager() *proton.Manager {
	return newManager(proton.WithHostURL(e.server.GetHostURL()), proton.WithTransport(proton.InsecureTransport()))
}

func (e *testEnv) login() {
	e.t.Helper()
	p := &scriptedPrompter{answers: []string{testUser, testPassword}}
	if err := login(context.Background(), e.cfg, e.manager(), p); err != nil {
		e.t.Fatal(err)
	}
}

// serve starts the SMTP server and returns its address once the first session check has run, and a function that stops it.
func (e *testEnv) serveStoppable() (string, func()) {
	e.t.Helper()

	listener, err := net.Listen("tcp", e.cfg.listen)
	if err != nil {
		e.t.Fatal(err)
	}

	cfg := e.cfg
	os.Remove(statusPath(cfg))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	m := e.serveBy
	if m == nil {
		m = e.manager()
	}
	go func() { done <- serveOn(ctx, cfg, m, listener) }()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				e.t.Error(err)
			}
		})
	}
	e.t.Cleanup(stop)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := os.ReadFile(statusPath(cfg))
		if text := strings.TrimSpace(string(status)); text != "" && text != "starting" {
			return listener.Addr().String(), stop
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatal("the first session check did not finish")
	return "", stop
}

func (e *testEnv) serve() string {
	addr, _ := e.serveStoppable()
	return addr
}

func (e *testEnv) inbox() []proton.MessageMetadata {
	e.t.Helper()

	c, _, err := e.manager().NewClientWithLogin(context.Background(), testUser, []byte(testPassword))
	if err != nil {
		e.t.Fatal(err)
	}
	defer c.Close()

	messages, err := c.GetMessageMetadata(context.Background(), proton.MessageFilter{LabelID: proton.InboxLabel})
	if err != nil {
		e.t.Fatal(err)
	}
	return messages
}

// sentCopies counts the messages under Sent.
func (e *testEnv) sentCopies() int {
	e.t.Helper()

	c, _, err := e.manager().NewClientWithLogin(context.Background(), testUser, []byte(testPassword))
	if err != nil {
		e.t.Fatal(err)
	}
	defer c.Close()

	messages, err := c.GetMessageMetadata(context.Background(), proton.MessageFilter{LabelID: proton.SentLabel})
	if err != nil {
		e.t.Fatal(err)
	}
	return len(messages)
}

func (e *testEnv) waitForNoSentCopy() {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.sentCopies() > 0 {
		if time.Now().After(deadline) {
			e.t.Fatal("the copy under Sent was not removed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *testEnv) body(messageID string) string {
	e.t.Helper()
	ctx := context.Background()

	c, addrKR := e.unlock(ctx, "")
	defer c.Close()

	message, err := c.GetMessage(ctx, messageID)
	if err != nil {
		e.t.Fatal(err)
	}
	body, err := message.Decrypt(addrKR)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(body)
}

// unlock signs in as the test user and unlocks the address with the given email, or else the first.
func (e *testEnv) unlock(ctx context.Context, email string) (*proton.Client, *crypto.KeyRing) {
	e.t.Helper()

	c, _, err := e.manager().NewClientWithLogin(ctx, testUser, []byte(testPassword))
	if err != nil {
		e.t.Fatal(err)
	}

	user, err := c.GetUser(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	salts, err := c.GetSalts(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	keyPass, err := salts.SaltForKey([]byte(testPassword), user.Keys.Primary().ID)
	if err != nil {
		e.t.Fatal(err)
	}
	userKR, err := user.Keys.Unlock(keyPass, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	addresses, err := c.GetAddresses(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	addr := addresses[0]
	for _, a := range addresses {
		if a.Email == email {
			addr = a
		}
	}
	addrKR, err := addr.Keys.Unlock(keyPass, userKR)
	if err != nil {
		e.t.Fatal(err)
	}
	return c, addrKR
}

const testMessage = "From: Jürgen Müller <juergen@example.org>\r\n" +
	"To: hello@example.com\r\n" +
	"Subject: Contact form\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Does it run on a Raspberry Pi?\r\n"

func send(addr string, auth smtp.Auth, to string) error {
	return smtp.SendMail(addr, auth, "juergen@example.org", []string{to}, []byte(testMessage))
}

func TestMessageLandsUnreadInTheInboxWithoutASentCopy(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	addr := e.serve()

	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}
	e.waitForNoSentCopy()

	messages := e.inbox()
	if len(messages) != 1 {
		t.Fatalf("inbox has %d messages, want 1", len(messages))
	}
	m := messages[0]
	if !bool(m.Unread) {
		t.Error("message is read")
	}
	if m.Flags&proton.MessageFlagReceived == 0 || m.Flags&proton.MessageFlagSent != 0 {
		t.Errorf("flags %b do not mark a received message", m.Flags)
	}
	if m.Subject != "Contact form" || m.Sender.Address != e.cfg.submission.sender || m.Sender.Name != "Jürgen Müller" {
		t.Errorf("got subject %q from %v", m.Subject, m.Sender)
	}
	if body := e.body(m.ID); !strings.Contains(body, "Does it run on a Raspberry Pi?") {
		t.Errorf("body is %q", body)
	}

	sent := e.submit.messages()
	if len(sent) != 1 {
		t.Fatalf("Proton Mail got %d messages, want 1", len(sent))
	}
	if got, _ := rfc822.GetHeaderValue(sent[0], "Reply-To"); got != "Jürgen Müller <juergen@example.org>" {
		t.Errorf("Reply-To is %q", got)
	}

	status, err := os.ReadFile(statusPath(e.cfg))
	if err != nil || strings.TrimSpace(string(status)) != statusOK {
		t.Errorf("status is %q (%v)", status, err)
	}
}

func TestSentCopyThatShowsUpLateIsRemoved(t *testing.T) {
	e := newTestEnv(t)
	e.submit.lag = 700 * time.Millisecond
	e.cfg.removalWaits = append(e.cfg.removalWaits, 2*time.Second, 4*time.Second)
	e.login()
	addr := e.serve()

	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	e.waitForNoSentCopy()

	if got := len(e.inbox()); got != 1 {
		t.Fatalf("inbox has %d messages, want 1", got)
	}
}

func TestSentCopyOfAMessageThatTimedOutIsRemoved(t *testing.T) {
	e := newTestEnv(t)
	e.cfg.deliveryTimeout = time.Second
	e.submit.delay = 2 * time.Second
	e.login()
	addr := e.serve()

	if err := send(addr, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
		t.Fatalf("got %v, want a 451 reply", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(e.inbox()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Proton Mail did not keep the message")
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.waitForNoSentCopy()
}

func TestUnconfirmedMessageWithoutASentCopyIsForgotten(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.removalWaits = []time.Duration{10 * time.Millisecond}
	e.submit.hangUp.Store(true)
	e.login()
	mb := newMailbox(e.manager(), e.cfg)
	defer mb.disconnect()
	ctx := context.Background()

	if _, err := mb.deliver(ctx, []string{"hello@example.com"}, []byte(testMessage)); !errors.Is(err, errUnconfirmed) {
		t.Fatalf("got %v, want errUnconfirmed", err)
	}
	mb.removals.Wait()
	if err := mb.check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.status(); got != statusOK {
		t.Errorf("status is %q", got)
	}
	if got := loadCopies(copiesPath(e.cfg)); len(got) != 0 {
		t.Errorf("sent-copies.json still holds %v", got)
	}
}

func TestRefusedMessageIsNotRetried(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.submit.refuse.Store(true)
	e.login()
	cfg := e.cfg
	cfg.listen = e.serve()

	if err := send(cfg.listen, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "554") {
		t.Fatalf("got %v, want a 554 reply", err)
	}
	if err := healthcheck(cfg); err != nil {
		t.Errorf("one refused message made the healthcheck fail: %v", err)
	}
}

func TestRefusedTokenIsReported(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.submission.token = "revoked"
	e.login()
	cfg := e.cfg
	cfg.listen = e.serve()

	if err := healthcheck(cfg); err == nil || !strings.Contains(err.Error(), "SMTP token") {
		t.Errorf("healthcheck gave %v", err)
	}
	if err := send(cfg.listen, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
		t.Fatalf("got %v, want a 451 reply", err)
	}
}

func TestSessionSurvivesARestart(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.server.SetAuthLife(time.Second)
	e.login()

	first, _ := loadSession(sessionPath(e.cfg))
	addr, stop := e.serveStoppable()
	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}
	stop()

	time.Sleep(2 * time.Second)
	addr = e.serve()
	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}

	if got := len(e.inbox()); got != 2 {
		t.Fatalf("inbox has %d messages, want 2", got)
	}
	if latest, _ := loadSession(sessionPath(e.cfg)); latest.RefreshToken == first.RefreshToken {
		t.Error("the refreshed token was not saved")
	}
}

func TestMailIsDeferredWhenNotSignedIn(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	addr := e.serve()

	if err := send(addr, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
		t.Fatalf("got %v, want a 451 reply", err)
	}

	if err := healthcheck(e.cfg); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("healthcheck gave %v", err)
	}
}

func TestSigningInLaterNeedsNoRestart(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	addr := e.serve()
	e.login()

	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := len(e.inbox()); got != 1 {
		t.Fatalf("inbox has %d messages, want 1", got)
	}
}

func TestRevokedSessionIsReported(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	addr := e.serve()

	if err := e.server.RevokeUser(e.userID); err != nil {
		t.Fatal(err)
	}

	if err := send(addr, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
		t.Fatalf("got %v, want a 451 reply", err)
	}
	if err := healthcheck(e.cfg); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("healthcheck gave %v", err)
	}
}

func TestSMTPAuthentication(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.username, e.cfg.password = "app", "secret"
	e.login()
	addr := e.serve()
	host, _, _ := net.SplitHostPort(addr)

	if err := send(addr, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "530") {
		t.Errorf("without credentials: got %v, want a 530 reply", err)
	}
	if err := send(addr, smtp.PlainAuth("", "app", "wrong", host), "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "535") {
		t.Errorf("with a wrong password: got %v, want a 535 reply", err)
	}
	if err := send(addr, smtp.PlainAuth("", "app", "secret", host), "hello@example.com"); err != nil {
		t.Errorf("with the right password: %v", err)
	}

	if got := len(e.inbox()); got != 1 {
		t.Fatalf("inbox has %d messages, want 1", got)
	}
}

func TestTooLargeMessageIsRefused(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.maxSize = 100
	e.login()
	addr := e.serve()

	if err := send(addr, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "552") {
		t.Fatalf("got %v, want a 552 reply", err)
	}
}

func TestHealthcheckPassesWhenSignedIn(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	cfg := e.cfg
	cfg.listen = e.serve()

	if err := healthcheck(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestHealthcheckFailsWhenNothingListens(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{dataDir: t.TempDir(), listen: l.Addr().String()}
	l.Close()
	if err := os.WriteFile(statusPath(cfg), []byte(statusOK+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := healthcheck(cfg); err == nil {
		t.Error("healthcheck passed while nothing listens")
	}
}

func TestHealthcheckFailsWhenTheChecksStop(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cfg := config{dataDir: t.TempDir(), listen: l.Addr().String()}
	if err := os.WriteFile(statusPath(cfg), []byte(statusOK+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := healthcheck(cfg); err != nil {
		t.Fatalf("with a fresh status: %v", err)
	}

	stale := time.Now().Add(-3 * checkInterval)
	if err := os.Chtimes(statusPath(cfg), stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := healthcheck(cfg); err == nil {
		t.Error("healthcheck passed although nothing checked for three hours")
	}
}

func TestMessageGoesToTheAddressARecipientNames(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	second, err := e.server.CreateAddress(e.userID, "alias@proton.local", []byte(testPassword), true)
	if err != nil {
		t.Fatal(err)
	}
	e.login()
	addr := e.serve()

	if err := send(addr, nil, "ALIAS@proton.local"); err != nil {
		t.Fatal(err)
	}

	messages := e.inbox()
	if len(messages) != 1 || messages[0].AddressID != second {
		t.Fatalf("got %+v, want one message for %s", messages, second)
	}
}

func TestPickAddress(t *testing.T) {
	enabled := proton.AddressStatusEnabled
	addresses := []proton.Address{
		{ID: "off", Email: "off@example.com", Status: proton.AddressStatusDisabled, Order: 0},
		{ID: "second", Email: "second@example.com", Status: enabled, Order: 2},
		{ID: "first", Email: "first@example.com", Status: enabled, Order: 1},
	}

	for _, tc := range []struct {
		recipients []string
		want       string
	}{
		{[]string{"Second@Example.com"}, "second"},
		{[]string{"someone@else.com"}, "first"},
		{[]string{"off@example.com"}, "first"},
		{nil, "first"},
	} {
		got, err := pickAddress(addresses, tc.recipients)
		if err != nil || got.ID != tc.want {
			t.Errorf("pickAddress(%v) = %s, %v; want %s", tc.recipients, got.ID, err, tc.want)
		}
	}

	if _, err := pickAddress(addresses[:1], nil); err == nil {
		t.Error("an account without enabled addresses gave no error")
	}
}

func TestLoadConfig(t *testing.T) {
	for _, tc := range []struct {
		env   map[string]string
		valid bool
	}{
		{map[string]string{}, true},
		{map[string]string{"SMTP_USERNAME": "app", "SMTP_PASSWORD": "secret"}, true},
		{map[string]string{"SMTP_USERNAME": "app"}, false},
		{map[string]string{"SMTP_PASSWORD": "secret"}, false},
		{map[string]string{"MAX_MESSAGE_SIZE": "1048576"}, true},
		{map[string]string{"MAX_MESSAGE_SIZE": "0"}, false},
		{map[string]string{"MAX_MESSAGE_SIZE": "1MB"}, false},
		{map[string]string{"MAX_MESSAGE_SIZE": "36480000"}, true},
		{map[string]string{"MAX_MESSAGE_SIZE": "36480001"}, false},
		{map[string]string{"PROTON_SMTP_USERNAME": "hello@example.com", "PROTON_SMTP_TOKEN": "token"}, true},
		{map[string]string{"PROTON_SMTP_USERNAME": "hello@example.com"}, false},
		{map[string]string{"PROTON_SMTP_TOKEN": "token"}, false},
		{map[string]string{"PROTON_SMTP_USERNAME": "hello", "PROTON_SMTP_TOKEN": "token"}, false},
	} {
		for _, key := range []string{"SMTP_LISTEN", "SMTP_USERNAME", "SMTP_PASSWORD", "DATA_DIR", "MAX_MESSAGE_SIZE", "PROTON_SMTP_USERNAME", "PROTON_SMTP_TOKEN"} {
			t.Setenv(key, tc.env[key])
		}
		if _, err := loadConfig(); (err == nil) != tc.valid {
			t.Errorf("loadConfig with %v: %v", tc.env, err)
		}
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	for _, key := range []string{"SMTP_LISTEN", "SMTP_USERNAME", "SMTP_PASSWORD", "DATA_DIR", "MAX_MESSAGE_SIZE", "PROTON_SMTP_USERNAME", "PROTON_SMTP_TOKEN"} {
		t.Setenv(key, "")
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.listen != "127.0.0.1:2525" || cfg.maxSize != 26214400 ||
		cfg.dataDir != filepath.Join(configDir, "proton_mail_forwarder") || cfg.submission.addr != "smtp.protonmail.ch:587" {
		t.Errorf("defaults differ from the README: listen %q, size %d, data %q, submission %q",
			cfg.listen, cfg.maxSize, cfg.dataDir, cfg.submission.addr)
	}
	if cfg.checkEvery != time.Hour || cfg.retryEvery != time.Minute {
		t.Errorf("checks every %v and retries every %v, want hourly and every minute", cfg.checkEvery, cfg.retryEvery)
	}
}

func TestLoadConfigReadsEveryVariable(t *testing.T) {
	for key, value := range map[string]string{
		"SMTP_LISTEN": "0.0.0.0:2525", "SMTP_USERNAME": "app", "SMTP_PASSWORD": "secret", "DATA_DIR": "/srv/forwarder",
		"MAX_MESSAGE_SIZE": "1048576", "PROTON_SMTP_USERNAME": "hello@example.com", "PROTON_SMTP_TOKEN": "token",
	} {
		t.Setenv(key, value)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := config{
		listen: "0.0.0.0:2525", username: "app", password: "secret", dataDir: "/srv/forwarder", maxSize: 1 << 20,
		submission:   submission{addr: "smtp.protonmail.ch:587", sender: "hello@example.com", token: "token"},
		checkTimeout: cfg.checkTimeout, deliveryTimeout: cfg.deliveryTimeout, removalWaits: cfg.removalWaits,
		checkEvery: cfg.checkEvery, retryEvery: cfg.retryEvery,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigWithoutDataDirNeedsAConfigDir(t *testing.T) {
	for _, key := range []string{"DATA_DIR", "HOME", "XDG_CONFIG_HOME", "AppData", "SMTP_USERNAME", "SMTP_PASSWORD", "MAX_MESSAGE_SIZE", "PROTON_SMTP_USERNAME", "PROTON_SMTP_TOKEN"} {
		t.Setenv(key, "")
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "DATA_DIR") {
		t.Errorf("got %v, want an error that names DATA_DIR", err)
	}
}

func TestWritableTellsAFolderThatCannotBeWrittenTo(t *testing.T) {
	if err := writable(t.TempDir()); err != nil {
		t.Error(err)
	}
	if err := writable(filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "cannot write to") {
		t.Errorf("got %v", err)
	}
}

func TestHealthcheckFailsWithoutAStatus(t *testing.T) {
	cfg := config{dataDir: t.TempDir(), listen: "127.0.0.1:2525"}
	if err := healthcheck(cfg); err == nil || !strings.Contains(err.Error(), "no status yet") {
		t.Errorf("got %v", err)
	}
}

func TestHealthcheckFailsOnceTwoChecksWereMissed(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cfg := config{dataDir: t.TempDir(), listen: l.Addr().String()}
	if err := os.WriteFile(statusPath(cfg), []byte(statusOK+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		age     time.Duration
		healthy bool
	}{
		{2*checkInterval - time.Minute, true},
		{2*checkInterval + time.Minute, false},
	} {
		at := time.Now().Add(-tc.age)
		if err := os.Chtimes(statusPath(cfg), at, at); err != nil {
			t.Fatal(err)
		}
		if err := healthcheck(cfg); (err == nil) != tc.healthy {
			t.Errorf("with a status from %v ago: %v", tc.age, err)
		}
	}
}

func TestHealthcheckDialsLocalhostForAWildcardListen(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	cfg := config{dataDir: t.TempDir()}
	if err := os.WriteFile(statusPath(cfg), []byte(statusOK+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, cfg.listen = range []string{":" + port, "0.0.0.0:" + port, "[::]:" + port} {
		if err := healthcheck(cfg); err != nil {
			t.Errorf("listening on %s: %v", cfg.listen, err)
		}
	}
	cfg.listen = "nonsense"
	if err := healthcheck(cfg); err == nil {
		t.Error("a listen address without a port passed")
	}
}
