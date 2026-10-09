package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// PR envs are opt-in: pr_envs.enabled in the stack file at the config
// branch head. Off or absent makes nothing; closing removes regardless.
func TestPREnvsOptIn(t *testing.T) {
	ctx := context.Background()
	g := servicetest.NewGit(t)
	env := servicetest.NewWith(t, []service.Option{g.Option()})
	org := env.Org(t, "acme")
	conn := env.Connector(t, org, "whsec")
	st, err := env.Orch.CreateStack(ctx, org, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.SetConfigRepo(ctx, st.ID, conn, "acme/shop", "", ""); err != nil {
		t.Fatal(err)
	}
	hook := func(event, body string) {
		t.Helper()
		mac := hmac.New(sha256.New, []byte("whsec"))
		mac.Write([]byte(body))
		if err := env.Orch.Webhook(ctx, conn, event, "sha256="+hex.EncodeToString(mac.Sum(nil)), []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	file := func(extra string) {
		t.Helper()
		sha := g.Commit(t, "acme/shop", "main", map[string]string{
			"stackr-compose.yml": "version: 1\nstack: shop\nladder:\n  - dev\nhead: main\n" + extra,
		})
		hook("push", `{"ref":"refs/heads/main","after":"`+sha+`",`+
			`"repository":{"clone_url":"https://github.com/acme/shop.git","default_branch":"main"},`+
			`"commits":[{"modified":["stackr-compose.yml"]}]}`)
	}
	pr := func(action string) {
		t.Helper()
		hook("pull_request", `{"action":"`+action+`","number":7,"repository":{"clone_url":"https://github.com/acme/shop.git"},`+
			`"pull_request":{"head":{"ref":"feat","sha":"def5678"},"base":{"ref":"main"}}}`)
	}
	has := func() bool {
		es, err := env.Orch.Envs(ctx, st.ID)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(es, func(e service.Environment) bool { return e.Name == "pr-7" })
	}
	hasDev := func() bool {
		es, err := env.Orch.Envs(ctx, st.ID)
		return err == nil && slices.ContainsFunc(es, func(e service.Environment) bool { return e.Name == "dev" })
	}

	file("")
	eventually(t, "dev", hasDev)
	pr("opened")
	// the PR job runs after the push job (same queue); give it a moment
	// by queueing a second, observable job behind it.
	file("")
	eventually(t, "the queue to drain", func() bool { return hasDev() })
	if has() {
		t.Fatal("pr-7 made with no pr_envs")
	}

	file("pr_envs:\n  enabled: true\n")
	pr("opened")
	eventually(t, "pr-7 made", has)

	file("")
	pr("closed")
	eventually(t, "pr-7 removed", func() bool { return !has() })
}

// A stack file that does not load is not "PR envs are off": the job log says
// why, and pr_envs.against is refused until it is built.
func TestPREnvsBadFileSaysWhy(t *testing.T) {
	ctx := context.Background()
	g := servicetest.NewGit(t)
	env := servicetest.NewWith(t, []service.Option{g.Option()})
	org := env.Org(t, "acme")
	conn := env.Connector(t, org, "whsec")
	st, err := env.Orch.CreateStack(ctx, org, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.SetConfigRepo(ctx, st.ID, conn, "acme/shop", "", ""); err != nil {
		t.Fatal(err)
	}
	g.Commit(t, "acme/shop", "main", map[string]string{
		"stackr-compose.yml": "version: 1\nstack: shop\nladder:\n  - dev\nhead: main\npr_envs:\n  enabled: true\n  against: [dev]\n",
	})
	body := `{"action":"opened","number":7,"repository":{"clone_url":"https://github.com/acme/shop.git"},` +
		`"pull_request":{"head":{"ref":"feat","sha":"def5678"},"base":{"ref":"main"}}}`
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write([]byte(body))
	if err := env.Orch.Webhook(ctx, conn, "pull_request", "sha256="+hex.EncodeToString(mac.Sum(nil)), []byte(body)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the job log names the file error", func() bool {
		js, err := env.Orch.Jobs(ctx, 10)
		if err != nil {
			return false
		}
		for _, j := range js {
			if _, l, err := env.Orch.PollJob(ctx, j.ID, 0); err == nil && strings.Contains(string(l.Chunk), "pr_envs.against is not built yet; remove it; PR envs off") {
				return true
			}
		}
		return false
	})
}
