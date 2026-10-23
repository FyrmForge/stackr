// Package github is the GitHub connector kind: a GitHub App, made by
// GitHub's manifest flow or pasted in, with the Git source capability.
// api.go wraps GitHub's API, webhook.go its deliveries.
package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
)

const kindName = "github"

// Kind is GitHub.
type Kind struct{}

func (Kind) Name() string  { return kindName }
func (Kind) Label() string { return "GitHub" }
func (Kind) Hint() string {
	return "A GitHub App: stackr clones your repositories and deploys on push."
}

func (Kind) New() connectors.Conn { return &Conn{} }

// Steps: the account the App is made under, GitHub's manifest flow, then
// its install, which comes back through the App's setup_url.
func (Kind) Steps() []connectors.Step {
	return []connectors.Step{
		{Title: "GitHub org", Fields: []connectors.Field{
			{Name: "owner", Label: "GitHub org", Optional: true, Help: "Leave empty for your personal account."},
		}},
		{Title: "Create on GitHub", Intro: "GitHub creates the app, then sends you back here.", Out: true},
		{Title: "Install", Intro: "Pick the repositories the app may read.", Out: true},
	}
}

// Alt is an App made on GitHub by hand.
func (Kind) Alt() connectors.Alt {
	return connectors.Alt{Label: "Use an existing App", Steps: []connectors.Step{{
		Title: "Existing App",
		Intro: "Point the App's webhook at the URL its settings show once saved.",
		Fields: []connectors.Field{
			{Name: "app_id", Label: "App ID", Help: "The number on the App's page."},
			{Name: "slug", Label: "Slug", Help: "The last part of the App's github.com/apps/ address."},
			{Name: "private_key", Label: "Private key", Secret: true, Multiline: true, Help: "The whole .pem file."},
			{Name: "webhook_secret", Label: "Webhook secret", Secret: true},
		},
	}}}
}

// Conn is one GitHub App: its settings blob. Owner is the GitHub org the
// manifest flow makes it under, until GitHub has made it.
type Conn struct {
	App
	Owner string `json:"owner,omitempty"`
}

var (
	_ connectors.GitSource  = (*Conn)(nil)
	_ connectors.Redirector = (*Conn)(nil)
	_ connectors.Installer  = (*Conn)(nil)
	_ connectors.Settled    = (*Conn)(nil)
)

func (c *Conn) Name() string { return "GitHub · " + c.Slug }

// Setup takes the GitHub org of a manifest flow, or the Alt form's App,
// whose key must parse: a bad one would only fail at the first clone.
func (c *Conn) Setup(v map[string]string) error {
	if _, alt := v["app_id"]; !alt {
		c.Owner = strings.TrimSpace(v["owner"])
		return nil
	}
	var bad []error
	id, err := strconv.ParseInt(strings.TrimSpace(v["app_id"]), 10, 64)
	if err != nil || id <= 0 {
		bad = append(bad, errs.Invalidf("app_id", "The App ID is the number on the App's page"))
	}
	c.App = App{ID: id, Slug: strings.TrimSpace(v["slug"]), PEM: v["private_key"], WebhookSecret: strings.TrimSpace(v["webhook_secret"])}
	if c.Slug == "" {
		bad = append(bad, errs.Invalidf("slug", "The App's slug is required"))
	}
	if _, err := ParseKey(c.PEM); err != nil {
		bad = append(bad, errs.Invalidf("private_key", "The private key does not parse. Paste the whole .pem file"))
	}
	if c.WebhookSecret == "" {
		bad = append(bad, errs.Invalidf("webhook_secret", "The webhook secret is required"))
	}
	return errors.Join(bad...)
}

// Values are the Alt form's fields, for an update in place.
func (c *Conn) Values() map[string]string {
	return map[string]string{"app_id": strconv.FormatInt(c.ID, 10), "slug": c.Slug, "private_key": c.PEM, "webhook_secret": c.WebhookSecret}
}

// Settled: the Alt form gave the App itself.
func (c *Conn) Settled() bool { return c.ID > 0 }

// Out posts the App's manifest to GitHub; back is the core callback, so
// the panel's own URL is what is left of it.
func (c *Conn) Out(_ context.Context, id, back, state string) (connectors.Out, error) {
	base := strings.TrimSuffix(back, connectors.CallbackPath)
	action, manifest, err := Manifest(base, back, id, c.Owner, state)
	if err != nil {
		return connectors.Out{}, err
	}
	return connectors.Out{Method: http.MethodPost, URL: action, Values: map[string]string{"manifest": manifest}}, nil
}

// Back trades GitHub's one-shot code for the App.
func (c *Conn) Back(ctx context.Context, q url.Values) error {
	code := q.Get("code")
	if code == "" {
		return errs.Refusedf("GitHub sent no code")
	}
	app, err := api.ConvertManifest(ctx, code)
	if err != nil {
		return err
	}
	c.App, c.Owner = app, ""
	return nil
}

// Install is GitHub's page for installing the App on an account and
// picking its repos. Making the App is not installing it: without an
// install there is no token to clone with.
func (c *Conn) Install(state string) string {
	u := "https://github.com/apps/" + c.Slug + "/installations/new"
	if state != "" {
		u += "?state=" + url.QueryEscape(state)
	}
	return u
}

// The Git source capability.

func (c *Conn) Host() string { return host }

func (c *Conn) RepoURL(raw string) (string, bool) {
	u := RepoURL(raw)
	return u, strings.HasPrefix(u, "https://"+host+"/")
}

// Repos is empty, no error, while the App is installed nowhere.
func (c *Conn) Repos(ctx context.Context) ([]connectors.Repo, error) {
	tok, err := api.Token(ctx, c.App)
	if errors.Is(err, ErrNotInstalled) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rs, err := api.Repos(ctx, tok)
	out := make([]connectors.Repo, len(rs))
	for i, r := range rs {
		out[i] = connectors.Repo{FullName: r.FullName, URL: "https://" + host + "/" + r.FullName, Private: r.Private, DefaultBranch: r.DefaultBranch}
	}
	return out, err
}

func (c *Conn) Branches(ctx context.Context, repo string) ([]string, error) {
	tok, err := api.Token(ctx, c.App)
	if err != nil {
		return nil, err
	}
	return api.Branches(ctx, tok, repo)
}

func (c *Conn) YAMLFiles(ctx context.Context, repo, ref string) ([]string, error) {
	tok, err := api.Token(ctx, c.App)
	if err != nil {
		return nil, err
	}
	return api.YAMLFiles(ctx, tok, repo, ref)
}

func (c *Conn) CloneEnv(ctx context.Context, repoURL string) ([]string, error) {
	tok, err := api.Token(ctx, c.App)
	if err != nil {
		return nil, err
	}
	return CloneAuth(repoURL, tok), nil
}

// Hook verifies a delivery against the App's webhook secret before it
// decodes anything.
func (c *Conn) Hook(_ context.Context, h http.Header, body []byte) (connectors.Event, error) {
	ev, err := Receive(h.Get("X-GitHub-Event"), h.Get("X-Hub-Signature-256"), body, c.WebhookSecret)
	if err != nil {
		return connectors.Event{}, err
	}
	return neutral(ev), nil
}

func (c *Conn) SetHook(ctx context.Context, hook string) error {
	return api.SetHookURL(ctx, hook, c.App)
}
