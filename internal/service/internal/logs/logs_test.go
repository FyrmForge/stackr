package logs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFilterQuery(t *testing.T) {
	if _, err := (Filter{Tile: "t"}).Query(10); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("no org: %v", err)
	}
	q, _ := Filter{Org: "o1", Env: "e1", Source: "run", Text: "oops"}.Query(500)
	if q != `org_id:="o1" env_id:="e1" source:="run" "oops" | sort by (_time desc) | limit 500` {
		t.Errorf("query %q", q)
	}
	// Text cannot end its phrase: the quote and the backslash are escaped.
	q, _ = Filter{Org: "o", Text: `x" or org_id:* | delete \`}.Query(1)
	want := `org_id:="o" "x\" or org_id:* | delete \\" | sort by (_time desc) | limit 1`
	if q != want {
		t.Errorf("query %q, want %q", q, want)
	}
}

func TestVictoria(t *testing.T) {
	var wroteQ, wroteBody, query, start string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/insert/jsonline":
			b, _ := io.ReadAll(r.Body)
			wroteQ, wroteBody = r.URL.RawQuery, string(b)
			w.WriteHeader(http.StatusNoContent)
		case "/select/logsql/query":
			_ = r.ParseForm()
			query, start = r.PostForm.Get("query"), r.PostForm.Get("start")
			_, _ = io.WriteString(w, `{"_time":"2026-10-21T10:00:00Z","_msg":"hi","org_id":"o1","tile":"web","http.status":"200"}`+"\n")
		}
	}))
	defer srv.Close()
	v := &Victoria{URL: srv.URL}
	t0 := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	err := v.Write(context.Background(), []Line{{Time: t0, Msg: "hi", OrgID: "o1", Source: "app", HTTP: map[string]string{"http.status": "200"}}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(wroteBody), &m); err != nil {
		t.Fatal(err)
	}
	if m["msg"] != "hi" || m["org_id"] != "o1" || m["http.status"] != "200" || m["time"] != "2026-10-21T10:00:00Z" {
		t.Errorf("written %v", m)
	}
	if _, ok := m["tile_id"]; ok {
		t.Errorf("empty field written: %v", m)
	}
	if !strings.Contains(wroteQ, "_time_field=time") || !strings.Contains(wroteQ, "_stream_fields=") {
		t.Errorf("params %q", wroteQ)
	}
	got, err := v.Query(context.Background(), Filter{Org: "o1"}, t0, t0.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Msg != "hi" || got[0].Tile != "web" || got[0].HTTP["http.status"] != "200" || !got[0].Time.Equal(t0) {
		t.Errorf("got %+v", got)
	}
	if !strings.HasPrefix(query, `org_id:="o1"`) || start != "2026-10-21T10:00:00Z" {
		t.Errorf("query %q start %q", query, start)
	}
	if _, err := v.Query(context.Background(), Filter{}, t0, t0, 1); !errors.Is(err, ErrNoOrg) {
		t.Errorf("no org: %v", err)
	}
}

func TestVictoriaNewest(t *testing.T) {
	body := ""
	var q, start string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q, start = r.PostForm.Get("query"), r.PostForm.Get("start")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	v := &Victoria{URL: srv.URL}
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	body = `{"_time":"2026-10-21T10:00:00.5Z","_msg":"x"}` + "\n"
	got, err := v.Newest(context.Background(), from)
	if err != nil || !got.Equal(time.Date(2026, 10, 21, 10, 0, 0, 500_000_000, time.UTC)) {
		t.Fatalf("newest %v %v", got, err)
	}
	if q != "* | sort by (_time desc) | limit 1" || start != "2026-10-20T00:00:00Z" {
		t.Errorf("query %q start %q", q, start)
	}
	body = ""
	if got, err := v.Newest(context.Background(), from); err != nil || !got.IsZero() {
		t.Errorf("empty store: %v %v", got, err)
	}
}

func TestVictoriaDelete(t *testing.T) {
	var calls int
	var path, filter string
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/delete/active_tasks" {
			// the task runs for two polls, beside another one
			if polls++; polls <= 2 {
				_, _ = io.WriteString(w, `[{"task_id":"0"},{"task_id":"1"}]`)
			} else {
				_, _ = io.WriteString(w, `[{"task_id":"0"}]`)
			}
			return
		}
		_ = r.ParseForm()
		calls++
		path, filter = r.URL.Path, r.PostForm.Get("filter")
		_, _ = io.WriteString(w, `{"task_id":"1"}`)
	}))
	defer srv.Close()
	v := &Victoria{URL: srv.URL}
	ctx := context.Background()
	// never without both the org and the env
	if err := v.Delete(ctx, Filter{Env: "e1"}); !errors.Is(err, ErrNoOrg) {
		t.Errorf("no org: %v", err)
	}
	if err := v.Delete(ctx, Filter{Org: "o1", Tile: "t"}); !errors.Is(err, ErrNoEnv) {
		t.Errorf("no env: %v", err)
	}
	if calls != 0 {
		t.Fatalf("%d requests went out for an unscoped delete", calls)
	}
	// other fields never widen or narrow it; the values are quoted
	if err := v.Delete(ctx, Filter{Org: "o1", Env: `e"1`, Tile: "t"}); err != nil {
		t.Fatal(err)
	}
	if path != "/delete/run_task" || filter != `org_id:="o1" env_id:="e\"1"` {
		t.Errorf("path %q filter %q", path, filter)
	}
	// it answers once the task is done, not when it starts
	if polls != 3 {
		t.Errorf("returned after %d polls of active_tasks, want 3", polls)
	}
}
