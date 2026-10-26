package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Version is one image version of a repo: what the registry holds, with the
// detail of its row.
type Version struct {
	Tag       string    `json:"tag"`
	Digest    string    `json:"digest"`
	Platforms []string  `json:"platforms"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	// Source is "built", "imported" or "system"; "" for an image with no row
	// (pushed before records existed).
	Source      string `json:"source"`
	UpstreamRef string `json:"upstream_ref"`
	CreatedBy   string `json:"created_by"`
	// UsedBy names the envs that run it. The leaf cannot know; the caller
	// (service) fills it from the envs.
	UsedBy []string `json:"used_by"`
}

// Versions are the repo's versions, one per tag, newest first: the registry
// lists the tags and digests, the table adds detail. A row whose image is
// gone is not shown; an image with no row is shown with what the manifest
// says. Tags on one digest share its row. A repo the registry does not have
// (lost with its storage, say) has no versions.
func (l *Leaf) Versions(ctx context.Context, repo string) ([]Version, error) {
	c := &client{base: l.base, http: &http.Client{Timeout: 5 * time.Second, Transport: l.tr}}
	var tl struct {
		Tags []string `json:"tags"`
	}
	if _, err := c.get(ctx, "/v2/"+repo+"/tags/list", "", &tl); err != nil {
		if se, ok := errors.AsType[statusErr](err); ok && se.code == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("the registry did not answer: %w", err)
	}
	rows, err := l.rows(ctx)
	if err != nil {
		return nil, err
	}
	var out []Version
	for _, tag := range tl.Tags {
		in, err := c.inspect(ctx, repo, tag)
		if err != nil {
			continue // a tag that cannot be read is not shown
		}
		v := Version{Tag: tag, Digest: in.digest, Platforms: in.platforms, Size: in.size, CreatedAt: in.created}
		if row, ok := rows[repo+"@"+in.digest]; ok {
			v.Source, v.UpstreamRef, v.CreatedBy = row.Source, row.UpstreamRef, row.CreatedBy
			v.Size, v.CreatedAt = row.Size, row.CreatedAt
			if p := SplitPlatforms(row.Platforms); p != nil {
				v.Platforms = p
			}
		}
		out = append(out, v)
	}
	slices.SortStableFunc(out, func(a, b Version) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Tag, b.Tag)
	})
	return out, nil
}
