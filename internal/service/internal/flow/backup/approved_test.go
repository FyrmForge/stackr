package backup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// An org's destination on a private address (here loopback) is refused with
// the waiting message until a server admin approved that host:port; another
// port stays refused.
func TestPrivateOrgDestWaitsForApproval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	org := "o1"
	d := store.BackupDest{OrgID: &org, Kind: "s3", Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"}
	f := &Flow{}

	err := f.Probe(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "waiting for the server admin to approve LAN access") {
		t.Fatalf("pending dest = %v", err)
	}
	d.ApprovedEndpoint = "127.0.0.1:1"
	if err := f.Probe(context.Background(), d); err == nil {
		t.Fatal("another port's approval lifted it")
	}
	d.ApprovedEndpoint = u.Host
	if err := f.Probe(context.Background(), d); err != nil {
		t.Fatalf("approved dest = %v", err)
	}
}
