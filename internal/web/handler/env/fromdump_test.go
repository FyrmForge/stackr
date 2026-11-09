package env_test

import (
	"bytes"
	"context"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The create dialog takes a dump past the 2M body limit, as multipart, with
// the CSRF token in its header; a dump stackr does not load marks the field.
func TestCreateOperatedFromDump(t *testing.T) {
	s := webtest.New(t)
	u := s.User(t, "dba@acme.test", false)
	s.Member(t, s.Org, u, "owner")
	session := s.Session(t, u)
	post := func(name string, dump []byte) *httptest.ResponseRecorder {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		for k, v := range map[string]string{"source": "operated", "name": name, "engine": "postgres"} {
			_ = mw.WriteField(k, v)
		}
		fw, _ := mw.CreateFormFile("dump", "shop.dump")
		_, _ = fw.Write(dump)
		_ = mw.Close()
		req := httptest.NewRequest("POST", "/acme/shop/dev/-/new-tile", &b)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-CSRF-Token", "tok")
		req.AddCookie(&http.Cookie{Name: "__Host-csrf", Value: "tok"})
		req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: session})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	rec := post("bad", []byte{0x1f, 0x8b, 0x08})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "gunzip") {
		t.Errorf("gzip = %d, want 422 with the hint: %s", rec.Code, rec.Body)
	}
	rec = post("empty", nil)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "empty") {
		t.Errorf("empty dump = %d, want 422: %s", rec.Code, rec.Body)
	}
	rec = post("pg", append([]byte("PGDMP"), bytes.Repeat([]byte("x"), 3<<20)...))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("HX-Redirect"), "?drawer=") {
		t.Fatalf("from dump = %d %s", rec.Code, rec.Body)
	}
	ts, err := s.Orch.Tiles(context.Background(), s.Tile.Env)
	if err != nil {
		t.Fatal(err)
	}
	var made []string
	for _, x := range ts {
		made = append(made, x.Slug)
	}
	if got := " " + strings.Join(made, " ") + " "; !strings.Contains(got, " pg ") || !strings.Contains(got, " pg-db ") {
		t.Errorf("tiles = %s, want pg and pg-db", got)
	}
}

// A failed seed shows in the instance and the slice drawer, error and all:
// neither has a deploy tab, so this is the only place it reads.
func TestFailedSeedShowsInDrawers(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	in := service.Tile{EnvironmentID: s.Tile.Env, Name: "pg"}
	pg, j, err := s.Orch.CreateOperatedTileFromDump(ctx, in, "postgres", "", 0, strings.NewReader("PGDMP"))
	if err != nil {
		t.Fatal(err)
	}
	// the fake runs no postgres, so the seed fails
	deadline := time.Now().Add(10 * time.Second)
	for {
		if j, err = s.Orch.GetJob(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
		if j.FinishedAt != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if j.State != "failed" || j.Error == "" {
		t.Fatalf("seed job = %s %q, want failed with an error", j.State, j.Error)
	}
	for _, d := range []string{"/acme/shop/dev/-/instances/" + pg.Slug, "/acme/shop/dev/-/slices/pg-db"} {
		rec := s.Do(t, "GET", d, nil)
		body := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(body, "Last job") || !strings.Contains(body, ">seed<") ||
			!strings.Contains(body, html.EscapeString(j.Error)) {
			t.Errorf("%s = %d, lacks the failed seed %q:\n%s", d, rec.Code, j.Error, body)
		}
	}
}

// A dump over seed_max_gb (1 GiB at the least) is refused saying so, not
// with "a tile needs a name": Go drops every field of an overrun form.
func TestCreateOperatedDumpOverLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("streams a GiB through the form parser")
	}
	if raceOn { // CI runs it once more without -race (ci.yml)
		t.Skip("the race detector takes the GiB past the request deadline")
	}
	s := webtest.New(t)
	u := s.User(t, "big@acme.test", false)
	s.Member(t, s.Org, u, "owner")
	if err := s.Orch.SetSetting(context.Background(), "seed_max_gb", "1"); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		for k, v := range map[string]string{"source": "operated", "name": "big", "engine": "postgres"} {
			_ = mw.WriteField(k, v)
		}
		fw, _ := mw.CreateFormFile("dump", "big.dump")
		_, _ = fw.Write([]byte("PGDMP"))
		zeros := make([]byte, 1<<20)
		var err error
		for i := 0; i < 1100 && err == nil; i++ { // 1100 MiB, past the 1 GiB floor
			_, err = fw.Write(zeros)
		}
		_ = mw.Close()
		_ = pw.CloseWithError(err)
	}()
	req := httptest.NewRequest("POST", "/acme/shop/dev/-/new-tile", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-CSRF-Token", "tok")
	req.AddCookie(&http.Cookie{Name: "__Host-csrf", Value: "tok"})
	req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: s.Session(t, u)})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "larger than this server takes") || strings.Contains(rec.Body.String(), "needs a name") {
		t.Errorf("over-limit dump = %d, want 422 saying the dump is too large: %.400s", rec.Code, rec.Body)
	}
}
