package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"net/mail"
	"strings"

	"github.com/ProtonMail/gluon/rfc5322"
	"github.com/ProtonMail/gluon/rfc822"
	"github.com/emersion/go-message/charset"
)

func init() { rfc5322.CharsetReader = charset.Reader }

// rewrite puts the message under the account's own address with the original sender's name, sends replies to the original sender and gives it a new Message-Id.
func rewrite(literal []byte, sender, messageID string) []byte {
	header, body := rfc822.Split(literal)
	h, err := rfc822.NewHeader(bytes.Clone(header))
	if err != nil || bytes.IndexAny(header, " \t") == 0 {
		h, body = rfc822.NewEmptyHeader(), literal
	}

	from := h.Get("From")
	name, replyTo := from, ""
	if addresses, err := rfc5322.ParseAddressList(from); err == nil && len(addresses) > 0 {
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
	h.Set("From", (&mail.Address{Name: name, Address: sender}).String())

	return append(h.Raw(), body...)
}

func newMessageID(sender string) string {
	random := make([]byte, 16)
	rand.Read(random)
	return hex.EncodeToString(random) + sender[strings.LastIndex(sender, "@"):]
}
