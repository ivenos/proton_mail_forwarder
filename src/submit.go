package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

var (
	errTokenRefused    = errors.New("Proton Mail refused the SMTP token")
	errEnvelopeRefused = errors.New("Proton Mail refused the sender or the recipient")
	errRefused         = errors.New("Proton Mail refused the message")
)

type submission struct {
	addr   string
	sender string
	token  string
	tls    *tls.Config
}

func (s submission) send(ctx context.Context, to string, literal []byte) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return err
	}
	// go-smtp replaces connection deadlines with its own on every command.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	config := s.tls
	if config == nil {
		host, _, _ := net.SplitHostPort(s.addr)
		config = &tls.Config{ServerName: host}
	}
	c, err := smtp.NewClientStartTLS(conn, config)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()

	if err := c.Auth(sasl.NewPlainClient("", s.sender, s.token)); err != nil {
		var smtpErr *smtp.SMTPError
		if errors.As(err, &smtpErr) && smtpErr.Code == 535 {
			return errTokenRefused
		}
		return replyError(err, nil)
	}
	if literal == nil {
		return c.Quit()
	}

	if err := c.Mail(s.sender, nil); err != nil {
		return replyError(err, errEnvelopeRefused)
	}
	if err := c.Rcpt(to, nil); err != nil {
		return replyError(err, errEnvelopeRefused)
	}
	w, err := c.Data()
	if err != nil {
		return replyError(err, errEnvelopeRefused)
	}
	if _, err := w.Write(literal); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return replyError(err, errRefused)
	}
	c.Quit()
	return nil
}

func replyError(err, permanent error) error {
	var smtpErr *smtp.SMTPError
	if !errors.As(err, &smtpErr) {
		return err
	}
	if permanent != nil && smtpErr.Code >= 500 {
		return fmt.Errorf("%w (%d)", permanent, smtpErr.Code)
	}
	return fmt.Errorf("Proton Mail replied %d", smtpErr.Code)
}
