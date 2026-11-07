package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Delete removes one version: the manifest by digest at the registry (which
// drops every tag on it) and its row, together; the row stays when the
// registry call fails. Deleting what is already gone is not an error. Whether
// an env still uses the version is not decided here: the service checks that
// and warns first, and Clean asks its inUse. The blobs stay until the next
// garbage collect.
func (l *Leaf) Delete(ctx context.Context, repo, digest string) error {
	resp, err := l.do(ctx, http.MethodDelete, "/v2/"+repo+"/manifests/"+digest)
	if err != nil {
		return fmt.Errorf("the registry did not answer: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("the registry refused the delete of %s@%s: %s", repo, digest, resp.Status)
	}
	return l.images.DeleteByRepoDigest(ctx, repo, digest)
}

// orgRepos are the catalog's repositories under the org slug.
func (l *Leaf) orgRepos(ctx context.Context, org string) ([]string, error) {
	all, err := l.catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("the registry did not answer: %w", err)
	}
	return slices.DeleteFunc(all, func(r string) bool { return !strings.HasPrefix(r, org+"/") }), nil
}

// Holds says whether the registry still has tagged images under the org slug
// (an emptied repository stays in the catalog and does not count). Registry
// rights are keyed on the slug, so a new org must not take one that does.
func (l *Leaf) Holds(ctx context.Context, org string) (bool, error) {
	rs, err := l.orgRepos(ctx, org)
	if err != nil {
		return false, err
	}
	for _, r := range rs {
		var tl struct {
			Tags []string `json:"tags"`
		}
		if err := l.getJSON(ctx, "/v2/"+r+"/tags/list", &tl); err != nil {
			return false, err
		}
		if len(tl.Tags) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// PurgeOrg deletes every version of every repository under the org slug, for
// an org that is gone. The blobs stay until the next garbage collect.
func (l *Leaf) PurgeOrg(ctx context.Context, org string) error {
	rs, err := l.orgRepos(ctx, org)
	if err != nil {
		return err
	}
	var all error
	for _, r := range rs {
		vs, err := l.versions(ctx, r)
		if err != nil {
			all = errors.Join(all, err)
			continue
		}
		for _, v := range vs {
			all = errors.Join(all, l.Delete(ctx, r, v.digest))
		}
	}
	return all
}
