package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type adminReq struct {
	Method, Path, Query, Body, Hash, Auth string
}

// fakeAdmin records admin requests and answers from reply (nil = 200).
type fakeAdmin struct {
	*httptest.Server
	mu    sync.Mutex
	Reqs  []adminReq
	reply func(n int, r adminReq) (int, string)
}

func newFakeAdmin(t *testing.T, reply func(n int, r adminReq) (int, string)) *fakeAdmin {
	f := &fakeAdmin{reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		q := adminReq{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Get("X-Amz-Content-Sha256"), r.Header.Get("Authorization")}
		f.mu.Lock()
		f.Reqs = append(f.Reqs, q)
		n := len(f.Reqs)
		f.mu.Unlock()
		code, body := 200, ""
		if f.reply != nil {
			code, body = f.reply(n, q)
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAdmin) admin() Admin {
	return Admin{Endpoint: f.URL, AccessKey: "root", SecretKey: "root-secret"}
}

func checkReq(t *testing.T, got adminReq, method, path, query, body string) {
	t.Helper()
	if got.Method != method || got.Path != path || got.Query != query || got.Body != body {
		t.Fatalf("got %s %s ?%s %s, want %s %s ?%s %s", got.Method, got.Path, got.Query, got.Body, method, path, query, body)
	}
	sum := sha256.Sum256([]byte(body))
	if got.Hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("%s: content hash %q", path, got.Hash)
	}
	if !strings.Contains(got.Auth, "/us-east-1/s3/aws4_request") {
		t.Fatalf("%s: auth scope: %q", path, got.Auth)
	}
	_, signed, _ := strings.Cut(got.Auth, "SignedHeaders=")
	signed, _, _ = strings.Cut(signed, ",")
	for _, h := range []string{"host", "x-amz-content-sha256", "x-amz-date"} {
		if !strings.Contains(";"+signed+";", ";"+h+";") {
			t.Fatalf("%s: %s not signed: %q", path, h, signed)
		}
	}
}

func TestAdminRequests(t *testing.T) {
	f := newFakeAdmin(t, nil)
	a := f.admin()
	ctx := context.Background()
	if err := a.AddUser(ctx, "u1", "sec"); err != nil {
		t.Fatal(err)
	}
	if err := a.GrantUser(ctx, "u1", "bk", false); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveUser(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if len(f.Reqs) != 5 {
		t.Fatalf("%d requests", len(f.Reqs))
	}
	const p = "/rustfs/admin/v3/"
	checkReq(t, f.Reqs[0], "PUT", p+"add-user", "accessKey=u1", `{"secretKey":"sec","status":"enabled"}`)
	policy := `{"Version":"2012-10-17","Statement":[` +
		`{"Effect":"Allow","Action":["s3:GetBucketLocation","s3:ListBucket"],"Resource":["arn:aws:s3:::bk"]},` +
		`{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::bk/*"]}]}`
	checkReq(t, f.Reqs[1], "PUT", p+"add-canned-policy", "name=u1", policy)
	checkReq(t, f.Reqs[2], "POST", p+"idp/builtin/policy/attach", "", `{"policies":["u1"],"user":"u1"}`)
	checkReq(t, f.Reqs[3], "DELETE", p+"remove-user", "accessKey=u1", "")
	checkReq(t, f.Reqs[4], "DELETE", p+"remove-canned-policy", "name=u1", "")
}

func TestAdminStatus(t *testing.T) {
	ctx := context.Background()
	f := newFakeAdmin(t, func(int, adminReq) (int, string) { return 204, "" })
	if err := f.admin().AddUser(ctx, "u", "s"); err != nil {
		t.Fatalf("204: %v", err)
	}

	f = newFakeAdmin(t, func(n int, _ adminReq) (int, string) {
		if n == 1 {
			return 503, "busy"
		}
		return 200, ""
	})
	if err := f.admin().AddUser(ctx, "u", "s"); err != nil || len(f.Reqs) != 2 {
		t.Fatalf("503 then 200: %v after %d", err, len(f.Reqs))
	}

	f = newFakeAdmin(t, func(int, adminReq) (int, string) { return 400, "bad key" })
	err := f.admin().AddUser(ctx, "u", "s")
	if err == nil || len(f.Reqs) != 1 || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("400: %v after %d", err, len(f.Reqs))
	}
}

func TestAdminRemoveGone(t *testing.T) {
	f := newFakeAdmin(t, func(_ int, r adminReq) (int, string) {
		if strings.HasSuffix(r.Path, "remove-user") {
			return 404, ""
		}
		return 500, "policy not found"
	})
	if err := f.admin().RemoveUser(context.Background(), "u"); err != nil {
		t.Fatal(err)
	}
	f = newFakeAdmin(t, func(int, adminReq) (int, string) { return 500, "disk on fire" })
	if err := f.admin().RemoveUser(context.Background(), "u"); err == nil {
		t.Fatal("other 500 swallowed")
	}
}

func TestAdminTrailingSlash(t *testing.T) {
	f := newFakeAdmin(t, nil)
	a := f.admin()
	a.Endpoint += "/"
	if err := a.AddUser(context.Background(), "u", "s"); err != nil {
		t.Fatal(err)
	}
	if got := f.Reqs[0].Path; got != "/rustfs/admin/v3/add-user" {
		t.Fatalf("path %q", got)
	}
}
