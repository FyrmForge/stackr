package env_test

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The create dialog takes a dump past the 2M body limit, as multipart, with
// the CSRF token in its header; a dump stackr does not load marks the field.
func TestCreateManagedFromDump(t *testing.T) {
	s := webtest.New(t)
	u := s.User(t, "dba@acme.test", false)
	s.Member(t, s.Org, u, "owner")
	session := s.Session(t, u)
	post := func(name string, dump []byte) *httptest.ResponseRecorder {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		for k, v := range map[string]string{"source": "managed", "name": name, "engine": "postgres"} {
			_ = mw.WriteField(k, v)
		}
		fw, _ := mw.CreateFormFile("dump", "shop.dump")
		_, _ = fw.Write(dump)
		_ = mw.Close()
		req := httptest.NewRequest("POST", "/acme/shop/dev/-/new-tile", &b)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-CSRF-Token", "tok")
		req.AddCookie(&http.Cookie{Name: "csrf", Value: "tok"})
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
