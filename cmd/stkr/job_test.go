package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const longErr = "creating stackr-bazarr-1: pq: permission denied for schema public while running the bootstrap statements"

// jobServer answers one failed job whose log stops short of its error.
func jobServer(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/log"):
			_, _ = io.WriteString(w, `{"log":"creating stackr-bazarr-1\n"}`)
		default:
			_, _ = io.WriteString(w, `{"id":"346602d6-0000-0000-0000-000000000001","state":"failed","error":"`+longErr+`"}`)
		}
	}))
	t.Cleanup(srv.Close)
	useServer(t, srv.URL)
}

// job get never trims a value, and job log ends with the failed job's error.
func TestJobErrorReadableInFull(t *testing.T) {
	jobServer(t)
	old := termWidth
	termWidth = func() int { return 40 }
	t.Cleanup(func() { termWidth = old })

	var out, errw bytes.Buffer
	if code := run(context.Background(), []string{"job", "get", "j1"}, strings.NewReader(""), &out, &errw, true); code != 0 {
		t.Fatalf("job get = %d %s", code, errw.String())
	}
	if !strings.Contains(out.String(), longErr) {
		t.Errorf("job get trimmed the error:\n%s", out.String())
	}

	code, got, e := cli(t, "job", "log", "j1")
	if code != 0 {
		t.Fatalf("job log = %d %s", code, e)
	}
	if want := "creating stackr-bazarr-1\nerror: " + longErr + "\n"; got != want {
		t.Errorf("job log =\n%q\nwant\n%q", got, want)
	}
}
