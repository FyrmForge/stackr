package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/api"
	"github.com/FyrmForge/stackr/internal/api/handler/v1"
	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// world: acme with shop/dev/api, an owner, a stranger from another org, an
// admin, each with a key.
type world struct {
	env                    *servicetest.Env
	h                      http.Handler
	acme                   string
	tile                   servicetest.Tile
	owner, stranger, admin string
}

func newWorld(t *testing.T, opts ...service.Option) *world {
	t.Helper()
	env := servicetest.NewWith(t, opts)
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	api.RegisterRoutes(srv, &api.Deps{Service: env.O, Access: middleware.NewAccess(env.O), DevMode: true})
	w := &world{env: env, h: srv.Echo(), acme: env.Org(t, "acme")}
	other := env.Org(t, "other")
	o, s := env.User(t, "owner@x", false), env.User(t, "stranger@x", false)
	env.Member(t, w.acme, o, "owner")
	env.Member(t, other, s, "owner")
	w.owner, w.stranger = env.APIKey(t, o, w.acme), env.APIKey(t, s, other)
	w.admin = env.APIKey(t, env.User(t, "admin@x", true), "")
	w.tile = env.Tile(t, w.acme)
	return w
}

func (w *world) do(t *testing.T, key, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

// fill puts the seeded slugs in a route path; ids the world lacks get "x".
func fill(path string) string {
	r := strings.NewReplacer(":org", "acme", ":stack", "shop", ":env", "dev", ":tile", "api")
	parts := strings.Split(r.Replace(path), "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			parts[i] = "x"
		}
	}
	return strings.Join(parts, "/")
}

// The table itself: unique ops, known verbs, every param checked.
func TestRouteTable(t *testing.T) {
	ops := map[string]bool{}
	for _, r := range api.Routes(&v1.H{}) {
		if ops[r.Op] {
			t.Errorf("op %s twice", r.Op)
		}
		ops[r.Op] = true
		if r.Verb != api.Self && r.Verb != api.Public && !authz.KnownVerb(r.Verb) {
			t.Errorf("%s %s: unknown verb %q", r.Method, r.Path, r.Verb)
		}
		for _, seg := range strings.Split(r.Path, "/") {
			if name, ok := strings.CutPrefix(seg, ":"); ok && !middleware.KnownParam(name) {
				t.Errorf("%s %s: param %s has no org check", r.Method, r.Path, name)
			}
		}
	}
}

// Every route, three callers: anonymous is 401, a stranger to acme is 404
// (403 on an admin verb), an owner below an admin verb is 403.
func TestEveryRouteGated(t *testing.T) {
	w := newWorld(t)
	for _, r := range api.Routes(&v1.H{}) {
		if r.Verb == api.Public {
			continue
		}
		path := fill(r.Path)
		if got, _ := w.do(t, "", r.Method, path, "{}"); got != 401 {
			t.Errorf("anonymous %s %s = %d, want 401", r.Method, path, got)
		}
		if r.Verb == api.Self {
			continue
		}
		admin := authz.LevelOf(r.Verb) == authz.LevelAdmin
		if strings.HasPrefix(r.Path, "/orgs/:org") {
			want := 404
			if admin {
				want = 403
			}
			if got, _ := w.do(t, w.stranger, r.Method, path, "{}"); got != want {
				t.Errorf("stranger %s %s = %d, want %d", r.Method, path, got, want)
			}
		}
		if admin {
			if got, _ := w.do(t, w.owner, r.Method, path, "{}"); got != 403 {
				t.Errorf("owner %s %s = %d, want 403", r.Method, path, got)
			}
		}
	}
}

// Every read an owner can make: no seeded secret comes back, nothing 500s,
// and an empty list is [] not null.
func TestReadsLeakNoSecret(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	o := w.env.O
	if _, err := o.CreateCredential(ctx, w.acme, service.CredentialSpec{Name: "hub", URL: "r.io", Username: "u", Password: "LEAK-cred"}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.CreateBackupDest(ctx, &w.acme, service.BackupDestSpec{Name: "s3", Endpoint: "https://s3.example.com", Bucket: "b",
		AccessKey: "LEAK-ak", SecretKey: "LEAK-sk"}); err != nil {
		t.Fatal(err)
	}
	if err := o.SetParams(ctx, service.ParamScope{Kind: "org", ID: w.acme},
		[]service.ParamEntry{{Collection: "app", Name: "token", Kind: "secret", Value: "LEAK-param"}}); err != nil {
		t.Fatal(err)
	}
	if code, body := w.do(t, w.owner, "POST", "/orgs/acme/keys", `{"name":"ci"}`); code != 201 || !strings.Contains(body, `"token"`) {
		t.Fatalf("mint = %d %s", code, body)
	}
	for _, r := range api.Routes(&v1.H{}) {
		if r.Method != http.MethodGet || r.Verb == "variable.write" {
			continue
		}
		path := fill(r.Path)
		code, body := w.do(t, w.owner, r.Method, path, "")
		if code >= 500 || strings.Contains(body, "LEAK") || strings.Contains(body, "token_hash") || strings.HasPrefix(body, "null") {
			t.Errorf("GET %s = %d %s", path, code, body)
		}
	}
	// The stronger verb reads the secret (B37).
	if _, body := w.do(t, w.owner, "GET", "/orgs/acme/params/secrets", ""); !strings.Contains(body, "LEAK-param") {
		t.Errorf("secrets route lacks the secret: %s", body)
	}
	if _, body := w.do(t, w.owner, "GET", "/orgs/acme/volumes", ""); body != "[]\n" {
		t.Errorf("empty list = %q, want []", body)
	}
}

// B1, B21, B22: a patch sets only what it sends; identity is not editable.
func TestTilePatch(t *testing.T) {
	w := newWorld(t)
	const path = "/orgs/acme/stacks/shop/envs/dev/tiles/api"
	if code, body := w.do(t, w.owner, "PATCH", path, `{"health_path":"/up"}`); code != 200 {
		t.Fatalf("patch = %d %s", code, body)
	}
	_, body := w.do(t, w.owner, "GET", path, "")
	var got service.Tile
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.HealthPath != "/up" || got.ImageRef != "nginx:1" || got.ContainerPort != 80 {
		t.Errorf("after patch: %+v", got)
	}
	var e middleware.APIError
	code, body := w.do(t, w.owner, "PATCH", path, `{"kind":"git"}`)
	if err := json.Unmarshal([]byte(body), &e); err != nil || code != 400 || e.Status != 400 || e.Error == "" {
		t.Errorf("kind patch = %d %s, want a 400 APIError", code, body)
	}
}

// Container work answers 202 with the job, and the job polls under its org.
func TestDeployIsAccepted(t *testing.T) {
	w := newWorld(t)
	code, body := w.do(t, w.owner, "POST", "/orgs/acme/stacks/shop/envs/dev/tiles/api/deploy", "")
	var j service.Job
	if err := json.Unmarshal([]byte(body), &j); err != nil || code != 202 || j.ID == "" {
		t.Fatalf("deploy = %d %s", code, body)
	}
	if code, body := w.do(t, w.owner, "GET", "/orgs/acme/jobs/"+j.ID+"/log?offset=0", ""); code != 200 || !strings.Contains(body, `"next"`) {
		t.Errorf("poll = %d %s", code, body)
	}
	if code, _ := w.do(t, w.owner, "GET", "/orgs/acme/jobs/"+j.ID+"/log?offset=abc", ""); code != 400 {
		t.Errorf("offset typo = %d, want 400 (B24)", code)
	}
}

// A job's events read end to end: updates as its log grows, then one end
// event with the finished job.
func TestJobEvents(t *testing.T) {
	stream.PollEvery = 10 * time.Millisecond
	w := newWorld(t)
	_, body := w.do(t, w.owner, "POST", "/orgs/acme/stacks/shop/envs/dev/tiles/api/deploy", "")
	var j service.Job
	if err := json.Unmarshal([]byte(body), &j); err != nil {
		t.Fatal(err)
	}
	code, body := w.do(t, w.owner, "GET", "/orgs/acme/jobs/"+j.ID+"/events", "")
	if code != 200 {
		t.Fatalf("events = %d %s", code, body)
	}
	i := strings.Index(body, "event: end\ndata: ")
	if i < 0 {
		t.Fatalf("no end event in %q", body)
	}
	var end v1.JobLogOut
	if err := json.Unmarshal([]byte(strings.TrimSpace(body[i+len("event: end\ndata: "):])), &end); err != nil {
		t.Fatal(err)
	}
	if end.Job.ID != j.ID || !end.End || end.Job.FinishedAt == nil {
		t.Errorf("end event = %+v", end)
	}
	if !strings.Contains(body, "event: update") {
		t.Errorf("no update before the end: %q", body)
	}
}
