package installspec

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

var tlsIn = Input{
	Root:         "example.com",
	PanelHost:    "stkr.example.com",
	HTTPS:        true,
	Email:        "ops@example.com",
	Proxies:      "203.0.113.0/24",
	DNS01:        true,
	HTTPPort:     "80",
	HTTPSPort:    "443",
	DataDir:      "/var/lib/stackr",
	Bind:         "172.17.0.1",
	BridgeSubnet: "172.17.0.0/16",
}

var plainIn = Input{
	Root:      "*.example.com", // the panel env carries it bare
	PanelHost: "stkr.example.com",
	HTTPPort:  "8081",
	DataDir:   "/srv/stackr",
	Bind:      "172.18.0.1",
}

// The docker run lines are the contract with the box; a changed flag shows
// up as a golden diff.
func TestGolden(t *testing.T) {
	for name, c := range map[string]Container{
		"panel_tls":   Panel(Image("v1.2.3"), tlsIn),
		"panel_plain": Panel(Image("1.2.3"), plainIn),
		"proxy_tls":   Proxy(Image("1.2.3"), tlsIn, "cf-token"),
		"proxy_plain": Proxy(Image("1.2.3"), plainIn, ""),
	} {
		t.Run(name, func(t *testing.T) {
			got := "docker " + strings.Join(c.RunArgs(), "\n  ") + "\n"
			file := filepath.Join("testdata", name+".txt")
			if *update {
				if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if string(want) != got {
				t.Errorf("%s differs:\n%s", file, got)
			}
		})
	}
}

func TestBaseURL(t *testing.T) {
	for in, want := range map[Input]string{
		{PanelHost: "p.x.com", HTTPS: true, HTTPSPort: "443"}:  "https://p.x.com",
		{PanelHost: "p.x.com", HTTPS: true, HTTPSPort: "8443"}: "https://p.x.com:8443",
		{PanelHost: "p.x.com", HTTPPort: "80"}:                 "http://p.x.com",
		{PanelHost: "p.x.com", HTTPPort: "8080"}:               "http://p.x.com:8080",
	} {
		if got := in.BaseURL(); got != want {
			t.Errorf("%+v: %s, want %s", in, got, want)
		}
	}
}

func TestSaveLoad(t *testing.T) {
	in := tlsIn
	in.DataDir = t.TempDir()
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	got, err := Load(in.DataDir)
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestCheckRoot(t *testing.T) {
	for _, bad := range []string{
		"",
		"localhost",
		"127.0.0.1",
		"::1",
		"example",
		"ex ample.com",
		"-bad.com",
		"a..b.com",
		"example.123",
		strings.Repeat("a", 64) + ".com",
	} {
		if got, err := CheckRoot(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
	for in, want := range map[string]string{
		"https://Example.COM/path": "example.com",
		"HTTPS://Example.COM/":     "example.com",
		"example.com.":             "example.com",
		"example.com:8443":         "example.com",
		"*.apps.example.com":       "*.apps.example.com",
	} {
		if got, err := CheckRoot(in); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
}

// The panel's env carries the install id, so archives are named by it; an
// install without one sets nothing and the panel keeps "default".
func TestPanelInstallID(t *testing.T) {
	has := func(in Input) bool {
		return slices.Contains(Panel(Image("1.2.3"), in).Env, "STACKR_INSTALL_ID="+in.InstallID)
	}
	withID := plainIn
	withID.InstallID = "9f2c41d7a0b34e5a"
	if !has(withID) {
		t.Errorf("panel env lacks the install id: %v", Panel(Image("1.2.3"), withID).Env)
	}
	for _, e := range Panel(Image("1.2.3"), plainIn).Env {
		if strings.HasPrefix(e, "STACKR_INSTALL_ID") {
			t.Errorf("an install without an id sets %q", e)
		}
	}
}

// The stamp changes with anything the proxy is made from and with nothing
// else, so a re-run or upgrade recreates exactly the stale one.
func TestProxyHashStamp(t *testing.T) {
	in := Input{Root: "x.io", PanelHost: "x.io", HTTPPort: "80", DataDir: "/data"}
	base := Proxy("img:1", in, "")
	if base.Labels[LabelSpec] == "" || base.Labels[LabelSpec] != Proxy("img:1", in, "").Labels[LabelSpec] {
		t.Fatal("stamp missing or unstable")
	}
	other := in
	other.DataDir = "/other"
	for name, c := range map[string]Container{
		"image": Proxy("img:2", in, ""),
		"token": Proxy("img:1", in, "tok"),
		"mount": Proxy("img:1", other, ""),
	} {
		if c.Labels[LabelSpec] == base.Labels[LabelSpec] {
			t.Errorf("%s change kept the stamp", name)
		}
	}
	if got := TokenFrom(Proxy("img:1", in, "tok").Env); got != "tok" {
		t.Errorf("TokenFrom = %q", got)
	}
}
