package api_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
)

const repo = "https://github.com/acme/app.git"

// hookWorld is the world plus a git tile on acme/app@main, a connector
// with secret "whsec", and a builder that hands back a seeded image.
func hookWorld(t *testing.T) (*world, string) {
	t.Helper()
	var img string
	w := newWorld(t, service.WithBuild(func(context.Context, service.Stack, service.Tile, string, io.Writer) (string, error) {
		return img, nil
	}))
	img = w.env.Image(t, "stkr/acme_shop_web:abc")
	if _, err := w.env.Orch.CreateTile(context.Background(), service.Tile{StackID: w.tile.Stack, EnvironmentID: w.tile.Env,
		Name: "web", Kind: "service", GitURL: repo, GitBranch: "main", ContainerPort: 80}); err != nil {
		t.Fatal(err)
	}
	return w, w.env.Connector(t, w.acme, "whsec")
}

func (w *world) hook(t *testing.T, conn, event, secret, body string) int {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req := httptest.NewRequest("POST", "/hooks/connectors/"+conn, strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	return rec.Code
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

const push = `{"ref":"refs/heads/main","after":"abc1234","repository":{"clone_url":"` + repo + `"},"commits":[{"modified":["main.go"]}]}`

// A signed push lands a release; a bad signature or a garbled body lands
// nothing and says why.
func TestWebhookPush(t *testing.T) {
	w, conn := hookWorld(t)
	if code := w.hook(t, conn, "push", "wrong", push); code != 401 {
		t.Errorf("bad signature = %d, want 401", code)
	}
	if code := w.hook(t, conn, "push", "whsec", "{not json"); code != 400 {
		t.Errorf("bad payload = %d, want 400", code)
	}
	if code := w.hook(t, "nope", "push", "whsec", push); code != 404 {
		t.Errorf("unknown connector = %d, want 404", code)
	}
	if code := w.hook(t, conn, "push", "whsec", push); code != 204 {
		t.Fatalf("push = %d, want 204", code)
	}
	eventually(t, "a release", func() bool {
		rs, err := w.env.Orch.Releases(context.Background(), w.tile.Stack)
		return err == nil && len(rs) == 1
	})
}

// A PR opened against main makes pr-7 from dev; closing it removes it.
func TestWebhookPR(t *testing.T) {
	w, conn := hookWorld(t)
	pr := func(action string) string {
		return `{"action":"` + action + `","number":7,"repository":{"clone_url":"` + repo + `"},` +
			`"pull_request":{"head":{"ref":"feat","sha":"def5678"},"base":{"ref":"main"}}}`
	}
	has := func() bool {
		es, err := w.env.Orch.Envs(context.Background(), w.tile.Stack)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if e.Name == "pr-7" {
				return true
			}
		}
		return false
	}
	if code := w.hook(t, conn, "pull_request", "whsec", pr("opened")); code != 204 {
		t.Fatalf("opened = %d", code)
	}
	eventually(t, "pr-7 made", has)
	if code := w.hook(t, conn, "pull_request", "whsec", pr("closed")); code != 204 {
		t.Fatalf("closed = %d", code)
	}
	eventually(t, "pr-7 removed", func() bool { return !has() })
}
