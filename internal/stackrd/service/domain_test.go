package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type domStore struct {
	repo.Store
	stack    *repo.Stack
	domain   *repo.Domain // what GetDomain answers
	atHost   *repo.Domain // what GetDomainByHostPath answers
	dnsProv  string
	orgBySlg *repo.Org
	created  *repo.Domain
}

func (d *domStore) GetStack(context.Context, string) (*repo.Stack, error) { return d.stack, nil }
func (d *domStore) GetDomain(context.Context, string) (*repo.Domain, error) {
	return d.domain, nil
}
func (d *domStore) GetDomainByHostPath(context.Context, string, string) (*repo.Domain, error) {
	return d.atHost, nil
}
func (d *domStore) GetSetting(_ context.Context, key string) (string, error) {
	if key == "dns_provider" {
		return d.dnsProv, nil
	}
	return "", nil
}
func (d *domStore) GetOrgBySlug(context.Context, string) (*repo.Org, error) {
	return d.orgBySlg, nil
}
func (d *domStore) CreateDomain(_ context.Context, x *repo.Domain) error {
	d.created = x
	return nil
}

func domSvc(st *domStore) *DomainService {
	return NewDomainService(st, nil, NewGateService(st))
}

func tile() *repo.Tile {
	return &repo.Tile{ID: "t1", StackID: "s1", Kind: "service", ContainerPort: 8080}
}

// THE security row. The panel's HTTPS toggle and its live delete branch took
// whatever :domainID named, and the only check above them proved write access
// to the tile in the URL — so anyone with a tile of their own could flip or
// delete any domain in the install by id. A domain on another tile has to
// read as missing, not as forbidden: a 403 would confirm the id exists.
func TestADomainOnAnotherTileIsInvisible(t *testing.T) {
	st := &domStore{
		stack:  &repo.Stack{ID: "s1"},
		domain: &repo.Domain{ID: "d1", TileID: "someone-else", Host: "victim.example.com"},
	}
	s := domSvc(st)
	mine := tile()
	on := true

	if _, _, _, err := s.ToggleHTTPS(context.Background(), mine, "d1", Actor{Via: "api"}); err != svcerr.ErrNotFound {
		t.Errorf("ToggleHTTPS: got %v, want not found", err)
	}
	if _, _, err := s.SetTLS(context.Background(), mine, "d1", TLSPatch{HTTPS: &on}, Actor{Via: "api"}); err != svcerr.ErrNotFound {
		t.Errorf("SetTLS: got %v, want not found", err)
	}
	if _, _, err := s.Detach(context.Background(), mine, "d1", Actor{Via: "api"}); err != svcerr.ErrNotFound {
		t.Errorf("Detach: got %v, want not found", err)
	}
}

func TestAttachDefaults(t *testing.T) {
	st := &domStore{stack: &repo.Stack{ID: "s1"}}
	s := domSvc(st)

	// A bare attach produces a working HTTPS domain, and the redirect follows
	// it: "serve TLS but leave plain HTTP unredirected" is not what anyone
	// adding a hostname means.
	d, _, err := s.Attach(context.Background(), tile(), DomainSpec{Host: "a.example.com"}, Actor{Via: "api"})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !d.HTTPS || !d.ForceHTTPS {
		t.Errorf("https=%v force=%v, want both on", d.HTTPS, d.ForceHTTPS)
	}
	if d.Path != "/" {
		t.Errorf("path %q, want the normalised /", d.Path)
	}
	if d.ContainerPort != 8080 {
		t.Errorf("port %d, want the tile's", d.ContainerPort)
	}

	// A redirect never proxies, so its target port is unused — but zero
	// renders into the route as http://alias:0, which the API used to write.
	d, _, err = s.Attach(context.Background(), &repo.Tile{ID: "t2", StackID: "s1", Kind: "service"},
		DomainSpec{Host: "old.example.com", RedirectTo: "new.example.com"}, Actor{Via: "api"})
	if err != nil {
		t.Fatalf("redirect attach: %v", err)
	}
	if d.ContainerPort != 80 {
		t.Errorf("redirect port %d, want 80", d.ContainerPort)
	}

	// Explicit beats the default, and force follows https unless it is given.
	off := false
	d, _, err = s.Attach(context.Background(), tile(),
		DomainSpec{Host: "plain.example.com", HTTPS: &off}, Actor{Via: "api"})
	if err != nil {
		t.Fatalf("plain attach: %v", err)
	}
	if d.HTTPS || d.ForceHTTPS {
		t.Errorf("https=%v force=%v, want both off", d.HTTPS, d.ForceHTTPS)
	}
}

func TestAttachRefusals(t *testing.T) {
	base := func() *domStore { return &domStore{stack: &repo.Stack{ID: "s1", OrgID: "mine"}} }

	t.Run("a tile with no port and no redirect has nothing to route to", func(t *testing.T) {
		_, _, err := domSvc(base()).Attach(context.Background(),
			&repo.Tile{ID: "t2", StackID: "s1", Kind: "service"},
			DomainSpec{Host: "a.example.com"}, Actor{Via: "api"})
		if inv, ok := svcerr.IsInvalid(err); !ok || inv.Field != "container_port" {
			t.Errorf("got %v", err)
		}
	})

	t.Run("a wildcard cannot get a certificate over HTTP-01", func(t *testing.T) {
		_, _, err := domSvc(base()).Attach(context.Background(), tile(),
			DomainSpec{Host: "*.example.com"}, Actor{Via: "api"})
		if inv, ok := svcerr.IsInvalid(err); !ok || inv.Field != "host" {
			t.Errorf("got %v", err)
		}
		// With a DNS provider configured it is fine.
		st := base()
		st.dnsProv = "cloudflare"
		if _, _, err := domSvc(st).Attach(context.Background(), tile(),
			DomainSpec{Host: "*.example.com"}, Actor{Via: "api"}); err != nil {
			t.Errorf("with a DNS provider: %v", err)
		}
	})

	t.Run("another tile already answers there", func(t *testing.T) {
		st := base()
		st.atHost = &repo.Domain{ID: "d9", TileID: "other"}
		_, _, err := domSvc(st).Attach(context.Background(), tile(),
			DomainSpec{Host: "a.example.com"}, Actor{Via: "api"})
		if _, ok := svcerr.IsConflict(err); !ok {
			t.Errorf("got %v", err)
		}
	})

	// The API had no squat guard at all, which made the one on the org page
	// decorative: the same name could be claimed one tile down.
	t.Run("the host starts with another org's slug", func(t *testing.T) {
		st := base()
		st.orgBySlg = &repo.Org{ID: "theirs", Slug: "acme"}
		_, _, err := domSvc(st).Attach(context.Background(), tile(),
			DomainSpec{Host: "acme.example.com"}, Actor{Via: "api"})
		if _, ok := svcerr.IsConflict(err); !ok {
			t.Errorf("got %v", err)
		}
	})

	t.Run("a managed engine that does not speak HTTP", func(t *testing.T) {
		pg := &repo.Tile{ID: "db", StackID: "s1", Kind: "service", Engine: "postgres", ContainerPort: 5432}
		_, _, err := domSvc(base()).Attach(context.Background(), pg,
			DomainSpec{Host: "db.example.com"}, Actor{Via: "api"})
		if _, ok := svcerr.IsInvalid(err); !ok {
			t.Errorf("got %v", err)
		}
	})
}
