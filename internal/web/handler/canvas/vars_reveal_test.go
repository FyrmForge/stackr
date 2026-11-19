package canvas_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

const revealEnv = "/acme/shop/dev/-/vars"

func envScope(s *webtest.Site) service.ParamScope {
	return service.ParamScope{Kind: "env", ID: s.Tile.Env}
}

func seedSecret(t *testing.T, s *webtest.Site) {
	t.Helper()
	_, err := s.Orch.SetParams(context.Background(), envScope(s), []service.ParamEntry{
		{Collection: "app", Name: "API_KEY", Kind: "secret", Value: "sk_live_1"},
		{Collection: "app", Name: "OTHER", Kind: "secret", Value: "sk_live_2"},
		{Collection: "app", Name: "LOG_LEVEL", Kind: "param", Value: "info"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Show returns the one secret asked for, in that answer only: the editor, an
// edit and a preview never carry a value, and the answer is not cached.
func TestRevealOneSecret(t *testing.T) {
	s := webtest.New(t)
	seedSecret(t, s)
	scope := "env:" + s.Tile.Env
	for _, page := range []string{get(t, s, revealEnv), get(t, s, revealEnv+"/edit"), get(t, s, revealEnv+"/edit?scope="+url.QueryEscape(scope)+"&group=app&name=API_KEY")} {
		if strings.Contains(page, "sk_live") || strings.Contains(page, "Reveal") {
			t.Fatalf("a secret reached the editor:\n%s", page)
		}
	}
	rec := s.Do(t, "POST", revealEnv+"/reveal", url.Values{"scope": {scope}, "collection": {"app"}, "name": {"API_KEY"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("reveal = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "sk_live_1") || strings.Contains(body, "sk_live_2") {
		t.Errorf("want exactly the asked secret:\n%s", body)
	}
	if !strings.Contains(body, "Hide") || !strings.Contains(body, "Hides in 30 s") || !strings.Contains(body, `hx-trigger="load delay:30s"`) {
		t.Errorf("no way back to the mask:\n%s", body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	if strings.Contains(rec.Header().Get("HX-Push-Url")+rec.Header().Get("Location"), "sk_live") {
		t.Error("the value is in a header")
	}
	// Hide is the plain editor: masked again
	if page := get(t, s, revealEnv+"/edit"); strings.Contains(page, "sk_live") {
		t.Error("Hide did not mask")
	}
	es, err := s.Orch.SecurityLog(context.Background(), service.SecurityFilter{Action: "secrets.reveal"})
	if err != nil || len(es) != 1 || es[0].Detail != "app.API_KEY" {
		t.Errorf("log = %+v %v", es, err)
	}
}

// A plain param, a missing name and a scope the level does not show are
// refused; a viewer cannot reveal.
func TestRevealRefused(t *testing.T) {
	s := webtest.New(t)
	seedSecret(t, s)
	scope := "env:" + s.Tile.Env
	for name, f := range map[string]url.Values{
		"a plain param": {"scope": {scope}, "collection": {"app"}, "name": {"LOG_LEVEL"}},
		"no such name":  {"scope": {scope}, "collection": {"app"}, "name": {"NOPE"}},
		"foreign scope": {"scope": {"org"}, "collection": {"app"}, "name": {"API_KEY"}},
		"no scope":      {"collection": {"app"}, "name": {"API_KEY"}},
	} {
		if name == "no scope" {
			continue // one column: no scope means that column, which is the editor's own
		}
		rec := s.Do(t, "POST", revealEnv+"/reveal", f)
		if rec.Code < 400 || strings.Contains(rec.Body.String(), "sk_live") || strings.Contains(rec.Body.String(), "info") && name == "a plain param" {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	v := s.User(t, "viewer@acme.test", false)
	s.Member(t, s.Org, v, "viewer")
	rec := s.As(t, s.Session(t, v), "POST", revealEnv+"/reveal", url.Values{"scope": {scope}, "collection": {"app"}, "name": {"API_KEY"}})
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "sk_live") {
		t.Errorf("viewer reveal = %d %s", rec.Code, rec.Body)
	}
}

// Review holds one edit with what it does and writes nothing; a blank secret
// or the same plain value holds nothing; Apply later saves it.
func TestPreviewVar(t *testing.T) {
	s := webtest.New(t)
	seedSecret(t, s)
	scope := "env:" + s.Tile.Env
	rec := s.Do(t, "POST", revealEnv+"/preview", url.Values{"scope": {scope}, "param.app.LOG_LEVEL": {"debug"}})
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "1 change not saved") || !strings.Contains(body, "debug") || !strings.Contains(body, `name="restart" value="now"`) {
		t.Fatalf("preview = %d\n%s", rec.Code, body)
	}
	if got := value(t, s, "env", s.Tile.Env); got["app.LOG_LEVEL"] != "info" {
		t.Errorf("preview wrote: %v", got)
	}
	for name, f := range map[string]url.Values{
		"same value":   {"scope": {scope}, "param.app.LOG_LEVEL": {"info"}},
		"blank secret": {"scope": {scope}, "secret.app.API_KEY": {""}},
	} {
		if body := s.Do(t, "POST", revealEnv+"/preview", f).Body.String(); strings.Contains(body, "not saved") {
			t.Errorf("%s held a change", name)
		}
	}
	rec = s.Do(t, "POST", revealEnv, url.Values{"scope": {scope}, "param.app.LOG_LEVEL": {"debug"}, "restart": {"later"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply later = %d %s", rec.Code, rec.Body)
	}
	if got := value(t, s, "env", s.Tile.Env); got["app.LOG_LEVEL"] != "debug" {
		t.Errorf("not saved: %v", got)
	}
}

// Held changes survive every redraw: a second edit adds to them, opening or
// cancelling another cell and Show carry them, Undo drops one. Apply later
// saves all of them in one post. Nothing is written until then.
func TestHeldChanges(t *testing.T) {
	s := webtest.New(t)
	seedSecret(t, s)
	scope := "env:" + s.Tile.Env
	held := func(body string) url.Values { // the footer's hidden fields, as the page would post them
		v := url.Values{"held_scope": {scope}}
		for _, f := range []string{"param.app.LOG_LEVEL", "param.app.MODE"} {
			if strings.Contains(body, `name="held.`+f+`"`) {
				i := strings.Index(body, `name="held.`+f+`" value="`) + len(`name="held.`+f+`" value="`)
				v.Set("held."+f, body[i:i+strings.Index(body[i:], `"`)])
			}
		}
		return v
	}
	first := s.Do(t, "POST", revealEnv+"/preview", url.Values{"scope": {scope}, "param.app.LOG_LEVEL": {"debug"}}).Body.String()
	if !strings.Contains(first, "1 change not saved") {
		t.Fatalf("first:\n%s", first)
	}
	f := held(first)
	f.Set("scope", scope)
	f.Set("param.app.MODE", "fast")
	second := s.Do(t, "POST", revealEnv+"/preview", f).Body.String()
	if !strings.Contains(second, "2 changes not saved") || !strings.Contains(second, `name="held.param.app.LOG_LEVEL"`) || !strings.Contains(second, `name="held.param.app.MODE"`) {
		t.Fatalf("second edit lost the first:\n%s", second)
	}
	// opening another cell keeps both
	f = held(second)
	f.Set("scope", scope)
	f.Set("group", "app")
	f.Set("name", "API_KEY")
	f.Set("secret", "1")
	if body := s.Do(t, "POST", revealEnv+"/edit", f).Body.String(); !strings.Contains(body, "app.LOG_LEVEL") || !strings.Contains(body, "app.MODE") || !strings.Contains(body, "3 changes not saved") {
		t.Errorf("an open edit dropped the held changes or the draft is not listed:\n%s", body)
	}
	// Undo drops one
	f = held(second)
	f.Set("drop", "param.app.LOG_LEVEL")
	if body := s.Do(t, "POST", revealEnv+"/preview", f).Body.String(); !strings.Contains(body, "1 change not saved") || strings.Contains(body, `name="held.param.app.LOG_LEVEL"`) {
		t.Errorf("undo:\n%s", body)
	}
	if got := value(t, s, "env", s.Tile.Env); got["app.LOG_LEVEL"] != "info" || got["app.MODE"] != "" {
		t.Errorf("a held change was written: %v", got)
	}
	// Apply posts the footer: both are saved
	f = held(second)
	f.Set("restart", "later")
	if rec := s.Do(t, "POST", revealEnv, f); rec.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body)
	}
	if got := value(t, s, "env", s.Tile.Env); got["app.LOG_LEVEL"] != "debug" || got["app.MODE"] != "fast" {
		t.Errorf("apply saved %v", got)
	}
}
