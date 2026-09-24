package main

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
)

// stackrd proxy comes up with its admin API off loopback and takes a push
// that keeps the admin listener, from a host name like stackr-proxy.
func TestProxyAcceptsPush(t *testing.T) {
	// Both paths are fixed at init from XDG_*; point them at the test.
	dir := t.TempDir()
	caddy.ConfigAutosavePath = dir + "/autosave.json"
	caddy.DefaultStorage.Path = dir
	t.Setenv("XDG_DATA_HOME", dir)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	admin := "0.0.0.0:" + strconv.Itoa(port)
	if err := startProxy(admin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = caddy.Stop() })

	cfg := `{"admin":{"listen":"` + admin + `"},"apps":{"http":{"servers":{"s":{"listen":["127.0.0.1:0"],"automatic_https":{"disable":true},` +
		`"routes":[{"match":[{"host":["a.test"]}],"handle":[{"handler":"static_response","body":"hi"}]}]}}}}}`
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/load", strings.NewReader(cfg))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "stackr-proxy:2019"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("push: %s %s", res.Status, b)
	}
	get, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/config/", nil)
	r, err := http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(r.Body)
	_ = r.Body.Close()
	if !strings.Contains(string(b), `"a.test"`) {
		t.Fatalf("read back: %s", b)
	}

	// A restart resumes the autosaved routes.
	must(t, caddy.Stop())
	must(t, startProxy(admin))
	if got, _ := caddy.ActiveContext().App("http"); got == nil {
		t.Error("restart lost the routes")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
