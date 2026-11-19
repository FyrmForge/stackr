package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
			_, _ = io.WriteString(w, `{"job":{"state":"failed","error":"`+longErr+`"},"log":"creating stackr-bazarr-1\n"}`)
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

// ids: 3466 is shared by two, 9a1c by one.
const idList = `[
 {"id":"346602d6-0000-0000-0000-000000000001","number":1,"slug":"data"},
 {"id":"34660aaa-0000-0000-0000-000000000002","number":2,"slug":"logs"},
 {"id":"9a1c0000-0000-0000-0000-000000000003","number":3,"slug":"db"}]`

func listServer(t *testing.T) *[]string {
	t.Helper()
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/jobs") || strings.HasSuffix(r.URL.Path, "/releases") || strings.HasSuffix(r.URL.Path, "/volumes") {
			_, _ = io.WriteString(w, idList)
			return
		}
		_, _ = io.WriteString(w, `{"id":"x","state":"done"}`)
	}))
	t.Cleanup(srv.Close)
	useServer(t, srv.URL)
	return &reqs
}

func TestJobIDPrefix(t *testing.T) {
	reqs := listServer(t)
	full := "9a1c0000-0000-0000-0000-000000000003"
	if code, _, e := cli(t, "job", "get", "9a1c"); code != 0 {
		t.Fatalf("unique prefix = %d %s", code, e)
	}
	if got := (*reqs)[len(*reqs)-1]; got != "GET /api/v1/orgs/acme/jobs/"+full {
		t.Errorf("unique prefix asked %q", got)
	}
	if code, _, e := cli(t, "job", "get", full); code != 0 || strings.Contains(strings.Join(*reqs, "\n"), "/jobs/"+full+"/jobs") {
		t.Errorf("full id = %d %s", code, e)
	}
	code, _, e := cli(t, "job", "get", "3466")
	if code == 0 || !strings.Contains(e, `"3466" matches 2 jobs`) {
		t.Errorf("ambiguous prefix = %d %q", code, e)
	}
	*reqs = nil
	cli(t, "job", "get", "zzzz")
	if got := (*reqs)[len(*reqs)-1]; !strings.HasSuffix(got, "/jobs/zzzz") {
		t.Errorf("no match goes on as typed, last request %q", got)
	}
}

// release and volume go through find: an id prefix after the exact fields.
func TestFindIDPrefix(t *testing.T) {
	reqs := listServer(t)
	last := func() string { return (*reqs)[len(*reqs)-1] }
	if code, _, e := cli(t, "release", "get", "9a1c", "--stack", "s1"); code != 0 || last() != "GET /api/v1/orgs/acme/stacks/s1/releases/9a1c0000-0000-0000-0000-000000000003" {
		t.Errorf("release prefix = %d %s, last %q", code, e, last())
	}
	if code, _, e := cli(t, "release", "get", "2", "--stack", "s1"); code != 0 || !strings.HasSuffix(last(), "-000000000002") && !strings.Contains(last(), "/releases/34660aaa") {
		t.Errorf("release number = %d %s, last %q", code, e, last())
	}
	if code, _, e := cli(t, "release", "get", "3466", "--stack", "s1"); code == 0 || !strings.Contains(e, `"3466" matches 2 releases`) {
		t.Errorf("release ambiguous = %d %q", code, e)
	}
	if code, _, e := cli(t, "release", "get", "zzzz", "--stack", "s1"); code == 0 || !strings.Contains(e, `no release "zzzz"`) {
		t.Errorf("release none = %d %q", code, e)
	}
	if code, _, e := cli(t, "volume", "rm", "-y", "9a1c"); code != 0 || last() != "DELETE /api/v1/orgs/acme/volumes/9a1c0000-0000-0000-0000-000000000003" {
		t.Errorf("volume prefix = %d %s, last %q", code, e, last())
	}
	if code, _, e := cli(t, "volume", "rm", "-y", "data"); code != 0 || !strings.HasSuffix(last(), "-000000000001") {
		t.Errorf("volume slug = %d %s, last %q", code, e, last())
	}
	if code, _, e := cli(t, "volume", "rm", "-y", "3466"); code == 0 || !strings.Contains(e, `"3466" matches 2 volumes`) {
		t.Errorf("volume ambiguous = %d %q", code, e)
	}
}

func TestErrTail(t *testing.T) {
	for _, c := range []struct{ log, err, want string }{
		{"a\n", "boom", "error: boom\n"},
		{"a", "boom", "\nerror: boom\n"},
		{"", "boom", "error: boom\n"},
		{"boom happened\nerror: boom\n", "boom", ""},
		{"x boom then more\n", "boom", "error: boom\n"}, // mentioned earlier, not at the end
		{"a\n", " ", ""},
	} {
		if got := errTail(c.log, c.err); got != c.want {
			t.Errorf("errTail(%q, %q) = %q, want %q", c.log, c.err, got, c.want)
		}
	}
}

// --follow of a failed job ends its streamed log with the error too.
func TestJobFollowEndsWithError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			b, _ := json.Marshal(map[string]any{"job": map[string]any{"id": "j1", "state": "failed", "error": longErr}, "log": "creating stackr-bazarr-1\n"})
			_, _ = fmt.Fprintf(w, "event: end\ndata: %s\n\n", b)
			return
		}
		_, _ = io.WriteString(w, `{"id":"j1","state":"running"}`)
	}))
	t.Cleanup(srv.Close)
	useServer(t, srv.URL)
	_, got, _ := cli(t, "job", "log", "j1", "--follow")
	if want := "creating stackr-bazarr-1\nerror: " + longErr + "\n"; got != want {
		t.Errorf("follow =\n%q\nwant\n%q", got, want)
	}
}
