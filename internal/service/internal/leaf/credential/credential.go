// Package credential owns registry pull credentials: one org's user and
// password per registry host. They travel per request (never `docker
// login`: the daemon config is shared by every org on the box) and are only
// ever offered to the host they were saved for.
package credential

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Leaf struct{ creds store.CredentialStore }

func New(creds store.CredentialStore) *Leaf { return &Leaf{creds: creds} }

// Spec is a credential as the form or API gives it. An empty Password on
// Update keeps the stored one (the form never shows it back).
type Spec struct{ Name, URL, Username, Password string }

// Get returns the row with its password, for callers that pull. Views use List.
func (l *Leaf) Get(ctx context.Context, orgID, id string) (store.Credential, error) {
	c, err := l.creds.Get(ctx, id)
	if err == nil && c.OrgID != orgID {
		return store.Credential{}, errs.ErrNotFound
	}
	return c, err
}

// List is the org's credentials with passwords blanked.
func (l *Leaf) List(ctx context.Context, orgID string) ([]store.Credential, error) {
	cs, err := l.creds.ListByOrg(ctx, orgID)
	for i := range cs {
		cs[i].Password = ""
	}
	return cs, err
}

func (l *Leaf) Create(ctx context.Context, orgID string, s Spec) (store.Credential, error) {
	c := store.Credential{ID: uuid.NewString(), OrgID: orgID, CreatedAt: time.Now().UTC()}
	if s.Password == "" {
		return c, errs.Invalidf("password", "a credential needs a password or token")
	}
	if err := l.fill(ctx, &c, s); err != nil {
		return c, err
	}
	return c, l.creds.Create(ctx, c)
}

func (l *Leaf) Update(ctx context.Context, orgID, id string, s Spec) (store.Credential, error) {
	c, err := l.Get(ctx, orgID, id)
	if err != nil {
		return c, err
	}
	if s.Password == "" {
		s.Password = c.Password
	}
	if err := l.fill(ctx, &c, s); err != nil {
		return c, err
	}
	return c, l.creds.Update(ctx, c)
}

func (l *Leaf) Delete(ctx context.Context, orgID, id string) error {
	if _, err := l.Get(ctx, orgID, id); err != nil {
		return err
	}
	return l.creds.Delete(ctx, id)
}

func (l *Leaf) fill(ctx context.Context, c *store.Credential, s Spec) error {
	name := strings.TrimSpace(s.Name)
	if name == "" {
		return errs.Invalidf("name", "a credential needs a name")
	}
	host := Host(s.URL)
	if host == "" {
		return errs.Invalidf("url", "%q is not a registry host", s.URL)
	}
	if strings.TrimSpace(s.Username) == "" {
		return errs.Invalidf("username", "a credential needs a user name")
	}
	all, err := l.creds.ListByOrg(ctx, c.OrgID)
	if err != nil {
		return err
	}
	for _, o := range all {
		if o.ID == c.ID {
			continue
		}
		if o.Name == name {
			return errs.Conflictf("a credential named %q already exists", name)
		}
		if o.URL == host {
			return errs.Conflictf("%s already has a credential (%s)", host, o.Name)
		}
	}
	c.Name, c.URL, c.Username, c.Password = name, host, strings.TrimSpace(s.Username), s.Password
	return nil
}

// Host is a registry address reduced to the host the image refs name: no
// scheme or path, lower case, Docker Hub's aliases folded into one.
func Host(url string) string {
	h := strings.ToLower(strings.TrimSpace(url))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h, _, _ = strings.Cut(h, "/")
	if h == "" || strings.ContainsAny(h, " @") {
		return ""
	}
	if h == "docker.io" || h == "index.docker.io" {
		return "registry-1.docker.io"
	}
	return h
}

// For finds the org's credential for the host of an image ref, digest-pinned
// or not. ok=false = pull anonymously.
func (l *Leaf) For(ctx context.Context, orgID, ref string) (c store.Credential, ok bool, err error) {
	name, _, _ := strings.Cut(ref, "@")
	host, _, _, parsed := registry.ParseRef(name)
	if !parsed {
		return c, false, nil
	}
	cs, err := l.creds.ListByOrg(ctx, orgID)
	for _, c := range cs {
		if c.URL == host {
			return c, true, err
		}
	}
	return c, false, err
}

// Auth is the X-Registry-Auth blob Docker takes per pull.
func Auth(c store.Credential) string {
	b, _ := json.Marshal(map[string]string{"username": c.Username, "password": c.Password, "serveraddress": c.URL})
	return base64.URLEncoding.EncodeToString(b)
}
