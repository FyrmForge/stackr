package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
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
