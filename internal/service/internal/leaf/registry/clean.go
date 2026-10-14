package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
)

// InUse says whether an env runs, pins or can roll back to a version. Clean
// asks a repo's versions newest first.
type InUse func(ctx context.Context, repo, digest string) (bool, error)

// Cleaned is what a Clean did.
type Cleaned struct {
	Deleted int
	// Freed is the manifests' sizes by their rows; the disk comes back at the
	// next garbage collect.
	Freed  int64
	Errors []string
}

// manifestAccept makes the registry answer with the manifest as stored.
const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

// Wait returns once the registry accepts writes, or ctx's error. Import calls
// it before writing: the cleanup sweep shuts the gate around the garbage
// collect, and a blob written meanwhile could be collected.
func (l *Leaf) Wait(ctx context.Context) error { return l.gate.Wait(ctx) }

// Hold waits for the registry to accept writes and holds a sweep off until
// leave is called. Every direct write to the registry (an import, a system
// image push) holds it for the whole write.
func (l *Leaf) Hold(ctx context.Context) (leave func(), err error) { return l.gate.Enter(ctx) }

// Gate is the gate regfront reads (Server.Gate): shut, pushes through the
// front answer 503 with Retry-After.
func (l *Leaf) Gate() *regfront.Gate { return &l.gate }

// ErrSweeping says another sweep holds the gate.
var ErrSweeping = errors.New("a registry sweep is already running")

// WhileGated shuts the gate, runs sweep and opens the gate again, whatever
// sweep returns (its error, a timeout, a panic).
func (l *Leaf) WhileGated(ctx context.Context, sweep func(context.Context) error) error {
	open, ok := l.gate.Close()
	if !ok {
		return ErrSweeping
	}
	defer open()
	// Writes that started before the gate shut finish first: their blobs
	// are untagged until their manifest goes up.
	if err := l.gate.Drain(ctx); err != nil {
		return fmt.Errorf("waiting for registry writes: %w", err)
	}
	return sweep(ctx)
}

// Resolve is the digest a tag points at, "" when the repo or tag is not in
// the registry.
func (l *Leaf) Resolve(ctx context.Context, repo, tag string) (string, error) {
	resp, err := l.do(ctx, http.MethodHead, "/v2/"+repo+"/manifests/"+url.PathEscape(tag))
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Header.Get("Docker-Content-Digest"), nil
	case http.StatusNotFound:
		return "", nil
	}
	return "", fmt.Errorf("registry %s:%s: %s", repo, tag, resp.Status)
}

func (l *Leaf) do(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, l.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestAccept)
	return (&http.Client{Timeout: 10 * time.Second, Transport: l.tr}).Do(req)
}

func (l *Leaf) getJSON(ctx context.Context, path string, into any) error {
	resp, err := l.do(ctx, http.MethodGet, path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// catalog is every repository, paged: the registry refuses n above its
// maxentries (1000) with a 400, so a long list is read by "last".
func (l *Leaf) catalog(ctx context.Context) ([]string, error) {
	var all []string
	for {
		var page struct {
			Repositories []string `json:"repositories"`
		}
		path := "/v2/_catalog?n=1000"
		if len(all) > 0 {
			path += "&last=" + url.QueryEscape(all[len(all)-1])
		}
		if err := l.getJSON(ctx, path, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Repositories...)
		if len(page.Repositories) < 1000 {
			return all, nil
		}
	}
}

// version is one manifest digest of a repo, with the tags that name it.
type version struct {
	digest  string
	tags    []string
	created time.Time
}

// versions are the repo's tagged digests, newest first. The time is the row's
// created_at; a digest with no row (a hand push) falls back to the image
// config's "created", zero for an index or when it cannot be read, so it sorts
// as oldest.
func (l *Leaf) versions(ctx context.Context, repo string) ([]version, error) {
	var tl struct {
		Tags []string `json:"tags"`
	}
	if err := l.getJSON(ctx, "/v2/"+repo+"/tags/list", &tl); err != nil {
		return nil, err
	}
	rows, err := l.images.ListByRepo(ctx, repo)
	if err != nil {
		return nil, err
	}
	at := map[string]time.Time{}
	for _, r := range rows {
		at[r.Digest] = r.CreatedAt
	}
	var out []version
	for _, tag := range tl.Tags {
		d, err := l.Resolve(ctx, repo, tag)
		if err != nil {
			return nil, err
		}
		if d == "" {
			continue
		}
		i := slices.IndexFunc(out, func(v version) bool { return v.digest == d })
		if i < 0 {
			t, ok := at[d]
			if !ok {
				t = l.createdAt(ctx, repo, d)
			}
			out = append(out, version{digest: d, created: t})
			i = len(out) - 1
		}
		out[i].tags = append(out[i].tags, tag)
	}
	slices.SortFunc(out, func(a, b version) int {
		if c := b.created.Compare(a.created); c != 0 {
			return c
		}
		return strings.Compare(a.digest, b.digest)
	})
	return out, nil
}

// createdAt is the "created" of an image manifest's config, zero otherwise.
func (l *Leaf) createdAt(ctx context.Context, repo, digest string) time.Time {
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	var cfg struct {
		Created time.Time `json:"created"`
	}
	if l.getJSON(ctx, "/v2/"+repo+"/manifests/"+digest, &m) != nil || m.Config.Digest == "" ||
		l.getJSON(ctx, "/v2/"+repo+"/blobs/"+m.Config.Digest, &cfg) != nil {
		return time.Time{}
	}
	return cfg.Created
}

// Clean is the mark half of the nightly cleanup: per repo, every version
// beyond the newest keep(repo) that inUse refuses, deleted as Delete does (manifest
// and row). A digest is deleted only when no tag on it is in use: inUse is
// asked of the digest, which every tag on it shares. One repo failing is
// noted in Errors and the rest go on. The sweep (gate pushes, garbage collect
// --delete-untagged, ungate) is the service's, since it needs Docker; see
// WhileGated.
// ponytail: tags only; a manifest nothing tags is the garbage collect's.
func (l *Leaf) Clean(ctx context.Context, keep func(repo string) int, inUse InUse) (Cleaned, error) {
	var res Cleaned
	repos, err := l.catalog(ctx)
	if err != nil {
		return res, fmt.Errorf("the registry did not answer: %w", err)
	}
	note := func(repo string, err error) { res.Errors = append(res.Errors, repo+": "+err.Error()) }
	for _, repo := range repos {
		vs, err := l.versions(ctx, repo)
		if err != nil {
			note(repo, err)
			continue
		}
		for _, v := range vs[min(keep(repo), len(vs)):] {
			used, err := inUse(ctx, repo, v.digest)
			if err != nil {
				// Unknown is as good as in use.
				note(repo, err)
				continue
			}
			if used {
				continue
			}
			var size int64
			if row, err := l.images.GetByRepoDigest(ctx, repo, v.digest); err == nil {
				size = row.Size
			}
			if err := l.Delete(ctx, repo, v.digest); err != nil {
				note(repo, err)
				continue
			}
			res.Deleted++
			res.Freed += size
		}
	}
	return res, nil
}
