// Package mail is stackr's outbound transactional email: one SMTP2GO adapter
// behind hamr's email.Sender, plus the dev inbox.
//
// Mail is optional. A self-hosted install with no provider configured still
// works, every message stackr sends (today: org invites) has a link the sender
// can copy and pass on by hand, so an unconfigured server degrades to that
// rather than failing.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/email"
	"github.com/FyrmForge/hamr/pkg/emailmock"
)

// Mailer is a configured sender plus the address it sends as. Nil means no
// provider is configured; callers check Enabled and fall back to copy-the-link.
type Mailer struct {
	sender email.Sender
	from   email.Address
}

// FromEnv wires the mailer from the environment. Nil when nothing is set:
//
//	SMTP2GO_API_KEY   api key; without it there is no provider
//	MAIL_FROM         sender address, e.g. "stackr <no-reply@example.com>"
//	EMAIL_MOCK=true   send to hamr dev's inbox at HAMR_DEV_URL instead
func FromEnv() *Mailer {
	from := parseAddr(os.Getenv("MAIL_FROM"))
	if os.Getenv("EMAIL_MOCK") == "true" {
		if from.Email == "" {
			from = email.Addr("stackr", "no-reply@stackr.local")
		}
		return &Mailer{sender: emailmock.New(os.Getenv("HAMR_DEV_URL")), from: from}
	}
	key := strings.TrimSpace(os.Getenv("SMTP2GO_API_KEY"))
	if key == "" || from.Email == "" {
		return nil
	}
	return &Mailer{sender: &smtp2go{key: key, http: &http.Client{Timeout: 15 * time.Second}}, from: from}
}

// Enabled reports whether mail can actually be sent.
func (m *Mailer) Enabled() bool { return m != nil && m.sender != nil }

// Send delivers one message from the configured address. A nil mailer is not
// an error: it means this server has no provider, which callers treat as
// "hand the link over yourself".
func (m *Mailer) Send(ctx context.Context, to email.Address, subject, text, html string) error {
	if !m.Enabled() {
		return nil
	}
	_, err := m.sender.Send(ctx, email.Message{
		From: m.from, To: []email.Address{to}, Subject: subject, Text: text, HTML: html,
	})
	return err
}

// parseAddr reads "Name <addr@example.com>" or a bare address.
func parseAddr(s string) email.Address {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "<"); i >= 0 && strings.HasSuffix(s, ">") {
		return email.Addr(strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:len(s)-1]))
	}
	if s == "" {
		return email.Address{}
	}
	return email.Addr("", s)
}

// smtp2go posts to SMTP2GO's v3 send endpoint. The API takes plain JSON with
// the key in the body, so there is nothing to sign and no SDK to carry.
type smtp2go struct {
	key  string
	http *http.Client
}

func (s *smtp2go) Send(ctx context.Context, msg email.Message) (*email.Result, error) {
	body := map[string]any{
		"api_key":   s.key,
		"sender":    format(msg.From),
		"to":        formatAll(msg.To),
		"subject":   msg.Subject,
		"text_body": msg.Text,
		"html_body": msg.HTML,
	}
	if len(msg.Cc) > 0 {
		body["cc"] = formatAll(msg.Cc)
	}
	if len(msg.Bcc) > 0 {
		body["bcc"] = formatAll(msg.Bcc)
	}
	if len(msg.Headers) > 0 {
		body["custom_headers"] = headers(msg.Headers)
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.smtp2go.com/v3/email/send", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("smtp2go: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	// The API answers 200 with a per-request failure count, so the status code
	// alone does not mean the message went anywhere.
	var out struct {
		Data struct {
			Succeeded int      `json:"succeeded"`
			Failed    int      `json:"failed"`
			Failures  []string `json:"failures"`
			EmailID   string   `json:"email_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("smtp2go: bad response: %w", err)
	}
	if out.Data.Succeeded == 0 {
		return nil, fmt.Errorf("smtp2go: not delivered: %s", strings.Join(out.Data.Failures, "; "))
	}
	return &email.Result{ID: out.Data.EmailID}, nil
}

func format(a email.Address) string {
	if a.Name == "" {
		return a.Email
	}
	return a.Name + " <" + a.Email + ">"
}

func formatAll(as []email.Address) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, format(a))
	}
	return out
}

func headers(h map[string]string) []map[string]string {
	out := make([]map[string]string, 0, len(h))
	for k, v := range h {
		out = append(out, map[string]string{"header": k, "value": v})
	}
	return out
}
