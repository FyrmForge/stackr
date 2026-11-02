package api_test

import (
	"encoding/json"
	"testing"
	"time"
)

// Key lifecycle: an expiring key dies on time, a rotation swaps the secret,
// an org owner lists and revokes the org's keys, the admin sees every key.
func TestKeyLifecycleAPI(t *testing.T) {
	w := newWorld(t)

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if code, out := w.do(t, w.owner, "POST", "/orgs/acme/keys", `{"name":"x","expires_at":"`+past+`"}`); code != 400 {
		t.Errorf("expiry in the past = %d %s, want 400", code, out)
	}
	soon := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	code, out := w.do(t, w.owner, "POST", "/orgs/acme/keys", `{"name":"ci","expires_at":"`+soon+`"}`)
	var minted struct {
		Token string
		Key   struct{ ID string }
	}
	if err := json.Unmarshal([]byte(out), &minted); code != 201 || err != nil {
		t.Fatalf("mint = %d %s", code, out)
	}

	code, out = w.do(t, minted.Token, "POST", "/me/keys/"+minted.Key.ID+"/rotate", "")
	var rotated struct {
		Token string
		Key   struct{ ID string }
	}
	if err := json.Unmarshal([]byte(out), &rotated); code != 200 || err != nil || rotated.Token == minted.Token || rotated.Key.ID != minted.Key.ID {
		t.Fatalf("rotate = %d %s", code, out)
	}
	if code, _ = w.do(t, minted.Token, "GET", "/me", ""); code != 401 {
		t.Errorf("old secret after rotate = %d, want 401", code)
	}
	if code, _ = w.do(t, rotated.Token, "GET", "/me", ""); code != 200 {
		t.Errorf("new secret = %d, want 200", code)
	}

	// the org's owner lists the key, last_used_at is stamped, a stranger sees none of it
	code, out = w.do(t, w.owner, "GET", "/orgs/acme/keys", "")
	var ks []struct {
		ID         string
		Owner      string
		LastUsedAt *time.Time `json:"last_used_at"`
		ExpiresAt  *time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(out), &ks); code != 200 || err != nil {
		t.Fatalf("org keys = %d %s", code, out)
	}
	var seen bool
	for _, k := range ks {
		if k.ID == minted.Key.ID {
			seen = k.LastUsedAt != nil && k.ExpiresAt != nil && k.Owner == "owner@x"
		}
	}
	if !seen {
		t.Errorf("org key list lacks the key's owner, expiry or last use: %s", out)
	}
	if code, _ = w.do(t, w.stranger, "GET", "/orgs/acme/keys", ""); code != 404 {
		t.Errorf("stranger lists org keys = %d, want 404", code)
	}
	if code, _ = w.do(t, w.stranger, "DELETE", "/orgs/other/keys/"+minted.Key.ID, ""); code != 404 {
		t.Errorf("revoking acme's key through another org = %d, want 404", code)
	}

	if code, out = w.do(t, w.admin, "GET", "/admin/keys", ""); code != 200 {
		t.Fatalf("admin keys = %d %s", code, out)
	}
	if code, _ = w.do(t, w.owner, "GET", "/admin/keys", ""); code != 403 {
		t.Errorf("owner lists all keys = %d, want 403", code)
	}

	if code, out = w.do(t, w.owner, "DELETE", "/orgs/acme/keys/"+minted.Key.ID, ""); code != 204 {
		t.Fatalf("org revoke = %d %s", code, out)
	}
	if code, _ = w.do(t, rotated.Token, "GET", "/me", ""); code != 401 {
		t.Errorf("revoked key = %d, want 401", code)
	}
}
