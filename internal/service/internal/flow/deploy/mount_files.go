package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/git"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// fileBinds writes the tile's files: lines under <DataDir>/files/<tile>/
// <key>/<n>, key = commit-<files hash>-<params version> and returns them as read-only Docker binds. The content is the
// config commit the env's release pins, read from the stack's config clone
// (promote's plan cloned it). A :template line goes whole through the
// resolver, secrets included; any other is copied byte for byte. Called only
// when the tile has files: lines.
// The folder is written once and never rewritten: an edit of the lines, a
// param or the commit makes a new key, and pruneFiles drops the old folder.
// The key holds no secret (the path shows in docker inspect): the params
// version is the count and newest updated_at of the params the tile sees.
func (f *Flow) fileBinds(
	ctx context.Context,
	t store.Tile,
	e store.Environment,
	st store.Stack,
	rr *params.Resolver,
) ([]string, error) {
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
	ver, err := f.paramsVersion(ctx, e, st)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(strings.Join(tile.Lines(t.Files), "\n")))
	base := filepath.Join(f.DataDir, "files", t.ID, commit+"-"+hex.EncodeToString(sum[:4])+"-"+ver)
	_, statErr := os.Stat(base)
	written := statErr == nil
	tmp := base + ".part"
	if !written {
		if !repo.Has(ctx, commit) {
			return nil, errs.Conflictf("%s: files: the config repo clone lacks %.12s; run a promote to fetch it", t.Slug, commit)
		}
		// The data dir folder is the only gate on a template's secrets.
		if err := os.MkdirAll(filepath.Join(f.DataDir, "files"), 0o700); err != nil {
			return nil, err
		}
		if err := os.RemoveAll(tmp); err != nil {
			return nil, err
		}
	}

	var binds []string
	for i, l := range tile.Lines(t.Files) {
		src, dst, err := tile.ParseFileMount(l)
		if err != nil {
			return nil, errs.Invalidf("files", "%s", err.Error())
		}
		tmpl := strings.HasSuffix(l, ":template")
		n := strconv.Itoa(i)
		var isDir bool
		if written {
			// ponytail: a folder holding one file named like its own
			// folder reads as a file line; mark the slot if that bites.
			fi, err := os.Stat(filepath.Join(base, n, path.Base(src)))
			isDir = err != nil || fi.IsDir()
		} else if isDir, err = repo.IsDir(ctx, commit, src); err != nil {
			return nil, err
		}
		host := filepath.Join(base, n)
		if !isDir {
			host = filepath.Join(host, path.Base(src))
		}
		binds = append(binds, host+":"+dst+":ro")
		if written {
			continue
		}
		names := []string{src}
		if isDir {
			if names, err = repo.ListTree(ctx, commit, src); err != nil {
				return nil, err
			}
		}
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
			p, err := slotPath(tmp, n, rel)
			if err != nil {
				return nil, err
			}
			if err := putFile(p, b); err != nil {
				return nil, err
			}
		}
	}
	if !written {
		// Whole commit or nothing: a killed write never reads as done.
		if err := syncDir(tmp); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, base); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if err := syncDir(filepath.Dir(base)); err != nil {
			return nil, err
		}
	}
	return binds, nil
}

// pruneFiles removes the tile's written commit folders that no container
// mount in mounts ("src -> dst", docker.Detail.Mounts) points into.
func (f *Flow) pruneFiles(t store.Tile, mounts []string) error {
	dir := filepath.Join(f.DataDir, "files", t.ID)
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, en := range ents {
		p := filepath.Join(dir, en.Name()) + string(filepath.Separator)
		used := false
		for _, m := range mounts {
			if strings.HasPrefix(m, p) {
				used = true
				break
			}
		}
		if !used {
			if err := os.RemoveAll(filepath.Join(dir, en.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// putFile writes b at p, folders made. The files sit under a 0700 data dir
// folder; the container's user, unknown here, must be able to read them.
func putFile(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { //nolint:gosec // see above
		return err
	}
	fh, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // see above
	if err != nil {
		return err
	}
	if _, err := fh.Write(b); err != nil {
		_ = fh.Close()
		return err
	}
	if err := fh.Sync(); err != nil {
		_ = fh.Close()
		return err
	}
	return fh.Close()
}

// slotPath is where tree entry rel lands under slot n of the folder at tmp;
// an entry that would leave the slot is refused.
func slotPath(tmp, n, rel string) (string, error) {
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", errs.Invalidf("files", "%s leaves the mount folder", rel)
	}
	return filepath.Join(tmp, n, filepath.FromSlash(rel)), nil
}

// syncDir flushes a folder's entries, so a rename survives a crash.
func syncDir(p string) error {
	d, err := os.Open(p) //nolint:gosec // our own data dir
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
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
