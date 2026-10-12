package worker

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDocker serves a unix socket: /info, an echoing hijack on attach, and a
// plain echo of the path for anything else.
func fakeDocker(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ServerVersion":"29.0.0","DockerRootDir":"`+dir+`"}`)
	})
	mux.HandleFunc("POST /{v}/containers/c1/attach", func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		rw.Flush()
		io.Copy(conn, rw) // echo until the client closes
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "docker:"+r.URL.Path)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	stdin []string
	// failDockerUser makes `iptables -n -L DOCKER-USER` fail this many times.
	failDockerUser int
}

func (f *fakeRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := name + " " + strings.Join(args, " ")
	if name == "iptables" && f.failDockerUser > 0 {
		f.failDockerUser--
		return []byte("no chain"), errors.New("exit 1")
	}
	f.calls = append(f.calls, call)
	if stdin != nil {
		f.stdin = append(f.stdin, string(stdin))
	}
	return nil, nil
}

type fakeWG struct {
	got  []string
	runr *fakeRunner
}

func (w *fakeWG) Apply(_ context.Context, c []byte) error {
	w.got = append(w.got, string(c))
	w.runr.mu.Lock()
	w.runr.calls = append(w.runr.calls, "wg")
	w.runr.mu.Unlock()
	return nil
}

func newTestServer(t *testing.T, r *fakeRunner, wg WGApplier) (*httptest.Server, Config) {
	t.Helper()
	cfg := Config{
		DockerSock: fakeDocker(t), StateDir: t.TempDir(), Runner: r, WG: wg,
		DockerUserPoll: time.Millisecond, DockerUserWait: time.Second,
	}
	ts := httptest.NewTLSServer(New(cfg).Handler())
	t.Cleanup(ts.Close)
	return ts, cfg
}

func TestDockerProxyAndVersion(t *testing.T) {
	ts, _ := newTestServer(t, &fakeRunner{}, nil)
	resp, err := ts.Client().Get(ts.URL + "/v1.43/containers/json")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "docker:/v1.43/containers/json" || resp.Header.Get(VersionHeader) != APIVersion {
		t.Fatalf("%q %v", b, resp.Header)
	}
}

func TestHijackedAttach(t *testing.T) {
	ts, _ := newTestServer(t, &fakeRunner{}, nil)
	conn, err := tls.Dial("tcp", ts.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "POST /v1.43/containers/c1/attach?stream=1 HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("%v %v", resp, err)
	}
	io.WriteString(conn, "ping\n")
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := br.ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("%q %v", line, err)
	}
}

func TestFacts(t *testing.T) {
	ts, _ := newTestServer(t, &fakeRunner{}, nil)
	resp, err := ts.Client().Get(ts.URL + "/v1/facts")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	s := string(b)
	for _, want := range []string{`"docker":"29.0.0"`, `"interfaces"`, `"cpus"`, `"disk_free"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
}

// A Hetzner box: /proc/net/route as the kernel writes it, and its /32
// private address widened to the route's network.
func TestRoutesAndLANs(t *testing.T) {
	table := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t0101FFAC\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"enp7s0\t0000000A\t0100000A\t0003\t0\t0\t100\t0000FFFF\t0\t0\t0\n" +
		"enp7s0\t0100000A\t00000000\t0005\t0\t0\t100\tFFFFFFFF\t0\t0\t0\n"
	rs := routes(table)
	if got := rs["enp7s0"]; len(got) != 2 || got[0] != "10.0.0.0/16" || got[1] != "10.0.0.1/32" {
		t.Fatalf("enp7s0 routes = %v", got)
	}
	if len(rs["eth0"]) != 0 {
		t.Errorf("eth0 routes = %v, want the default left out", rs["eth0"])
	}
	i := Iface{Name: "enp7s0", Prefixes: []string{"10.0.0.3/32", "fe80::1/64"}, Routes: rs["enp7s0"]}
	if got := i.LANs(); len(got) != 2 || got[0].String() != "10.0.0.3/16" || got[1].String() != "fe80::1/64" {
		t.Errorf("LANs = %v, want 10.0.0.3/16 and the v6 prefix as is", got)
	}
}

func TestIsPublic(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "10.1.2.3": false, "192.168.1.1": false,
		"100.64.1.1": false, "169.254.1.1": false, "127.0.0.1": false, "2001:4860::1": true,
	} {
		if got := isPublic(netip.MustParseAddr(ip)); got != want {
			t.Errorf("%s: %v", ip, got)
		}
	}
}

func TestStateReplayOrder(t *testing.T) {
	r := &fakeRunner{failDockerUser: 2}
	wg := &fakeWG{runr: r}
	ts, cfg := newTestServer(t, r, wg)
	c := ts.Client()

	if resp, _ := c.Get(ts.URL + "/v1/state"); resp.StatusCode != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	st := `{"wg":{"iface":"wg0"},"routes":[["route","replace","10.209.0.0/16","dev","wg0"]],"chains":"*filter\nCOMMIT\n","jumps":"*filter\n-I DOCKER-USER -j STACKR\nCOMMIT\n"}`
	req, _ := http.NewRequest("PUT", ts.URL+"/v1/state", strings.NewReader(st))
	if resp, err := c.Do(req); err != nil || resp.StatusCode != 204 {
		t.Fatalf("%v %v", resp, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "applied.json")); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(cfg.StateDir); len(ents) != 1 {
		t.Fatalf("temp file left behind: %v", ents)
	}
	resp, _ := c.Get(ts.URL + "/v1/state")
	if b, _ := io.ReadAll(resp.Body); string(b) != st {
		t.Fatalf("got %s", b)
	}

	resp, err := c.Post(ts.URL+"/v1/replay", "", nil)
	if err != nil || resp.StatusCode != 204 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%v %s", err, b)
	}
	want := []string{
		"wg",
		"ip route replace 10.209.0.0/16 dev wg0",
		"iptables-restore --noflush",
		"iptables -n -L DOCKER-USER", // two earlier failures were not recorded
		"iptables-restore --noflush",
	}
	if strings.Join(r.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("order:\n%v", r.calls)
	}
	if r.stdin[0] != "*filter\nCOMMIT\n" || !strings.Contains(r.stdin[1], "DOCKER-USER") {
		t.Fatalf("stdin %q", r.stdin)
	}
	if wg.got[0] != `{"iface":"wg0"}` {
		t.Fatalf("wg got %q", wg.got)
	}
}

func TestIPTablesAndWG501(t *testing.T) {
	r := &fakeRunner{}
	ts, _ := newTestServer(t, r, nil)
	c := ts.Client()
	resp, _ := c.Post(ts.URL+"/v1/iptables", "text/plain", strings.NewReader("*nat\nCOMMIT\n"))
	if resp.StatusCode != 204 || r.calls[0] != "iptables-restore --noflush" || r.stdin[0] != "*nat\nCOMMIT\n" {
		t.Fatalf("%d %v", resp.StatusCode, r.calls)
	}
	resp, _ = c.Post(ts.URL+"/v1/wg", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != 501 {
		t.Fatalf("want 501, got %d", resp.StatusCode)
	}
}

func TestCaddyPassThrough(t *testing.T) {
	dir, _ := os.MkdirTemp("", "wk")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "c.sock")
	ln, _ := net.Listen("unix", sock)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Method+" "+r.URL.Path)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	ts := httptest.NewTLSServer(New(Config{CaddySock: sock, DockerSock: "/nonexistent"}).Handler())
	defer ts.Close()
	resp, err := ts.Client().Post(ts.URL+"/v1/caddy/load", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(resp.Body); string(b) != "POST /load" {
		t.Fatalf("%q", b)
	}
}
