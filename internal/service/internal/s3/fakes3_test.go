package s3

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// fakeS3 is the slice of S3 an upload uses: single PUT, multipart create,
// part, complete and abort. Objects holds finished objects; Aborted counts
// aborted uploads; Open is the multipart uploads neither completed nor
// aborted (stray parts).
type fakeS3 struct {
	*httptest.Server
	mu       sync.Mutex
	Objects  map[string][]byte
	Aborted  int
	Open     map[string]map[int][]byte
	FailPart int // part number that answers 500, 0 = none
}

func newFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{Objects: map[string][]byte{}, Open: map[string]map[int][]byte{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeS3) bucket() Bucket {
	return Bucket{Endpoint: f.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"}
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.URL.Path // /b/<key>
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := "u" + strconv.Itoa(len(f.Open)+f.Aborted+1)
		f.Open[id] = map[int][]byte{}
		_, _ = w.Write([]byte(`<InitiateMultipartUploadResult><UploadId>` + id + `</UploadId></InitiateMultipartUploadResult>`))
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		if n == f.FailPart {
			http.Error(w, `<Error><Code>InternalError</Code><Message>boom</Message></Error>`, http.StatusInternalServerError)
			return
		}
		f.Open[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", `"p`+strconv.Itoa(n)+`"`)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		var all []byte
		parts := f.Open[q.Get("uploadId")]
		for n := 1; n <= len(parts); n++ {
			all = append(all, parts[n]...)
		}
		f.Objects[key] = all
		delete(f.Open, q.Get("uploadId"))
		_, _ = w.Write([]byte(`<CompleteMultipartUploadResult><Key>k</Key></CompleteMultipartUploadResult>`))
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(f.Open, q.Get("uploadId"))
		f.Aborted++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		f.Objects[key] = bytes.Clone(body)
		w.Header().Set("ETag", `"x"`)
	default:
		http.Error(w, "unsupported", http.StatusNotImplemented)
	}
}
