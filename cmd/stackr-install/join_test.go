package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/worker"
)

// fakePanel answers /join pending, then /join/status pending once, then the
// final answer. It returns the CA's real fingerprint.
func fakePanel(t *testing.T, final string) (*httptest.Server, string, *atomic.Int32) {
	t.Helper()
	ca, err := clusterca.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := clusterca.NewKey()
	csr, _ := clusterca.NewCSR(key)
	iss, err := ca.IssueNode(csr, "n1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})
	var polls atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch r.URL.Path {
		case "/api/v1/join":
			if in["token"] != "tok" || in["wg_public_key"] != "pub" || in["facts"] == nil || !strings.Contains(in["csr"].(string), "CERTIFICATE REQUEST") {
				http.Error(w, "bad request body", 400)
				return
			}
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"status":"pending","node_id":"n1"}`))
		case "/api/v1/join/status":
			if polls.Add(1) < 2 {
				_, _ = w.Write([]byte(`{"status":"pending","node_id":"n1"}`))
				return
			}
			if final == "rejected" {
				_, _ = w.Write([]byte(`{"status":"rejected"}`))
				return
			}
			st := joinStatus{Status: "confirmed", NodeID: "n1", WGAddress: "10.207.255.2",
				CertPEM: string(iss.CertPEM), CAPEM: string(caPEM)}
			if polls.Load() >= 3 { // confirmed first, the mesh spec a sync later
				st.Mesh = json.RawMessage(`{"listen_port":51820}`)
			}
			b, _ := json.Marshal(st)
			_, _ = w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	fp, err := fingerprint(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	return ts, fp, &polls
}

func callTo(ts *httptest.Server, fp string) joinCall {
	return joinCall{panel: ts.URL, token: "tok", caSHA: fp, client: ts.Client(), poll: time.Millisecond, say: func(...any) {}}
}

func TestJoinConfirmed(t *testing.T) {
	ts, fp, polls := fakePanel(t, "confirmed")
	key, _ := clusterca.NewKey()
	csr, _ := clusterca.NewCSR(key)
	st, err := callTo(ts, fp).run(context.Background(), csr, "pub", map[string]any{"interfaces": []any{}})
	if err != nil || st.WGAddress != "10.207.255.2" || polls.Load() != 3 || len(st.Mesh) == 0 {
		t.Fatalf("%+v %v polls=%d", st, err, polls.Load())
	}
}

func TestPinnedRootsDropsUnpinned(t *testing.T) {
	pemOf := func() string {
		ca, err := clusterca.NewCA(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw}))
	}
	good, evil := pemOf(), pemOf()
	pin, _ := fingerprint([]byte(good))
	got := pinnedRoots([]string{good, evil, "junk"}, pin)
	if len(got) != 1 || got[0] != good {
		t.Fatalf("kept %d roots, want only the pinned one", len(got))
	}
	if len(pinnedRoots([]string{evil}, pin)) != 0 {
		t.Fatal("an unpinned root survived")
	}
}

func TestJoinRefusals(t *testing.T) {
	key, _ := clusterca.NewKey()
	csr, _ := clusterca.NewCSR(key)
	ts, fp, _ := fakePanel(t, "confirmed")
	bad := callTo(ts, strings.Repeat("0", len(fp)))
	if _, err := bad.run(context.Background(), csr, "pub", map[string]any{}); err == nil || !strings.Contains(err.Error(), "refusing to trust") {
		t.Fatalf("wrong fingerprint was accepted: %v", err)
	}
	ts, fp, _ = fakePanel(t, "rejected")
	if _, err := callTo(ts, fp).run(context.Background(), csr, "pub", map[string]any{}); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("rejected = %v", err)
	}
}

func TestJoinArgsAndUnit(t *testing.T) {
	for _, args := range [][]string{
		{"--panel", "http://x.example.com", "--token", "t", "--ca-sha256", strings.Repeat("a", 64)},
		{"--panel", "https://x.example.com", "--ca-sha256", strings.Repeat("a", 64)},
		{"--panel", "https://x.example.com", "--token", "t", "--ca-sha256", "abc"},
	} {
		if err := join(context.Background(), append(args, "--dry-run"), &strings.Builder{}); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	if fp, err := normalizeFP(strings.ToUpper(strings.Repeat("ab:", 31) + "ab")); err != nil || fp != strings.Repeat("ab", 32) {
		t.Errorf("normalizeFP = %q %v", fp, err)
	}
	u := workerUnit("/var/lib/stackr-worker", "10.207.255.3", 51900, "https://stkr.example.com")
	for _, want := range []string{"After=network-online.target docker.service",
		"worker --state-dir /var/lib/stackr-worker --addr 10.207.255.3:7443 --wg-port 51900 --panel-url https://stkr.example.com"} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
}

// A box that joined before gets new WireGuard keys on a rejoin.
func TestDropWGKeys(t *testing.T) {
	dir := t.TempDir()
	before, err := worker.EnsureWGKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"exit.key", "wg.key.prev"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := dropWGKeys(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"wg.key", "exit.key", "wg.key.prev"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", f, err)
		}
	}
	after, err := worker.EnsureWGKey(dir)
	if err != nil || after == before {
		t.Fatalf("key after rejoin = same %v, err %v", after == before, err)
	}
	if err := dropWGKeys(dir + "/missing"); err != nil {
		t.Fatalf("missing keys: %v", err)
	}
}

// The join answer's node name is kept next to the certs: uninstall --purge
// asks for it, and the name the panel printed is all a person has.
func TestWriteJoinState(t *testing.T) {
	dir := t.TempDir()
	st := joinStatus{NodeID: "n1", NodeName: "h3", CertPEM: "cert", CAPEM: "ca", Mesh: json.RawMessage(`{"a":1}`)}
	if err := writeJoinState(dir, st, []byte("key")); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"node.name": "h3\n", "node.crt": "cert", "node.key": "key", "cluster-ca.crt": "ca"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	// an older panel names no node: no file, not an empty one
	dir = t.TempDir()
	if err := writeJoinState(dir, joinStatus{CertPEM: "c", CAPEM: "a", Mesh: json.RawMessage(`{}`)}, []byte("k")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node.name")); err == nil {
		t.Error("node.name written without a name")
	}
}

// A join takes the installer out of the panel image next to stackrd; both
// are staged and renamed in.
func TestFetchWorkerExtractsBothBinaries(t *testing.T) {
	var out strings.Builder
	r := runner{dry: true, out: &out}
	if err := r.fetchWorker(context.Background(), "ghcr.io/fyrmforge/stackr:0.6.0"); err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{":/stackrd " + joinBinary + ".new", ":/stackr-install " + joinInstaller + ".new"} {
		if !strings.Contains(flat, want) {
			t.Errorf("no docker cp for %q in:\n%s", want, out.String())
		}
	}
}
