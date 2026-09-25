package credential_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func seedOrg(t *testing.T, st *store.Store) string {
	t.Helper()
	id := uuid.NewString()
	if err := st.Orgs.Create(ctx, store.Org{ID: id, Name: id, Slug: id[:8], EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCredentials(t *testing.T) {
	st := servicetest.Store(t)
	l := credential.New(st.Credentials)
	org, other := seedOrg(t, st), seedOrg(t, st)

	c, err := l.Create(ctx, org, credential.Spec{Name: "ghcr", URL: "https://GHCR.io/v2/", Username: "bot", Password: "tok"})
	if err != nil || c.URL != "ghcr.io" {
		t.Fatalf("create = %+v, %v", c, err)
	}
	if _, err := l.Create(ctx, org, credential.Spec{Name: "again", URL: "ghcr.io", Username: "x", Password: "y"}); err == nil {
		t.Error("second credential for one host accepted")
	}
	hub, err := l.Create(ctx, org, credential.Spec{Name: "hub", URL: "docker.io", Username: "me", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}

	// Kept on an empty password; the list never carries it.
	if _, err := l.Update(ctx, org, c.ID, credential.Spec{Name: "ghcr", URL: "ghcr.io", Username: "bot2"}); err != nil {
		t.Fatal(err)
	}
	got, _ := l.Get(ctx, org, c.ID)
	if got.Password != "tok" || got.Username != "bot2" {
		t.Errorf("update = %+v", got)
	}
	list, _ := l.List(ctx, org)
	for _, x := range list {
		if x.Password != "" {
			t.Errorf("list leaked %s's password", x.Name)
		}
	}

	// Offered only to its own host, and only to its own org.
	for ref, want := range map[string]string{
		"ghcr.io/acme/api:1":                  c.ID,
		"ghcr.io/acme/api@sha256:abc":         c.ID,
		"nginx:1":                             hub.ID,
		"docker.io/library/redis":             hub.ID,
		"quay.io/x/y:1":                       "",
		"evil.example.com/ghcr.io/acme/api:1": "",
	} {
		got, ok, err := l.For(ctx, org, ref)
		if err != nil || ok != (want != "") || ok && got.ID != want {
			t.Errorf("For(%s) = %s %v %v, want %q", ref, got.ID, ok, err, want)
		}
	}
	if _, ok, _ := l.For(ctx, other, "ghcr.io/acme/api:1"); ok {
		t.Error("another org got the credential")
	}
	if _, err := l.Get(ctx, other, c.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("cross-org get = %v", err)
	}
	if err := l.Delete(ctx, other, c.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("cross-org delete = %v", err)
	}

	raw, _ := base64.URLEncoding.DecodeString(credential.Auth(got))
	if !strings.Contains(string(raw), `"password":"tok"`) {
		t.Errorf("auth = %s", raw)
	}
}
