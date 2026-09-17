// Package settings holds the cascading defaults: server -> organization ->
// stack -> environment -> tile. A tile's explicit value always wins;
// zero/empty means "inherit from the level above"; the chain bottoms out in
// built-ins.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Settings is one level's overrides. Nil field = inherit.
type Settings struct {
	CronTimeoutMin       *int     `json:"cron_timeout_min,omitempty"`
	CPULimit             *float64 `json:"cpu_limit,omitempty"`
	MemLimitMB           *int     `json:"mem_limit_mb,omitempty"`
	RunRetentionDays     *int     `json:"run_retention_days,omitempty"`
	MetricRetentionHours *int     `json:"metric_retention_hours,omitempty"`
	// Protect puts basic auth in front of every URL of every tile below this
	// level. User and password travel as a pair: the nearest level that sets
	// either supplies both (docs/plans/48-install-domains-and-basic-auth.md).
	// The password is plain text or a ${{ }} reference.
	Protect         *bool   `json:"protect,omitempty"`
	ProtectUser     *string `json:"protect_user,omitempty"`
	ProtectPassword *string `json:"protect_password,omitempty"`
	// NodeGroup constrains where tiles at this level may run, by the
	// stackr.group node label. Empty string is an explicit "anywhere" that
	// overrides a group set above (docs/plans/31-node-agent-open-questions.md,
	// node groups).
	NodeGroup *string `json:"node_group,omitempty"`
	// BuildNode is the swarm node id image builds run on, instance-wide and
	// only read at the server level. Empty means the manager, in a capped
	// builder per org (docs/plans/37-builds.md).
	BuildNode *string `json:"build_node,omitempty"`
	// How many of each queued kind run at once, instance-wide and only read
	// at the server level like BuildNode (docs/plans/33-workqueue.md).
	CronRunConcurrency       *int `json:"cron_run_concurrency,omitempty"`
	BackupRunConcurrency     *int `json:"backup_run_concurrency,omitempty"`
	BackupRestoreConcurrency *int `json:"backup_restore_concurrency,omitempty"`
	VolumeMoveConcurrency    *int `json:"volume_move_concurrency,omitempty"`
}

// Resolved is the fully-cascaded result, every field concrete.
type Resolved struct {
	CronTimeoutMin       int
	CPULimit             float64
	MemLimitMB           int
	RunRetentionDays     int
	MetricRetentionHours int
	// Protect gates every URL with basic auth. An empty user or password
	// while on locks the URL rather than opening it (proxy.WriteApp).
	Protect         bool
	ProtectUser     string
	ProtectPassword string
	// NodeGroup is the resolved placement group, "" = anywhere.
	NodeGroup string
	// BuildNode is the node id builds run on, "" = the manager.
	BuildNode string
	// Work queue limits per kind.
	CronRunConcurrency       int
	BackupRunConcurrency     int
	BackupRestoreConcurrency int
	VolumeMoveConcurrency    int
}

// Builtin defaults, the bottom of the cascade.
var builtin = Resolved{
	CronTimeoutMin:       30,
	CPULimit:             0, // unlimited
	MemLimitMB:           0, // unlimited
	RunRetentionDays:     30,
	MetricRetentionHours: 24,
	// Zero would stall a kind for ever, so every limit has a real default.
	CronRunConcurrency:       8,
	BackupRunConcurrency:     2,
	BackupRestoreConcurrency: 1,
	VolumeMoveConcurrency:    2,
}

// Parse decodes a settings JSON blob; bad or empty input is "no overrides".
func Parse(blob string) Settings {
	var s Settings
	_ = json.Unmarshal([]byte(blob), &s)
	return s
}

// JSON encodes for storage.
func (s Settings) JSON() string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Resolve applies override levels in order (server, org, stack, env); later
// levels win where set.
func Resolve(levels ...Settings) Resolved {
	r := builtin
	for _, s := range levels {
		if s.CronTimeoutMin != nil {
			r.CronTimeoutMin = *s.CronTimeoutMin
		}
		if s.CPULimit != nil {
			r.CPULimit = *s.CPULimit
		}
		if s.MemLimitMB != nil {
			r.MemLimitMB = *s.MemLimitMB
		}
		if s.RunRetentionDays != nil {
			r.RunRetentionDays = *s.RunRetentionDays
		}
		if s.MetricRetentionHours != nil {
			r.MetricRetentionHours = *s.MetricRetentionHours
		}
		if s.Protect != nil {
			r.Protect = *s.Protect
		}
		// One unit: a level setting only the user must not pair it with a
		// password from the level above.
		if s.ProtectUser != nil || s.ProtectPassword != nil {
			r.ProtectUser, r.ProtectPassword = deref(s.ProtectUser), deref(s.ProtectPassword)
		}
		if s.NodeGroup != nil {
			r.NodeGroup = *s.NodeGroup
		}
		if s.BuildNode != nil {
			r.BuildNode = *s.BuildNode
		}
		if s.CronRunConcurrency != nil {
			r.CronRunConcurrency = *s.CronRunConcurrency
		}
		if s.BackupRunConcurrency != nil {
			r.BackupRunConcurrency = *s.BackupRunConcurrency
		}
		if s.BackupRestoreConcurrency != nil {
			r.BackupRestoreConcurrency = *s.BackupRestoreConcurrency
		}
		if s.VolumeMoveConcurrency != nil {
			r.VolumeMoveConcurrency = *s.VolumeMoveConcurrency
		}
	}
	return r
}

// EffectiveLimits applies the cascade to a resource's explicit limits:
// zero means inherit the resolved default.
func (r Resolved) EffectiveLimits(cpu float64, memMB int) (float64, int) {
	if cpu == 0 {
		cpu = r.CPULimit
	}
	if memMB == 0 {
		memMB = r.MemLimitMB
	}
	return cpu, memMB
}

// EffectiveGroup applies the cascade to a tile's own node_group: empty
// inherits the level above, which bottoms out in "anywhere".
func (r Resolved) EffectiveGroup(group string) string {
	if group == "" {
		return r.NodeGroup
	}
	return group
}

// EffectiveTimeout applies the cascade to a cron timeout: <=0 inherits.
func (r Resolved) EffectiveTimeout(minutes int) int {
	if minutes <= 0 {
		return r.CronTimeoutMin
	}
	return minutes
}

// Merge folds submitted form values into an existing level's overrides. A key
// the form never submitted is left untouched, settings forms show a subset of
// the fields, and a partial form must not wipe the rest. A key submitted empty
// clears the override back to "inherit".
//
// CPULimit and MemLimitMB accept an explicit 0, which means "unlimited" and is
// how a lower level overrides a limit set above it. The timeout and retention
// fields have no meaningful zero, so a non-positive value clears them instead.
func Merge(s Settings, vals url.Values) Settings {
	num(vals, "cron_timeout_min", &s.CronTimeoutMin, false)
	numF(vals, "cpu_limit", &s.CPULimit, true)
	num(vals, "mem_limit_mb", &s.MemLimitMB, true)
	num(vals, "run_retention_days", &s.RunRetentionDays, false)
	num(vals, "metric_retention_hours", &s.MetricRetentionHours, false)
	num(vals, "cron_run_concurrency", &s.CronRunConcurrency, false)
	num(vals, "backup_run_concurrency", &s.BackupRunConcurrency, false)
	num(vals, "backup_restore_concurrency", &s.BackupRestoreConcurrency, false)
	num(vals, "volume_move_concurrency", &s.VolumeMoveConcurrency, false)
	// protect is a select with an inherit choice, so empty clears it.
	if v, ok := field(vals, "protect"); ok {
		if v == "" {
			s.Protect = nil
		} else {
			b := v == "1" || v == "true" || v == "on"
			s.Protect = &b
		}
	}
	str(vals, "protect_user", &s.ProtectUser)
	// The password is never rendered back into the form, so a blank one means
	// "leave it as it is", not "clear it". Clearing the user clears both: they
	// travel as a pair, and it is the only way to give them back to the level
	// above from a form that cannot show what is stored.
	if v, ok := field(vals, "protect_password"); ok && v != "" {
		s.ProtectPassword = &v
	}
	if s.ProtectUser == nil {
		s.ProtectPassword = nil
	}
	// node_group is the one string field, and its empty value is meaningful:
	// "any" is a real choice that has to beat a group set above, not a
	// request to inherit one. Submitting the key at all is the override.
	if v, ok := field(vals, "node_group"); ok {
		if v == "" || v == "any" {
			s.NodeGroup = nil
		} else {
			s.NodeGroup = &v
		}
	}
	// build_node: empty is "the manager", a real choice, so the key being
	// sent is the override, like node_group.
	if v, ok := field(vals, "build_node"); ok {
		if v == "" {
			s.BuildNode = nil
		} else {
			s.BuildNode = &v
		}
	}
	return s
}

// Check refuses a level that sets half of the basic auth pair. Resolve takes
// user and password as one unit, so a level with only one of them resolves
// the other to empty, and proxy.WriteApp locks every URL below it behind a
// password nobody knows. Caught at the save, where the operator can see it.
func (s Settings) Check() error {
	user, pass := s.ProtectUser != nil && *s.ProtectUser != "", s.ProtectPassword != nil && *s.ProtectPassword != ""
	if user == pass {
		return nil
	}
	if user {
		return errors.New("protection needs a password as well as a user")
	}
	return errors.New("protection needs a user as well as a password")
}

// field reports a form value and whether the key was submitted at all.
//
// The last value wins, which is what makes the hidden-input idiom work: an
// unchecked checkbox submits nothing, so forms pair it with a hidden "0"
// under the same name. Taking the first value instead would mean the hidden
// zero always beat the checked box.
func field(vals url.Values, key string) (string, bool) {
	v, ok := vals[key]
	if !ok || len(v) == 0 {
		return "", false
	}
	return strings.TrimSpace(v[len(v)-1]), true
}

// str folds a string field: submitted empty clears it back to inherit.
func str(vals url.Values, key string, dst **string) {
	v, ok := field(vals, key)
	if !ok {
		return
	}
	if v == "" {
		*dst = nil
		return
	}
	*dst = &v
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// num folds an int field. allowZero keeps an explicit 0 as a real override;
// otherwise a non-positive value clears it.
func num(vals url.Values, key string, dst **int, allowZero bool) {
	raw, ok := field(vals, key)
	if !ok {
		return
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 || (v == 0 && !allowZero) {
		*dst = nil
		return
	}
	*dst = &v
}

func numF(vals url.Values, key string, dst **float64, allowZero bool) {
	raw, ok := field(vals, key)
	if !ok {
		return
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || (v == 0 && !allowZero) {
		*dst = nil
		return
	}
	*dst = &v
}

// ForServer resolves the local server's defaults.
func ForServer(ctx context.Context, store repo.Store) Resolved {
	sv, err := store.GetServer(ctx, "local")
	if err != nil || sv == nil {
		return builtin
	}
	return Resolve(Parse(sv.Settings))
}

// ForOrg resolves server defaults + one org's overrides.
func ForOrg(ctx context.Context, store repo.Store, orgID string) Resolved {
	return Resolve(Chain(ctx, store, orgID, "", "")...)
}

// ForStack resolves server -> org -> stack.
func ForStack(ctx context.Context, store repo.Store, stackID string) Resolved {
	return Resolve(Chain(ctx, store, "", stackID, "")...)
}

// ForTile resolves the full chain for one tile: server -> org -> stack -> env.
func ForTile(ctx context.Context, store repo.Store, t *repo.Tile) Resolved {
	return Resolve(Chain(ctx, store, "", t.StackID, t.EnvironmentID)...)
}

// TryTile is ForTile for callers that must tell a missing level from an
// unreadable one, which is everything reading Protect: a swallowed error
// resolves to "not protected" and publishes the URL.
func TryTile(ctx context.Context, store repo.Store, t *repo.Tile) (Resolved, error) {
	chain, err := TryChain(ctx, store, "", t.StackID, t.EnvironmentID)
	if err != nil {
		return Resolved{}, err
	}
	return Resolve(chain...), nil
}

// Level is one rung of the cascade, named so a settings page can say where an
// inherited value came from.
type Level struct {
	Kind     string // server | org | stack | env
	Name     string
	Settings Settings
}

// Levels walks the chain down to the deepest id given and returns every rung,
// server first. The org is derived from the stack, and the stack from the
// environment, so a caller only ever names the level it is looking at.
//
// A store error is returned, not swallowed. A level that fails to load looks
// exactly like a level that sets nothing, and for Protect that reads as "not
// protected": the caller has to know the difference (TryTile).
func Levels(ctx context.Context, store repo.Store, orgID, stackID, envID string) ([]Level, error) {
	var env *repo.Environment
	if envID != "" {
		var err error
		if env, err = store.GetEnvironment(ctx, envID); err != nil {
			return nil, err
		}
		if env != nil {
			stackID = env.StackID
		}
	}
	var stack *repo.Stack
	if stackID != "" {
		var err error
		if stack, err = store.GetStack(ctx, stackID); err != nil {
			return nil, err
		}
		if stack != nil {
			orgID = stack.OrgID
		}
	}
	out := []Level{}
	sv, err := store.GetServer(ctx, "local")
	if err != nil {
		return nil, err
	}
	if sv != nil {
		out = append(out, Level{Kind: "server", Name: "this server", Settings: Parse(sv.Settings)})
	}
	if orgID != "" {
		o, err := store.GetOrg(ctx, orgID)
		if err != nil {
			return nil, err
		}
		if o != nil {
			out = append(out, Level{Kind: "org", Name: o.Name, Settings: Parse(o.Settings)})
		}
	}
	if stack != nil {
		out = append(out, Level{Kind: "stack", Name: stack.Name, Settings: Parse(stack.Settings)})
	}
	if env != nil {
		out = append(out, Level{Kind: "env", Name: env.Name, Settings: Parse(env.Settings)})
	}
	return out, nil
}

// Above resolves the levels over the one of this kind, which is what a
// settings page shows as the inherited value.
func Above(levels []Level, kind string) Resolved {
	var chain []Settings
	for _, l := range levels {
		if l.Kind == kind {
			break
		}
		chain = append(chain, l.Settings)
	}
	return Resolve(chain...)
}

// Chain is Levels' settings alone, ready for Resolve. A store error means no
// overrides, which is the right answer for the limits and placement fields:
// falling back to the built-in default is better than failing the caller.
// Anything that must not fail open goes through TryChain instead.
func Chain(ctx context.Context, store repo.Store, orgID, stackID, envID string) []Settings {
	out, _ := TryChain(ctx, store, orgID, stackID, envID)
	return out
}

// TryChain is Chain with the store error kept.
func TryChain(ctx context.Context, store repo.Store, orgID, stackID, envID string) ([]Settings, error) {
	ls, err := Levels(ctx, store, orgID, stackID, envID)
	if err != nil {
		return nil, err
	}
	out := make([]Settings, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Settings)
	}
	return out, nil
}
