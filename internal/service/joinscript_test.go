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

	"github.com/FyrmForge/stackr/internal/authz"
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

// The add-node choice rides on the token: no orgs = open to all, named orgs
// = only those. A rejoin keeps what the node had.
func TestJoinTokenCarriesOrgs(t *testing.T) {
	w, adm, _ := joinWorld(t)
	ctx := context.Background()

	if _, err := w.orch.CreateJoinToken(ctx, adm, JoinTokenSpec{NodeName: "w0", Orgs: []string{"nope"}}); !errors.As(err, new(errs.Invalid)) {
		t.Errorf("unknown org = %v", err)
	}
	all := mint(t, w, adm, "w1")
	st, err := w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, all.Token))
	must(t, err)
	n, err := w.st.Nodes.Get(ctx, st.NodeID)
	must(t, err)
	if !n.OpenToAll {
		t.Error("a node minted without orgs is not open to all")
	}

	some, err := w.orch.CreateJoinToken(ctx, adm, JoinTokenSpec{NodeName: "w2", Orgs: []string{"acme"}})
	must(t, err)
	st, err = w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, some.Token))
	must(t, err)
	n, err = w.st.Nodes.Get(ctx, st.NodeID)
	must(t, err)
	ids, err := w.st.NodeOrgs.OrgIDs(ctx, n.ID)
	must(t, err)
	if n.OpenToAll || len(ids) != 1 || ids[0] != w.org {
		t.Errorf("open to all = %v, orgs = %v", n.OpenToAll, ids)
	}
	if _, err := w.orch.ConfirmJoin(ctx, adm, n.ID); err != nil {
		t.Fatal(err)
	}
	// a rejoin keeps the node's own orgs, whatever its token says
	rj, err := w.orch.RejoinNode(ctx, adm, "w2")
	must(t, err)
	_, err = w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, rj.Token))
	must(t, err)
	n, err = w.st.Nodes.Get(ctx, n.ID)
	must(t, err)
	if ids, _ := w.st.NodeOrgs.OrgIDs(ctx, n.ID); n.OpenToAll || len(ids) != 1 {
		t.Errorf("after rejoin open to all = %v, orgs = %v", n.OpenToAll, ids)
	}
}

// Open to all lets every org pin; closing it falls back to the node's own
// list, which it never touches.
func TestNodeOpenToAll(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	adm := &Principal{User: User{ID: "u-admin"}, Access: authz.User{ID: "u-admin", Active: true, Admin: true}}
	d, err := n.orch.NodeDetail(ctx, "w1")
	must(t, err)
	org := d.Orgs[0].ID

	must(t, n.orch.SetNodeOpenToAll(ctx, adm, "w1", true))
	if ok, _ := n.orch.NodeOpenToOrg(ctx, "w1", org); !ok {
		t.Error("open to all, yet the org may not pin")
	}
	if _, err := n.orch.OpenNode(ctx, adm, "w1", "acme", false); err == nil {
		t.Error("closed one org on a node open to all")
	}
	if out, err := n.orch.OpenNodeAll(ctx, adm, "w1", false); err != nil || out.OpenToAll {
		t.Fatalf("close all = %+v, %v", out, err)
	}
	if ok, _ := n.orch.NodeOpenToOrg(ctx, "w1", org); ok {
		t.Error("closed, yet the org may pin")
	}
	if d, _ := n.orch.NodeDetail(ctx, "w1"); len(d.EnvList) != 1 {
		t.Errorf("closing moved envs: %+v", d.EnvList)
	}
	must(t, n.orch.SetNodeOrgs(ctx, adm, "w1", []string{org}))
	if ok, _ := n.orch.NodeOpenToOrg(ctx, "w1", org); !ok {
		t.Error("opened one by one, yet the org may not pin")
	}
	if err := n.orch.SetNodeOpenToAll(ctx, adm, n.ctl.ID, false); err == nil {
		t.Error("closed the controller")
	}
	if err := n.orch.SetNodeOpenToAll(ctx, &Principal{}, "w1", true); err == nil {
		t.Error("a non-admin opened a node")
	}
}
