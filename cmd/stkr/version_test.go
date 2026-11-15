package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShownVersion(t *testing.T) {
	for _, c := range []struct{ stamped, module, want string }{
		{"v0.8.24", "(devel)", "v0.8.24"},
		{"abc1234", "v0.8.24", "abc1234"},
		{"dev", "v0.8.24", "v0.8.24"},
		{"dev", "", "dev"},
		{"dev", "(devel)", "dev"},
		{"dev", "v0.8.24+dirty", "dev"},
		{"dev", "v0.8.25-0.20261115120000-0123456789ab", "dev"},
		{"dev", "v0.8.25-0.20261115120000-0123456789ab+dirty", "dev"},
	} {
		if got := shownVersion(c.stamped, c.module); got != c.want {
			t.Errorf("shownVersion(%q, %q) = %q, want %q", c.stamped, c.module, got, c.want)
		}
	}
}

func TestVersionWarning(t *testing.T) {
	const upd = " Update: go install github.com/FyrmForge/stackr/cmd/stkr@"
	for _, c := range []struct{ cli, server, want string }{
		{"v0.8.24", "v0.8.25", "stkr is v0.8.24, the server is v0.8.25." + upd + "v0.8.25"},
		{"v0.8.24", "0.8.25", "stkr is v0.8.24, the server is 0.8.25." + upd + "v0.8.25"},
		{"v0.8.24", "v0.8.24", ""},
		{"0.8.24", "v0.8.24", ""},
		{"v0.8.24", "", ""},
		{"dev", "v0.8.25", ""},
		{"v0.8.24", "dev", "stkr is v0.8.24, the server is dev."},
		{"v0.8.24", "v0.8.25-dev.3", "stkr is v0.8.24, the server is v0.8.25-dev.3."},
	} {
		if got := versionWarning(c.cli, c.server); got != c.want {
			t.Errorf("%q vs %q = %q, want %q", c.cli, c.server, got, c.want)
		}
	}
}

// Two calls against a differing server: one line on stderr, none on stdout.
func TestVersionWarningOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Stackr-Version", "v0.8.25")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	useServer(t, srv.URL)
	a := &app{ctx: context.Background(), ver: "v0.8.24", errw: &bytes.Buffer{}, cfg: config{Server: srv.URL, Key: "k"}, client: srv.Client()}
	for range 2 {
		res, err := a.request("GET", "/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
	}
	if got := a.errw.(*bytes.Buffer).String(); strings.Count(got, "\n") != 1 || !strings.HasPrefix(got, "stkr is v0.8.24, the server is v0.8.25.") {
		t.Fatalf("stderr %q", got)
	}
	var out, errw bytes.Buffer
	runAs("v0.8.24", context.Background(), []string{"--json", "tile", "ls", "--stack", "s", "--env", "e"}, strings.NewReader(""), &out, &errw, false)
	if strings.Contains(out.String(), "stkr is") || strings.Count(errw.String(), "stkr is") != 1 {
		t.Fatalf("out %q err %q", out.String(), errw.String())
	}
}
