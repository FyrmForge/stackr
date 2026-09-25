package installspec

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

var tlsIn = Input{
	Root:      "example.com",
	PanelHost: "stkr.example.com",
	HTTPS:     true,
	Email:     "ops@example.com",
	Proxies:   "203.0.113.0/24",
	DNS01:     true,
	HTTPPort:  "80",
	HTTPSPort: "443",
	DataDir:   "/var/lib/stackr",
	Bind:      "172.17.0.1",
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
		"example.com.":             "example.com",
		"example.com:8443":         "example.com",
		"*.apps.example.com":       "*.apps.example.com",
	} {
		if got, err := CheckRoot(in); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
}
