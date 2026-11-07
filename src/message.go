package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"mime"
	"net/mail"
	"strings"

	"github.com/ProtonMail/gluon/rfc5322"
	"github.com/ProtonMail/gluon/rfc822"
	"github.com/emersion/go-message/charset"
)

func init() { rfc5322.CharsetReader = charset.Reader }

var lenient = mail.AddressParser{WordDecoder: &mime.WordDecoder{CharsetReader: func(_ string, r io.Reader) (io.Reader, error) { return r, nil }}}

// rewrite puts the message under the account's own address with the original sender's name, sends replies to the original sender and gives it a new Message-Id.
func rewrite(literal []byte, sender, messageID string) []byte {
	header, body := rfc822.Split(literal)
	h, err := rfc822.NewHeader(bytes.Clone(header))
	first, _, _ := bytes.Cut(header, []byte("\n"))
	if err != nil || bytes.IndexAny(header, " \t") == 0 || len(bytes.TrimSpace(first)) > 0 && !bytes.Contains(first, []byte(":")) {
		h, body = rfc822.NewEmptyHeader(), literal
	}

	from := h.Get("From")
	name, replyTo := from, ""
	addresses, err := rfc5322.ParseAddressList(from)
	if err != nil {
		addresses, err = lenient.ParseList(from)
	}
	if err == nil && len(addresses) > 0 {
		name, replyTo = addresses[0].Name, from
		if name == "" {
			name = addresses[0].Address
		}
	}

	if h.Get("Reply-To") == "" && replyTo != "" {
		for h.Has("Reply-To") {
			h.Del("Reply-To")
		}
		h.Set("Reply-To", replyTo)
	}
	for _, key := range []string{"From", "Sender", "Message-Id", "DKIM-Signature"} {
		for h.Has(key) {
			h.Del(key)
		}
	}
	h.Set("Message-Id", "<"+messageID+">")
	h.Set("From", (&mail.Address{Name: strings.ToValidUTF8(name, "\uFFFD"), Address: sender}).String())

	return append(h.Raw(), body...)
}

func newMessageID(sender string) string {
	random := make([]byte, 16)
	rand.Read(random)
	return hex.EncodeToString(random) + sender[strings.LastIndex(sender, "@"):]
}
