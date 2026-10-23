package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
)

// The manifest points GitHub at the paths Apps already registered use:
// setup_url /settings/github/installed/<id>, the hook /hooks/connectors/<id>;
// GitHub's code comes back to the core callback.
func TestOutKeepsRegisteredPaths(t *testing.T) {
	c := &Conn{}
	if err := c.Setup(map[string]string{"owner": " acme "}); err != nil || c.Settled() {
		t.Fatalf("manifest setup: %v settled %v", err, c.Settled())
	}
	out, err := c.Out(context.Background(), "abcdef12", "https://p.example.com"+connectors.CallbackPath, "abcdef12.n")
	if err != nil || out.Method != "POST" || !strings.HasPrefix(out.URL, "https://github.com/organizations/acme/settings/apps/new?state=") {
		t.Fatalf("out = %+v %v", out, err)
	}
	var m struct {
		Redirect string               `json:"redirect_url"`
		Setup    string               `json:"setup_url"`
		Hook     struct{ URL string } `json:"hook_attributes"`
	}
	if err := json.Unmarshal([]byte(out.Values["manifest"]), &m); err != nil ||
		m.Setup != "https://p.example.com/settings/github/installed/abcdef12" ||
		m.Hook.URL != "https://p.example.com/hooks/connectors/abcdef12" ||
		m.Redirect != "https://p.example.com/-/connectors/callback" {
		t.Fatalf("manifest = %+v %v", m, err)
	}
	if got := (&Conn{App: App{Slug: "s"}}).Install("a b"); got != "https://github.com/apps/s/installations/new?state=a+b" {
		t.Errorf("install = %s", got)
	}
}

// The existing App form: every bad field at once, never the key in a
// message; a good one is settled, named by its slug.
func TestSetupExistingApp(t *testing.T) {
	c := &Conn{}
	err := c.Setup(map[string]string{"app_id": "x", "slug": "", "private_key": "nope", "webhook_secret": ""})
	for _, f := range []string{"app_id", "slug", "private_key", "webhook_secret"} {
		found := false
		for _, e := range err.(interface{ Unwrap() []error }).Unwrap() {
			if inv, ok := errs.IsInvalid(e); ok && inv.Field == f {
				found = true
			}
		}
		if !found {
			t.Errorf("no refusal on %s: %v", f, err)
		}
	}
	_, key := testKey(t)
	c = &Conn{}
	if err := c.Setup(map[string]string{"app_id": "42", "slug": "my-app", "private_key": key, "webhook_secret": "w"}); err != nil ||
		!c.Settled() || c.Name() != "GitHub · my-app" {
		t.Fatalf("good app: %v %q", err, c.Name())
	}
}

// A blob in the shape the rows have always had decodes, App fields flat.
func TestBlob(t *testing.T) {
	var c Conn
	if err := json.Unmarshal([]byte(`{"id":2,"slug":"srv","pem":"p","webhook_secret":"w"}`), &c); err != nil ||
		c.ID != 2 || c.Slug != "srv" || c.WebhookSecret != "w" {
		t.Fatalf("decoded %+v %v", c, err)
	}
}

func TestRepoURL(t *testing.T) {
	c := &Conn{}
	for raw, want := range map[string]string{
		"acme/api":                    "https://github.com/acme/api",
		"https://github.com/acme/api": "https://github.com/acme/api",
	} {
		if got, ok := c.RepoURL(raw); !ok || got != want {
			t.Errorf("%s = %s %v", raw, got, ok)
		}
	}
	if _, ok := c.RepoURL("https://gitlab.com/acme/api"); ok {
		t.Error("a GitLab URL is GitHub's")
	}
}

// Hook answers the neutral event: a branch push with its fields, a PR;
// a tag push or a deletion is nothing; a bad signature is refused.
func TestHook(t *testing.T) {
	c := &Conn{App: App{WebhookSecret: "k"}}
	h := func(event, body string) http.Header {
		mac := hmac.New(sha256.New, []byte("k"))
		mac.Write([]byte(body))
		return http.Header{"X-Github-Event": {event}, "X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}
	}
	ctx := context.Background()
	push := `{"ref":"refs/heads/main","after":"abc","head_commit":{"message":"m"},"repository":{"clone_url":"https://github.com/a/b.git","default_branch":"main"}}`
	ev, err := c.Hook(ctx, h("push", push), []byte(push))
	if err != nil || ev.Push == nil || ev.Push.Branch != "main" || ev.Push.Commit != "abc" || ev.Push.Message != "m" ||
		ev.Push.Repo != "https://github.com/a/b.git" || ev.Push.DefaultBranch != "main" || ev.Push.Changed != nil {
		t.Fatalf("push = %+v %v", ev.Push, err)
	}
	tag := `{"ref":"refs/tags/v1","after":"abc"}`
	if ev, err := c.Hook(ctx, h("push", tag), []byte(tag)); err != nil || ev.Push != nil {
		t.Errorf("tag push = %+v %v", ev, err)
	}
	pr := `{"action":"opened","number":3,"pull_request":{"head":{"ref":"f","sha":"s1"},"base":{"ref":"main"}},"repository":{"full_name":"a/b"}}`
	if ev, err := c.Hook(ctx, h("pull_request", pr), []byte(pr)); err != nil || ev.PR == nil || ev.PR.Number != 3 || !ev.PR.Fork || ev.PR.SHA != "s1" {
		t.Errorf("pr = %+v %v", ev.PR, err)
	}
	if _, err := c.Hook(ctx, http.Header{"X-Hub-Signature-256": {"sha256=00"}}, []byte(push)); !errors.Is(err, connectors.ErrBadSignature) {
		t.Errorf("bad signature: %v", err)
	}
	if _, err := (&Conn{}).Hook(ctx, h("push", push), []byte(push)); !errors.Is(err, connectors.ErrBadSignature) {
		t.Errorf("no secret verified: %v", err)
	}
}

// Back trades the code for the App; Repos lists through the minted token,
// none while the App is installed nowhere.
func TestBackAndRepos(t *testing.T) {
	_, key := testKey(t)
	installs := `[]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app-manifests/c0de/conversions":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(App{ID: 9, Slug: "made", PEM: key, WebhookSecret: "w"})
		case "GET /app/installations":
			_, _ = w.Write([]byte(installs))
		case "POST /app/installations/7/access_tokens":
			_, _ = w.Write([]byte(`{"token":"ghs_y","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
		case "GET /installation/repositories":
			_, _ = w.Write([]byte(`{"repositories":[{"full_name":"acme/api","default_branch":"main"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := api.apiURL
	api.apiURL = srv.URL
	defer func() { api.apiURL = old }()
	ctx := context.Background()
	c := &Conn{Owner: "acme"}
	if err := c.Back(ctx, url.Values{}); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("no code: %v", err)
	}
	if err := c.Back(ctx, url.Values{"code": {"c0de"}}); err != nil || c.ID != 9 || c.Owner != "" {
		t.Fatalf("back: %+v %v", c, err)
	}
	if rs, err := c.Repos(ctx); err != nil || rs != nil {
		t.Errorf("not installed = %v %v", rs, err)
	}
	installs = `[{"id":7}]`
	rs, err := c.Repos(ctx)
	if err != nil || len(rs) != 1 || rs[0].URL != "https://github.com/acme/api" {
		t.Fatalf("repos = %+v %v", rs, err)
	}
	if env, err := c.CloneEnv(ctx, "https://github.com/acme/api"); err != nil || len(env) != 3 {
		t.Errorf("clone env = %d lines %v", len(env), err)
	}
}
