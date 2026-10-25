package backup

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"

	bk "github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// s3Seen is what a PUT to the fake bucket found at the moment it arrived.
type s3Seen struct {
	key     string
	body    []byte
	scratch int      // files in the scratch dir
	order   []string // docker calls so far
}

// fakeBucket stores single-PUT objects (a small archive is one) and records
// the world as each upload starts. fail answers every PUT with a 500.
func fakeBucket(t *testing.T, w *world, fail bool) (store.BackupDest, func() []s3Seen) {
	var mu sync.Mutex
	var seen []s3Seen
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPut {
			rw.WriteHeader(http.StatusNoContent)
			return
		}
		files, _ := os.ReadDir(w.f.Scratch)
		mu.Lock()
		defer mu.Unlock()
		if fail {
			http.Error(rw, `<Error><Code>AccessDenied</Code><Message>no</Message></Error>`, http.StatusForbidden)
			return
		}
		seen = append(seen, s3Seen{r.URL.Path, body, len(files), order(w.fake)})
		rw.Header().Set("ETag", `"x"`)
	}))
	t.Cleanup(srv.Close)
	d := w.dest
	d.Kind, d.Endpoint, d.Bucket, d.AccessKey, d.SecretKey = bk.S3, srv.URL, "b", "a", "s"
	return d, func() []s3Seen {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

// A live copy streams into the upload: nothing in scratch while it goes up,
// and the recorded size is the object's.
func TestLiveBackupStreamsWithoutScratch(t *testing.T) {
	w := setup(t)
	dest, seen := fakeBucket(t, w, false)
	sp := w.spec(0)
	sp.Dest, sp.Mode = dest, bk.Live
	r, err := w.f.Backup(ctx, Subject{Volume: w.vol, Holders: nil}, sp, io.Discard)
	must(t, err)
	got := seen()
	if len(got) != 1 {
		t.Fatalf("%d uploads", len(got))
	}
	if got[0].scratch != 0 {
		t.Errorf("%d scratch files during a live upload", got[0].scratch)
	}
	if r.SizeBytes != int64(len(got[0].body)) || r.SizeBytes == 0 {
		t.Errorf("recorded %d bytes, object has %d", r.SizeBytes, len(got[0].body))
	}
	if slices.Contains(got[0].order, "Pause") {
		t.Errorf("a live copy paused the app: %v", got[0].order)
	}
}

// A pause copy still spools, and the app is running again before the upload.
func TestPauseBackupSpoolsAndResumesBeforeUpload(t *testing.T) {
	w := setup(t)
	dest, seen := fakeBucket(t, w, false)
	sp := w.spec(0)
	sp.Dest, sp.Mode = dest, bk.Pause
	r, err := w.f.Backup(ctx, Subject{Volume: w.vol, Holders: []store.Tile{w.api}}, sp, io.Discard)
	must(t, err)
	got := seen()
	if len(got) != 1 {
		t.Fatalf("%d uploads", len(got))
	}
	if got[0].scratch != 1 {
		t.Errorf("%d scratch files during the upload, want the spool", got[0].scratch)
	}
	if !slices.Equal(got[0].order, []string{"Pause", "TarVolume", "Unpause"}) {
		t.Errorf("before the upload: %v", got[0].order)
	}
	if r.SizeBytes != int64(len(got[0].body)) {
		t.Errorf("recorded %d bytes, object has %d", r.SizeBytes, len(got[0].body))
	}
}

// A refused upload fails the run and stores nothing, with no scratch left.
func TestLiveBackupUploadErrorLeavesNothing(t *testing.T) {
	w := setup(t)
	dest, seen := fakeBucket(t, w, true)
	sp := w.spec(0)
	sp.Dest, sp.Mode = dest, bk.Live
	if _, err := w.f.Backup(ctx, Subject{Volume: w.vol}, sp, io.Discard); err == nil {
		t.Fatal("a refused upload succeeded")
	}
	if n := len(seen()); n != 0 {
		t.Errorf("%d objects stored", n)
	}
	if files, _ := os.ReadDir(w.f.Scratch); len(files) != 0 {
		t.Errorf("scratch left: %d files", len(files))
	}
}
