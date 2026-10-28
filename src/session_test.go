package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/go-proton-api"
	"github.com/go-resty/resty/v2"
	"golang.org/x/term"
)

// hookTransport lets a test hold or rewrite requests to the fake API.
type hookTransport struct {
	base    http.RoundTripper
	down    atomic.Bool
	request atomic.Pointer[func(*http.Request)]
	auth    atomic.Pointer[func(map[string]any)]
}

func (h *hookTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if h.down.Load() {
		return nil, errors.New("API down")
	}
	if hook := h.request.Load(); hook != nil {
		(*hook)(r)
	}
	res, err := h.base.RoundTrip(r)
	hook := h.auth.Load()
	if err != nil || hook == nil || r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/auth/v4") || res.StatusCode != http.StatusOK {
		return res, err
	}

	var body map[string]any
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	(*hook)(body)
	data, _ = json.Marshal(body)
	res.Body = io.NopCloser(bytes.NewReader(data))
	res.ContentLength = int64(len(data))
	res.Header.Del("Content-Length")
	return res, nil
}

func (e *testEnv) hookedManager() (*proton.Manager, *hookTransport) {
	h := &hookTransport{base: proton.InsecureTransport()}
	return newManager(proton.WithHostURL(e.server.GetHostURL()), proton.WithTransport(h)), h
}

func (e *testEnv) inboxOf(username, password string) int {
	e.t.Helper()
	c, _, err := e.manager().NewClientWithLogin(context.Background(), username, []byte(password))
	if err != nil {
		e.t.Fatal(err)
	}
	defer c.Close()
	messages, err := c.GetMessageMetadata(context.Background(), proton.MessageFilter{LabelID: proton.InboxLabel})
	if err != nil {
		e.t.Fatal(err)
	}
	return len(messages)
}

// openSessions counts the sessions of the test user other than the one it opens to ask.
func (e *testEnv) openSessions() int {
	e.t.Helper()

	c, _, err := e.manager().NewClientWithLogin(context.Background(), testUser, []byte(testPassword))
	if err != nil {
		e.t.Fatal(err)
	}
	defer c.Close()
	defer c.AuthDelete(context.Background())

	sessions, err := c.AuthSessions(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return len(sessions) - 1
}

func (e *testEnv) status() string {
	e.t.Helper()
	status, err := os.ReadFile(statusPath(e.cfg))
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.TrimSpace(string(status))
}

func TestNewLoginWhileServingTakesOver(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.server.SetAuthLife(time.Second)
	if _, _, err := e.server.CreateUser("other", []byte("other password")); err != nil {
		t.Fatal(err)
	}
	e.login()
	addr := e.serve()
	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}

	p := &scriptedPrompter{answers: []string{"other", "other password"}}
	if err := login(context.Background(), e.cfg, e.manager(), p); err != nil {
		t.Fatal(err)
	}
	other, _ := loadSession(sessionPath(e.cfg))

	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}

	if got := e.inboxOf("other", "other password"); got != 2 {
		t.Errorf("the new account got %d messages, want 2", got)
	}
	if got := len(e.inbox()); got != 1 {
		t.Errorf("the old account got %d messages, want 1", got)
	}
	if s, _ := loadSession(sessionPath(e.cfg)); s.UID != other.UID {
		t.Error("the old session was written back over the new one")
	}
}

func TestLogoutWhileServingStopsDelivery(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.server.SetAuthLife(time.Second)
	e.login()
	cfg := e.cfg
	cfg.listen = e.serve()
	if err := send(cfg.listen, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}

	if err := logout(context.Background(), e.cfg, e.manager()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)

	if err := send(cfg.listen, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
		t.Errorf("after logout: got %v, want a 451 reply", err)
	}
	if _, err := loadSession(sessionPath(e.cfg)); err == nil {
		t.Error("the session came back after logout")
	}
	if err := healthcheck(cfg); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("healthcheck gave %v", err)
	}
}

func TestFailedDeliveryMakesTheHealthcheckFail(t *testing.T) {
	e := newTestEnv(t)
	e.cfg.checkTimeout, e.cfg.deliveryTimeout = 2*time.Second, 3*time.Second
	e.submit.lag = time.Millisecond
	e.login()
	m, hook := e.hookedManager()
	e.serveBy = m
	cfg := e.cfg
	cfg.listen = e.serve()

	hook.down.Store(true)
	if err := send(cfg.listen, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
		t.Errorf("API down: got %v, want a 451 reply", err)
	}
	if err := healthcheck(cfg); err == nil {
		t.Error("healthcheck passed while delivery fails")
	}

	hook.down.Store(false)
	if err := send(cfg.listen, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := healthcheck(cfg); err != nil {
		t.Errorf("healthcheck after recovery: %v", err)
	}
}

func TestHangingAPIBlocksNothingForLong(t *testing.T) {
	e := newTestEnv(t)
	e.cfg.checkTimeout, e.cfg.deliveryTimeout = time.Second, 3*time.Second
	e.submit.lag = time.Millisecond
	e.login()
	cfg := e.cfg

	m, hook := e.hookedManager()
	hang := func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/v4/refresh") {
			<-r.Context().Done()
		}
	}
	hook.request.Store(&hang)
	e.serveBy = m
	cfg.listen = e.serve()

	if err := healthcheck(cfg); err == nil {
		t.Error("healthcheck passed before any check succeeded")
	}

	start := time.Now()
	if err := send(cfg.listen, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
		t.Errorf("got %v, want a 451 reply", err)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("the reply took %s", took)
	}

	hook.request.Store(nil)
	if err := send(cfg.listen, nil, "hello@example.com"); err != nil {
		t.Errorf("after the API recovered: %v", err)
	}
}

func TestStoppingWaitsForADelivery(t *testing.T) {
	e := newTestEnv(t)
	e.login()

	e.submit.delay = 2 * time.Second
	m, hook := e.hookedManager()
	slow := func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mail/v4/messages/delete") {
			time.Sleep(time.Second)
		}
	}
	hook.request.Store(&slow)
	e.serveBy = m
	addr, stop := e.serveStoppable()

	sent := make(chan error, 1)
	go func() { sent <- send(addr, nil, "hello@example.com") }()
	time.Sleep(700 * time.Millisecond)
	stop()

	if got := e.sentCopies(); got != 0 {
		t.Errorf("%d copies are left under Sent", got)
	}
	if err := <-sent; err != nil {
		t.Errorf("the sender saw %v, so it would send again", err)
	}
	if got := len(e.inbox()); got != 1 {
		t.Errorf("inbox has %d messages, want 1", got)
	}
}

func TestFailedLoginEndsItsSession(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.dataDir = filepath.Join(e.cfg.dataDir, "missing")

	p := &scriptedPrompter{answers: []string{testUser, testPassword}}
	if err := login(context.Background(), e.cfg, e.manager(), p); err == nil {
		t.Fatal("login without a place for the session succeeded")
	}

	if got := e.openSessions(); got != 0 {
		t.Errorf("%d sessions are left open", got)
	}
}

func TestSessionHoldsNoKeyToTheMailbox(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	m, hook := e.hookedManager()
	twoPasswords := func(body map[string]any) { body["PasswordMode"] = int(proton.TwoPasswordMode) }
	hook.auth.Store(&twoPasswords)

	p := &scriptedPrompter{answers: []string{testUser, testPassword}}
	if err := login(context.Background(), e.cfg, m, p); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(sessionPath(e.cfg))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields["uid"] == nil || fields["refresh_token"] == nil {
		t.Errorf("session.json holds %v, want only uid and refresh_token", fields)
	}
}

func TestLogoutEndsTheSessionAtProton(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	if err := logout(context.Background(), e.cfg, e.manager()); err != nil {
		t.Fatal(err)
	}
	if got := e.openSessions(); got != 0 {
		t.Errorf("%d sessions are still open after logout", got)
	}
}

func TestSentCopyLeftByAnAPIFailureIsRemovedLater(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	m, hook := e.hookedManager()
	failDelete := func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mail/v4/messages/delete") {
			r.URL.Path += "-gone"
		}
	}
	hook.request.Store(&failDelete)
	mb := newMailbox(m, e.cfg)
	defer mb.disconnect()
	ctx := context.Background()

	if _, err := mb.deliver(ctx, []string{"hello@example.com"}, []byte(testMessage)); err != nil {
		t.Fatal(err)
	}
	mb.removals.Wait()
	if got := e.status(); got != "copies left under Sent: 1" {
		t.Errorf("status is %q while the copy is left", got)
	}

	hook.request.Store(nil)
	if err := mb.check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.status(); got != statusOK {
		t.Errorf("status is %q after the copy was removed", got)
	}
	if got := e.sentCopies(); got != 0 {
		t.Errorf("%d copies are left under Sent", got)
	}
}

func TestSentCopyLeftAtAStopIsRemovedAfterTheRestart(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	m, hook := e.hookedManager()
	failDelete := func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mail/v4/messages/delete") {
			r.URL.Path += "-gone"
		}
	}
	hook.request.Store(&failDelete)
	mb := newMailbox(m, e.cfg)
	if _, err := mb.deliver(context.Background(), []string{"hello@example.com"}, []byte(testMessage)); err != nil {
		t.Fatal(err)
	}
	close(mb.stopping)
	mb.removals.Wait()
	mb.disconnect()

	restarted := newMailbox(e.manager(), e.cfg)
	defer restarted.disconnect()
	if err := restarted.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.sentCopies(); got != 0 {
		t.Errorf("%d copies are left under Sent after the restart", got)
	}
	if got := len(newMailbox(e.manager(), e.cfg).unremoved); got != 0 {
		t.Errorf("%d removed copies are still remembered", got)
	}
}

func TestSentCopyThatNeverShowsUpIsGivenUpAfterADay(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	mb := newMailbox(e.manager(), e.cfg)
	defer mb.disconnect()
	mb.unremoved["old@proton.local"] = time.Now().Add(-removalGiveUp - time.Minute)
	mb.unremoved["new@proton.local"] = time.Now()

	if err := mb.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := mb.unremoved["old@proton.local"]; ok {
		t.Error("a copy that never showed up is still looked for after a day")
	}
	if got := e.status(); got != "copies left under Sent: 1" {
		t.Errorf("status is %q", got)
	}
}

func TestStoppingClosesIdleConnections(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	addr, stop := e.serveStoppable()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	stop()
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("stopping took %s with an idle connection open", took)
	}
	if _, err := r.ReadString('\n'); err == nil {
		t.Error("the idle connection is still open")
	}
}

func (e *testEnv) sentExternalIDs() []string {
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
	var ids []string
	for _, m := range messages {
		ids = append(ids, m.ExternalID)
	}
	return ids
}

func (e *testEnv) sessionOpen(uid string) bool {
	e.t.Helper()

	c, _, err := e.manager().NewClientWithLogin(context.Background(), testUser, []byte(testPassword))
	if err != nil {
		e.t.Fatal(err)
	}
	defer c.Close()
	defer c.AuthDelete(context.Background())

	sessions, err := c.AuthSessions(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return slices.ContainsFunc(sessions, func(s proton.AuthSession) bool { return s.UID == uid })
}

func TestOwnSentMailIsKept(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	own := "From: User <" + e.submit.sender + ">\r\n" +
		"Subject: Written by hand\r\n" +
		"Message-Id: <own@proton.local>\r\n" +
		"\r\n" +
		"Hello\r\n"
	if err := e.submit.deliver([]string{e.submit.sender}, []byte(own)); err != nil {
		t.Fatal(err)
	}
	e.login()
	addr := e.serve()

	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for len(e.sentExternalIDs()) > 1 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if ids := e.sentExternalIDs(); len(ids) != 1 || ids[0] != "own@proton.local" {
		t.Errorf("Sent holds %v, want only own@proton.local", ids)
	}
}

func TestInboxCopyIsNeverDeleted(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()

	m, hook := e.hookedManager()
	anyLabel := func(r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/mail/v4/messages") || r.Body == nil {
			return
		}
		var filter map[string]any
		data, _ := io.ReadAll(r.Body)
		if json.Unmarshal(data, &filter) == nil {
			delete(filter, "LabelID")
			data, _ = json.Marshal(filter)
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		r.ContentLength = int64(len(data))
	}
	hook.request.Store(&anyLabel)
	e.serveBy = m
	addr := e.serve()

	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}
	e.waitForNoSentCopy()
	time.Sleep(300 * time.Millisecond)

	if got := len(e.inbox()); got != 1 {
		t.Errorf("inbox has %d messages, want 1", got)
	}
}

func TestTokenRotatedWhileServingSurvivesARestart(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.server.SetAuthLife(time.Second)
	e.login()

	addr, stop := e.serveStoppable()
	time.Sleep(2 * time.Second)
	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}
	e.waitForNoSentCopy()
	stop()

	addr = e.serve()
	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatalf("after the restart: %v", err)
	}
}

func TestSessionRevokedWhileStoppedIsReported(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	if err := e.server.RevokeUser(e.userID); err != nil {
		t.Fatal(err)
	}
	cfg := e.cfg
	cfg.listen = e.serve()

	if err := healthcheck(cfg); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("healthcheck gave %v", err)
	}
	if err := send(cfg.listen, nil, "hello@example.com"); err == nil || !strings.Contains(err.Error(), "Not signed in") {
		t.Errorf("got %v, want a reply that says not signed in", err)
	}
}

func TestSessionRevokedWhileIdleIsReportedByTheCheck(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	mb := newMailbox(e.manager(), e.cfg)
	defer mb.disconnect()

	if err := mb.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.server.RevokeUser(e.userID); err != nil {
		t.Fatal(err)
	}

	if err := mb.check(context.Background()); !errors.Is(err, errNotSignedIn) {
		t.Errorf("the check gave %v", err)
	}
	if got := e.status(); !strings.Contains(got, "not signed in") {
		t.Errorf("status is %q", got)
	}
}

func TestNewLoginEndsTheOldSession(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	first, _ := loadSession(sessionPath(e.cfg))
	e.login()

	if e.sessionOpen(first.UID) {
		t.Error("the session of the first login is still open at Proton")
	}
}

func TestSentCopyIsRemovedWhenStoppingRightAfterADelivery(t *testing.T) {
	e := newTestEnv(t)
	e.submit.lag = 500 * time.Millisecond
	e.login()
	addr, stop := e.serveStoppable()

	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}
	stop()
	e.submit.copies.Wait()

	if got := e.sentCopies(); got != 0 {
		t.Errorf("%d copies are left under Sent", got)
	}
}

func TestRefusedAppVersionIsReportedOnEveryPath(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()

	var refusedPath atomic.Pointer[string]
	m := newManager(proton.WithHostURL(e.server.GetHostURL()), proton.WithTransport(&cannedAPI{base: proton.InsecureTransport(), answer: func(r *http.Request) (int, string, bool) {
		if path := refusedPath.Load(); path != nil && strings.Contains(r.URL.Path, *path) {
			return 400, `{"Code":5003,"Error":"This version of the app is no longer supported, please update to continue using the app"}`, true
		}
		return 0, "", false
	}}))
	var refused atomic.Int32
	m.AddErrorHandler(proton.AppVersionBadCode, func() { refused.Add(1) })
	e.serveBy = m
	cfg := e.cfg
	cfg.listen = e.serve()

	for _, path := range []string{"/mail/v4/messages", "/core/v4/addresses"} {
		refusedPath.Store(&path)
		before := refused.Load()
		err := send(cfg.listen, nil, "hello@example.com")
		time.Sleep(300 * time.Millisecond)
		if refused.Load() == before {
			t.Errorf("a refused app version on %s did not reach the error handler", path)
		}
		if path == "/core/v4/addresses" {
			if err == nil || !strings.HasPrefix(err.Error(), "451") {
				t.Errorf("got %v, want a 451 reply", err)
			}
			if err := healthcheck(cfg); err == nil || !strings.Contains(err.Error(), "no longer supported") {
				t.Errorf("healthcheck gave %v", err)
			}
		}
	}

	refusedPath.Store(new("/auth/v4/refresh"))
	if err := e.server.RevokeUser(e.userID); err != nil {
		t.Fatal(err)
	}
	before := refused.Load()
	if err := send(cfg.listen, nil, "hello@example.com"); err == nil {
		t.Error("delivered with a refused app version")
	}
	if refused.Load() == before {
		t.Error("a refused app version on the refresh did not reach the error handler")
	}
	t.Logf("healthcheck after a refused refresh: %v", healthcheck(cfg))
}

func TestDataFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permissions")
	}
	e := newTestEnv(t)
	e.login()
	if err := send(e.serve(), nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{sessionPath(e.cfg), statusPath(e.cfg), copiesPath(e.cfg)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s has mode %o, want 600", path, perm)
		}
	}
}

type cannedAPI struct {
	base   http.RoundTripper
	answer func(*http.Request) (int, string, bool)
}

func (c *cannedAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body, ok := c.answer(r)
	if !ok {
		return c.base.RoundTrip(r)
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: h, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
}

func TestCaptchaLinkAndRetry(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	var asked atomic.Int32
	var retried atomic.Pointer[string]
	m := newManager(proton.WithHostURL(e.server.GetHostURL()), proton.WithTransport(&cannedAPI{base: proton.InsecureTransport(), answer: func(r *http.Request) (int, string, bool) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/auth/v4") {
			return 0, "", false
		}
		if asked.Add(1) == 1 {
			return 422, `{"Code":9001,"Error":"Human verification required","Details":{"HumanVerificationMethods":["captcha"],"HumanVerificationToken":"abc"}}`, true
		}
		header := r.Header.Get("x-pm-human-verification-token") + " " + r.Header.Get("x-pm-human-verification-token-type")
		retried.Store(&header)
		return 0, "", false
	}}))
	var userHV atomic.Pointer[string]
	m.AddPreRequestHook(func(_ *resty.Client, r *resty.Request) error {
		if strings.HasSuffix(r.URL, "/core/v4/users") {
			token := r.Header.Get("x-pm-human-verification-token")
			userHV.Store(&token)
		}
		return nil
	})

	p := &scriptedPrompter{answers: []string{testUser, testPassword, ""}}
	if err := login(context.Background(), e.cfg, m, p); err != nil {
		t.Fatal(err)
	}
	if len(p.told) == 0 || !strings.Contains(p.told[0], "https://verify.proton.me/?methods=captcha&token=abc") {
		t.Errorf("told %q", p.told)
	}
	if got := retried.Load(); got == nil || *got != "abc captcha" {
		t.Errorf("the retry carried %v", got)
	}
	if got := userHV.Load(); got == nil || *got != "abc" {
		t.Errorf("the first request after the login carried %v", got)
	}
}

func TestTwoFactorLogin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		enabled  proton.TwoFAStatus
		wantCode bool
	}{
		{proton.HasTOTP, true},
		{proton.HasFIDO2AndTOTP, true},
		{proton.HasFIDO2, false},
	} {
		e := newTestEnv(t)
		var code atomic.Pointer[string]
		hook := &hookTransport{base: &cannedAPI{base: proton.InsecureTransport(), answer: func(r *http.Request) (int, string, bool) {
			if !strings.HasSuffix(r.URL.Path, "/auth/v4/2fa") {
				return 0, "", false
			}
			var req proton.Auth2FAReq
			data, _ := io.ReadAll(r.Body)
			json.Unmarshal(data, &req)
			code.Store(&req.TwoFactorCode)
			return 200, `{"Code":1000}`, true
		}}}
		enabled := func(body map[string]any) { body["2FA"] = map[string]any{"Enabled": int(tc.enabled)} }
		hook.auth.Store(&enabled)
		m := newManager(proton.WithHostURL(e.server.GetHostURL()), proton.WithTransport(hook))

		p := &scriptedPrompter{answers: []string{testUser, testPassword, "123456"}}
		err := login(context.Background(), e.cfg, m, p)
		if tc.wantCode {
			if err != nil || code.Load() == nil || *code.Load() != "123456" {
				t.Errorf("2FA %d: %v, code %v", tc.enabled, err, code.Load())
			}
		} else {
			if err == nil {
				t.Errorf("2FA %d: login succeeded with only a security key", tc.enabled)
			}
			if got := e.openSessions(); got != 0 {
				t.Errorf("2FA %d: %d sessions left open", tc.enabled, got)
			}
		}
	}
}

func TestLogoutWhileTheServerRefreshesEndsTheSession(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.server.SetAuthLife(time.Second)
	e.login()
	addr := e.serve()
	time.Sleep(2 * time.Second)

	m, hook := e.hookedManager()
	var once atomic.Bool
	serverRefreshesFirst := func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/v4/refresh") && once.CompareAndSwap(false, true) {
			if err := send(addr, nil, "hello@example.com"); err != nil {
				t.Error(err)
			}
		}
	}
	hook.request.Store(&serverRefreshesFirst)
	s, _ := loadSession(sessionPath(e.cfg))
	if err := logout(context.Background(), e.cfg, m); err != nil {
		t.Fatal(err)
	}
	if err := send(addr, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
		t.Errorf("after logout: got %v, want a 451 reply", err)
	}

	if e.sessionOpen(s.UID) {
		t.Error("logout reported success but the session is still open at Proton")
	}
}

func TestSentCopyIsRemovedOnceTheAPIRecovers(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	m, hook := e.hookedManager()
	e.serveBy = m
	addr := e.serve()

	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}
	hook.down.Store(true)
	time.Sleep(2500 * time.Millisecond)
	hook.down.Store(false)

	e.waitForNoSentCopy()
}

func TestSessionSurvivesAFailedSave(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	m, hook := e.hookedManager()
	defer m.Close()
	mb := newMailbox(m, e.cfg)
	defer mb.disconnect()

	dir := e.cfg.dataDir
	unwritable := func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/v4/refresh") {
			hook.request.Store(nil)
			if err := os.Rename(dir, dir+".away"); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(dir, nil, 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	hook.request.Store(&unwritable)
	if err := mb.check(context.Background()); err == nil {
		t.Fatal("the check passed although the session could not be saved")
	}

	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir+".away", dir); err != nil {
		t.Fatal(err)
	}
	if err := mb.check(context.Background()); err != nil {
		t.Errorf("once the folder is writable again: %v", err)
	}
	if saved, err := loadSession(sessionPath(e.cfg)); err != nil || saved.RefreshToken != mb.session.RefreshToken {
		t.Errorf("the session file holds %v (%v), not the token in use", saved, err)
	}
}

func TestDeadSessionIsNotRetriedUntilTheNextLogin(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	if err := e.server.RevokeUser(e.userID); err != nil {
		t.Fatal(err)
	}
	m, hook := e.hookedManager()
	var refreshes atomic.Int32
	count := func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/v4/refresh") {
			refreshes.Add(1)
		}
	}
	hook.request.Store(&count)
	e.serveBy = m
	addr := e.serve()

	for range 3 {
		if err := send(addr, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
			t.Fatalf("got %v, want a 451 reply", err)
		}
	}
	if got := refreshes.Load(); got != 1 {
		t.Errorf("the dead session was tried %d times, want once", got)
	}

	e.login()
	if err := send(addr, nil, "hello@example.com"); err != nil {
		t.Errorf("after a new login: %v", err)
	}
}

func TestLogoutWaitsForTheServerToSaveTheSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := (session{UID: "uid", RefreshToken: "token"}).save(path); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockSession(path)
	if err != nil {
		t.Fatal(err)
	}

	removed := make(chan error, 1)
	go func() { removed <- removeSession(path) }()
	select {
	case <-removed:
		t.Fatal("the session was removed while it was locked")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
}

func TestIsAuthError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&proton.APIError{Status: 401}, true},
		{&proton.APIError{Status: 422}, true},
		{&proton.APIError{Status: 400, Code: proton.AuthRefreshTokenInvalid}, true},
		{&proton.APIError{Status: 400, Code: proton.AppVersionBadCode}, false},
		{&proton.APIError{Status: 503}, false},
		{errors.New("API down"), false},
	} {
		if got := isAuthError(tc.err); got != tc.want {
			t.Errorf("isAuthError(%v) = %v", tc.err, got)
		}
	}
}

func TestSentCopyLeftWhileTheLockIsBusyIsStillRetried(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.checkTimeout = 200 * time.Millisecond
	e.cfg.removalWaits = []time.Duration{10 * time.Millisecond}
	e.login()
	m, hook := e.hookedManager()
	failDelete := func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mail/v4/messages/delete") {
			r.URL.Path += "-gone"
		}
	}
	hook.request.Store(&failDelete)
	mb := newMailbox(m, e.cfg)
	defer mb.disconnect()
	ctx := context.Background()

	if _, err := mb.deliver(ctx, []string{"hello@example.com"}, []byte(testMessage)); err != nil {
		t.Fatal(err)
	}
	if err := mb.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * e.cfg.checkTimeout)
	mb.release()
	mb.removals.Wait()
	if got := e.status(); got != "copies left under Sent: 1" {
		t.Errorf("status is %q while the copy is left", got)
	}

	hook.request.Store(nil)
	mb.checkTimeout = time.Minute
	if err := mb.check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.sentCopies(); got != 0 {
		t.Errorf("%d copies are left under Sent after the check", got)
	}
}

func TestCheckGivesUpWhenTheLockStaysBusy(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	mb := newMailbox(e.manager(), e.cfg)
	defer mb.disconnect()
	ctx := context.Background()
	if err := mb.check(ctx); err != nil {
		t.Fatal(err)
	}
	mb.checkTimeout = 200 * time.Millisecond

	if err := mb.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	defer mb.release()
	start := time.Now()
	err := mb.check(ctx)
	if took := time.Since(start); !errors.Is(err, context.DeadlineExceeded) || took > time.Second {
		t.Errorf("got %v after %v, want a timeout after %v", err, took.Round(time.Millisecond), mb.checkTimeout)
	}
	if got := e.status(); got != statusOK {
		t.Errorf("a check that never ran changed the status to %q", got)
	}
}

func TestMailWaitingInLineIsDeferredAtTheDeliveryTimeout(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.checkTimeout, e.cfg.deliveryTimeout = 5*time.Second, time.Second
	e.cfg.removalWaits = []time.Duration{10 * time.Millisecond}
	e.submit.lag = time.Millisecond
	e.login()
	m, hook := e.hookedManager()
	hang := func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mail/v4/messages") {
			<-r.Context().Done()
		}
	}
	e.serveBy = m
	cfg := e.cfg
	cfg.listen = e.serve()

	hook.request.Store(&hang)
	if err := send(cfg.listen, nil, "hello@example.com"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	err := send(cfg.listen, nil, "hello@example.com")
	if took := time.Since(start); err == nil || !strings.HasPrefix(err.Error(), "451") || took > 2500*time.Millisecond {
		t.Fatalf("got %v after %v, want a 451 reply at the delivery timeout", err, took.Round(time.Millisecond))
	}
	if err := healthcheck(cfg); err != nil {
		t.Errorf("one message that waited too long made the healthcheck fail: %v", err)
	}
}

func TestCorruptSessionIsReported(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	if err := os.WriteFile(sessionPath(e.cfg), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	mb := newMailbox(e.manager(), e.cfg)
	defer mb.disconnect()

	err := mb.check(context.Background())
	if err == nil || errors.Is(err, errNotSignedIn) || !strings.Contains(err.Error(), "session.json") {
		t.Errorf("the check gave %v, want an error that names the file", err)
	}
	if got := e.status(); !strings.Contains(got, "session.json") {
		t.Errorf("status is %q", got)
	}
}

func TestCorruptSentCopiesFileIsReplaced(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	if err := os.WriteFile(copiesPath(e.cfg), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	logged := captureLog(t)
	mb := newMailbox(e.manager(), e.cfg)
	defer mb.disconnect()
	if len(mb.unremoved) != 0 || !strings.Contains(logged.String(), "sent-copies.json") {
		t.Errorf("loaded %v from a corrupt file, logged %q", mb.unremoved, logged)
	}

	if _, err := mb.deliver(context.Background(), []string{"hello@example.com"}, []byte(testMessage)); err != nil {
		t.Fatal(err)
	}
	mb.removals.Wait()
	if data, err := os.ReadFile(copiesPath(e.cfg)); err != nil || !json.Valid(data) {
		t.Errorf("the file holds %q (%v)", data, err)
	}
}

func TestSentCopiesThatCannotBeSavedAreLogged(t *testing.T) {
	e := newTestEnv(t)
	mb := newMailbox(e.manager(), e.cfg)
	mb.copiesPath = filepath.Join(e.cfg.dataDir, "missing", "sent-copies.json")
	logged := captureLog(t)
	mb.forget("x")
	if !strings.Contains(logged.String(), "save the sent copies") {
		t.Errorf("logged %q", logged)
	}
}

func TestSentCopyThatCannotBeLookedUpIsKept(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	m := newManager(proton.WithHostURL(e.server.GetHostURL()), proton.WithTransport(&cannedAPI{base: proton.InsecureTransport(), answer: func(r *http.Request) (int, string, bool) {
		if strings.HasSuffix(r.URL.Path, "/mail/v4/messages") {
			return 500, `{"Code":500,"Error":"Internal server error"}`, true
		}
		return 0, "", false
	}}))
	mb := newMailbox(m, e.cfg)
	defer mb.disconnect()
	mb.unremoved["old@proton.local"] = time.Now().Add(-removalGiveUp - time.Minute)

	if err := mb.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := mb.unremoved["old@proton.local"]; !ok {
		t.Error("a copy was given up although Proton could not be asked about it")
	}
	if got := e.status(); got != "copies left under Sent: 1" {
		t.Errorf("status is %q", got)
	}
}

func TestLoginWithAWrongPasswordLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	p := &scriptedPrompter{answers: []string{testUser, "wrong"}}
	err := login(context.Background(), e.cfg, e.manager(), p)
	if err == nil || !strings.HasPrefix(err.Error(), "sign in: ") {
		t.Fatalf("got %v, want a sign-in error", err)
	}
	if _, err := os.Stat(sessionPath(e.cfg)); err == nil {
		t.Error("a session was saved")
	}
	if got := e.openSessions(); got != 0 {
		t.Errorf("%d sessions are open", got)
	}
}

func TestWrongTwoFactorCodeEndsTheSession(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	hook := &hookTransport{base: &cannedAPI{base: proton.InsecureTransport(), answer: func(r *http.Request) (int, string, bool) {
		if strings.HasSuffix(r.URL.Path, "/auth/v4/2fa") {
			return 422, `{"Code":8002,"Error":"Incorrect login credentials. Please try again"}`, true
		}
		return 0, "", false
	}}}
	enabled := func(body map[string]any) { body["2FA"] = map[string]any{"Enabled": int(proton.HasTOTP)} }
	hook.auth.Store(&enabled)
	m := newManager(proton.WithHostURL(e.server.GetHostURL()), proton.WithTransport(hook))

	p := &scriptedPrompter{answers: []string{testUser, testPassword, "000000"}}
	err := login(context.Background(), e.cfg, m, p)
	if err == nil || !strings.HasPrefix(err.Error(), "two-factor code: ") {
		t.Fatalf("got %v, want a two-factor error", err)
	}
	if _, err := os.Stat(sessionPath(e.cfg)); err == nil {
		t.Error("a session was saved")
	}
	if got := e.openSessions(); got != 0 {
		t.Errorf("%d sessions are left open", got)
	}
}

func TestTokenRotatedDuringLoginIsTheOneSaved(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	var expired atomic.Bool
	hook := &hookTransport{base: &cannedAPI{base: proton.InsecureTransport(), answer: func(r *http.Request) (int, string, bool) {
		if strings.HasSuffix(r.URL.Path, "/core/v4/users") && expired.CompareAndSwap(false, true) {
			return 401, `{"Code":401,"Error":"Invalid access token"}`, true
		}
		return 0, "", false
	}}}
	var first atomic.Pointer[string]
	remember := func(body map[string]any) {
		token, _ := body["RefreshToken"].(string)
		first.Store(&token)
	}
	hook.auth.Store(&remember)
	m := newManager(proton.WithHostURL(e.server.GetHostURL()), proton.WithTransport(hook))

	p := &scriptedPrompter{answers: []string{testUser, testPassword}}
	if err := login(context.Background(), e.cfg, m, p); err != nil {
		t.Fatal(err)
	}
	s, err := loadSession(sessionPath(e.cfg))
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Load(); got == nil || *got == "" || s.RefreshToken == *got {
		t.Error("the session holds the token from before the refresh")
	}
	mb := newMailbox(e.manager(), e.cfg)
	defer mb.disconnect()
	if err := mb.check(context.Background()); err != nil {
		t.Errorf("the saved session does not work: %v", err)
	}
}

func TestLogoutWithoutASessionSaysSo(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	if err := logout(context.Background(), e.cfg, e.manager()); err == nil || err.Error() != "not signed in" {
		t.Errorf("got %v", err)
	}
}

func TestLogoutWhenProtonIsUnreachableStillSignsOutLocally(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	m, hook := e.hookedManager()
	hook.down.Store(true)
	if err := logout(context.Background(), e.cfg, m); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSession(sessionPath(e.cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the session is still there: %v", err)
	}
}

func TestStoppingDoesNotWaitForAHangingCheck(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.checkTimeout = time.Minute
	e.login()
	m, hook := e.hookedManager()
	hang := func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/v4/refresh") {
			<-r.Context().Done()
		}
	}
	hook.request.Store(&hang)
	listener, err := net.Listen("tcp", e.cfg.listen)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveOn(ctx, e.cfg, m, listener) }()
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if took := time.Since(start); err != nil || took > 5*time.Second {
			t.Errorf("stopping gave %v after %v", err, took.Round(time.Millisecond))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stopping waits for a hanging check")
	}
}

func TestTerminalPrompterReadsOneLinePerQuestion(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("a terminal on stdin would be asked for the secret")
	}
	p := &terminalPrompter{in: bufio.NewReader(strings.NewReader(" user \r\n pass word \r\nlast"))}
	if got, err := p.ask("Username"); got != "user" || err != nil {
		t.Errorf("ask gave %q, %v", got, err)
	}
	if got, err := p.askSecret("Password"); string(got) != " pass word " || err != nil {
		t.Errorf("askSecret gave %q, %v", got, err)
	}
	if got, err := p.ask("Code"); got != "last" || err != nil {
		t.Errorf("a last line without a line end gave %q, %v", got, err)
	}
	if _, err := p.ask("Code"); err == nil {
		t.Error("asking at the end of the input gave no error")
	}
	p.tell("done")
}

func TestNewLoginAfterARevokedSessionIsReusedBetweenMessages(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.login()
	m, hook := e.hookedManager()
	var refreshes atomic.Int32
	count := func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/v4/refresh") {
			refreshes.Add(1)
		}
	}
	hook.request.Store(&count)
	e.serveBy = m
	addr := e.serve()

	if err := e.server.RevokeUser(e.userID); err != nil {
		t.Fatal(err)
	}
	if err := send(addr, nil, "hello@example.com"); err == nil || !strings.HasPrefix(err.Error(), "451") {
		t.Fatalf("got %v, want a 451 reply", err)
	}

	e.login()
	before := refreshes.Load()
	for range 2 {
		if err := send(addr, nil, "hello@example.com"); err != nil {
			t.Fatalf("after the new login: %v", err)
		}
	}
	if got := refreshes.Load() - before; got != 1 {
		t.Errorf("the new session was refreshed %d times for two messages, want once", got)
	}
}

func TestLogoutRemovesAnUnreadableSession(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	if err := os.WriteFile(sessionPath(e.cfg), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := logout(context.Background(), e.cfg, e.manager()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionPath(e.cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the session is still there: %v", err)
	}
}

func TestStoppingEndsBeforeDockerKillsTheContainer(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	e.cfg.deliveryTimeout = 15 * time.Second
	e.login()
	m, hook := e.hookedManager()
	e.serveBy = m
	addr, stop := e.serveStoppable()

	hang := func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/core/v4/addresses") {
			<-r.Context().Done()
		}
	}
	hook.request.Store(&hang)
	go send(addr, nil, "hello@example.com")
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	stop()
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("stopping took %s while a delivery hangs", took.Round(time.Millisecond))
	}
}
