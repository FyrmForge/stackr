package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/cliconfig"
	"github.com/FyrmForge/stackr/internal/installspec"
)

func TestCheck(t *testing.T) {
	ok := installspec.Input{
		Root:      "https://Example.com/",
		HTTPS:     true,
		Email:     "Ops@Example.com",
		Proxies:   "192.168.1.100, 10.0.0.0/8,10.0.0.0/8",
		HTTPPort:  "80",
		HTTPSPort: "443",
		DataDir:   "/var/lib/stackr/",
	}
	if err := check(&ok, nil); err != nil {
		t.Fatal(err)
	}
	want := installspec.Input{
		Root:      "example.com",
		PanelHost: "stkr.example.com",
		HTTPS:     true,
		Email:     "ops@example.com",
		Proxies:   "192.168.1.100/32,10.0.0.0/8",
		HTTPPort:  "80",
		HTTPSPort: "443",
		DataDir:   "/var/lib/stackr",
	}
	if ok != want {
		t.Fatalf("got %+v", ok)
	}

	plain := installspec.Input{
		Root:      "example.com",
		Email:     "x",
		HTTPSPort: "443",
		HTTPPort:  "8080",
		DataDir:   "/d",
	}
	if err := check(&plain, nil); err != nil || plain.Email != "" || plain.HTTPSPort != "" {
		t.Fatalf("plain HTTP keeps TLS answers: %+v %v", plain, err)
	}

	base := func() installspec.Input {
		return installspec.Input{
			Root:      "example.com",
			HTTPS:     true,
			Email:     "a@example.com",
			HTTPPort:  "80",
			HTTPSPort: "443",
			DataDir:   "/d",
		}
	}
	for name, mut := range map[string]func(*installspec.Input){
		"ip domain":       func(in *installspec.Input) { in.Root = "1.2.3.4" },
		"host elsewhere":  func(in *installspec.Input) { in.PanelHost = "panel.other.com" },
		"email localhost": func(in *installspec.Input) { in.Email = "ops@localhost" },
		"email ip":        func(in *installspec.Input) { in.Email = "ops@1.2.3.4" },
		"no email":        func(in *installspec.Input) { in.Email = "" },
		"port zero lead":  func(in *installspec.Input) { in.HTTPPort = "080" },
		"port range":      func(in *installspec.Input) { in.HTTPPort = "70000" },
		"same ports":      func(in *installspec.Input) { in.HTTPSPort = "80" },
		"proxy all":       func(in *installspec.Input) { in.Proxies = "0.0.0.0/0" },
		"proxy v6 all":    func(in *installspec.Input) { in.Proxies = "::/0" },
		"proxy junk":      func(in *installspec.Input) { in.Proxies = "nope" },
		"relative dir":    func(in *installspec.Input) { in.DataDir = "var/lib" },
		"root dir":        func(in *installspec.Input) { in.DataDir = "/" },
		"comma dir":       func(in *installspec.Input) { in.DataDir = "/a,b" },
		"colon dir":       func(in *installspec.Input) { in.DataDir = "/a:b" },
	} {
		in := base()
		mut(&in)
		if err := check(&in, nil); err == nil {
			t.Errorf("%s: accepted %+v", name, in)
		}
	}

	in := base()
	if err := check(&in, func(p string) bool { return p == "443" }); err == nil ||
		!strings.Contains(err.Error(), "https port") {
		t.Errorf("busy port: %v", err)
	}
}

func TestCheckVersion(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"", "v1.2.3"}:        "1.2.3",
		{"latest", "1.2.3"}:   "1.2.3",
		{"v0.4.0", "dev"}:     "0.4.0",
		{"1.0.0-rc.1", "dev"}: "1.0.0-rc.1",
		{"0.10.0", "v0.9.0"}:  "0.10.0",
	} {
		if got, err := checkVersion(in[0], in[1]); err != nil || got != want {
			t.Errorf("%v: %q %v", in, got, err)
		}
	}
	for _, bad := range [][2]string{
		{"", "dev"},
		{"1.2", "dev"},
		{"1.2.3;rm", "dev"},
		{"latest", "abc123"},
	} {
		if got, err := checkVersion(bad[0], bad[1]); err == nil {
			t.Errorf("%v accepted as %q", bad, got)
		}
	}
}

func TestDoneText(t *testing.T) {
	in := installspec.Input{
		Root:      "example.com",
		PanelHost: "stkr.example.com",
		HTTPS:     true,
		HTTPSPort: "443",
		DataDir:   "/d",
	}
	s := doneText(in, "k3y", "me@example.com", true, false)
	for _, want := range []string{"https://stkr.example.com", "k3y", "me@example.com", "*.example.com", "Caddy",
		"CLI on this box is logged in", docsCLILogin} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	rerun := doneText(in, "", "", false, false)
	if strings.Contains(rerun, "Recovery key") || strings.Contains(rerun, "log in") || strings.Contains(rerun, "logged in") {
		t.Error("a re-run printed the recovery key or the new admin")
	}
}

// The installer logs the box's CLI in by writing its config, no call: a
// fresh box has no DNS or certificate yet.
func TestLoginCLIWritesTheConfig(t *testing.T) {
	in := installspec.Input{Root: "example.com", PanelHost: "stkr.example.com", HTTPS: true, HTTPSPort: "443", DataDir: t.TempDir()}
	p := filepath.Join(in.DataDir, "cli.json")
	if err := (runner{dry: true, out: &strings.Builder{}}).loginCLI(in, "k3y"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("a dry run wrote %s", p)
	}
	if err := (runner{out: &strings.Builder{}}).loginCLI(in, "k3y"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cli.json = %v, %v; want 0600", fi, err)
	}
	b, _ := os.ReadFile(p)
	var c cliconfig.Config
	if err := json.Unmarshal(b, &c); err != nil || c.Server != "https://stkr.example.com" || c.Key != "k3y" || c.Org != "" {
		t.Errorf("cli.json = %+v, %v", c, err)
	}
}

// Each fresh install names its own panel archives; a re-run keeps the saved
// id (a different one would fail the "other answers" check, and orphan the
// archives already in the bucket).
func TestInstallID(t *testing.T) {
	a, b := newInstallID(), newInstallID()
	if len(a) != 16 || strings.Trim(a, "0123456789abcdef") != "" || a == b {
		t.Errorf("ids = %q %q, want two distinct 16-char hex strings", a, b)
	}
	saved := installspec.Input{Root: "example.com", InstallID: "9f2c41d7a0b34e5a"}
	in := installspec.Input{}
	fillFrom(&in, saved, map[string]bool{})
	if in.InstallID != saved.InstallID {
		t.Errorf("a re-run dropped the saved install id: %q", in.InstallID)
	}
}

// An install.json saved before BridgeSubnet existed, or on a box whose
// bridge moved, still re-runs: box facts are not answers.
func TestWithBoxIgnoresBoxFacts(t *testing.T) {
	now := installspec.Input{Root: "example.com", Bind: "172.17.0.1", BridgeSubnet: "172.17.0.0/16"}
	old := installspec.Input{Root: "example.com", Bind: "172.18.0.1"}
	if !reflect.DeepEqual(withBox(old, now), now) {
		t.Error("box facts made a re-run look like other answers")
	}
	old.Root = "other.com"
	if reflect.DeepEqual(withBox(old, now), now) {
		t.Error("a changed answer was hidden")
	}
}

// The wrapper passes the CLI's STACKR_* variables into the panel container
// by name, values off argv, and keeps its own STACKR_CONFIG.
func TestWrapperPassesStackrEnv(t *testing.T) {
	dir := t.TempDir()
	fake := "#!/bin/sh\nif [ \"$1\" = ps ]; then echo c1; exit; fi\necho \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	w := filepath.Join(dir, "stackr")
	if err := os.WriteFile(w, []byte(wrapper("/data")), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(w, "org", "ls")
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "STACKR_KEY=s3cret", "STACKR_CONFIG=/elsewhere", "OTHER=1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if want := "exec -i -e STACKR_KEY -e STACKR_CONFIG=/data/cli.json c1 /stackr org ls"; got != want {
		t.Fatalf("docker got %q, want %q", got, want)
	}
}

// A stray word (stackr-install version) is refused before any check runs,
// instead of starting an install.
func TestRunRefusesUnknownCommand(t *testing.T) {
	err := run(context.Background(), []string{"version"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `unknown command "version"`) {
		t.Fatalf("got %v", err)
	}
}
