package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// Repository is one image repository of an org: "<org>/<stack>/<tile>".
type Repository struct {
	Name     string    `json:"name"`
	Tags     int       `json:"tags"`
	Latest   string    `json:"latest"` // the newest tag by build time; "" when unknown
	LatestAt time.Time `json:"latest_at"`
	Size     int64     `json:"size"` // bytes of the latest tag's layers
}

// OrgRepositories lists the org's repositories from the local registry, by
// name. The registry being down is an error the page shows as its text.
func (o *Orchestrator) OrgRepositories(ctx context.Context, orgID string) ([]Repository, error) {
	og, err := o.store.Orgs.Get(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return listRepositories(ctx, "http://127.0.0.1:"+installspec.RegistryPort, og.Slug)
}

// RegistryHost is the address the registry answers at ("" until the panel
// has a domain).
func (o *Orchestrator) RegistryHost(ctx context.Context) string {
	panel, _ := o.settings.Get(ctx, "panel_domain")
	if panel == "" {
		return ""
	}
	return RegistryHostPrefix + installspec.CleanHost(panel)
}

// registryTagCap bounds the manifests read per repository to find the
// newest tag. ponytail: a repository with more tags than this may show a
// slightly older "latest"; keep a tag index in the store if it matters.
const registryTagCap = 20

func listRepositories(ctx context.Context, base, org string) ([]Repository, error) {
	c := &regClient{base: base, http: &http.Client{Timeout: 5 * time.Second}}
	var cat struct {
		Repositories []string `json:"repositories"`
	}
	if err := c.get(ctx, "/v2/_catalog?n=10000", "", &cat); err != nil {
		return nil, fmt.Errorf("the registry did not answer: %w", err)
	}
	slices.Sort(cat.Repositories)
	var out []Repository
	for _, name := range cat.Repositories {
		if !strings.HasPrefix(name, org+"/") {
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
		for _, tag := range tl.Tags[:min(len(tl.Tags), registryTagCap)] {
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

type regClient struct {
	base string
	http *http.Client
}

func (c *regClient) get(ctx context.Context, path, accept string, into any) error {
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// built is when the tag's image was built (its config's "created") and the
// size of its layers.
func (c *regClient) built(ctx context.Context, repo, tag string) (time.Time, int64, error) {
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
