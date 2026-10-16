package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/jointoken"
)

// The join script and its installer download answer a live token only, and
// leave it unspent.
func TestJoinScriptGate(t *testing.T) {
	w, adm, ca := joinWorld(t)
	ctx := context.Background()
	w.orch.cfg.BaseURL, w.orch.cfg.Version = "https://panel.test", "v0.7.0"
	bin := filepath.Join(t.TempDir(), "stackr-install")
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))
	old := installerPath
	installerPath = func() string { return bin }
	t.Cleanup(func() { installerPath = old })

	out := mint(t, w, adm, "w1")
	if out.OneLine != "curl -fsSL https://panel.test/join/"+out.Token+" | sh" || out.TwoStep != "" {
		t.Errorf("one line = %q, two step = %q", out.OneLine, out.TwoStep)
	}
	s, err := w.orch.JoinScript(ctx, httpsMeta, out.Token)
	must(t, err)
	for _, want := range []string{
		"'https://panel.test/join/" + out.Token + "/stackr-install'",
		"'join' '--panel' 'https://panel.test' '--token' '" + out.Token + "' '--ca-sha256' '" + caFingerprint(ca) + "' '--version' '0.7.0' </dev/null",
		"mktemp -d", "trap 'rm -rf \"$tmp\"' EXIT", "\nmain\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	if p, err := w.orch.JoinInstaller(ctx, httpsMeta, out.Token); err != nil || p != bin {
		t.Errorf("installer = %q, %v", p, err)
	}
	// neither spent it
	if _, err := w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, out.Token)); err != nil {
		t.Fatalf("join after the script = %v", err)
	}

	exp := mint(t, w, adm, "w2")
	_, err = w.st.DB().Exec(`UPDATE join_tokens SET expires_at = ? WHERE hash = ?`, time.Now().UTC().Add(-time.Minute), jointoken.Hash(exp.Token))
	must(t, err)
	for name, tok := range map[string]string{
		"spent": out.Token, "expired": exp.Token, "unknown": jointoken.Prefix + strings.Repeat("a", 32),
		"malformed": "../../etc/passwd", "empty": "",
	} {
		if _, err := w.orch.JoinScript(ctx, httpsMeta, tok); !isNotFound(err) {
			t.Errorf("script for %s token = %v", name, err)
		}
		if _, err := w.orch.JoinInstaller(ctx, httpsMeta, tok); !isNotFound(err) {
			t.Errorf("installer for %s token = %v", name, err)
		}
	}

	// no installer next to stackrd: a refusal, not a 404
	live := mint(t, w, adm, "w3")
	installerPath = func() string { return filepath.Join(t.TempDir(), "missing") }
	if _, err := w.orch.JoinInstaller(ctx, httpsMeta, live.Token); err == nil || isNotFound(err) {
		t.Errorf("missing installer = %v", err)
	}
}

// A hostile panel URL stays one quoted word: sh parses the script, and the
// value comes back unchanged.
func TestJoinScriptQuoting(t *testing.T) {
	w, adm, _ := joinWorld(t)
	w.orch.cfg.BaseURL = "https://x'$(touch /tmp/pwned)`id`\"\n;"
	out := mint(t, w, adm, "w1")
	if !strings.Contains(out.Command, shQuote(w.orch.cfg.BaseURL)) || !strings.Contains(out.OneLine, shQuote(w.orch.cfg.BaseURL+"/join/"+out.Token)) {
		t.Errorf("printed commands do not quote the panel:\n%s\n%s", out.Command, out.OneLine)
	}
	s, err := w.orch.JoinScript(context.Background(), httpsMeta, out.Token)
	must(t, err)
	if !strings.Contains(s, shQuote(w.orch.cfg.BaseURL)) {
		t.Errorf("panel not quoted:\n%s", s)
	}
	if b, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v %s", err, b)
	}
	got, err := exec.Command("sh", "-c", "printf %s "+shQuote(w.orch.cfg.BaseURL)).Output()
	if err != nil || string(got) != w.orch.cfg.BaseURL {
		t.Errorf("round trip = %q, %v", got, err)
	}
}

// A panel without a trusted certificate prints the two-step command, and
// serves no script: one fetched without it could be swapped on the way. Nor
// does any panel serve it over plain http.
func TestJoinTwoStep(t *testing.T) {
	w, adm, _ := joinWorld(t)
	ctx := context.Background()
	w.orch.cfg.BaseURL = "https://panel.test"
	tls := mint(t, w, adm, "w0")
	if _, err := w.orch.JoinScript(ctx, JoinMeta{}, tls.Token); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("script over http = %v", err)
	}
	if _, err := w.orch.JoinInstaller(ctx, JoinMeta{}, tls.Token); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("installer over http = %v", err)
	}
	w.orch.cfg.BaseURL, w.orch.cfg.TLSOff = "http://panel.test", true
	out := mint(t, w, adm, "w1")
	if out.OneLine != "" || !strings.Contains(out.TwoStep, "releases/latest/download/stackr-install-linux-amd64") ||
		!strings.HasSuffix(out.TwoStep, "\n./"+out.Command) {
		t.Errorf("one line = %q, two step = %q", out.OneLine, out.TwoStep)
	}
	if _, err := w.orch.JoinScript(ctx, httpsMeta, out.Token); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("script without a trusted certificate = %v", err)
	}
	w.orch.cfg.Version = "v0.7.0"
	if out := mint(t, w, adm, "w2"); !strings.Contains(out.TwoStep, "releases/download/v0.7.0/") {
		t.Errorf("two step = %q", out.TwoStep)
	}
}
