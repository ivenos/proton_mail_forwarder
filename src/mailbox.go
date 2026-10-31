package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ProtonMail/go-proton-api"
)

const (
	statusOK = "ok"

	removalGiveUp = 24 * time.Hour
)

var errNotSignedIn = errors.New("not signed in, run: proton_mail_forwarder login")

type session struct {
	UID          string `json:"uid"`
	RefreshToken string `json:"refresh_token"`
}

func loadSession(path string) (session, error) {
	unlock, err := lockSession(path)
	if err != nil {
		return session{}, err
	}
	defer unlock()
	return readSession(path)
}

func (s session) save(path string) error {
	unlock, err := lockSession(path)
	if err != nil {
		return err
	}
	defer unlock()
	return s.write(path)
}

func (s session) update(path string) (bool, error) {
	unlock, err := lockSession(path)
	if err != nil {
		return false, err
	}
	defer unlock()
	if saved, err := readSession(path); err != nil || saved.UID != s.UID {
		return false, nil
	}
	if err := s.write(path); err != nil {
		return false, err
	}
	return true, nil
}

func removeSession(path string) error {
	unlock, err := lockSession(path)
	if err != nil {
		return err
	}
	defer unlock()
	return os.Remove(path)
}

// lockSession keeps login, logout and the server from changing the session file at the same time.
func lockSession(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func readSession(path string) (session, error) {
	var s session
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("read %s: %w", path, err)
	}
	return s, nil
}

func (s session) write(path string) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return replaceFile(path, data)
}

func loadCopies(path string) map[string]time.Time {
	copies := make(map[string]time.Time)
	var saved map[string]time.Time
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &saved)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("read %s: %v", path, err)
	}
	maps.Copy(copies, saved)
	return copies
}

func replaceFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".save-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

type mailbox struct {
	manager         *proton.Manager
	submission      submission
	sessionPath     string
	statusPath      string
	copiesPath      string
	checkTimeout    time.Duration
	deliveryTimeout time.Duration
	removalWaits    []time.Duration

	lock      chan struct{}
	client    *proton.Client
	session   session
	saved     bool
	deadToken string
	deauthed  atomic.Bool
	removing  map[string]time.Time
	unremoved map[string]time.Time

	removals sync.WaitGroup
	stopping chan struct{}
}

func newMailbox(m *proton.Manager, cfg config) *mailbox {
	return &mailbox{
		manager:         m,
		submission:      cfg.submission,
		sessionPath:     sessionPath(cfg),
		statusPath:      statusPath(cfg),
		copiesPath:      copiesPath(cfg),
		checkTimeout:    cfg.checkTimeout,
		deliveryTimeout: cfg.deliveryTimeout,
		removalWaits:    cfg.removalWaits,
		lock:            make(chan struct{}, 1),
		removing:        make(map[string]time.Time),
		unremoved:       loadCopies(copiesPath(cfg)),
		stopping:        make(chan struct{}),
	}
}

func (mb *mailbox) acquire(ctx context.Context) error {
	select {
	case mb.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (mb *mailbox) release() { <-mb.lock }

// connect follows the saved session: a new login replaces the client, a logout ends it. It needs the lock.
func (mb *mailbox) connect(ctx context.Context) error {
	s, err := loadSession(mb.sessionPath)
	if errors.Is(err, os.ErrNotExist) {
		mb.endSession(ctx)
		return errNotSignedIn
	}
	if err != nil {
		return err
	}
	if mb.client != nil && !mb.deauthed.Load() && s.UID == mb.session.UID {
		if !mb.saved {
			return mb.saveSession()
		}
		return nil
	}
	if s.UID != mb.session.UID {
		mb.endSession(ctx)
	}
	mb.disconnect()
	if s.RefreshToken == mb.deadToken {
		return errNotSignedIn
	}

	c, auth, err := mb.manager.NewClientWithRefresh(ctx, s.UID, s.RefreshToken)
	if err != nil {
		if isAuthError(err) {
			mb.deadToken = s.RefreshToken
			return fmt.Errorf("%w (%v)", errNotSignedIn, err)
		}
		return err
	}

	s.UID, s.RefreshToken = auth.UID, auth.RefreshToken
	c.AddAuthHandler(func(auth proton.Auth) { mb.saveRefresh(s.UID, auth) })
	c.AddDeauthHandler(func() { mb.deauthed.Store(true) })

	mb.client, mb.session = c, s
	mb.deauthed.Store(false)
	return mb.saveSession()
}

func (mb *mailbox) saveSession() error {
	saved, err := mb.session.update(mb.sessionPath)
	mb.saved = saved
	return err
}

// saveRefresh keeps a rotated token, unless a new login or a logout has replaced the session since. Requests run under the lock, and so does this.
func (mb *mailbox) saveRefresh(uid string, auth proton.Auth) {
	if auth.UID != uid || mb.session.UID != uid {
		return
	}
	mb.session.RefreshToken = auth.RefreshToken
	if err := mb.saveSession(); err != nil {
		log.Printf("save session: %v", err)
	}
}

func (mb *mailbox) endSession(ctx context.Context) {
	if mb.client != nil && !mb.deauthed.Load() {
		mb.client.AuthDelete(ctx)
	}
	mb.disconnect()
}

func (mb *mailbox) disconnect() {
	if mb.client != nil {
		mb.client.Close()
		mb.client = nil
	}
}

func isAuthError(err error) bool {
	var apiErr *proton.APIError
	return errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 422 || apiErr.Code == proton.AuthRefreshTokenInvalid)
}

// check signs in from the saved session if needed, keeps it alive, tries the SMTP token, looks again for sent copies that were left and records the result for the healthcheck.
func (mb *mailbox) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, mb.checkTimeout)
	defer cancel()

	if err := mb.acquire(ctx); err != nil {
		return err
	}
	defer mb.release()

	err := mb.connect(ctx)
	if err == nil {
		if _, err = mb.client.GetUser(ctx); err != nil && mb.deauthed.Load() {
			err = fmt.Errorf("%w (%v)", errNotSignedIn, err)
		}
	}
	if err == nil {
		err = mb.submission.send(ctx, "", nil)
		mb.retryRemovals(ctx)
	}
	mb.setStatus(err)
	return err
}

func (mb *mailbox) setStatus(err error) {
	text := statusOK
	if err != nil {
		text = err.Error()
	} else if len(mb.unremoved) > 0 {
		text = fmt.Sprintf("copies left under Sent: %d", len(mb.unremoved))
	}
	if werr := os.WriteFile(mb.statusPath, []byte(text+"\n"), 0o600); werr != nil {
		log.Printf("write status: %v", werr)
	}
}

func (mb *mailbox) deliver(wait context.Context, recipients []string, literal []byte) (string, error) {
	if err := mb.acquire(wait); err != nil {
		return "", err
	}
	defer mb.release()

	ctx, cancel := context.WithTimeout(context.Background(), mb.deliveryTimeout)
	defer cancel()

	id, err := mb.send(ctx, recipients, literal)
	if err == nil || errors.Is(err, errUnconfirmed) {
		confirmed := err == nil
		mb.removing[id] = time.Now()
		mb.saveCopies()
		mb.removals.Go(func() { mb.removeSentCopy(id, confirmed) })
	}
	switch {
	case err == nil:
		mb.setStatus(nil)
	case errors.Is(err, errRefused):
	case errors.Is(err, errNotSignedIn), errors.Is(err, errTokenRefused), errors.Is(err, errEnvelopeRefused):
		mb.setStatus(err)
	case mb.deauthed.Load():
		err = fmt.Errorf("%w (%v)", errNotSignedIn, err)
		mb.setStatus(err)
	default:
		mb.setStatus(fmt.Errorf("delivery failed: %w", err))
	}
	return id, err
}

// send delivers the message to an address of the account through Proton Mail's SMTP server and returns its new Message-Id.
func (mb *mailbox) send(ctx context.Context, recipients []string, literal []byte) (string, error) {
	if err := mb.connect(ctx); err != nil {
		return "", err
	}

	addresses, err := mb.client.GetAddresses(ctx)
	if err != nil {
		return "", err
	}
	addr, err := pickAddress(addresses, recipients)
	if err != nil {
		return "", err
	}

	id := newMessageID(mb.submission.sender)
	return id, mb.submission.send(ctx, addr.Email, rewrite(literal, mb.submission.sender, id))
}

// removeSentCopy deletes the copy Proton Mail keeps under Sent, tries again while it has not shown up, and then leaves it to check. A message Proton Mail did not confirm may have no copy, so it is forgotten instead.
func (mb *mailbox) removeSentCopy(id string, confirmed bool) {
	for _, wait := range mb.removalWaits {
		select {
		case <-time.After(wait):
		case <-mb.stopping:
			log.Printf("the sent copy of %s is still under Sent", id)
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), mb.checkTimeout)
		found, err := mb.deleteSentCopy(ctx, id)
		cancel()
		if err != nil {
			log.Printf("remove the sent copy of %s: %v", id, err)
		} else if found {
			return
		}
	}
	if confirmed {
		log.Printf("the sent copy of %s is still under Sent, trying again later", id)
	} else {
		log.Printf("no sent copy of %s showed up", id)
	}

	select {
	case mb.lock <- struct{}{}:
	case <-mb.stopping:
		return
	}
	if confirmed {
		mb.unremoved[id] = mb.removing[id]
		delete(mb.removing, id)
		mb.setStatus(nil)
	} else {
		mb.forget(id)
	}
	mb.release()
}

// retryRemovals looks again for the sent copies that were left, and gives up on those older than removalGiveUp. It needs the lock and a client.
func (mb *mailbox) retryRemovals(ctx context.Context) {
	for id, since := range mb.unremoved {
		switch found, err := mb.deleteCopy(ctx, id); {
		case err != nil:
			log.Printf("remove the sent copy of %s: %v", id, err)
		case !found && time.Since(since) > removalGiveUp:
			log.Printf("gave up on the sent copy of %s", id)
			mb.forget(id)
		}
	}
}

func (mb *mailbox) deleteSentCopy(ctx context.Context, id string) (bool, error) {
	if err := mb.acquire(ctx); err != nil {
		return false, err
	}
	defer mb.release()

	if err := mb.connect(ctx); err != nil {
		return false, err
	}
	return mb.deleteCopy(ctx, id)
}

func (mb *mailbox) deleteCopy(ctx context.Context, id string) (bool, error) {
	messages, err := mb.client.GetMessageMetadata(ctx, proton.MessageFilter{ExternalID: id, LabelID: proton.SentLabel})
	if err != nil {
		return false, err
	}

	var ids []string
	for _, m := range messages {
		if !slices.Contains(m.LabelIDs, proton.InboxLabel) {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return false, nil
	}
	if err := mb.client.DeleteMessage(ctx, ids...); err != nil {
		return true, err
	}
	mb.forget(id)
	return true, nil
}

func (mb *mailbox) forget(id string) {
	delete(mb.removing, id)
	delete(mb.unremoved, id)
	mb.saveCopies()
}

func (mb *mailbox) saveCopies() {
	copies := maps.Clone(mb.unremoved)
	maps.Copy(copies, mb.removing)
	data, err := json.Marshal(copies)
	if err == nil {
		err = replaceFile(mb.copiesPath, data)
	}
	if err != nil {
		log.Printf("save the sent copies: %v", err)
	}
}

// pickAddress takes the enabled address a recipient names, or else the primary one.
func pickAddress(addresses []proton.Address, recipients []string) (proton.Address, error) {
	var primary *proton.Address

	for i, addr := range addresses {
		if addr.Status != proton.AddressStatusEnabled {
			continue
		}
		for _, rcpt := range recipients {
			if strings.EqualFold(rcpt, addr.Email) {
				return addr, nil
			}
		}
		if primary == nil || addr.Order < primary.Order {
			primary = &addresses[i]
		}
	}

	if primary == nil {
		return proton.Address{}, errors.New("the account has no enabled address")
	}
	return *primary, nil
}
