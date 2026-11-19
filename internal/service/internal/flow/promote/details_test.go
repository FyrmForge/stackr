package promote

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A create row says what the new tile will be; empty lines are left out.
func TestCreateDetails(t *testing.T) {
	got := createDetails(
		TileConf{Engine: "postgres", Major: 18, Volumes: []string{"pgdata:/var/lib/postgresql", "bk:/b"}},
		store.Tile{ImageRef: "postgres:18.6"},
		"vm-hel-2",
		nil,
	)
	want := "Engine: postgres\nMajor: 18\nImage: postgres:18.6\nNode: vm-hel-2\nVolumes: pgdata:/var/lib/postgresql, bk:/b"
	if got != want {
		t.Errorf("details = %q, want %q", got, want)
	}
	if got := createDetails(TileConf{}, store.Tile{}, "", nil); got != "" {
		t.Errorf("an empty block has details %q", got)
	}
}

// A create row's volumes read as names, new ones flagged, with the file's cap.
func TestCreateDetailsVolumes(t *testing.T) {
	word := func(spec string) string { return "pgdata, new, 5120 MB" }
	got := createDetails(TileConf{Volumes: []string{"pgdata:/var/lib/postgresql"}}, store.Tile{}, "", word)
	if got != "Volumes: pgdata, new, 5120 MB" {
		t.Errorf("details = %q", got)
	}
}

// A changed domain names each field, never a password; an equal row says nothing.
func TestDomainDiff(t *testing.T) {
	row := store.Domain{ContainerPort: 8080, HTTPS: true, ForceHTTPS: true, ProxyJSON: `{"basic_auth":{"user":"u","password":"s3cret"}}`}
	sp := domain.Spec{Port: 9090, Extras: domain.Extras{ForwardAuth: &domain.ForwardAuth{URL: "https://auth.x"}}}
	det, note := domainDiff(row, sp)
	for _, w := range []string{"port: 8080 to 9090", "basic_auth: removed", "forward_auth url: added https://auth.x"} {
		if !strings.Contains(det, w) {
			t.Errorf("details %q lack %q", det, w)
		}
	}
	if note != "port changed, basic_auth removed, forward_auth added" {
		t.Errorf("note = %q", note)
	}
	sp = domain.Spec{Port: 8080, Extras: domain.Extras{BasicAuth: &domain.BasicAuth{User: "u", Password: "other"}}}
	det, _ = domainDiff(row, sp)
	if det != "basic_auth password: changed" || strings.Contains(det, "other") || strings.Contains(det, "s3cret") {
		t.Errorf("details = %q", det)
	}
	if det, note := domainDiff(row, domain.Spec{Port: 8080, Extras: domain.Extras{BasicAuth: &domain.BasicAuth{User: "u", Password: "s3cret"}}}); det != "" || note != "" {
		t.Errorf("equal rows differ: %q %q", det, note)
	}
	det, note = resDiff(store.DomainResource{ACMEEmail: "a@x"}, store.DomainResource{IncludeEnvOnDefault: true, ACMEEmail: "b@x"})
	if det != "include_env_on_default: false to true\nacme_email: a@x to b@x" || note != "include_env_on_default changed, acme_email changed" {
		t.Errorf("res: %q %q", det, note)
	}
}
