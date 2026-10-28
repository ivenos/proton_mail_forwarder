package main

import (
	"bytes"
	"net/mail"
	"strings"
	"testing"

	"github.com/ProtonMail/gluon/rfc5322"
	"github.com/ProtonMail/gluon/rfc822"
)

func TestRewrite(t *testing.T) {
	for _, tc := range []struct {
		name, header          string
		wantFrom, wantReplyTo string
	}{
		{
			"name and address",
			"From: =?utf-8?q?J=C3=BCrgen_M=C3=BCller?= <juergen@example.org>\r\n",
			"=?utf-8?q?J=C3=BCrgen_M=C3=BCller?= <hello@example.com>",
			"=?utf-8?q?J=C3=BCrgen_M=C3=BCller?= <juergen@example.org>",
		},
		{
			"address only",
			"From: juergen@example.org\r\n",
			`"juergen@example.org" <hello@example.com>`,
			"juergen@example.org",
		},
		{
			"own Reply-To",
			"From: Shop <shop@example.org>\r\nReply-To: Support <support@example.org>\r\n",
			`"Shop" <hello@example.com>`,
			"Support <support@example.org>",
		},
		{
			"Sender, signature and Message-Id",
			"From: Shop <shop@example.org>\r\nSender: bounce@example.org\r\nDKIM-Signature: v=1; d=example.org\r\nMessage-Id: <old@example.org>\r\n",
			`"Shop" <hello@example.com>`,
			"Shop <shop@example.org>",
		},
		{
			"name without address",
			"From: Contact form\r\n",
			`"Contact form" <hello@example.com>`,
			"",
		},
		{
			"no From",
			"",
			"<hello@example.com>",
			"",
		},
	} {
		body := "--b\r\nContent-Type: text/plain\r\n\r\nHi\r\n--b--\r\n"
		literal := tc.header + "Subject: Hello\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" + body

		out := rewrite([]byte(literal), "hello@example.com", "new@example.com")
		h, err := rfc822.NewHeader(rfc822.Parse(out).Header())
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		if got := h.Get("From"); got != tc.wantFrom {
			t.Errorf("%s: From is %q, want %q", tc.name, got, tc.wantFrom)
		}
		if got := h.Get("Reply-To"); got != tc.wantReplyTo {
			t.Errorf("%s: Reply-To is %q, want %q", tc.name, got, tc.wantReplyTo)
		}
		if got := h.Get("Message-Id"); got != "<new@example.com>" {
			t.Errorf("%s: Message-Id is %q", tc.name, got)
		}
		for _, key := range []string{"Sender", "DKIM-Signature"} {
			if h.Has(key) {
				t.Errorf("%s: %s is still there", tc.name, key)
			}
		}
		if h.Get("Subject") != "Hello" || !strings.HasSuffix(string(out), "\r\n\r\n"+body) {
			t.Errorf("%s: the rest changed:\n%s", tc.name, out)
		}
	}
}

func TestNewMessageIDUsesTheSendersDomain(t *testing.T) {
	a, b := newMessageID("hello@example.com"), newMessageID("hello@example.com")
	if a == b || !strings.HasSuffix(a, "@example.com") || strings.ContainsAny(a, "<> ") {
		t.Errorf("got %q and %q", a, b)
	}
}

func TestRewriteDecodesTheNameInOtherCharsets(t *testing.T) {
	for _, from := range []string{
		"=?windows-1252?Q?J=FCrgen_M=FCller?= <juergen@example.org>",
		"=?iso-8859-15?Q?J=FCrgen_M=FCller?= <juergen@example.org>",
		"=?ISO-2022-JP?B?GyRCJWEhPCVrGyhC?= <juergen@example.org>",
	} {
		out := rewrite([]byte("From: "+from+"\r\nSubject: Hello\r\n\r\nHi\r\n"), "hello@example.com", "new@example.com")
		h, err := rfc822.NewHeader(rfc822.Parse(out).Header())
		if err != nil {
			t.Fatal(err)
		}
		got, err := mail.ParseAddress(h.Get("From"))
		if err != nil {
			t.Fatalf("%s: %v", from, err)
		}
		if got.Address != "hello@example.com" || strings.Contains(got.Name, "=?") || strings.Contains(got.Name, "juergen@example.org") {
			t.Errorf("%s: From is %q", from, h.Get("From"))
		}
	}
}

func TestRewriteKeepsEveryNameInOneSafeFromHeader(t *testing.T) {
	for _, tc := range []struct{ header, wantName string }{
		{"From: \"Doe, John \\\"JD\\\" <boss@example.org>\" <john@example.org>\r\n", `Doe, John "JD" <boss@example.org>`},
		{"From: \"back\\\\slash\" <a@example.org>\r\n", `back\slash`},
		{"From: =?utf-8?q?Evil=0D=0ABcc:_victim@example.org?= <a@example.org>\r\n", "Evil\r\nBcc: victim@example.org"},
		{"From: Jürgen Müller <juergen@example.org>\r\n", "Jürgen Müller"},
		{"From: Jürgen\r\n Müller\r\n\t<juergen@example.org>\r\n", "Jürgen Müller"},
		{"From: A <a@example.org>, B <b@example.org>\r\n", "A"},
		{"From: First <first@example.org>\r\nFROM: Second <second@example.org>\r\n", "First"},
		{"From: Team: a@example.org, b@example.org;\r\n", "a@example.org"},
		{"From: a@example.org (Comment)\r\n", "a@example.org"},
	} {
		literal := tc.header + "MESSAGE-ID: <old@example.org>\r\nSubject: Hello\r\n\r\nHi\r\n"
		out := rewrite([]byte(literal), "hello@example.com", "new@example.com")
		header, body := rfc822.Split(out)
		h, err := rfc822.NewHeader(header)
		if err != nil {
			t.Fatalf("%q: %v", tc.header, err)
		}

		var froms, ids, others int
		h.Entries(func(key, _ string) {
			switch strings.ToLower(key) {
			case "from":
				froms++
			case "message-id":
				ids++
			case "subject", "reply-to":
			default:
				others++
			}
		})
		if froms != 1 || ids != 1 || others != 0 || string(body) != "Hi\r\n" {
			t.Errorf("%q: got\n%s", tc.header, out)
			continue
		}
		got, err := mail.ParseAddress(h.Get("From"))
		if err != nil || got.Address != "hello@example.com" || got.Name != tc.wantName {
			t.Errorf("%q: From %q parses to %v (%v), want name %q", tc.header, h.Get("From"), got, err, tc.wantName)
		}
	}
}

func TestRewriteRepliesToTheSenderWhenReplyToIsEmpty(t *testing.T) {
	out := rewrite([]byte("From: Shop <shop@example.org>\r\nReply-To: \r\nSubject: Hello\r\n\r\nHi\r\n"), "hello@example.com", "new@example.com")
	if got := bytes.Count(out, []byte("Reply-To:")); got != 1 {
		t.Errorf("%d Reply-To headers in\n%s", got, out)
	}
	if got, _ := rfc822.GetHeaderValue(out, "Reply-To"); got != "Shop <shop@example.org>" {
		t.Errorf("Reply-To is %q", got)
	}
}

func FuzzRewriteKeepsOneSafeFromAndTheBody(f *testing.F) {
	f.Add([]byte(testMessage))
	f.Add([]byte("From: =?utf-8?q?Evil=0D=0ABcc:_victim@example.org?= <a@example.org>\r\nReply-To: \r\nSubject: x\r\n\r\nHi\r\n"))
	f.Add([]byte("From: Team: a@example.org, b@example.org;\r\nSender: c@example.org\r\nFROM: d@example.org\r\n\r\n"))
	f.Add([]byte("Backup failed: disk full\r\n"))
	f.Add([]byte("From: A <a@example.org>\nSubject: x\n\nbody\n"))
	f.Add([]byte("  Backup failed\r\n\r\nbody\r\n"))
	f.Add([]byte("From: \"Müller \\\\ Sohn\" <a@example.org>\r\n\r\nHi\r\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, literal []byte) {
		_, original := rfc822.Split(bytes.Clone(literal))
		out := rewrite(literal, "hello@example.com", "new@example.com")
		header, body := rfc822.Split(out)
		h, err := rfc822.NewHeader(header)
		if err != nil {
			t.Fatalf("the rewritten header does not parse: %v\n%q", err, out)
		}
		var froms, ids int
		h.Entries(func(key, _ string) {
			switch strings.ToLower(key) {
			case "from":
				froms++
			case "message-id":
				ids++
			case "sender", "dkim-signature":
				t.Errorf("%s survived", key)
			}
		})
		if froms != 1 || ids != 1 || h.Get("Message-Id") != "<new@example.com>" {
			t.Errorf("%d From and %d Message-Id headers in\n%q", froms, ids, out)
		}
		if got, err := rfc5322.ParseAddressList(h.Get("From")); err != nil || len(got) != 1 || got[0].Address != "hello@example.com" {
			t.Errorf("From %q parses to %v (%v)", h.Get("From"), got, err)
		}
		if !bytes.Equal(body, original) && !bytes.HasSuffix(out, literal) {
			t.Errorf("the body changed from %q to %q", original, body)
		}
	})
}

func TestRewriteKeepsAMessageThatStartsWithWhitespaceOutOfTheHeader(t *testing.T) {
	for _, literal := range []string{
		"  Backup failed\r\n\r\nbody\r\n",
		"\tindented\r\nmore\r\n",
		" \r\nSubject: x\r\n\r\nbody\r\n",
	} {
		out := rewrite([]byte(literal), "hello@example.com", "new@example.com")
		header, body := rfc822.Split(out)
		h, err := rfc822.NewHeader(header)
		if err != nil {
			t.Fatalf("%q: %v", literal, err)
		}
		if got := h.Get("Message-Id"); got != "<new@example.com>" {
			t.Errorf("%q: Message-Id is %q", literal, got)
		}
		if !strings.HasSuffix(string(body), strings.TrimLeft(literal, " \t\r\n")) {
			t.Errorf("%q: the body is %q", literal, body)
		}
	}
}
