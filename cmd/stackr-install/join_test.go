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
// final answer. It returns the CA's real fingerprint and the count of
// /join posts. final "fresh" is a token never spent: /join/status is a 404
// until /join comes; "gone" is one used up: both are 404s.
func fakePanel(t *testing.T, final string, tamper ...func(*joinStatus)) (*httptest.Server, string, *atomic.Int32, *atomic.Int32) {
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
	var polls, joins atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if final == "gone" || final == "fresh" && r.URL.Path == "/api/v1/join/status" && joins.Load() == 0 {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/v1/join":
			joins.Add(1)
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
			for _, f := range tamper {
				f(&st)
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
	return ts, fp, &polls, &joins
}

func callTo(ts *httptest.Server, fp string) joinCall {
	return joinCall{panel: ts.URL, token: "tok", caSHA: fp, client: ts.Client(), poll: time.Millisecond, say: func(...any) {}}
}

func TestJoinConfirmed(t *testing.T) {
	ts, fp, polls, _ := fakePanel(t, "confirmed")
	key, _ := clusterca.NewKey()
	csr, _ := clusterca.NewCSR(key)
	st, err := callTo(ts, fp).run(context.Background(), csr, "pub", map[string]any{"interfaces": []any{}}, false)
	if err != nil || st.WGAddress != "10.207.255.2" || polls.Load() != 3 || len(st.Mesh) == 0 {
		t.Fatalf("%+v %v polls=%d", st, err, polls.Load())
	}
}

// H1: a second root after the pinned one in ca_pem must not reach the
// trust files, also when the answer names no trust roots.
func TestJoinDropsRootAfterPinned(t *testing.T) {
	evil, err := clusterca.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ts, fp, _, _ := fakePanel(t, "confirmed", func(st *joinStatus) {
		st.CAPEM += string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: evil.Cert.Raw}))
	})
	key, _ := clusterca.NewKey()
	csr, _ := clusterca.NewCSR(key)
	st, err := callTo(ts, fp).run(context.Background(), csr, "pub", map[string]any{}, false)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := writeJoinState(dir, st, []byte("k")); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "cluster-ca.crt")
	if err := worker.ResetTrust(ca, pinnedRoots(nil, fp), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(worker.NewTrust(ca).Roots()); n != 1 {
		t.Fatalf("trusts %d roots, want only the pinned one", n)
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
	ts, fp, _, _ := fakePanel(t, "confirmed")
	bad := callTo(ts, strings.Repeat("0", len(fp)))
	if _, err := bad.run(context.Background(), csr, "pub", map[string]any{}, false); err == nil || !strings.Contains(err.Error(), "refusing to trust") {
		t.Fatalf("wrong fingerprint was accepted: %v", err)
	}
	ts, fp, _, _ = fakePanel(t, "rejected")
	if _, err := callTo(ts, fp).run(context.Background(), csr, "pub", map[string]any{}, false); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("rejected = %v", err)
	}
}

// The same command run again after a dead session picks up where it
// stopped: the request already in is waited on, not posted again; one that
// never reached the panel is posted; a used-up one says to get a new one.
func TestJoinResumes(t *testing.T) {
	key, _ := clusterca.NewKey()
	csr, _ := clusterca.NewCSR(key)
	for _, c := range []struct {
		final string
		posts int32
		fails string
	}{
		{"confirmed", 0, ""},
		{"fresh", 1, ""},
		{"gone", 0, "Get a new one"}, // its post is turned away before it counts
	} {
		ts, fp, _, joins := fakePanel(t, c.final)
		st, err := callTo(ts, fp).run(context.Background(), csr, "pub", map[string]any{}, true)
		switch {
		case c.fails != "" && (err == nil || !strings.Contains(err.Error(), c.fails)):
			t.Errorf("%s: %v, want %q", c.final, err, c.fails)
		case c.fails == "" && (err != nil || st.Status != "confirmed"):
			t.Errorf("%s: %+v %v", c.final, st, err)
		case joins.Load() != c.posts:
			t.Errorf("%s: %d join posts, want %d", c.final, joins.Load(), c.posts)
		}
	}
}

// joinKey keeps the key a command sent, for that command only.
func TestJoinKeyResumesTheSameCommand(t *testing.T) {
	dir := t.TempDir()
	_, first, resume, err := joinKey(dir, "tok")
	if err != nil || resume {
		t.Fatalf("first run: resume %v, %v", resume, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, pendingFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("pending file: %v %v", fi, err)
	}
	_, again, resume, err := joinKey(dir, "tok")
	if err != nil || !resume || string(again) != string(first) {
		t.Fatalf("same command: resume %v, same key %v, %v", resume, string(again) == string(first), err)
	}
	_, other, resume, err := joinKey(dir, "tok2")
	if err != nil || resume || string(other) == string(first) {
		t.Fatalf("another command: resume %v, same key %v, %v", resume, string(other) == string(first), err)
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

// A box keeps its envs only when the join kept its node: a forgotten node
// rejoining by a new token gets a new id, and its leftovers are cleared.
func TestStaleMembership(t *testing.T) {
	dir := t.TempDir()
	if id := oldNodeID(dir); id != "" || staleMembership(id, "n2") {
		t.Fatalf("a box with no cert: id %q", id)
	}
	ca, _ := clusterca.NewCA(time.Now())
	key, _ := clusterca.NewKey()
	csr, _ := clusterca.NewCSR(key)
	iss, err := ca.IssueNode(csr, "n1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.crt"), iss.CertPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	id := oldNodeID(dir)
	if id != "n1" {
		t.Fatalf("old id = %q", id)
	}
	if staleMembership(id, "n1") {
		t.Error("a rejoin of the same node is cleared")
	}
	if !staleMembership(id, "n2") {
		t.Error("a forgotten node joining as a new one keeps its leftovers")
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
