package main

import (
	"strings"
	"testing"

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
		DNS01:     true,
		HTTPPort:  "8080",
		DataDir:   "/d",
	}
	if err := check(&plain, nil); err != nil || plain.Email != "" || plain.HTTPSPort != "" || plain.DNS01 {
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
	s := doneText(in, "k3y", false)
	for _, want := range []string{"https://stkr.example.com", "k3y", "*.example.com", "Caddy"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(doneText(in, "", false), "Recovery passphrase") {
		t.Error("a re-run printed the passphrase section")
	}
}
