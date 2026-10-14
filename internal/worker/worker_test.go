package worker

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/wgapply"
)

// fakeDocker serves a unix socket: /info, an echoing hijack on attach, and a
// plain echo of the path for anything else.
func fakeDocker(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ServerVersion":"29.0.0","DockerRootDir":"`+dir+`"}`)
	})
	mux.HandleFunc("POST /{v}/containers/c1/attach", func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, rw) // echo until the client closes
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "docker:"+r.URL.Path)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	stdin []string
	// failDockerUser makes `iptables -n -L DOCKER-USER` fail this many times.
	failDockerUser int
	// haveJumps makes `iptables -S <hook>` show our jump first; unset, it
	// is not in place.
	haveJumps bool
}

func (f *fakeRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := name + " " + strings.Join(args, " ")
	if name == "iptables" && slices.Contains(args, "-D") {
		return []byte("no rule"), errors.New("exit 1") // nothing to delete
	}
	if name == "iptables" && slices.Contains(args, "-S") {
		f.calls = append(f.calls, call)
		if f.haveJumps {
			return []byte("-N " + args[3] + "\n-A " + args[3] + " -j STACKR\n"), nil
		}
		return nil, nil
	}
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
	_ = resp.Body.Close()
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
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "POST /v1.43/containers/c1/attach?stream=1 HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("%v %v", resp, err)
	}
	_, _ = io.WriteString(conn, "ping\n")
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
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
	for _, want := range []string{`"docker":"29.0.0"`, `"interfaces"`, `"cpus"`, `"disk_free"`, `"disk_total"`, `"wg_port":51871`, `"arch":"` + runtime.GOARCH + `"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
}

// Facts carry each peer's last handshake and the bytes sent to it.
func TestFactsHandshakeAndTx(t *testing.T) {
	rec := &wgapply.Recorder{}
	s := New(Config{StateDir: t.TempDir(), DockerSock: fakeDocker(t), Mesh: MeshConfig{WG: rec}})
	t.Cleanup(s.Stop)
	at := time.Unix(1700000000, 0)
	rec.SetHandshake("k1", at)
	rec.SetTx("k1", 42)
	rec.AddRx("k2", 7) // no handshake yet: left out of the handshake map
	f := s.Facts(context.Background())
	if f.Handshake["k1"] != at.Unix() || f.Tx["k1"] != 42 || f.Rx["k2"] != 7 {
		t.Fatalf("handshake=%v tx=%v rx=%v", f.Handshake, f.Tx, f.Rx)
	}
	if _, ok := f.Handshake["k2"]; ok {
		t.Errorf("zero handshake reported: %v", f.Handshake)
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
		"iptables -t filter -S DOCKER-USER",
		"iptables -t filter -I DOCKER-USER 1 -j STACKR",
	}
	if strings.Join(r.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("order:\n%v", r.calls)
	}
	if len(r.stdin) != 1 || r.stdin[0] != "*filter\nCOMMIT\n" {
		t.Fatalf("stdin %q", r.stdin)
	}
	if wg.got[0] != `{"iface":"wg0"}` {
		t.Fatalf("wg got %q", wg.got)
	}

	// A restart without a reboot: the jumps are still there, none is added.
	r.mu.Lock()
	r.haveJumps = true
	n := len(r.calls)
	r.mu.Unlock()
	if resp, err := c.Post(ts.URL+"/v1/replay", "", nil); err != nil || resp.StatusCode != 204 {
		t.Fatalf("%v %v", resp, err)
	}
	if last := r.calls[len(r.calls)-1]; strings.Contains(last, " -I ") || len(r.calls) == n {
		t.Fatalf("jump inserted again: %v", r.calls[n:])
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
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "c.sock")
	ln, _ := net.Listen("unix", sock)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Method+" "+r.URL.Path)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
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

// The probe fetches only probe URLs and reports who answered; nothing
// answering is an empty result, not an error.
func TestProbe(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "node-1\n")
	}))
	defer target.Close()
	call := func(u string) (int, ProbeResult) {
		rec := httptest.NewRecorder()
		(&Server{}).probe(rec, httptest.NewRequest("POST", "/v1/probe", strings.NewReader(`{"url":"`+u+`"}`)))
		var r ProbeResult
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		return rec.Code, r
	}
	if code, r := call(target.URL + ProbePrefix + "abc"); code != 200 || r.Status != 200 || r.Body != "node-1" {
		t.Errorf("probe = %d %+v", code, r)
	}
	if code, _ := call(target.URL + "/admin"); code != 400 {
		t.Errorf("a non-probe URL = %d, want 400", code)
	}
	if code, r := call("http://127.0.0.1:1" + ProbePrefix + "abc"); code != 200 || r.Status != 0 {
		t.Errorf("nothing listening = %d %+v, want an empty result", code, r)
	}
}

func TestOnlyFirst(t *testing.T) {
	const j = "-A INPUT -j STACKR-IN"
	for in, want := range map[string]bool{
		"-P INPUT ACCEPT\n" + j + "\n-A INPUT -j X\n": true,
		"-P INPUT ACCEPT\n" + j + "\n" + j + "\n":     false, // a stacked copy
		"-P INPUT ACCEPT\n-A INPUT -j X\n" + j + "\n": false, // not first
		"-P INPUT ACCEPT\n":                           false,
	} {
		if got := onlyFirst(in, j); got != want {
			t.Errorf("onlyFirst(%q) = %v", in, got)
		}
	}
}
