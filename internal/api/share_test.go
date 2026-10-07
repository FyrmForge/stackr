package api_test

import (
	"context"
	"encoding/json"
	"testing"
)

// Shares: an owner adds, lists and removes the org's own; a member only
// reads; another org's share is a 404; a mounted share cannot go.
func TestShares(t *testing.T) {
	w := newWorld(t)
	u := w.env.User(t, "member@x", false)
	w.env.Member(t, w.acme, u, "member")
	member := w.env.APIKey(t, u, w.acme)

	const body = `{"slug":"media","kind":"nfs","source":"nas:/export","options":"nfsvers=4"}`
	if code, _ := w.do(t, w.stranger, "POST", "/orgs/acme/shares", body); code != 404 && code != 403 {
		t.Errorf("stranger create = %d, want 403 or 404", code)
	}
	code, out := w.do(t, w.owner, "POST", "/orgs/acme/shares", body)
	var s struct{ ID, Slug, Kind string }
	if err := json.Unmarshal([]byte(out), &s); code != 201 || err != nil || s.Slug != "media" || s.ID == "" {
		t.Fatalf("create = %d %s", code, out)
	}
	if code, _ := w.do(t, w.owner, "POST", "/orgs/acme/shares", body); code != 409 {
		t.Errorf("duplicate slug = %d, want 409", code)
	}
	for name, bad := range map[string]string{
		"kind":     `{"slug":"a","kind":"ftp","source":"x"}`,
		"password": `{"slug":"a","kind":"smb","source":"//nas/x","user":"bob","password_ref":"hunter2"}`,
	} {
		if code, _ := w.do(t, w.owner, "POST", "/orgs/acme/shares", bad); code != 422 && code != 400 {
			t.Errorf("bad %s = %d, want 400 or 422", name, code)
		}
	}

	code, out = w.do(t, w.owner, "GET", "/orgs/acme/shares", "")
	var ls []struct{ Slug string }
	if err := json.Unmarshal([]byte(out), &ls); code != 200 || err != nil || len(ls) != 1 {
		t.Errorf("list = %d %s", code, out)
	}
	if code, out := w.do(t, w.stranger, "GET", "/orgs/other/shares", ""); code != 200 || out != "[]\n" && out != "[]" {
		t.Errorf("other org list = %d %q, want its own empty list", code, out)
	}
	if code, _ := w.do(t, w.stranger, "DELETE", "/orgs/other/shares/"+s.ID, ""); code != 404 {
		t.Errorf("another org's share by id = %d, want 404", code)
	}
	if code, _ := w.do(t, member, "DELETE", "/orgs/acme/shares/media", ""); code != 403 {
		t.Errorf("member delete = %d, want 403", code)
	}

	ctx := context.Background()
	tl, err := w.env.Store.Tiles.Get(ctx, w.tile.ID)
	if err != nil {
		t.Fatal(err)
	}
	tl.Volumes = "share:media/a:/data"
	if err := w.env.Store.Tiles.Update(ctx, tl); err != nil {
		t.Fatal(err)
	}
	if code, out := w.do(t, w.owner, "DELETE", "/orgs/acme/shares/media", ""); code != 409 {
		t.Errorf("delete while mounted = %d %s, want 409", code, out)
	}
	tl.Volumes = ""
	if err := w.env.Store.Tiles.Update(ctx, tl); err != nil {
		t.Fatal(err)
	}
	if code, out := w.do(t, w.owner, "DELETE", "/orgs/acme/shares/media", ""); code != 204 && code != 200 {
		t.Errorf("delete = %d %s", code, out)
	}
}
