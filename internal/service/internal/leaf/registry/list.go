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
)

// Repository is one image repository: "<org>/<stack>/<tile>" for a tile's
// builds, "<org>/<host>/<path...>" for an import.
type Repository struct {
	Name     string    `json:"name"`
	Tags     int       `json:"tags"`
	Latest   string    `json:"latest"` // the newest tag by build time; "" when unknown
	LatestAt time.Time `json:"latest_at"`
	Size     int64     `json:"size"` // bytes of the latest tag's layers
}

// tagCap bounds the manifests read per repository to find the newest tag.
// ponytail: a repository with more tags than this may show a slightly older
// "latest"; read the registry_images rows instead if it matters.
const tagCap = 20

// List is the registry's repositories under the org slug, by name; "" lists
// every org's. The registry being down is an error the page shows as its
// text.
func (l *Leaf) List(ctx context.Context, org string) ([]Repository, error) {
	c := &client{base: l.base, http: &http.Client{Timeout: 5 * time.Second}}
	var cat struct {
		Repositories []string `json:"repositories"`
	}
	if err := c.get(ctx, "/v2/_catalog?n=10000", "", &cat); err != nil {
		return nil, fmt.Errorf("the registry did not answer: %w", err)
	}
	slices.Sort(cat.Repositories)
	var out []Repository
	for _, name := range cat.Repositories {
		if org != "" && !strings.HasPrefix(name, org+"/") {
			continue
		}
		r := Repository{Name: name}
		var tl struct {
			Tags []string `json:"tags"`
		}
		if err := c.get(ctx, "/v2/"+name+"/tags/list", "", &tl); err != nil {
			return nil, err
		}
		r.Tags = len(tl.Tags)
		for _, tag := range tl.Tags[:min(len(tl.Tags), tagCap)] {
			at, size, err := c.built(ctx, name, tag)
			if err != nil {
				continue // a tag that cannot be read is not the latest
			}
			if r.Latest == "" || at.After(r.LatestAt) {
				r.Latest, r.LatestAt, r.Size = tag, at, size
			}
		}
		out = append(out, r)
	}
	return out, nil
}

type client struct {
	base string
	http *http.Client
}

func (c *client) get(ctx context.Context, path, accept string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// built is when the tag's image was built (its config's "created") and the
// size of its layers.
func (c *client) built(ctx context.Context, repo, tag string) (time.Time, int64, error) {
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Size int64 `json:"size"`
		} `json:"layers"`
	}
	const accept = "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json"
	if err := c.get(ctx, "/v2/"+repo+"/manifests/"+url.PathEscape(tag), accept, &m); err != nil {
		return time.Time{}, 0, err
	}
	var cfg struct {
		Created time.Time `json:"created"`
	}
	if err := c.get(ctx, "/v2/"+repo+"/blobs/"+m.Config.Digest, "", &cfg); err != nil {
		return time.Time{}, 0, err
	}
	var size int64
	for _, l := range m.Layers {
		size += l.Size
	}
	return cfg.Created, size, nil
}
