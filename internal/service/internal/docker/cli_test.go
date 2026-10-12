package docker

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The CLI proxy pipes a unix socket to the worker over TLS, both ways, and
// goes away with its last user.
func TestCLIProxy(t *testing.T) {
	srv := httptest.NewTLSServer(nil) // only for its self-signed pair
	cert := srv.TLS.Certificates[0]
	srv.Close()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := tls.NewListener(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c) // until the proxy half-closes
		_, _ = c.Write([]byte(strings.ToUpper(string(b))))
	}()

	d := &Client{px: newCLIProxy(ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})} //nolint:gosec // test server
	cmd, done, err := d.command(t.Context(), "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "DOCKER_HOST=unix://"+d.px.path) {
		t.Fatal("no DOCKER_HOST")
	}
	c, err := net.Dial("unix", d.px.path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("ping"))
	_ = c.(*net.UnixConn).CloseWrite()
	b, _ := io.ReadAll(c)
	if string(b) != "PING" {
		t.Fatalf("got %q", b)
	}
	done()
	if _, err := os.Stat(d.px.path); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
	if got := d.builderName("stackr-builder"); got == "stackr-builder" || strings.ContainsAny(got, ".:") {
		t.Fatalf("builder name = %q", got)
	}
}

// The SDK wraps the transport in place; a worker's client must still get its
// CLI proxy, or builds and pushes run against the controller's daemon.
func TestNewRemoteKeepsCLIProxy(t *testing.T) {
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{}}}
	d, err := NewRemote("tcp://10.207.255.2:7443", hc)
	if err != nil {
		t.Fatal(err)
	}
	if d.px == nil || d.builderName("stackr-builder") != "stackr-builder-10-207-255-2-7443" {
		t.Fatalf("worker client has no CLI proxy")
	}
}
