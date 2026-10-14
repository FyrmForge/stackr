package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/git"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// fileMounts puts the tile's files: lines in a volume on the env's node and
// returns them as read-only mounts of it, one subpath per line. key =
// commit-<files hash>-<params version> names the volume. The content is the
// config commit the env's release pins, read from the stack's config clone
// (promote's plan cloned it). A :template line goes whole through the
// resolver, secrets included; any other is copied byte for byte. Called only
// when the tile has files: lines.
// A volume is written once and never rewritten: an edit of the lines, a
// param or the commit makes a new key, and the post-deploy prune drops the
// old volume. The key holds no secret (the name shows in docker inspect): the
// params version is the count and newest updated_at of the params the tile sees.
func (f *Flow) fileMounts(
	ctx context.Context,
	t store.Tile,
	e store.Environment,
	st store.Stack,
	rr *params.Resolver,
) ([]docker.Mount, error) {
	refuse := errs.Conflictf("%s: files: needs the stack's config repo", t.Slug)
	if st.ConfigRepo == "" || e.ReleaseID == nil {
		return nil, refuse
	}
	pins, err := f.Releases.Pins(ctx, *e.ReleaseID)
	if err != nil {
		return nil, err
	}
	commit := pins[release.ConfigSlug].CommitSHA
	if commit == "" {
		return nil, refuse
	}
	repo := git.Repo{Dir: filepath.Join(f.DataDir, "repos", "config-"+st.ID)}
	if !repo.Has(ctx, commit) {
		return nil, errs.Conflictf("%s: files: the config repo clone lacks %.12s; run a promote to fetch it", t.Slug, commit)
	}
	ver, err := f.paramsVersion(ctx, e, st)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(strings.Join(tile.Lines(t.Files), "\n")))
	key := commit + "-" + hex.EncodeToString(sum[:4]) + "-" + ver

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	put := func(name string, mode int64, typ byte, b []byte) error {
		h := &tar.Header{Name: name, Mode: mode, Typeflag: typ, Size: int64(len(b)), ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	type slot struct {
		sub, dst string
	}
	var slots []slot
	for i, l := range tile.Lines(t.Files) {
		src, dst, err := tile.ParseFileMount(l)
		if err != nil {
			return nil, errs.Invalidf("files", "%s", err.Error())
		}
		tmpl := strings.HasSuffix(l, ":template")
		n := strconv.Itoa(i)
		isDir, err := repo.IsDir(ctx, commit, src)
		if err != nil {
			return nil, err
		}
		names := []string{src}
		sub := path.Join(n, path.Base(src))
		if isDir {
			sub = n
			// An empty folder still has to exist to be mounted.
			if err := put(n+"/", 0o755, tar.TypeDir, nil); err != nil {
				return nil, err
			}
			if names, err = repo.ListTree(ctx, commit, src); err != nil {
				return nil, err
			}
		}
		slots = append(slots, slot{sub, dst})
		for _, name := range names {
			rel := path.Base(src)
			if isDir {
				rel = strings.TrimPrefix(name, path.Clean(src)+"/")
			}
			b, err := repo.ReadFile(ctx, commit, name)
			if err != nil {
				return nil, errs.Conflictf("%s: files: %s is not in the config repo at %.12s", t.Slug, name, commit)
			}
			if tmpl {
				x, err := rr.Expand(params.InEnv, string(b))
				if err != nil {
					return nil, err
				}
				b = []byte(x)
			}
			p, err := slotPath(n, rel)
			if err != nil {
				return nil, err
			}
			// The container's user is unknown here: world-readable.
			if err := put(p, 0o644, tar.TypeReg, b); err != nil {
				return nil, err
			}
		}
	}
	if err := put(tile.FilesDone, 0o644, tar.TypeReg, nil); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	vol, err := f.Tiles.WriteFiles(ctx, t, key, buf.Bytes())
	if err != nil {
		return nil, err
	}
	var ms []docker.Mount
	for _, s := range slots {
		ms = append(ms, docker.Mount{Volume: vol, Subpath: s.sub, Target: s.dst, ReadOnly: true})
	}
	return ms, nil
}

// slotPath is where tree entry rel lands in the archive under slot n; an
// entry that would leave the slot is refused.
func slotPath(n, rel string) (string, error) {
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", errs.Invalidf("files", "%s leaves the mount folder", rel)
	}
	return path.Join(n, rel), nil
}

// paramsVersion is a non-secret stamp of the params a template can see: the
// scopes it reads (its own blocks and every one a [x] ref may name), by count and newest updated_at. Masked never
// reads a secret value.
func (f *Flow) paramsVersion(ctx context.Context, e store.Environment, st store.Stack) (string, error) {
	var n int
	var newest time.Time
	tiers, err := f.Tiers.List(ctx, st.OrgID)
	if err != nil {
		return "", err
	}
	es, err := f.Envs.List(ctx, st.ID)
	if err != nil {
		return "", err
	}
	env, org, _ := ownScopes(e, st, tiers)
	scopes := []params.Scope{env, {Kind: "stack_pr", ID: st.ID}, {Kind: "org_pr", ID: st.OrgID}}
	if org != nil {
		scopes = append(scopes, *org)
	}
	for _, t := range tiers {
		scopes = append(scopes, params.Scope{Kind: "tier", ID: t.ID})
	}
	for _, x := range es {
		if x.Type != environment.Ephemeral {
			scopes = append(scopes, params.Scope{Kind: "env", ID: x.ID})
		}
	}
	for _, s := range scopes {
		ps, err := f.Params.Masked(ctx, s)
		if err != nil {
			return "", err
		}
		for _, p := range ps {
			n++
			if p.UpdatedAt.After(newest) {
				newest = p.UpdatedAt
			}
		}
	}
	// a lock or a tier rename moves what [x] refs may read without touching a row
	var locks []string
	for _, t := range tiers {
		locks = append(locks, t.Slug+":"+strconv.FormatBool(t.Locked))
	}
	for _, x := range es {
		if x.Type != environment.Ephemeral {
			locks = append(locks, x.Slug+":"+strconv.FormatBool(x.Locked))
		}
	}
	_, _, tierSlug := ownScopes(e, st, tiers)
	sum := sha256.Sum256([]byte(strings.Join(locks, ",") + "|" + tierSlug))
	return strconv.Itoa(n) + "-" + strconv.FormatInt(newest.UnixMicro(), 36) + "-" + hex.EncodeToString(sum[:3]), nil
}
