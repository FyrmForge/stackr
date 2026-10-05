package main

import (
	"strings"
	"testing"
)

// answer turns what was typed into the value: defaults, y/n, picks, and the
// check's cleaned form or its fix-it message.
func TestAnswer(t *testing.T) {
	var a answers
	qs := installQuestions(&a, map[string]bool{}, true, false, false, func() string { return "" })
	by := map[string]question{}
	for _, q := range qs {
		by[q.title] = q
	}
	for _, c := range []struct {
		q, typed, want, err string
	}{
		{q: "Root domain", typed: "", err: "required"},
		{q: "Root domain", typed: "192.168.1.5", err: "an address"},
		{q: "Root domain", typed: " HTTPS://Example.COM/ ", want: "example.com"},
		{q: "Serve HTTPS with automatic certificates?", typed: "", want: "Yes"},
		{q: "Serve HTTPS with automatic certificates?", typed: "maybe", err: "type y or n"},
		{q: "Behind a CDN or load balancer?", typed: "", want: "No"},
		{q: "Its IP addresses or ranges", typed: "", err: "at least one"},
		{q: "Its IP addresses or ranges", typed: "0.0.0.0/0", err: "every address"},
		{q: "Its IP addresses or ranges", typed: "10.0.0.0/8 10.1.2.3, 10.0.0.0/8", want: "10.0.0.0/8,10.1.2.3/32"},
		{q: "Admin email", typed: " Me@Example.com ", want: "me@example.com"},
		{q: "Admin email", typed: "me@", err: "real domain"},
		{q: "Admin password", typed: "hunter22", err: "uppercase"},
		{q: "Admin password", typed: " Hunter22! ", want: " Hunter22! "}, // a secret is kept as typed
	} {
		got, err := answer(by[c.q], c.typed, 0)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s %q: err %v, want one saying %q", c.q, c.typed, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s %q = %q %v, want %q", c.q, c.typed, got, err, c.want)
		}
	}

	a.root = "example.com"
	if got, _ := answer(by["Panel address"], "", 0); got != "stkr.example.com" {
		t.Errorf("panel default = %q", got)
	}
	if _, err := answer(by["Panel address"], "other.org", 0); err == nil {
		t.Error("a panel outside the root was accepted")
	}
	a.password = "Hunter22!"
	if _, err := answer(by["Confirm password"], "Hunter22", 0); err == nil {
		t.Error("a mismatched confirm was accepted")
	}
	if got, _ := answer(by["What's next?"], "", 1); got != nextDryRun {
		t.Errorf("pick 1 = %q", got)
	}
}

// A flag given is never asked; follow-ups wait on their answer.
func TestQuestionsSkip(t *testing.T) {
	var a answers
	qs := installQuestions(&a, map[string]bool{"domain": true, "proxy": true}, false, false, false, nil)
	asked := func(title string) bool {
		for _, q := range qs {
			if q.title == title {
				return q.skip == nil || !q.skip()
			}
		}
		return false
	}
	if asked("Root domain") || asked("Behind a CDN or load balancer?") || asked("Admin email") {
		t.Error("asked for an answer a flag gave, or for an admin not needed")
	}
	if asked("Cloudflare API token") {
		t.Error("asked for a token before wildcard was picked")
	}
	a.https = "No"
	if asked("Email for Let's Encrypt") {
		t.Error("asked for the ACME email with HTTPS off")
	}
}
