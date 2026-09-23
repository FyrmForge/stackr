package service

import (
	"context"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/config/runpolicy"
	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/placement"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/storagetiles"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// invalid names the request key the caller can fix. Every refusal in this
// file names one, because "400 bad request" on a form with forty fields is
// not an answer.
func invalid(field, msg string) error { return svcerr.Invalid{Field: field, Msg: msg} }

// splitLines is the tile-column convention: one item per line, blanks
// dropped.
func splitLines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Validate is the union of every rule the two surfaces used to apply
// separately, applied to a finished tile row. It is the widest set, not the
// intersection: where the panel silently coerced a bad value to a default and
// the API refused it, the refusal wins (SP1 — a coerced value is a write the
// user did not ask for, and it is the pattern behind most of the drift rows).
//
// It takes the whole row rather than a patch because a rule like "a git
// source needs a git_url" is about the tile the write leaves behind, not
// about which keys the request happened to carry.
//
// Rules needing another row (storage attachments, the connector's org) do
// their own lookups; that is the point of being a service rather than a
// helper.
func (s *TileService) Validate(ctx context.Context, t *repo.Tile) error {
	if t == nil {
		return svcerr.ErrNotFound
	}
	pol, runnable := runpolicy.For(t.Kind)

	if err := s.validateSource(ctx, t, pol, runnable); err != nil {
		return err
	}
	if err := validateVolume(t); err != nil {
		return err
	}
	if err := validateLimits(t); err != nil {
		return err
	}
	if err := validateLists(t); err != nil {
		return err
	}
	if err := s.validateStorage(ctx, t); err != nil {
		return err
	}
	if err := validateRunKind(t, pol, runnable); err != nil {
		return err
	}
	return s.validatePlacement(ctx, t)
}

// validateSource covers where the artifact comes from and who may fetch it.
func (s *TileService) validateSource(ctx context.Context, t *repo.Tile, pol runpolicy.Policy, runnable bool) error {
	if !runnable {
		return nil
	}
	switch t.SourceType {
	case "image":
		// Blanking a cron's image used to slip through and strand a job that
		// looked configured and could not run.
		if t.ImageRef == "" {
			return invalid("image", "an image source needs an image (set git_url to switch to a git build)")
		}
	case "git":
		if t.GitURL == "" {
			return invalid("git_url", "a git source needs a git_url")
		}
		if !repo.ValidGitURL(t.GitURL) {
			return invalid("git_url", "use a GitHub URL: https://github.com/owner/repo or git@github.com:owner/repo")
		}
	}
	// A connector grants credentials, so only one from the tile's own org is
	// accepted. The panel silently blanked a foreign connector, the API 400'd
	// and a config apply did not look at all; a silent blank is the worst of
	// the three because the next build fails for no stated reason.
	if t.ConnectorID != "" {
		cn, err := s.store.GetConnector(ctx, t.ConnectorID)
		if err != nil || cn == nil {
			return invalid("connector", "no such connector")
		}
		stack, err := s.store.GetStack(ctx, t.StackID)
		if err != nil || stack == nil || stack.OrgID != cn.OrgID {
			return invalid("connector", "that connector belongs to another organization")
		}
	}
	// Each knob belongs to one source; neither may be smuggled across by a
	// save that also switched the source.
	if t.UpdatePolicy == "" {
		t.UpdatePolicy = "off" // canonical form of the enum's default
	}
	if t.UpdatePolicy != "off" {
		if t.UpdatePolicy != "notify" && t.UpdatePolicy != "auto" {
			return invalid("update_policy", "update_policy must be off, notify or auto")
		}
		if t.SourceType != "image" {
			return invalid("update_policy", "update_policy watches an image source")
		}
	}
	if t.WaitForCI && t.SourceType != "git" {
		return invalid("wait_for_ci", "wait_for_ci needs a git-built source")
	}
	if !pol.AllowsIngress && t.ContainerPort != 0 {
		return invalid("port", "a "+t.Kind+" has no endpoint; port does not apply")
	}
	return nil
}

// validateLimits covers every number that used to be clamped on one surface
// and refused on the other.
// validateVolume covers the two columns only a volume tile uses. The name
// lands in a docker bind string verbatim, so an unchecked one can name the
// node's root filesystem; only the API ever checked it, and only the API
// accepted the fields at all.
func validateVolume(t *repo.Tile) error {
	if !t.IsVolume() {
		return nil
	}
	if t.VolumeName != "" && !repo.ValidVolumeName(t.VolumeName) {
		return invalid("volume_name", "letters, digits, _ . - only")
	}
	if t.MaxSizeMB < 0 {
		return invalid("max_size_mb", "must not be negative")
	}
	return nil
}

func validateLimits(t *repo.Tile) error {
	if t.CPULimit < 0 {
		return invalid("limits.cpu", "cpu limit must not be negative")
	}
	if t.MemLimitMB < 0 {
		return invalid("limits.memory_mb", "memory limit must not be negative")
	}
	if t.ShmSizeMB < 0 {
		return invalid("shm_size_mb", "shm_size_mb must not be negative")
	}
	// Negative only. The panel used to clamp to [1, 1440] silently, and SP1
	// says a silent clamp becomes a refusal — but the *upper* bound was only
	// ever the panel's private opinion: neither the API nor a config file had
	// one. Turning it into a refusal here would reject a config file that
	// applies today, and only on two of the three surfaces, since a config
	// apply does not run this validator. One rule everywhere beats a tighter
	// rule in two places. If 24h should be a real cap it belongs in the
	// config schema too, which is a separate change.
	if t.TimeoutMinutes < 0 {
		return invalid("timeout_minutes", "timeout_minutes must not be negative")
	}
	if t.HealthcheckIntervalS < 0 || t.HealthcheckTimeoutS < 0 ||
		t.HealthcheckRetries < 0 || t.HealthcheckStartPeriodS < 0 {
		return invalid("healthcheck", "healthcheck knobs must not be negative")
	}
	if t.Replicas < 0 {
		return invalid("replicas", "replicas: a whole number, at least 1")
	}
	return nil
}

// validateLists covers the newline-separated columns whose lines each have a
// grammar. Every one of these was parsed on one surface and not the other.
func validateLists(t *repo.Tile) error {
	for _, l := range splitLines(t.Files) {
		if _, _, _, err := runtime.ParseFileMount(l); err != nil {
			return invalid("files", err.Error())
		}
	}
	for _, l := range splitLines(t.Devices) {
		if _, err := runtime.ParseDevice(l); err != nil {
			return invalid("devices", err.Error())
		}
	}
	for _, l := range splitLines(t.DependsOn) {
		if _, _, err := runtime.ParseDep(l); err != nil {
			return invalid("depends_on", err.Error())
		}
	}
	rp, err := runtime.NormalizeRestart(t.RestartPolicy)
	if err != nil {
		return invalid("restart", err.Error())
	}
	t.RestartPolicy = rp
	// Basic auth is a pair or nothing: a user with no password is an open
	// door wearing a lock, and a password with no user is dead weight the
	// route writer would hash and never use.
	if t.BasicAuthUser == "" {
		t.BasicAuthPassword = ""
	} else if t.BasicAuthPassword == "" {
		return invalid("basic_auth_password", "basic auth needs a password as well as a user")
	}
	return nil
}

// validateStorage resolves every attachment line to a real share and checks
// the tile may mount it. Only the API did this.
func (s *TileService) validateStorage(ctx context.Context, t *repo.Tile) error {
	if t.Storage != "" && t.IsVolume() {
		// A volume tile has no container of its own to mount anything into.
		// The config path refused everything but a service, which is too
		// narrow — a cron mounts a share to write into, and a managed
		// instance is exactly what ValidateAttach's local-backed rule below
		// is about — and the API and the panel checked nothing at all.
		return invalid("storage", "a volume tile has nothing to mount a share into")
	}
	for _, l := range splitLines(t.Storage) {
		slug, _, _, _, err := storagetiles.ParseAttachment(l)
		if err != nil {
			return invalid("storage", err.Error())
		}
		var st *repo.Storage
		if name := varref.OrgStorageRef(l); name != "" {
			stack, serr := s.store.GetStack(ctx, t.StackID)
			if serr != nil || stack == nil {
				return svcerr.ErrNotFound
			}
			st, serr = s.store.GetOrgStorageBySlug(ctx, stack.OrgID, name)
			if serr != nil || st == nil {
				return invalid("storage", "org share "+name+" not found")
			}
		} else if st, err = s.store.GetStorageBySlug(ctx, slug); err != nil || st == nil {
			return invalid("storage", "storage "+slug+" not found")
		}
		if err := storagetiles.ValidateAttach(st, t); err != nil {
			return invalid("storage", err.Error())
		}
	}
	return nil
}

// validateRunKind covers the keys that only apply to some kinds. The panel
// hand-switched on three kind literals and skipped runpolicy entirely, which
// is how a schedule ended up stored on a service.
func validateRunKind(t *repo.Tile, pol runpolicy.Policy, runnable bool) error {
	if pol.RequiresSchedule {
		// An unvalidated expression lands in the DB, LoadSchedules skips it,
		// and the job looks configured while never running.
		if err := jobs.ValidateCron(t.Cron); err != nil {
			return invalid("schedule", err.Error())
		}
	} else if strings.TrimSpace(t.Cron) != "" {
		return invalid("schedule", "schedule applies to cron tiles only")
	}
	if !pol.AllowsCommand && strings.TrimSpace(t.Command) != "" {
		return invalid("command", "command does not apply to a "+t.Kind)
	}
	if t.RunOnDeploy && t.Kind != "function" {
		return invalid("run_on_deploy", "run_on_deploy applies to function tiles only")
	}
	if t.Kind != "service" && runnable {
		// These are all docker service knobs; a run-to-completion kind has
		// no long-lived container for them to describe.
		switch {
		case t.User != "":
			return invalid("user", "user applies to service tiles only")
		case t.Privileged:
			return invalid("privileged", "privileged applies to service tiles only")
		case strings.TrimSpace(t.Devices) != "":
			return invalid("devices", "devices apply to service tiles only")
		case t.HealthcheckCmd != "":
			return invalid("healthcheck", "healthcheck applies to service tiles only")
		}
	}
	return nil
}

// validatePlacement is the one rule that reads the rest of the environment:
// two writers on one volume corrupt it, so a tile that holds one runs alone.
func (s *TileService) validatePlacement(ctx context.Context, t *repo.Tile) error {
	if t.Replicas > 1 && placement.IsPinned(ctx, s.store, t) {
		return invalid("replicas",
			"this tile holds a volume, so it can only run one replica: two writers on one volume corrupt it")
	}
	return nil
}
