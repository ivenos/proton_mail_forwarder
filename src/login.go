package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ProtonMail/go-proton-api"
	"golang.org/x/term"
)

type prompter interface {
	ask(question string) (string, error)
	askSecret(question string) ([]byte, error)
	tell(text string)
}

func login(ctx context.Context, cfg config, m *proton.Manager, p prompter) error {
	defer m.Close()

	username, err := p.ask("Username")
	if err != nil {
		return err
	}
	password, err := p.askSecret("Password")
	if err != nil {
		return err
	}

	c, auth, err := m.NewClientWithLogin(ctx, username, password)
	hv := hvDetails(err)
	if hv != nil {
		p.tell("Proton asks for a CAPTCHA. Solve it in a browser, then come back:\n" + hvURL(hv))
		if _, err := p.ask("Press Enter when done"); err != nil {
			return err
		}
		c, auth, err = m.NewClientWithLoginWithHVToken(ctx, username, password, hv)
	}
	if err != nil {
		return fmt.Errorf("sign in: %w", err)
	}
	defer c.Close()

	saved := false
	defer func() {
		if !saved {
			c.AuthDelete(context.Background())
		}
	}()

	s := session{UID: auth.UID, RefreshToken: auth.RefreshToken}
	c.AddAuthHandler(func(auth proton.Auth) { s.UID, s.RefreshToken = auth.UID, auth.RefreshToken })

	if auth.TwoFA.Enabled&proton.HasTOTP != 0 {
		code, err := p.ask("Two-factor code")
		if err != nil {
			return err
		}
		if err := c.Auth2FA(ctx, proton.Auth2FAReq{TwoFactorCode: code}); err != nil {
			return fmt.Errorf("two-factor code: %w", err)
		}
	} else if auth.TwoFA.Enabled != 0 {
		return errors.New("only an authenticator app works as second factor, security keys alone are not supported")
	}

	user, err := c.GetUserWithHV(ctx, hv)
	if err != nil {
		return err
	}

	previous, previousErr := loadSession(sessionPath(cfg))
	if err := s.save(sessionPath(cfg)); err != nil {
		return err
	}
	saved = true
	if previousErr == nil && previous.UID != s.UID {
		if pc, _, err := m.NewClientWithRefresh(ctx, previous.UID, previous.RefreshToken); err == nil {
			pc.AuthDelete(ctx)
			pc.Close()
		}
	}
	p.tell("Signed in as " + user.Email + ".")
	return nil
}

func logout(ctx context.Context, cfg config, m *proton.Manager) error {
	defer m.Close()

	s, err := loadSession(sessionPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("not signed in")
	}
	if err == nil {
		var c *proton.Client
		if c, _, err = m.NewClientWithRefresh(ctx, s.UID, s.RefreshToken); err == nil {
			err = c.AuthDelete(ctx)
			c.Close()
		}
	}
	if err != nil && !isAuthError(err) {
		fmt.Fprintf(os.Stderr, "The session could not be ended at Proton (%v). End it in the session list of your Proton account.\n", err)
	}

	if err := removeSession(sessionPath(cfg)); err != nil {
		return err
	}
	fmt.Println("Signed out.")
	return nil
}

func hvDetails(err error) *proton.APIHVDetails {
	var apiErr *proton.APIError
	if !errors.As(err, &apiErr) || !apiErr.IsHVError() {
		return nil
	}
	hv, err := apiErr.GetHVDetails()
	if err != nil {
		return nil
	}
	return hv
}

func hvURL(hv *proton.APIHVDetails) string {
	return fmt.Sprintf("https://verify.proton.me/?methods=%s&token=%s", strings.Join(hv.Methods, ","), hv.Token)
}

type terminalPrompter struct {
	in *bufio.Reader
}

func newTerminalPrompter() *terminalPrompter {
	return &terminalPrompter{in: bufio.NewReader(os.Stdin)}
}

func (t *terminalPrompter) ask(question string) (string, error) {
	line, err := t.line(question)
	return strings.TrimSpace(line), err
}

func (t *terminalPrompter) askSecret(question string) ([]byte, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := t.line(question)
		return []byte(line), err
	}

	fmt.Print(question + ": ")
	secret, err := term.ReadPassword(fd)
	fmt.Println()
	return secret, err
}

func (t *terminalPrompter) line(question string) (string, error) {
	fmt.Print(question + ": ")
	line, err := t.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (t *terminalPrompter) tell(text string) { fmt.Println(text) }
