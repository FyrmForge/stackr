package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// fakeDocker puts a docker script on PATH that logs every call. have lists
// the containers that exist; "run" fails when failRun is set. Inspecting the
// proxy's spec label answers "old", so the spec always differs.
func fakeDocker(t *testing.T, have string, failRun bool) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	fail := "0"
	if failRun {
		fail = "1"
	}
	script := `#!/bin/sh
echo "$*" >> ` + log + `
case "$1" in
container) for h in ` + have + `; do [ "$h" = "$3" ] && exit 0; done; exit 1;;
inspect) case "$*" in *Labels*) echo old;; *Env*) echo DNS_API_TOKEN=live-secret;; esac; exit 0;;
run) [ ` + fail + ` = 1 ] && exit 1; exit 0;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return log
}

func proxyCalls(t *testing.T, log string) []string {
	b, _ := os.ReadFile(log)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] != "container" && f[0] != "inspect" {
			out = append(out, f[0]+" "+f[1])
		}
	}
	return out
}

func runEnsure(t *testing.T, r runner, up bool) error {
	in := installspec.Input{Root: "x.io", PanelHost: "p.x.io", HTTPPort: "80", DataDir: "/data"}
	return r.ensureProxy(context.Background(), "img:1", in, "", up)
}

func TestEnsureProxySwap(t *testing.T) {
	log := fakeDocker(t, "stackr-proxy", false)
	if err := runEnsure(t, runner{out: &bytes.Buffer{}}, true); err != nil {
		t.Fatal(err)
	}
	want := "rename stackr-proxy|stop stackr-proxy-old|run -d|rm -f"
	if got := strings.Join(proxyCalls(t, log), "|"); got != want {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

func TestEnsureProxyRollback(t *testing.T) {
	log := fakeDocker(t, "stackr-proxy", true)
	if err := runEnsure(t, runner{out: &bytes.Buffer{}}, true); err == nil {
		t.Fatal("want the run failure")
	}
	got := strings.Join(proxyCalls(t, log), "|")
	want := "rename stackr-proxy|stop stackr-proxy-old|run -d|rm -f|rename stackr-proxy-old|start stackr-proxy"
	if got != want {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

func TestEnsureProxyLeftover(t *testing.T) {
	log := fakeDocker(t, "stackr-proxy stackr-proxy-old", false)
	if err := runEnsure(t, runner{out: &bytes.Buffer{}}, true); err != nil {
		t.Fatal(err)
	}
	if got := proxyCalls(t, log); got[0] != "rm -f" {
		t.Fatalf("leftover not removed first: %v", got)
	}
	// No main proxy, only the aside: it is restored, then judged like any proxy.
	log = fakeDocker(t, "stackr-proxy-old", false)
	if err := runEnsure(t, runner{out: &bytes.Buffer{}}, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(proxyCalls(t, log)[:2], "|"); got != "rename stackr-proxy-old|start stackr-proxy" {
		t.Fatalf("not restored: %v", proxyCalls(t, log))
	}
}

func TestDryRunMasksToken(t *testing.T) {
	fakeDocker(t, "stackr-proxy", false)
	var out bytes.Buffer
	if err := runEnsure(t, runner{dry: true, out: &out}, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "live-secret") || !strings.Contains(out.String(), "DNS_API_TOKEN=***") {
		t.Fatalf("token not masked:\n%s", out.String())
	}
}
