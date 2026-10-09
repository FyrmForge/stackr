package upgrade

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.1.1", "v0.1.0", true},
		{"v0.2.0", "v0.1.9", true},
		{"v0.10.0", "v0.9.0", true},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.2.0", false},
		{"dev", "v0.1.0", false},
		{"v0.1.0", "dev", false},
		{"0.2.0", "v0.1.0", false},
		{"", "v0.1.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
	if got := ImageRef("v0.1.1"); got != "ghcr.io/fyrmforge/stackr:0.1.1" {
		t.Errorf("ImageRef = %q", got)
	}
}

func TestCheckAndUpgrade(t *testing.T) {
	ctx := context.Background()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v0.2.0"}`)
	}))
	defer gh.Close()
	fake := dockerfake.New()
	fake.Err = map[string]error{"LocalDigest": errors.New("no such image")}
	var order []string
	f := &Flow{
		Panel:   panel.New(fake),
		Version: "v0.1.0",
		URL:     gh.URL,
		Archive: func(context.Context, io.Writer) (string, error) {
			order = append(order, "archive")
			return "/data/backups/pre-upgrade-v0.1.0.tar.gz", nil
		},
		Spec: func(image string) docker.ContainerSpec { return docker.ContainerSpec{Name: "stackr", Image: image} },
	}

	tag, err := f.Check(ctx)
	if err != nil || tag != "v0.2.0" {
		t.Fatalf("check = %q %v", tag, err)
	}
	if _, ok := f.Available(tag); !ok {
		t.Error("v0.2.0 not offered over v0.1.0")
	}
	if _, err := f.Upgrade(ctx, "v0.1.0", io.Discard); err == nil {
		t.Error("upgrade to the same version ran")
	}

	archive, err := f.Upgrade(ctx, tag, io.Discard)
	if err != nil || !strings.HasSuffix(archive, ".tar.gz") {
		t.Fatalf("upgrade = %q %v", archive, err)
	}
	var seq []string
	for _, c := range fake.Calls() {
		if c.Method == "Pull" || c.Method == "Run" {
			seq = append(seq, c.Method+" "+c.Args[0])
		}
	}
	if strings.Join(seq, ", ") != "Pull ghcr.io/fyrmforge/stackr:0.2.0, Run stackr-upgrader" || len(order) != 1 {
		t.Errorf("sequence = %v, archive calls %d", seq, len(order))
	}
	var sp docker.ContainerSpec
	t.Setenv(panel.SpecEnv, strings.TrimPrefix(fake.Specs[0].Env[0], panel.SpecEnv+"="))
	if sp, err = panel.SpecFromEnv(); err != nil || sp.Name != "stackr-0.2.0" ||
		sp.Env[0] != "STACKR_IMAGE=ghcr.io/fyrmforge/stackr:0.2.0" {
		t.Errorf("new panel spec = %+v %v", sp, err)
	}

	// A failed archive never reaches the helper.
	fake2 := dockerfake.New()
	f.Panel = panel.New(fake2)
	f.Archive = func(context.Context, io.Writer) (string, error) { return "", errors.New("disk full") }
	if _, err := f.Upgrade(ctx, tag, io.Discard); err == nil || len(fake2.Specs) != 0 {
		t.Errorf("archive failure: %v, runs %d", err, len(fake2.Specs))
	}

	// Dev builds refuse every path.
	f.Version = "dev"
	if tag, _ := f.Check(ctx); tag != "" {
		t.Error("dev build checked")
	}
	if _, err := f.Upgrade(ctx, "v9.0.0", io.Discard); err == nil {
		t.Error("dev build upgraded")
	}
}

func proxyIn() installspec.Input {
	return installspec.Input{Root: "x.io", PanelHost: "p.x.io", HTTPPort: "80", DataDir: "/data"}
}

func proxyFlow(fake *dockerfake.Fake) *Flow {
	return &Flow{Docker: fake, ProxySpec: func(image, tok string) docker.ContainerSpec {
		c := installspec.Proxy(image, proxyIn(), tok)
		return docker.ContainerSpec{Name: c.Name, Image: c.Image, Env: c.Env, Labels: c.Labels}
	}}
}

// A proxy made before the admin socket (no stamp) is replaced with its DNS
// token kept; one already current is left alone.
func TestEnsureProxy(t *testing.T) {
	ctx := context.Background()
	const img = "img:0.2.0"
	cur := installspec.Proxy(img, proxyIn(), "tok").Labels
	for _, tc := range []struct {
		name     string
		labels   map[string]string
		recreate bool
	}{
		{"unstamped", map[string]string{installspec.LabelRole: "proxy"}, true},
		{"other image", installspec.Proxy("img:0.1.0", proxyIn(), "tok").Labels, true},
		{"current", cur, false},
	} {
		fake := dockerfake.New()
		fake.Containers = []docker.Container{{ID: "old", Labels: tc.labels}}
		fake.Details = map[string]docker.Detail{"old": {Env: []string{"A=1", "DNS_API_TOKEN=tok"}}}
		if err := proxyFlow(fake).EnsureProxy(ctx, img); err != nil {
			t.Fatal(tc.name, err)
		}
		if got := len(fake.Specs) == 1; got != tc.recreate {
			t.Errorf("%s: recreated = %v", tc.name, got)
		}
		if tc.recreate && !slices.Contains(fake.Specs[0].Env, "DNS_API_TOKEN=tok") {
			t.Errorf("%s: token lost: %v", tc.name, fake.Specs[0].Env)
		}
	}
	// No proxy container: nothing to do.
	fake := dockerfake.New()
	if err := proxyFlow(fake).EnsureProxy(ctx, img); err != nil || len(fake.Specs) != 0 {
		t.Errorf("no proxy: %v %v", err, fake.Specs)
	}
}

func calls(f *dockerfake.Fake) []string {
	var out []string
	for _, c := range f.Calls() {
		out = append(out, c.String())
	}
	return out
}

func oldProxy() *dockerfake.Fake {
	fake := dockerfake.New()
	fake.Containers = []docker.Container{{ID: "old", Name: "stackr-proxy", Labels: map[string]string{installspec.LabelRole: "proxy"}}}
	fake.Details = map[string]docker.Detail{"old": {Env: []string{"DNS_API_TOKEN=tok"}}}
	return fake
}

// The old proxy is set aside and stopped before the run, removed after it.
func TestEnsureProxySwapOrder(t *testing.T) {
	fake := oldProxy()
	if err := proxyFlow(fake).EnsureProxy(context.Background(), "img:0.2.0"); err != nil {
		t.Fatal(err)
	}
	want := []string{"List()", "Inspect(old)", "Rename(old, stackr-proxy-old)", "Stop(old)",
		"Run(stackr-proxy, img:0.2.0)", "StopRemove(old)"}
	if got := calls(fake); !slices.Equal(got, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", got, want)
	}
}

// A new proxy that will not run leaves the old one back under its name, running.
func TestEnsureProxyRollback(t *testing.T) {
	fake := oldProxy()
	fake.Err = map[string]error{"Run": errors.New("port in use")}
	err := proxyFlow(fake).EnsureProxy(context.Background(), "img:0.2.0")
	if err == nil || !strings.Contains(err.Error(), "port in use") {
		t.Fatalf("err = %v", err)
	}
	got := calls(fake)
	tail := got[len(got)-3:]
	want := []string{"StopRemove(stackr-proxy)", "Rename(old, stackr-proxy)", "Start(old)"}
	if !slices.Equal(tail, want) {
		t.Fatalf("rollback calls:\n%v\nwant:\n%v", tail, want)
	}
}

// A -old left by a crashed attempt: removed when the main proxy runs,
// restored when there is none.
func TestEnsureProxyLeftover(t *testing.T) {
	aside := docker.Container{ID: "aside", Name: "stackr-proxy-old", Labels: map[string]string{installspec.LabelRole: "proxy"}}
	fake := oldProxy()
	fake.Containers = append(fake.Containers, aside)
	if err := proxyFlow(fake).EnsureProxy(context.Background(), "img:0.2.0"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(calls(fake), "StopRemove(aside)") {
		t.Errorf("leftover not removed: %v", calls(fake))
	}
	fake = oldProxy()
	fake.Containers = []docker.Container{aside}
	fake.Details = map[string]docker.Detail{"aside": {Env: []string{"DNS_API_TOKEN=tok"}}}
	// current spec: only the restore happens
	cur := installspec.Proxy("img:0.2.0", proxyIn(), "tok").Labels
	fake.Containers[0].Labels = cur
	if err := proxyFlow(fake).EnsureProxy(context.Background(), "img:0.2.0"); err != nil {
		t.Fatal(err)
	}
	got := calls(fake)
	if !slices.Contains(got, "Rename(aside, stackr-proxy)") || !slices.Contains(got, "Start(aside)") || len(fake.Specs) != 0 {
		t.Errorf("not restored: %v", got)
	}
}
