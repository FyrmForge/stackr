package s3

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVersioned answers the version-aware calls: it lists the given entries
// and records every write as "METHOD path?query".
type fakeVersioned struct {
	*httptest.Server
	mu    sync.Mutex
	Calls []string
	Copy  []string // x-amz-copy-source of each copy
	Body  map[string]string
}

func newFakeVersioned(t *testing.T, list string) *fakeVersioned {
	f := &fakeVersioned{Body: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Query().Has("versions") {
			_, _ = io.WriteString(w, `<ListVersionsResult><IsTruncated>false</IsTruncated>`+list+`</ListVersionsResult>`)
			return
		}
		call := r.Method + " " + r.URL.Path
		q := r.URL.Query()
		q.Del("x-id")
		if len(q) > 0 {
			call += "?" + strings.TrimSuffix(q.Encode(), "=")
		}
		f.Calls = append(f.Calls, call)
		f.Body[call] = string(b)
		if c := r.Header.Get("X-Amz-Copy-Source"); c != "" {
			f.Copy = append(f.Copy, c)
			_, _ = io.WriteString(w, `<CopyObjectResult><ETag>"x"</ETag></CopyObjectResult>`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeVersioned) admin() Admin {
	return Admin{Endpoint: f.URL, AccessKey: "root", SecretKey: "root-secret"}
}

func ver(key, id string, latest bool, at string, size int) string {
	return fmt.Sprintf(`<Version><Key>%s</Key><VersionId>%s</VersionId><IsLatest>%t</IsLatest><LastModified>%s</LastModified><Size>%d</Size></Version>`, key, id, latest, at, size)
}

func marker(key, id string, latest bool, at string) string {
	return fmt.Sprintf(`<DeleteMarker><Key>%s</Key><VersionId>%s</VersionId><IsLatest>%t</IsLatest><LastModified>%s</LastModified></DeleteMarker>`, key, id, latest, at)
}

func TestDropBucketPurgesEveryVersion(t *testing.T) {
	f := newFakeVersioned(t,
		ver("a", "a2", true, "2026-01-02T00:00:00Z", 1)+ver("a", "a1", false, "2026-01-01T00:00:00Z", 1)+
			ver("b", "b1", false, "2026-01-01T00:00:00Z", 1)+marker("b", "b2", true, "2026-01-03T00:00:00Z"))
	if err := f.admin().DropBucket(context.Background(), "bk"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"DELETE /bk/a?versionId=a2", "DELETE /bk/a?versionId=a1",
		"DELETE /bk/b?versionId=b1", "DELETE /bk/b?versionId=b2", "DELETE /bk",
	}
	if strings.Join(f.Calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v\nwant %v", f.Calls, want)
	}
}

func TestPick(t *testing.T) {
	at := func(s string) time.Time { x, _ := time.Parse(time.RFC3339, s); return x }
	es := func() []entry {
		return []entry{
			{ID: "v1", Modified: at("2026-01-01T00:00:00Z")},
			{ID: "m2", Modified: at("2026-01-02T00:00:00Z"), Marker: true},
			{ID: "v3", Modified: at("2026-01-03T00:00:00Z"), Latest: true},
		}
	}
	for _, c := range []struct {
		when string
		id   string
		ok   bool
	}{
		{"2025-12-31T00:00:00Z", "", false}, // did not exist yet
		{"2026-01-01T12:00:00Z", "v1", true},
		{"2026-01-02T12:00:00Z", "m2", false}, // deleted then: left alone
		{"2026-02-01T00:00:00Z", "v3", false}, // already current
	} {
		e, ok := pick(es(), at(c.when))
		if ok != c.ok || (ok && e.ID != c.id) {
			t.Errorf("%s: got %q %v, want %q %v", c.when, e.ID, ok, c.id, c.ok)
		}
	}
}

func TestRestore(t *testing.T) {
	list := ver("dir/a b", "a2", true, "2026-01-03T00:00:00Z", 1) + ver("dir/a b", "a1", false, "2026-01-01T00:00:00Z", 1) +
		ver("dir/c", "c1", false, "2026-01-01T00:00:00Z", 1) + marker("dir/c", "c2", true, "2026-01-02T00:00:00Z") +
		ver("dir/new", "n1", true, "2026-01-05T00:00:00Z", 1) +
		ver("dir/ab2", "x1", true, "2026-01-01T00:00:00Z", 1) + ver("dir/ab2", "x0", false, "2025-01-01T00:00:00Z", 1)
	at, _ := time.Parse(time.RFC3339, "2026-01-02T12:00:00Z")

	f := newFakeVersioned(t, list)
	r, err := f.admin().Restore(context.Background(), "bk", "dir/", false, at, nil)
	if err != nil || r.Restored != 1 || r.Skipped != 3 || r.Failed != 0 {
		t.Fatalf("prefix: %+v %v", r, err)
	}
	// a b restored from a1 (the version then); c was deleted then; new did not exist.
	if len(f.Copy) != 1 || f.Copy[0] != "bk/dir/a%20b?versionId=a1" {
		t.Fatalf("copies %v", f.Copy)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "DELETE") {
			t.Fatalf("restore deleted: %s", c)
		}
	}

	f = newFakeVersioned(t, list)
	r, err = f.admin().Restore(context.Background(), "bk", "dir/a b", true, at, nil)
	if err != nil || r.Restored != 1 || len(f.Copy) != 1 {
		t.Fatalf("exact: %+v %v %v", r, err, f.Copy)
	}
	// "dir/a" as an exact key must not catch "dir/a b".
	f = newFakeVersioned(t, list)
	if r, _ = f.admin().Restore(context.Background(), "bk", "dir/a", true, at, nil); r.Restored+r.Skipped != 0 {
		t.Fatalf("exact matched a longer key: %+v", r)
	}
}

func TestSizes(t *testing.T) {
	f := newFakeVersioned(t, ver("a", "a2", true, "2026-01-02T00:00:00Z", 10)+ver("a", "a1", false, "2026-01-01T00:00:00Z", 4)+
		marker("b", "b2", true, "2026-01-03T00:00:00Z")+ver("b", "b1", false, "2026-01-01T00:00:00Z", 6))
	cur, old, err := f.admin().Sizes(context.Background(), "bk")
	if err != nil || cur != 10 || old != 10 {
		t.Fatalf("%d %d %v", cur, old, err)
	}
}

func TestProtectWrites(t *testing.T) {
	f := newFakeVersioned(t, "")
	err := f.admin().Protect(context.Background(), "bk", Protect{VersionDays: 14, QuotaBytes: 1 << 30, CORS: []string{"https://x.example"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"PUT /bk?versioning", "PUT /bk?lifecycle", "PUT /bk?cors", "PUT /rustfs/admin/v3/set-bucket-quota?bucket=bk"} {
		if _, ok := f.Body[c]; !ok {
			t.Errorf("no %s in %v", c, f.Calls)
		}
	}
	if b := f.Body["PUT /bk?versioning"]; !strings.Contains(b, "<Status>Enabled</Status>") {
		t.Errorf("versioning body %s", b)
	}
	lc := f.Body["PUT /bk?lifecycle"]
	for _, w := range []string{"<NoncurrentDays>14</NoncurrentDays>", "<DaysAfterInitiation>7</DaysAfterInitiation>", "<ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker>"} {
		if !strings.Contains(lc, w) {
			t.Errorf("lifecycle lacks %s: %s", w, lc)
		}
	}
	if q := f.Body["PUT /rustfs/admin/v3/set-bucket-quota?bucket=bk"]; q != `{"quota":1073741824,"quotatype":"HARD"}` {
		t.Errorf("quota body %s", q)
	}

	// Off, no cap, no CORS: versioning suspended, old versions still age out.
	f = newFakeVersioned(t, "")
	if err := f.admin().Protect(context.Background(), "bk", Protect{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Body["PUT /bk?versioning"], "<Status>Suspended</Status>") ||
		!strings.Contains(f.Body["PUT /bk?lifecycle"], "<NoncurrentDays>7</NoncurrentDays>") {
		t.Errorf("off: %v", f.Body)
	}
	if _, ok := f.Body["DELETE /bk?cors"]; !ok {
		t.Errorf("empty CORS did not clear: %v", f.Calls)
	}
}

// An app key (read or write) never holds the actions that purge history or
// switch the protection off; only the root key does.
func TestGrantPolicyNeverPurges(t *testing.T) {
	allowed := map[string]bool{
		"s3:GetBucketLocation": true, "s3:ListBucket": true, "s3:GetObject": true,
		"s3:ListBucketMultipartUploads": true, "s3:PutObject": true, "s3:DeleteObject": true,
		"s3:AbortMultipartUpload": true, "s3:ListMultipartUploadParts": true,
	}
	for _, write := range []bool{false, true} {
		var doc struct {
			Statement []struct{ Action []string }
		}
		if err := json.Unmarshal([]byte(grantPolicy("bk", write)), &doc); err != nil {
			t.Fatal(err)
		}
		for _, s := range doc.Statement {
			for _, a := range s.Action {
				if !allowed[a] {
					t.Errorf("write=%v: policy grants %s", write, a)
				}
				if write == false && (strings.Contains(a, "Put") || strings.Contains(a, "Delete")) {
					t.Errorf("read policy grants %s", a)
				}
			}
		}
	}
}
