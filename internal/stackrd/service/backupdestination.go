package service

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// BackupDestinationService owns the bucket a backup is written to: who may see
// one, who may make one, and what happens to the schedules pointed at one when
// it goes.
//
// A destination carries the credentials the archive is written with, so its
// visibility rule is the sensitive part, and it was written out three times —
// the admin page, the org page and the API each deciding for themselves which
// destinations to offer. The panel trimmed whitespace off the name and the API
// stored it raw, which mattered because the config file resolves a destination
// by name: a destination created over the API as "prod " could never be
// referenced from a file.
type BackupDestinationService struct {
	store repo.Store
	sched *scheduler.Service
}

func NewBackupDestinationService(store repo.Store, sched *scheduler.Service) *BackupDestinationService {
	return &BackupDestinationService{store: store, sched: sched}
}

// Viewer is who is asking. It is the caller's job to fill it in from the
// session or the key; the service does not look at HTTP.
type Viewer struct {
	// Admin sees every destination, which is what the admin page is.
	Admin bool
	// Orgs are the organizations this viewer belongs to.
	Orgs map[string]bool
}

func (v Viewer) inOrg(orgID string) bool { return v.Orgs[orgID] }

// Visible lists the destinations this viewer may pick from. One rule: an admin
// sees all; an unshared server-wide destination is nobody else's, because the
// admin has not handed its credentials out; an org's own are that org's.
func (s *BackupDestinationService) Visible(ctx context.Context, v Viewer) ([]repo.BackupDestination, error) {
	all, err := s.store.ListBackupDestinations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]repo.BackupDestination, 0, len(all))
	for i := range all {
		d := all[i]
		switch {
		case v.Admin:
		case d.Global() && !d.Shared:
			continue
		case !d.Global() && !v.inOrg(d.OrgID.String):
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// AtScope lists the destinations one page administers: an org's own for an org
// page, the server-wide ones for the admin page. Not the same question as
// Visible, which is "what may this tile be backed up to".
func (s *BackupDestinationService) AtScope(ctx context.Context, orgID string) ([]repo.BackupDestination, error) {
	all, err := s.store.ListBackupDestinations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]repo.BackupDestination, 0, len(all))
	for _, d := range all {
		if (orgID == "" && d.Global()) || (orgID != "" && d.OrgID.String == orgID) {
			out = append(out, d)
		}
	}
	return out, nil
}

// NewDestination is a destination as a caller asks for it.
type NewDestination struct {
	Name      string
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	// OrgID empty makes it server-wide, which only an admin may do; Shared
	// then decides whether every org may write into it.
	OrgID  string
	Shared bool
}

// Create verifies the bucket and stores the destination. Access is the
// caller's to check — an org write role, or admin for a server-wide one —
// because who may act in which org is not this service's question.
func (s *BackupDestinationService) Create(ctx context.Context, in NewDestination) (*repo.BackupDestination, error) {
	// Trimmed, on every path. The config file resolves a destination by name
	// and the API stored what it was given, so a trailing space made a
	// destination that no file could ever reference.
	d := &repo.BackupDestination{
		ID:        uuid.New().String(),
		Name:      strings.TrimSpace(in.Name),
		Endpoint:  strings.TrimSpace(in.Endpoint),
		Bucket:    strings.TrimSpace(in.Bucket),
		Region:    strings.TrimSpace(in.Region),
		AccessKey: in.AccessKey,
		SecretKey: in.SecretKey,
		CreatedAt: time.Now().UTC(),
	}
	if d.Name == "" || d.Endpoint == "" || d.Bucket == "" {
		return nil, invalid("", "name, endpoint and bucket are required")
	}
	if in.OrgID == "" {
		d.Shared = in.Shared
	} else {
		d.OrgID = sql.NullString{String: in.OrgID, Valid: true}
	}
	// Verified before it is stored: a destination that cannot be written to
	// is a backup that fails at 3am for a reason nobody can see.
	if err := backup.TestDestination(ctx, d); err != nil {
		return nil, invalid("", "could not write to that bucket: "+err.Error())
	}
	if err := s.store.CreateBackupDestination(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

// Delete removes a destination. Every schedule pointed at it goes too, by
// cascade; the archives already in the bucket are left alone.
func (s *BackupDestinationService) Delete(ctx context.Context, d *repo.BackupDestination) error {
	if d == nil {
		return svcerr.ErrNotFound
	}
	if err := s.store.DeleteBackupDestination(ctx, d.ID); err != nil {
		return err
	}
	// The cascade (001_initial.up.sql:52) leaves the scheduler holding cron
	// entries for rows that no longer exist, which then fire Start against
	// nothing, every tick, for ever.
	s.sched.ReloadBackups(ctx)
	return nil
}

// SetShared turns a server-wide destination's sharing on or off. Turning it
// off is refused while an org still writes to it: the schedules would keep
// their row and start failing at 3am.
func (s *BackupDestinationService) SetShared(ctx context.Context, d *repo.BackupDestination, shared bool) error {
	if d == nil {
		return svcerr.ErrNotFound
	}
	if !d.Global() {
		return invalid("", "sharing applies to server-wide destinations only")
	}
	if !shared && d.Shared {
		if users, _ := s.Users(ctx, d.ID); len(users) > 0 {
			return svcerr.Conflictf("still used by %s; move those schedules first", strings.Join(users, ", "))
		}
	}
	d.Shared = shared
	return s.store.UpdateBackupDestination(ctx, d)
}

// Users names the stacks whose tiles back up to this destination. One copy,
// where the admin page and the API each had the same twenty lines.
//
// Walks every backup row. Tens of rows; add a store query if an install ever
// grows big enough to notice.
func (s *BackupDestinationService) Users(ctx context.Context, destID string) ([]string, error) {
	bs, err := s.store.ListBackups(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for i := range bs {
		// A NULL tile is the panel's own backup; admins own both ends of it.
		if bs[i].DestinationID != destID || !bs[i].TileID.Valid {
			continue
		}
		t, err := s.store.GetTile(ctx, bs[i].TileID.String)
		if err != nil || t == nil {
			continue
		}
		st, err := s.store.GetStack(ctx, t.StackID)
		if err != nil || st == nil {
			continue
		}
		name := st.Slug + "/" + t.Slug
		if !seen[name] {
			seen[name], out = true, append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Resolve turns what a caller wrote — a destination id, or the reference form
// the config file uses (${{ org.backups.NAME }}) — into the destination, and
// refuses one the org may not write to.
//
// Not-found rather than invalid for anything that does not resolve: the
// destination is the tenant boundary on this path, and "that id exists but is
// not yours" and "that id does not exist" have to be one answer.
func (s *BackupDestinationService) Resolve(ctx context.Context, orgID, ref string) (*repo.BackupDestination, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, invalid("destination_id", "required")
	}
	if scope, name, ok := backup.ParseRef(ref); ok {
		d, err := backup.ResolveNamed(ctx, s.store, orgID, scope, name)
		if err != nil {
			return nil, svcerr.ErrNotFound
		}
		return d, nil
	}
	d, err := backup.ResolveDestination(ctx, s.store, orgID, ref)
	if err != nil {
		return nil, svcerr.ErrNotFound
	}
	return d, nil
}

// Get is one destination by id, with no visibility filter.
//
// Visible and AtScope are the filtered reads and stay the ones a listing
// uses; this answers a route that already named an id the gate has resolved
// tenancy for. Anything that lists destinations to a person wants Visible.
func (s *BackupDestinationService) Get(ctx context.Context, id string) (*repo.BackupDestination, error) {
	d, err := s.store.GetBackupDestination(ctx, id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, svcerr.ErrNotFound
	}
	return d, nil
}

// Save writes a destination row back. SetShared and Delete own the rules
// about what a destination may become; this is the field edit.
func (s *BackupDestinationService) Save(ctx context.Context, d *repo.BackupDestination) error {
	return s.store.UpdateBackupDestination(ctx, d)
}
