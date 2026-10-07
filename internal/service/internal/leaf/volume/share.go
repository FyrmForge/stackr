package volume

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A share is an org's NFS or SMB export. Its row is a recipe, not a volume:
// a tile line "share:<slug>/<sub>:/abs[:ro]" becomes one Docker volume per
// sub path (EnsureShare), driver local with nfs or cifs opts. Share volumes
// have no row in volumes, so no backup, orphan job or retention touches
// them; they carry the label stackr.share=<share id>. Deleting the share
// removes its volumes first (refused while a container holds one), and
// SweepShares drops the ones no tile row names once a tile goes or a share
// is edited.

// ShareLabel marks a Docker volume made for a share.
const ShareLabel = "stackr.share"

const (
	NFS = "nfs"
	SMB = "smb"
)

var (
	nfsSource = regexp.MustCompile(`^([^:/\s,]+):(/\S*)$`)
	smbSource = regexp.MustCompile(`^//([^/\s,]+)(/\S+)$`)
	orgRef    = regexp.MustCompile(`^\$\{\{\s*org\.params\.[a-z0-9_]+\.[a-z0-9_]+\s*\}\}$`)
	plainUser = regexp.MustCompile(`^[^\s,]+$`)
)

// ShareSpec is what a caller says about a share. User and PasswordRef are
// ${{ org.params }} refs (a plain User is fine), never a password.
type ShareSpec struct {
	Slug, Kind, Source, Options, User, PasswordRef string
}

// WithShares gives the leaf the shares table; call it where the leaf is built.
func (l *Leaf) WithShares(s store.ShareStore) *Leaf {
	l.shares = s
	return l
}

// CheckShare is every rule a share row obeys, pure.
func CheckShare(s ShareSpec) error {
	if !slug.Valid(s.Slug) {
		return errs.Invalidf("slug", "%q: a share name is lower-case letters, digits and single hyphens", s.Slug)
	}
	switch s.Kind {
	case NFS:
		if !nfsSource.MatchString(s.Source) {
			return errs.Invalidf("source", "%q: an NFS source is host:/exported/path", s.Source)
		}
		if s.User != "" || s.PasswordRef != "" {
			return errs.Invalidf("user", "NFS has no credentials; leave user and password empty")
		}
	case SMB:
		if !smbSource.MatchString(s.Source) {
			return errs.Invalidf("source", "%q: an SMB source is //host/share[/path]", s.Source)
		}
		if (s.User == "") != (s.PasswordRef == "") {
			return errs.Invalidf("user", "user and password go together")
		}
		if s.User != "" && !plainUser.MatchString(s.User) && !orgRef.MatchString(s.User) {
			return errs.Invalidf("user", "a user is plain text or an ${{ org.params.<collection>.<name> }} ref")
		}
		if s.PasswordRef != "" && !orgRef.MatchString(s.PasswordRef) {
			return errs.Invalidf("password", "a password is an ${{ org.params.<collection>.<name> }} ref, never the value")
		}
	default:
		return errs.Invalidf("kind", "%q: a share is nfs or smb", s.Kind)
	}
	if strings.ContainsAny(s.Options, " \t\n") {
		return errs.Invalidf("options", "comma-separated mount options, no spaces")
	}
	for _, o := range strings.Split(s.Options, ",") {
		k, _, _ := strings.Cut(o, "=")
		if slices.Contains([]string{"type", "device", "addr", "username", "password", "user", "pass"}, k) {
			return errs.Invalidf("options", "%s is set by stackr", k)
		}
	}
	return nil
}

func (l *Leaf) Shares(ctx context.Context, orgID string) ([]store.Share, error) {
	return l.shares.ListByOrg(ctx, orgID)
}

// ShareOf is one of the org's shares by slug or id; another org's is not
// found.
func (l *Leaf) ShareOf(ctx context.Context, orgID, ref string) (store.Share, error) {
	s, err := l.shares.GetBySlug(ctx, orgID, ref)
	if errors.Is(err, errs.ErrNotFound) {
		s, err = l.shares.Get(ctx, ref)
		if err == nil && s.OrgID != orgID {
			return store.Share{}, errs.ErrNotFound
		}
	}
	return s, err
}

// ShareBySlug is one of the org's shares by slug alone: what a tile line
// names, so an id never stands in for it.
func (l *Leaf) ShareBySlug(ctx context.Context, orgID, slug string) (store.Share, error) {
	return l.shares.GetBySlug(ctx, orgID, slug)
}

// CreateShare adds a share; the slug is the org's own.
func (l *Leaf) CreateShare(ctx context.Context, orgID string, sp ShareSpec) (store.Share, error) {
	if err := CheckShare(sp); err != nil {
		return store.Share{}, err
	}
	if _, err := l.shares.GetBySlug(ctx, orgID, sp.Slug); err == nil {
		return store.Share{}, errs.Conflictf("this organization already has a share %q", sp.Slug)
	} else if !errors.Is(err, errs.ErrNotFound) {
		return store.Share{}, err
	}
	s := store.Share{
		ID:          uuid.NewString(),
		OrgID:       orgID,
		Slug:        sp.Slug,
		Kind:        sp.Kind,
		Source:      sp.Source,
		Options:     sp.Options,
		User:        sp.User,
		PasswordRef: sp.PasswordRef,
		CreatedAt:   time.Now().UTC(),
	}
	return s, l.shares.Create(ctx, s)
}

// UpdateShare rewrites a share's fields; its slug and id stay. Volumes made
// from the old recipe stay until a SweepShares drops them (the caller runs
// one); the next deploy mounts new ones (the volume name hashes the recipe).
func (l *Leaf) UpdateShare(ctx context.Context, s store.Share, sp ShareSpec) (store.Share, error) {
	sp.Slug = s.Slug
	if err := CheckShare(sp); err != nil {
		return s, err
	}
	s.Kind, s.Source, s.Options, s.User, s.PasswordRef = sp.Kind, sp.Source, sp.Options, sp.User, sp.PasswordRef
	return s, l.shares.Update(ctx, s)
}

// DeleteShare removes the share's Docker volumes, then the row; mountedBy
// are the tiles whose lines name it. A volume a container still holds
// refuses the delete, row kept.
func (l *Leaf) DeleteShare(ctx context.Context, s store.Share, mountedBy []string) error {
	if len(mountedBy) > 0 {
		return errs.Conflictf("detach the share from %s before deleting it", strings.Join(mountedBy, ", "))
	}
	vs, err := l.docker.ListVolumes(ctx, map[string]string{ShareLabel: s.ID})
	if err != nil {
		return err
	}
	for _, v := range vs {
		if err := l.docker.RemoveVolume(ctx, v.Name); docker.IsVolumeInUse(err) {
			return errs.Conflictf("share %s: a container still holds %s; remove it first", s.Slug, v.Name)
		} else if err != nil {
			return err
		}
	}
	return l.shares.Delete(ctx, s.ID)
}

// ShareUse is one tile line's claim on a share volume: "share:<Share>/<Sub>".
type ShareUse struct{ Share, Sub string }

// SweepShares removes the org's share volumes no tile row names. uses is
// every share line of every tile row the org still has: the keep set comes
// from rows, never from what a container holds right now, because a deploy's
// fresh volume sits unheld while its image pulls and Docker would recreate a
// removed one as a plain local dir. A volume a container still holds stays,
// its name is returned; any other Docker error is returned as one.
func (l *Leaf) SweepShares(ctx context.Context, orgID string, uses []ShareUse) (held []string, err error) {
	if l.shares == nil {
		return nil, nil
	}
	ss, err := l.shares.ListByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for _, s := range ss {
		keep := map[string]bool{}
		for _, u := range uses {
			if u.Share == s.Slug {
				keep[ShareVolumeName(s, u.Sub)] = true
			}
		}
		vs, err := l.docker.ListVolumes(ctx, map[string]string{ShareLabel: s.ID})
		if err != nil {
			return held, err
		}
		for _, v := range vs {
			if keep[v.Name] {
				continue
			}
			switch err := l.docker.RemoveVolume(ctx, v.Name); {
			case docker.IsVolumeInUse(err):
				held = append(held, v.Name)
			case err != nil:
				return held, err
			}
		}
	}
	return held, nil
}

// ShareOpts are the Docker local-driver options for sub inside s. user and
// password are expanded values ("" for none or NFS).
func ShareOpts(s store.Share, sub, user, password string) (map[string]string, error) {
	if sub != "" && (path.IsAbs(sub) || path.Clean(sub) != sub || sub == ".." || strings.HasPrefix(sub, "../")) {
		return nil, errs.Invalidf("volumes", "share %s: %q is not a plain sub path", s.Slug, sub)
	}
	if strings.ContainsAny(user+password, ", \n") {
		return nil, errs.Conflictf("share %s: a user or password with a comma or space cannot go in a mount option", s.Slug)
	}
	var host, root, o string
	switch s.Kind {
	case NFS:
		m := nfsSource.FindStringSubmatch(s.Source)
		if m == nil {
			return nil, errs.Conflictf("share %s: %q is not host:/path", s.Slug, s.Source)
		}
		host, root, o = m[1], m[2], "addr="+m[1]
	case SMB:
		m := smbSource.FindStringSubmatch(s.Source)
		if m == nil {
			return nil, errs.Conflictf("share %s: %q is not //host/share", s.Slug, s.Source)
		}
		host, root, o = m[1], m[2], "addr="+m[1]
		if user != "" {
			o += ",username=" + user + ",password=" + password
		}
	default:
		return nil, errs.Conflictf("share %s: kind %q", s.Slug, s.Kind)
	}
	if s.Options != "" {
		o += "," + s.Options
	}
	dev := path.Join(root, sub)
	if s.Kind == NFS {
		return map[string]string{"type": "nfs", "o": o, "device": ":" + dev}, nil
	}
	return map[string]string{"type": "cifs", "o": o, "device": "//" + host + dev}, nil
}

// ShareVolumeName is stackr-share-<share id>-<hash>; the hash covers sub and
// the share's recipe as stored (kind, source, options, user and password
// REFS), so a share edited later mounts a fresh volume (Docker keeps an
// existing volume's opts, whatever CreateVolume is given) and the name is
// computable from rows alone, which SweepShares needs. Never the expanded
// secret.
// ponytail: a rotated secret value keeps the name, so the old mount (which
// holds the old password) lives until its container is recreated; a new name
// per rotation would need the value in the hash, and so in `docker volume ls`.
func ShareVolumeName(s store.Share, sub string) string {
	h := sha256.New()
	for _, f := range []string{sub, s.Kind, s.Source, s.Options, s.User, s.PasswordRef} {
		_, _ = fmt.Fprint(h, f, "\x00")
	}
	return "stackr-share-" + s.ID + "-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// EnsureShare makes the Docker volume for sub inside s and returns its name.
func (l *Leaf) EnsureShare(ctx context.Context, s store.Share, sub, user, password string) (string, error) {
	opts, err := ShareOpts(s, sub, user, password)
	if err != nil {
		return "", err
	}
	name := ShareVolumeName(s, sub)
	return name, l.docker.CreateVolume(ctx, name, "local", opts, map[string]string{ShareLabel: s.ID})
}
