package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Repository is one image repository: "<org>/<stack>/<tile>" for a tile's
// builds, "<org>/<host>/<path...>" for an import.
type Repository struct {
	Name     string    `json:"name"`
	Tags     int       `json:"tags"`
	Latest   string    `json:"latest"` // the newest tag by build or arrival time; "" when unknown
	LatestAt time.Time `json:"latest_at"`
	Size     int64     `json:"size"` // bytes of the latest tag's layers
}

// tagCap bounds the manifests read per repository to find the newest tag.
// ponytail: a repository with more tags than this may show a slightly older
// "latest"; Versions reads them all.
const tagCap = 20

// List is the registry's repositories under the org slug, by name; "" lists
// every org's. The registry being down is an error the page shows as its
// text. A tag with a row is dated by its arrival and sized by the row: an
// imported index has no layers of its own, so the row is what lets it show as
// the latest.
func (l *Leaf) List(ctx context.Context, org string) ([]Repository, error) {
	c := &client{base: l.base, http: &http.Client{Timeout: 5 * time.Second, Transport: l.tr}}
	repos, err := l.catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("the registry did not answer: %w", err)
	}
	slices.Sort(repos)
	rows, err := l.rows(ctx)
	if err != nil {
		return nil, err
	}
	var out []Repository
	for _, name := range repos {
		if org != "" && !strings.HasPrefix(name, org+"/") {
			continue
		}
		r := Repository{Name: name}
		var tl struct {
			Tags []string `json:"tags"`
		}
		if _, err := c.get(ctx, "/v2/"+name+"/tags/list", "", &tl); err != nil {
			return nil, err
		}
		r.Tags = len(tl.Tags)
		for _, tag := range tl.Tags[:min(len(tl.Tags), tagCap)] {
			in, err := c.inspect(ctx, name, tag)
			if err != nil {
				continue // a tag that cannot be read is not the latest
			}
			at, size := in.created, in.size
			if row, ok := rows[name+"@"+in.digest]; ok {
				at, size = row.CreatedAt, row.Size
			}
			if r.Latest == "" || at.After(r.LatestAt) {
				r.Latest, r.LatestAt, r.Size = tag, at, size
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// rows are the table's rows by "repo@digest"; empty when the leaf has no store.
// ponytail: reads every row per call; ListByOrg or ListByRepo when it is large.
func (l *Leaf) rows(ctx context.Context) (map[string]store.RegistryImage, error) {
	out := map[string]store.RegistryImage{}
	if l.images == nil {
		return out, nil
	}
	all, err := l.images.ListAll(ctx)
	for _, r := range all {
		out[r.Repo+"@"+r.Digest] = r
	}
	return out, err
}

type client struct {
	base string
	http *http.Client
}

// get decodes a 200 answer into into and returns its headers.
func (c *client) get(ctx context.Context, path, accept string, into any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry %s: %s", path, resp.Status)
	}
	return resp.Header, json.NewDecoder(resp.Body).Decode(into)
}

// info is what the manifest of a tag says.
type info struct {
	digest    string
	created   time.Time // the config's "created"; zero for an index
	size      int64     // bytes of the layers; 0 for an index
	platforms []string
}

// inspect reads the manifest at ref (a tag or a digest): an image gives its
// build time, layer size and CPU type from its config; an index gives only
// the CPU types of its children (attestations, "unknown", left out).
func (c *client) inspect(ctx context.Context, repo, ref string) (info, error) {
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Size int64 `json:"size"`
		} `json:"layers"`
		Manifests []struct {
			Platform struct {
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	h, err := c.get(ctx, "/v2/"+repo+"/manifests/"+url.PathEscape(ref), manifestAccept, &m)
	if err != nil {
		return info{}, err
	}
	in := info{digest: h.Get("Docker-Content-Digest")}
	if len(m.Manifests) > 0 {
		for _, x := range m.Manifests {
			if a := x.Platform.Architecture; a != "" && a != "unknown" {
				in.platforms = append(in.platforms, a)
			}
		}
		return in, nil
	}
	var cfg struct {
		Created      time.Time `json:"created"`
		Architecture string    `json:"architecture"`
	}
	if _, err := c.get(ctx, "/v2/"+repo+"/blobs/"+m.Config.Digest, "", &cfg); err != nil {
		return info{}, err
	}
	in.created = cfg.Created
	if cfg.Architecture != "" {
		in.platforms = []string{cfg.Architecture}
	}
	for _, l := range m.Layers {
		in.size += l.Size
	}
	return in, nil
}

// Usage is what one org's versions take in the registry; Org is the first
// path part of the repo ("stackr.system" for stackr's own images).
type Usage struct {
	Org      string `json:"org"`
	Versions int    `json:"versions"`
	Bytes    int64  `json:"bytes"`
}

// Usage sums the rows per org, for the repos the registry still holds (a row
// with no image counts for nothing). Bytes are the versions' sizes added up,
// so layers two versions share count twice.
// ponytail: an overestimate of the disk; ask the registry volume for the
// real figure if it matters.
func (l *Leaf) Usage(ctx context.Context) ([]Usage, error) {
	repos, err := l.catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("the registry did not answer: %w", err)
	}
	rows, err := l.rows(ctx)
	if err != nil {
		return nil, err
	}
	by := map[string]*Usage{}
	for _, r := range rows {
		if !slices.Contains(repos, r.Repo) {
			continue
		}
		org, _, _ := strings.Cut(r.Repo, "/")
		u := by[org]
		if u == nil {
			u = &Usage{Org: org}
			by[org] = u
		}
		u.Versions++
		u.Bytes += r.Size
	}
	out := make([]Usage, 0, len(by))
	for _, u := range by {
		out = append(out, *u)
	}
	slices.SortFunc(out, func(a, b Usage) int { return strings.Compare(a.Org, b.Org) })
	return out, nil
}
