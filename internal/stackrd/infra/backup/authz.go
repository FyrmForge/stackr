package backup

import (
	"context"
	"fmt"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ResolveDestination loads a destination and refuses it unless the caller's
// org may use it: its own, or an admin-global one the admin has marked shared.
//
// This is the check a route-level "tile → org → membership" pass does not
// make. The tile genuinely belongs to the caller's org; the destination is
// what crosses the tenant boundary, and it carries the credentials the archive
// is written with. Both the web handlers and the v1 API go through here.
func ResolveDestination(ctx context.Context, store repo.Store, orgID, destID string) (*repo.BackupDestination, error) {
	d, err := store.GetBackupDestination(ctx, destID)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("destination not found")
	}
	if d.VisibleTo(orgID) {
		return d, nil
	}
	return nil, fmt.Errorf("destination not found") // not "forbidden": ids must not leak across orgs
}

// OrgOf returns the org that owns a tile.
func OrgOf(ctx context.Context, store repo.Store, t *repo.Tile) (string, error) {
	st, err := store.GetStack(ctx, t.StackID)
	if err != nil || st == nil {
		return "", fmt.Errorf("stack not found")
	}
	return st.OrgID, nil
}

// ValidMode reports whether m is one of the three container modes.
func ValidMode(m string) bool {
	return m == repo.ModePause || m == repo.ModeStop || m == repo.ModeLive
}

// Validate checks a backup config before it is saved: known kind, known mode,
// parseable schedule, sane retention.
func Validate(b *repo.Backup) error {
	switch b.Kind {
	case repo.BackupDump, repo.BackupVolume, repo.BackupStackr:
	default:
		return fmt.Errorf("unknown backup kind %q", b.Kind)
	}
	if b.Kind == repo.BackupVolume && !ValidMode(b.ContainerMode) {
		return fmt.Errorf("container mode must be pause, stop, or live")
	}
	if b.Cron == "" {
		return fmt.Errorf("a schedule is required")
	}
	if err := jobs.ValidateCron(b.Schedule()); err != nil {
		return fmt.Errorf("invalid schedule: %w", err)
	}
	if b.KeepLatest < 0 {
		return fmt.Errorf("keep count cannot be negative")
	}
	return nil
}

// ResolveNamed finds the destination a config file's `dest:` reference points
// at. scope is "org" for ${{ org.backups.NAME }} and "stackr" for
// ${{ stackr.backups.NAME }}.
//
// Names, not ids: the file is in git and shared across environments, and a
// destination id means nothing to a person reading it. Two destinations with
// the same name in reach is an error rather than a coin flip.
func ResolveNamed(ctx context.Context, store repo.Store, orgID, scope, name string) (*repo.BackupDestination, error) {
	all, err := store.ListBackupDestinations(ctx)
	if err != nil {
		return nil, err
	}
	var match *repo.BackupDestination
	for i := range all {
		d := &all[i]
		if d.Name != name {
			continue
		}
		switch scope {
		case "stackr":
			// Admin-owned, and only once the admin has shared it: the
			// destination carries the bucket credentials.
			if !d.Global() || !d.Shared {
				continue
			}
		default:
			if d.Global() || d.OrgID.String != orgID {
				continue
			}
		}
		if match != nil {
			return nil, fmt.Errorf("more than one %s backup destination named %q", scope, name)
		}
		match = d
	}
	if match == nil {
		return nil, fmt.Errorf("no %s backup destination named %q", scope, name)
	}
	return match, nil
}

// ParseRef reads a destination reference, ${{ org.backups.NAME }} or
// ${{ stackr.backups.NAME }}, into its scope and name. ok is false for anything
// that is not a reference, which the caller then treats as a destination id.
func ParseRef(s string) (scope, name string, ok bool) {
	body := strings.TrimSpace(s)
	body, found := strings.CutPrefix(body, "${{")
	if !found {
		return "", "", false
	}
	body, found = strings.CutSuffix(body, "}}")
	if !found {
		return "", "", false
	}
	parts := strings.Split(strings.TrimSpace(body), ".")
	if len(parts) != 3 || parts[1] != "backups" {
		return "", "", false
	}
	if parts[0] != "org" && parts[0] != "stackr" {
		return "", "", false
	}
	return parts[0], parts[2], true
}
