package repo

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// Org groups stacks. Grouping only for now, membership/roles come later.
type Org struct {
	ID   string `db:"id"`
	Name string `db:"name"`
	Slug string `db:"slug"`
	// AvatarPath is the org's logo in FileStorage, "" = fall back to initials.
	AvatarPath string    `db:"avatar_path"`
	CreatedAt  time.Time `db:"created_at"`

	// Org-level config-as-code binding (§6), mirroring the stack fields.
	ConfigConnectorID string `db:"config_connector_id"`
	ConfigRepo        string `db:"config_repo"`
	ConfigBranch      string `db:"config_branch"`
	ConfigPath        string `db:"config_path"`

	// SetupDoneAt closes the onboarding wizard for this org. Nil means the
	// wizard is still the right place to send its owner; set means every
	// /setup step redirects to the settings tab that owns the same thing.
	SetupDoneAt *time.Time `db:"setup_done_at"`

	// SetupMode is the wizard branch: "config" (a file declares the org) or
	// "ui" (filled in step by step). Read it through SetupConfigBranch, never
	// directly, an org created before the wizard had branches has neither.
	SetupMode string `db:"setup_mode"`

	// UIEditsDefault is the org file's defaults.ui_edits, inherited by every
	// stack that does not declare its own. Blank = block. Read it through
	// Stack.UIEdits, never directly.
	UIEditsDefault string `db:"ui_edits"`

	// EnvColors is a json object of env slug to colour, the org-wide default
	// for every environment with that slug. See envcolor.
	EnvColors string `db:"env_colors"`

	// Settings is this org's level of the defaults cascade (server -> org ->
	// stack -> env -> tile). JSON overrides, see internal/stackrd/config/settings.
	Settings string `db:"settings"`
}

// ConfigManaged reports whether the org's structure is owned by a config file.
func (o *Org) ConfigManaged() bool { return o.ConfigConnectorID != "" && o.ConfigRepo != "" }

// SetupConfigBranch reports whether the wizard is walking this org down the
// config-as-code branch, which has no name step and no domain step.
func (o *Org) SetupConfigBranch() bool { return o.SetupMode == "config" }

// OrgMember links a user to an org with a role.
// Roles: "owner" (manage org + members), "member" (full content access),
// "viewer" (read-only). Server admins (User.Role == "admin") bypass all of it.
type OrgMember struct {
	OrgID     string    `db:"org_id"`
	UserID    string    `db:"user_id"`
	Role      string    `db:"role"`
	CreatedAt time.Time `db:"created_at"`

	// Joined for display (ListOrgMembers).
	Email      string `db:"email"`
	Name       string `db:"name"`
	AvatarPath string `db:"avatar_path"`
}

// Invite is a shareable join link for an org.
type Invite struct {
	ID        string       `db:"id"` // the token in the invite URL
	OrgID     string       `db:"org_id"`
	Email     string       `db:"email"` // optional restriction, "" = anyone with the link
	Role      string       `db:"role"`
	CreatedBy string       `db:"created_by"`
	CreatedAt time.Time    `db:"created_at"`
	ExpiresAt time.Time    `db:"expires_at"`
	UsedAt    sql.NullTime `db:"used_at"`

	// MailFailed is set for the request that created the invite when sending
	// its email did not work, so the page can say "copy the link instead". Not
	// stored: the next page load has the link either way.
	MailFailed bool `db:"-"`
}

// Connector is a per-org integration (github, gitlab, slack, ...). Config is
// provider-specific JSON; capabilities are decided by provider in code.
type Connector struct {
	ID        string    `db:"id"`
	OrgID     string    `db:"org_id"`
	Provider  string    `db:"provider"`
	Name      string    `db:"name"`
	Config    string    `db:"config"`
	CreatedAt time.Time `db:"created_at"`
}

// Stack groups environments (formerly "project").
type Stack struct {
	ID      string `db:"id"`
	OrgID   string `db:"org_id"`
	OrgSlug string `db:"-"` // filled by handlers for URL building

	Name        string `db:"name"`
	Slug        string `db:"slug"`
	Description string `db:"description"`
	Settings    string `db:"settings"` // JSON overrides, see internal/settings

	// Config-as-code binding: repo holding stackr-compose.yml. Empty
	// connector = UI-managed stack.
	ConfigConnectorID string `db:"config_connector_id"`
	ConfigRepo        string `db:"config_repo"`   // "owner/name"
	ConfigBranch      string `db:"config_branch"` // "" = repo default branch
	ConfigPath        string `db:"config_path"`   // "" = stackconf.DefaultPath

	// OrgDeclared marks a stack the org config file declares under stacks:.
	// The org file owns such a stack's name (the instantiator names the
	// instance, Helm-style), so the stack file's stack: field cannot rename
	// it. Refreshed on every org plan and set at org apply.
	OrgDeclared bool `db:"org_declared"`

	// UIEditsMode is what ApplyPlan resolved from the config files (the stack
	// file, then the org file's defaults:). Blank until a config-managed stack
	// has applied once. Read it through UIEdits().
	UIEditsMode string `db:"ui_edits"`

	CreatedAt time.Time `db:"created_at"`
}

// How the panel treats an edit to a field the config file owns.
const (
	UIEditsBlock = "block" // refuse it: the file is the source of truth
	UIEditsStage = "stage" // hold it in the env's pending set for review
)

// UIEdits is the stack's edit mode, block unless the config says otherwise.
// Only meaningful on a config-managed stack, a UI-managed one owns its tiles
// outright and always stages.
func (s *Stack) UIEdits() string {
	if s.UIEditsMode == UIEditsStage {
		return UIEditsStage
	}
	return UIEditsBlock
}

// ConfigManaged reports whether the stack is bound to a config repo.
func (s *Stack) ConfigManaged() bool { return s.ConfigConnectorID != "" && s.ConfigRepo != "" }

// ConfigPlan is one stored plan run for a config-managed stack.
type ConfigPlan struct {
	ID        string       `db:"id"`
	StackID   string       `db:"stack_id"`
	EnvSlug   string       `db:"env_slug"` // "" = whole-stack plan
	CommitSHA string       `db:"commit_sha"`
	Summary   string       `db:"summary"`
	Plan      string       `db:"plan"`   // JSON stackconf.Plan
	Status    string       `db:"status"` // pending | superseded | applied | rejected | error
	Error     string       `db:"error"`
	CreatedAt time.Time    `db:"created_at"`
	DecidedAt sql.NullTime `db:"decided_at"`
}

// Intended is one key on one tile whose difference from the first environment
// someone has said is on purpose. Value is the key's value when it was marked
// ("" = declared per environment by the config file, any value).
type Intended struct {
	EnvironmentID string `db:"environment_id"`
	TileSlug      string `db:"tile_slug"`
	Key           string `db:"key"`
	Value         string `db:"value"`
}

// HomeSlug is the slug (and type) of a stack's home environment: the hidden
// env holding its shared tiles. Created with the stack, never listed by
// ListEnvironmentsByStack, found through HomeEnvironment. Reserved: the
// stack file refuses an environment by this name.
const HomeSlug = "stack"

// HomeEnv is a stack's home environment row, ready to insert.
func HomeEnv(stackID string, at time.Time) *Environment {
	return &Environment{ID: uuid.New().String(), StackID: stackID, Name: "Stack", Slug: HomeSlug,
		Type: HomeSlug, Settings: "{}", Position: -1, CreatedAt: at}
}

// Environment is the isolation unit inside a stack: its own tiles, network,
// and graph. Type "static" = created by hand (production, staging);
// "ephemeral" = disposable clone (temp env, PR env); "stack" = the stack's
// home for shared tiles (one per stack, see HomeSlug).
type Environment struct {
	ID        string `db:"id"`
	StackID   string `db:"stack_id"`
	Name      string `db:"name"`
	Slug      string `db:"slug"`
	Type      string `db:"type"`        // static | ephemeral | stack
	BaseEnvID string `db:"base_env_id"` // env this one was cloned from, "" if none
	Settings  string `db:"settings"`

	// Config-as-code: the branch whose copy of the config file drives this
	// env ("" = the stack's bound branch), and the apply policy for plans
	// touching it ("auto" | "manual"; "" = manual for the default env, auto
	// for the rest).
	ConfigBranch string `db:"config_branch"`
	ApplyPolicy  string `db:"apply_policy"`

	// Color is this env's own colour: a palette name ("teal") or a hex
	// value. "" falls back to the org's default for the slug, then to the
	// ladder default. Resolve it through envcolor, never read it directly.
	Color string `db:"color"`

	// Position is the env's rung on the ladder: its index in the config
	// file's environments order, written by apply. 0 on UI stacks, where
	// creation order decides. Read the list in store order, never sort here.
	Position int `db:"position"`

	// Where traefik sits on this env's docker network, recorded when it
	// attaches. Both feed ${{ stackr.PROXY_IP }} / ${{ stackr.PROXY_CIDR }};
	// empty means it has not attached yet. Per-env because traefik joins every
	// environment network and holds a different address on each.
	ProxyIP   string `db:"proxy_ip"`
	ProxyCIDR string `db:"proxy_cidr"`

	// Network is the overlay this env holds out of the pool
	// (infra/netpool). Empty until the first deploy claims one. The name
	// carries no meaning; this column is the only mapping.
	Network string `db:"network"`

	CreatedAt time.Time `db:"created_at"`
}

// Storage is a server-scoped share or pool (§2.7): an nfs/smb network share
// or a local path pool, consumed through declared sub-paths, each backed by a
// docker local-driver volume. Never mounted on the host by stackr, docker
// performs the mount at container start.
type Storage struct {
	ID        string    `db:"id"`
	ServerID  string    `db:"server_id"`
	Name      string    `db:"name"`
	Slug      string    `db:"slug"`
	Backend   string    `db:"backend"` // nfs | smb | local
	Address   string    `db:"address"`
	Export    string    `db:"export"`
	Username  string    `db:"username"`
	Password  string    `db:"password"` // decrypted in the store layer
	Opts      string    `db:"opts"`
	Status    string    `db:"status"` // ok | error | unknown, last probe
	StatusMsg string    `db:"status_msg"`
	CreatedAt time.Time `db:"created_at"`
}

// StoragePath is one declared sub-path on a Storage, the only unit tiles may
// attach (§2.7 strict sub-paths). Each is its own docker volume.
type StoragePath struct {
	ID        string    `db:"id"`
	StorageID string    `db:"storage_id"`
	Name      string    `db:"name"`
	Subpath   string    `db:"subpath"`
	ForcedRO  bool      `db:"forced_ro"`
	CreatedAt time.Time `db:"created_at"`
}

// StorageVolume is the docker volume backing one sub-path, one volume per
// path, shared by every consumer, same naming family as DockerVolume().
// The prefix stays "stackr-" while every other docker object moved to
// "stkr-": docker cannot rename a volume, so a prefix change makes the
// next deploy ask for a name that does not exist, get a fresh empty
// volume, and leave the data detached. Volumes are id-named and already
// outside the slug scheme, so the shorter prefix bought nothing here.
func StorageVolume(pathID string) string { return "stackr-stor-" + pathID[:8] }

// Server is one Docker host stackr manages: the "local" row is the swarm
// manager and runs the panel, every other row is a swarm node added through
// Add node (docs/plans/32-multi-node-ui.md).
//
// Name is the display name the operator typed; Hostname arrives with the join.
// NodeID is empty until the node shows up in `docker node ls`, which is what
// "pending" means. Role, Status and LastSeenAt are mirrored from swarm on
// every load of the servers list, not authored here.
type Server struct {
	ID        string    `db:"id"`
	Name      string    `db:"name"`
	Kind      string    `db:"kind"` // local | swarm | remote
	Endpoint  string    `db:"endpoint"`
	Settings  string    `db:"settings"` // JSON defaults, see internal/settings
	CreatedAt time.Time `db:"created_at"`

	NodeID     string       `db:"node_id"`
	Hostname   string       `db:"hostname"`
	Address    string       `db:"address"`
	Role       string       `db:"role"`   // manager | worker
	Status     string       `db:"status"` // pending | ready | draining | down
	LastSeenAt sql.NullTime `db:"last_seen_at"`
}

// JoinKey is a one-time swarm join secret: bound to the address the operator
// typed on Add node, burned the moment it is used, and dead after an hour.
type JoinKey struct {
	Key       string       `db:"key"`
	ServerID  string       `db:"server_id"`
	Address   string       `db:"address"`
	ExpiresAt time.Time    `db:"expires_at"`
	UsedAt    sql.NullTime `db:"used_at"`
	CreatedAt time.Time    `db:"created_at"`
}

// Spent reports whether the key can no longer be used: burned already, or
// past its hour.
func (k JoinKey) Spent() bool { return k.UsedAt.Valid || time.Now().After(k.ExpiresAt) }

// Tile is anything on an environment's canvas. Kind picks the lifecycle:
// "service" (long-running, default) or "cron" (one-shot container on a
// schedule, k8s CronJob-style). A tile with Engine != "" is a database
// preset, same service lifecycle plus connection-string handling.
type Tile struct {
	ID             string `db:"id"`
	StackID        string `db:"stack_id"` // denormalized from environment for cheap stack-wide queries
	EnvironmentID  string `db:"environment_id"`
	Name           string `db:"name"`
	Slug           string `db:"slug"`
	Kind           string `db:"kind"`        // service | cron
	SourceType     string `db:"source_type"` // git | image
	GitURL         string `db:"git_url"`
	GitBranch      string `db:"git_branch"`
	ConnectorID    string `db:"connector_id"` // git connector for clone auth, "" = public/manual
	ImageRef       string `db:"image_ref"`
	DockerfilePath string `db:"dockerfile_path"`
	BuildContext   string `db:"build_context"`
	Env            string `db:"env"`
	BuildArgs      string `db:"build_args"`
	Volumes        string `db:"volumes"`
	ContainerPort  int    `db:"container_port"`
	HealthcheckCmd string `db:"healthcheck_cmd"`
	// Docker-native HEALTHCHECK knobs for HealthcheckCmd (0 = docker default).
	HealthcheckIntervalS    int `db:"healthcheck_interval_s"`
	HealthcheckTimeoutS     int `db:"healthcheck_timeout_s"`
	HealthcheckRetries      int `db:"healthcheck_retries"`
	HealthcheckStartPeriodS int `db:"healthcheck_start_period_s"`
	// RunOnDeploy is the function-tile trigger: run after its own deploy
	// finishes, in addition to manual "Run now".
	RunOnDeploy bool `db:"run_on_deploy"`
	// DependsOn: startup-order dependencies, one per line, "slug" or
	// "slug:condition" (started | healthy | completed). Bulk starts only.
	DependsOn string `db:"depends_on"`
	// Files ships repo files into the container, one per line:
	// "repo/path:/container/path[:template]", fetched at deploy, ro-mounted.
	Files string `db:"files"`
	// Storage attaches declared storage sub-paths, one per line:
	// "storage-slug/path-name:/mount[:ro]" (§2.7). Resolved to docker
	// local-driver volumes at deploy.
	Storage      string  `db:"storage"`
	WebhookToken string  `db:"webhook_token"`
	Status       string  `db:"status"`
	CPULimit     float64 `db:"cpu_limit"`    // cores, 0 = unlimited
	MemLimitMB   int     `db:"mem_limit_mb"` // MB, 0 = unlimited

	// Container runtime fields (services). User is docker's --user ("uid[:gid]"),
	// ShmSizeMB sizes /dev/shm (0 = docker default; also honored on managed
	// tiles), Privileged + Devices ("host[:container[:perms]]" per line) are the
	// host-access escape hatch, RestartPolicy is "" (restart on any exit),
	// "on-failure" or "no" (runtime.NormalizeRestart).
	User          string `db:"user"`
	ShmSizeMB     int    `db:"shm_size_mb"`
	Privileged    bool   `db:"privileged"`
	Devices       string `db:"devices"`
	RestartPolicy string `db:"restart_policy"`

	// WatchPaths filters push auto-deploys: one regex per line matched
	// against changed file paths, "!" prefix = ignore. Empty = any change
	// deploys.
	WatchPaths string `db:"watch_paths"`

	// Image-watch fields (image-source tiles). UpdatePolicy: off | notify |
	// auto. ImageDigest is what the last deploy pulled, LatestDigest the
	// newest the registry watcher has seen, unequal = "new version" badge.
	UpdatePolicy string `db:"update_policy"`
	ImageDigest  string `db:"image_digest"`
	LatestDigest string `db:"latest_digest"`
	// WaitForCI parks push auto-deploys as waiting_ci until the commit's
	// checks pass (git-source tiles).
	WaitForCI bool `db:"wait_for_ci"`

	// Volume-kind fields: which service mounts this volume (single-attach by
	// design, shared writers corrupt data), where, and the docker volume
	// name ("" = derived stackr-vol-<id8>; conversions keep legacy names).
	AttachedTileID string `db:"attached_tile_id"`
	MountPath      string `db:"mount_path"`
	VolumeName     string `db:"volume_name"`
	// MaxSizeMB is a warn-only ceiling (0 = none): docker's local driver has
	// no portable quota, so nothing stops a volume growing past it.
	MaxSizeMB int `db:"max_size_mb"`

	// Placement (docs/plans/32-multi-node-ui.md). HomeNode is the swarm node
	// ID a pinned tile's volume lives on: state, set on first deploy, changed
	// only by the Move action, and required before a pinned tile can deploy.
	// Replicas is config, stateless tiles only. NodeGroup is the
	// `stackr.group` node label a tile is constrained to, "" = anywhere.
	HomeNode  string `db:"home_node"`
	Replicas  int    `db:"replicas"`
	NodeGroup string `db:"node_group"`

	// Proxy/routing extras rendered into the tile's Traefik config.
	BasicAuthUser   string `db:"basic_auth_user"`  // "" = no basic auth
	BasicAuthHash   string `db:"basic_auth_hash"`  // bcrypt hash for BasicAuthUser
	SecHeaders      bool   `db:"sec_headers"`      // HSTS + nosniff + frame-deny preset
	PublishedPorts  string `db:"published_ports"`  // "host:container[/udp]" per line, applied at deploy
	TraefikOverride string `db:"traefik_override"` // raw dynamic config, replaces the generated file

	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`

	// Database-preset fields (Engine != "" marks a db tile).
	Engine       string `db:"engine"` // a key of databases.Engines, that registry is the list
	DBName       string `db:"db_name"`
	DBUser       string `db:"db_user"`
	DBPassword   string `db:"db_password"`
	ExternalPort int    `db:"external_port"` // 0 = internal only

	// Cron-kind fields: schedule, command override (empty = image CMD), and
	// the last run's outcome recorded directly on the row.
	Cron           string       `db:"cron"`
	Command        string       `db:"command"`
	AllowOverlap   bool         `db:"allow_overlap"`
	TimeoutMinutes int          `db:"timeout_minutes"`
	LastRunAt      sql.NullTime `db:"last_run_at"`
	LastStatus     string       `db:"last_status"`
	LastOutput     string       `db:"last_output"`

	// Scope: where the tile lives. "env" (default, ScopeID ""), "stack" or
	// "org", see managedtiles.Eligible. "server" is not implemented.
	ScopeKind string `db:"scope_kind"`
	ScopeID   string `db:"scope_id"`

	// Endpoint shape published to referencing tiles as STACKR_* outputs.
	// EndpointPortVar names an extra variable to inject ContainerPort under
	// ("" = none); ContainerPort stays canonical.
	EndpointProtocol string `db:"endpoint_protocol"` // http | https | tcp
	EndpointPortVar  string `db:"endpoint_port_var"`

	// SharedNetName is the db-pool overlay a shared managed instance holds,
	// claimed on provision and returned on delete. Read it through
	// SharedNet(), never directly.
	SharedNetName string `db:"shared_net"`
}

// StagedChange is one pending structural edit in the UI-staging buffer:
// applied to a tile's desired config, reviewed, then committed as a
// transaction through the reconcile engine. Payload is a JSON {field: value}.
type StagedChange struct {
	ID         string    `db:"id"`
	StackID    string    `db:"stack_id"`
	EnvID      string    `db:"env_id"`
	TileSlug   string    `db:"tile_slug"`
	AuthorID   string    `db:"author_id"`
	AuthorName string    `db:"author_name"`
	Summary    string    `db:"summary"`
	Payload    string    `db:"payload"`
	CreatedAt  time.Time `db:"created_at"`
}

// Provision is one logical database inside a shared db instance, owned by a
// consumer tile. Creds live here (encrypted) and are published as the env
// managed resource, so the consumer references ${{ tile.<slug>.DATABASE_URL }}.
// SecretName is vestigial, kept until the legacy-drop migration.
type Provision struct {
	ID             string `db:"id"`
	InstanceTileID string `db:"instance_tile_id"`
	ConsumerTileID string `db:"consumer_tile_id"` // "" = orphaned
	EnvID          string `db:"env_id"`
	DBName         string `db:"db_name"`
	DBUser         string `db:"db_user"`
	DBPassword     string `db:"db_password"`
	SecretName     string `db:"secret_name"`
	Status         string `db:"status"` // active | orphaned
	Public         bool   `db:"public"` // s3: bucket has a public-read policy
	// OnRemove is what happens when the config file stops declaring this slice:
	// "" / "detach" orphan it and keep the data, "drop" destroys it. Kept on the
	// row because the declaration that carried the policy is gone by the time
	// the removal is applied.
	OnRemove string `db:"on_remove"`
	// ResourceSlug is the reference name of a config-declared slice, the
	// config key. "" keeps the derived <instance>-<dbname> form
	// (panel/API-provisioned slices). Read through databases.ResourceSlug.
	ResourceSlug string    `db:"resource_slug"`
	CreatedAt    time.Time `db:"created_at"`
}

// Owner kinds for Variable.
const (
	OwnerTile  = "tile"
	OwnerStack = "stack"
	OwnerOrg   = "org"
	// OwnerEnv holds an environment's own value of a stack-declared secret,
	// the resolver reads it before the stack-wide row for ${{ stack.NAME }}.
	OwnerEnv = "env"
)

// Variable is one named value owned by a tile, stack or org. Values may embed
// ${{ ... }} references to other variables and to managed-resource outputs;
// they are resolved at deploy time, never at write time. Value is always
// encrypted at rest, Secret controls masking and read authorization, not
// storage, so a flag flip can't leak a value that was written in the clear.
type Variable struct {
	OwnerKind string    `db:"owner_kind"` // tile | stack | org
	OwnerID   string    `db:"owner_id"`
	Name      string    `db:"name"`
	Value     string    `db:"value"`
	Secret    bool      `db:"secret"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// AuditEvent is one read or write of a secret variable's value, see
// migrations/036_audit_events.up.sql for what counts as a read.
type AuditEvent struct {
	Actor     string    `db:"actor"`      // user email, "api:<key>" or "share-link:<id>"
	Action    string    `db:"action"`     // reveal | copy | edit | set | delete | share
	OwnerKind string    `db:"owner_kind"` // tile | env | stack | org
	OwnerID   string    `db:"owner_id"`
	Name      string    `db:"name"`
	CreatedAt time.Time `db:"created_at"`
}

// Secret-link kinds and states.
const (
	LinkDrop  = "drop"  // inbound: they fill a form, values land in Variables
	LinkShare = "share" // outbound: they see values we point at, once

	LinkOpen    = "open"
	LinkBurned  = "burned"
	LinkRevoked = "revoked"
	LinkLocked  = "locked" // too many wrong passphrases
)

// MaxLinkAttempts is how many wrong passphrases a link tolerates before it locks.
const MaxLinkAttempts = 5

// SecretLinkField is one entry in SecretLink.Fields. For a drop box it
// describes an input to collect; for a share link it names an existing
// variable in the link's scope (a pointer, the value is read at reveal, so
// revoking the link leaves nothing to clean up).
type SecretLinkField struct {
	Name   string `json:"name"`
	Secret bool   `json:"secret"`
	Hint   string `json:"hint,omitempty"`
}

// SecretLink is a one-shot exchange with someone who has no account. The row
// holds field names only, never values, so it doubles as the audit line.
type SecretLink struct {
	ID        string `db:"id"`
	Kind      string `db:"kind"`
	OwnerKind string `db:"owner_kind"`
	OwnerID   string `db:"owner_id"`
	Label     string `db:"label"`
	TokenHash string `db:"token_hash"`
	PassHash  string `db:"pass_hash"` // "" = no passphrase
	Fields    string `db:"fields"`    // JSON []SecretLinkField
	State     string `db:"state"`
	Attempts  int    `db:"attempts"`
	// WindowMinutes keeps a share link alive this long after first access, so
	// the recipient can copy without racing the burn. 0 = burn immediately.
	WindowMinutes int          `db:"window_minutes"`
	ExpiresAt     time.Time    `db:"expires_at"`
	OpenedAt      sql.NullTime `db:"opened_at"`
	CreatedBy     string       `db:"created_by"`
	CreatedAt     time.Time    `db:"created_at"`
}

// FieldList decodes Fields, returning nil on malformed JSON.
func (l *SecretLink) FieldList() []SecretLinkField {
	var out []SecretLinkField
	_ = json.Unmarshal([]byte(l.Fields), &out)
	return out
}

// Dead reports whether the link can no longer be used. Expiry is evaluated
// here, at read time, so a link stops working the moment it lapses rather
// than whenever a sweep next runs.
func (l *SecretLink) Dead(now time.Time) bool {
	if l.State != LinkOpen || now.After(l.ExpiresAt) {
		return true
	}
	// A share link with a grace window dies when the window closes.
	if l.Kind == LinkShare && l.OpenedAt.Valid &&
		now.After(l.OpenedAt.Time.Add(time.Duration(l.WindowMinutes)*time.Minute)) {
		return true
	}
	return false
}

// ManagedResource is a logical resource a provider tile hands out, a database
// inside a postgres instance, a bucket on an s3 server. Referenced by Slug,
// which shares the environment's tile namespace: ${{ tile.<slug>.<output> }}.
type ManagedResource struct {
	ID             string    `db:"id"`
	EnvironmentID  string    `db:"environment_id"`
	ProviderTileID string    `db:"provider_tile_id"`
	Name           string    `db:"name"`
	Slug           string    `db:"slug"`
	Kind           string    `db:"kind"`   // the providing instance's engine (databases.Engines key)
	Status         string    `db:"status"` // active | orphaned
	Public         bool      `db:"public"`
	CreatedAt      time.Time `db:"created_at"`
	UpdatedAt      time.Time `db:"updated_at"`
}

// ResourceOutput is one named value a resource publishes (DATABASE_URL, PGHOST,
// S3_BUCKET…). RequiresNetwork marks outputs only reachable over the provider's
// shared docker network, so the resolver knows to attach it.
type ResourceOutput struct {
	ResourceID      string `db:"resource_id"`
	Name            string `db:"name"`
	Value           string `db:"value"`
	Secret          bool   `db:"secret"`
	RequiresNetwork bool   `db:"requires_network"`
}

// ResourceBinding grants one consumer tile access to a resource's outputs.
// Without it the resolver refuses the reference.
type ResourceBinding struct {
	ResourceID     string    `db:"resource_id"`
	ConsumerTileID string    `db:"consumer_tile_id"`
	CreatedAt      time.Time `db:"created_at"`
}

// IsManaged reports whether the tile is a managed instance (an engine preset: postgres, mysql, s3, ...).
func (t *Tile) IsManaged() bool { return t.Engine != "" }

// SharedNet is the docker network a db/s3 instance shares with consumers
// outside its own environment: one overlay out of the db pool, claimed on
// provision (infra/netpool). Empty on a tile that is not a shared instance,
// and on one provisioned before the pool existed until it next deploys.
// Lives here so the resolver can name it without importing the databases
// package (which will import the resolver).
func (t *Tile) SharedNet() string { return t.SharedNetName }

// IsVolume reports whether the tile is a volume.
func (t *Tile) IsVolume() bool { return t.Kind == "volume" }

// HasImageUpdate: the registry watcher has seen a digest newer than what
// runs. Empty ImageDigest means the baseline is unknown, not an update.
func (t *Tile) HasImageUpdate() bool {
	return t.LatestDigest != "" && t.ImageDigest != "" && t.LatestDigest != t.ImageDigest
}

// volumeName is docker's own grammar for a named volume. Anything else is a
// bind source: "/" mounts the node's root filesystem into the container, and
// DockerVolume hands whatever is stored here straight into a bind string.
var volumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidVolumeName reports whether s is a docker volume name. Every writer of
// Tile.VolumeName checks it: empty is valid too and means the id-derived name.
func ValidVolumeName(s string) bool { return volumeName.MatchString(s) }

// DockerVolume is the docker named volume backing a volume tile. Keeps the
// "stackr-" prefix, see StorageVolume for why.
func (t *Tile) DockerVolume() string {
	if t.VolumeName != "" {
		return t.VolumeName
	}
	return "stackr-vol-" + t.ID[:8]
}

// Deployment is one build+run attempt of a Tile.
type Deployment struct {
	ID         string       `db:"id"`
	TileID     string       `db:"tile_id"`
	Status     string       `db:"status"` // waiting_ci | queued | running | done | error | cancelled
	Trigger    string       `db:"trigger"`
	CommitSHA  string       `db:"commit_sha"`
	ImageTag   string       `db:"image_tag"`
	Error      string       `db:"error"`
	CreatedAt  time.Time    `db:"created_at"`
	StartedAt  sql.NullTime `db:"started_at"`
	FinishedAt sql.NullTime `db:"finished_at"`
}

// InterruptedMsg is what SweepStaleRuns writes on a row the previous process
// was driving. Shared because the deploy engine matches on it to put a
// requeued deployment back, and a drift between the two means a requeued
// deploy silently declines to run.
const InterruptedMsg = "interrupted by a server restart"

// WorkItem is one job on the durable queue (docs/plans/33-workqueue.md).
//
// The point of the table rather than a channel is what happens across a
// restart: a row left running is either requeued or failed on boot, per kind,
// instead of being lost with the process that held it.
type WorkItem struct {
	ID   string `db:"id"`
	Kind string `db:"kind"`
	// DedupeKey scopes supersession within a kind: a newer item with the same
	// key marks the older queued ones superseded. Empty means never dedupe.
	DedupeKey string `db:"dedupe_key"`
	Payload   string `db:"payload"` // JSON, whatever the handler for this kind reads
	// queued | running | done | error | cancelled | superseded
	Status string `db:"status"`
	// Step is coarse progress and doubles as the resume point for a kind that
	// can resume. Progress is JSON for the byte counters a volume move needs
	// and a restart currently loses.
	Step       string       `db:"step"`
	Progress   string       `db:"progress"`
	Error      string       `db:"error"`
	Attempts   int          `db:"attempts"`
	CreatedAt  time.Time    `db:"created_at"`
	StartedAt  sql.NullTime `db:"started_at"`
	FinishedAt sql.NullTime `db:"finished_at"`
}

// Done reports that the item will not run again, whatever the outcome.
func (w WorkItem) Done() bool {
	switch w.Status {
	case "done", "error", "cancelled", "superseded":
		return true
	}
	return false
}

// Domain routes a hostname through Traefik to a Tile container port.
type Domain struct {
	ID            string `db:"id"`
	TileID        string `db:"tile_id"`
	Host          string `db:"host"`
	Path          string `db:"path"`
	ContainerPort int    `db:"container_port"`
	HTTPS         bool   `db:"https"`
	// ForceHTTPS bounces plain HTTP to the TLS router. Distinct from HTTPS,
	// which is only "serve TLS here": a host that must still answer on http
	// (a legacy client, a check that cannot follow a redirect) can have a
	// certificate without being redirected onto it. Default on.
	ForceHTTPS bool   `db:"force_https"`
	RedirectTo string `db:"redirect_to"` // target host: requests 301 there instead of proxying
	CertPEM    string `db:"cert_pem"`    // custom certificate chain (PEM), "" = ACME
	KeyPEM     string `db:"key_pem"`     // private key for CertPEM, encrypted at rest
	// Auto marks a hostname stackr generated under a domain resource
	// (EnsureAutoDomain / config `auto: true`) rather than one a person
	// attached. Only these can be put behind the panel's session check.
	Auto bool `db:"auto"`
	// Position is the declaration order among the tile's domains, first
	// listed is primary, what STACKR_PUBLIC_URL resolves to.
	Position  int       `db:"position"`
	CreatedAt time.Time `db:"created_at"`
}

// DomainResource is a domain owned at a level, instance (server), org or
// stack. Auto-generated tile hostnames nest under the nearest visible
// resource; an apex claim hands the bare host to one tile.
type DomainResource struct {
	ID      string `db:"id"`
	Level   string `db:"level"`    // instance | org | stack
	OwnerID string `db:"owner_id"` // server / org / stack id
	Host    string `db:"host"`
	// IncludeEnvOnDefault keeps the default env's slug in generated names
	// (api.prod.stack.org instead of api.stack.org). Default off.
	IncludeEnvOnDefault bool `db:"include_env_on_default"`
	// ACMEEmail is the Let's Encrypt account certificates under this resource
	// are issued on. Blank uses the instance's. One resolver per distinct
	// address; changing one restarts traefik once.
	ACMEEmail string `db:"acme_email"`
	// Declared marks a row the config file created (mirrors
	// Stack.OrgDeclared). Only a declared row is the file's to delete, a
	// host added in the panel is not, and every row predating domains: is
	// a panel row.
	Declared  bool      `db:"declared"`
	CreatedAt time.Time `db:"created_at"`
}

// Registry is a Docker registry credential set.
type Registry struct {
	ID        string    `db:"id"`
	Name      string    `db:"name"`
	URL       string    `db:"url"`
	Domain    string    `db:"domain"` // TLS domain routed via Traefik (managed registry)
	Username  string    `db:"username"`
	Password  string    `db:"password"`
	Managed   bool      `db:"managed"`
	CreatedAt time.Time `db:"created_at"`
}

// PushURL is the host an image is tagged and pushed under. The TLS domain
// wins when one is set: that is the name a worker node can pull from, and
// under swarm the image ref recorded on a deployment has to be resolvable
// from every node, not just the manager's own localhost
// (docs/plans/30-docker-swarm.md, decision 3).
func (r *Registry) PushURL() string {
	if r.Domain != "" {
		return r.Domain
	}
	return r.URL
}

// CronRun is one execution of a cron-kind tile ("app:<id>").
type CronRun struct {
	ID         string       `db:"id"`
	Ref        string       `db:"ref"`
	Status     string       `db:"status"`  // running | ok | error | skipped | stopped
	Trigger    string       `db:"trigger"` // schedule | manual web | manual api | deploy
	Actor      string       `db:"actor"`   // user email or api key name; blank on a schedule
	Output     string       `db:"output"`
	StartedAt  time.Time    `db:"started_at"`
	FinishedAt sql.NullTime `db:"finished_at"` // NULL while the run is still going
}

// Running reports whether the run has not finished yet.
func (r CronRun) Running() bool { return !r.FinishedAt.Valid }

// Backup kinds and container modes.
const (
	BackupDump   = "dump"   // engine-native dump of a database tile
	BackupVolume = "volume" // tar of a named docker volume
	BackupStackr = "stackr" // the panel's own SQLite database (admin-only)

	ModePause = "pause" // freeze the container while the tar runs (default)
	ModeStop  = "stop"  // stop it instead, for services that dislike a freeze
	ModeLive  = "live"  // don't touch it, may produce a torn copy
)

// BackupDestination is an S3-compatible bucket backups are written to. OrgID
// is NULL for an admin-global destination, which is the only kind the panel's
// own database may use.
type BackupDestination struct {
	ID        string         `db:"id"`
	OrgID     sql.NullString `db:"org_id"`
	Name      string         `db:"name"`
	Endpoint  string         `db:"endpoint"`
	Region    string         `db:"region"`
	Bucket    string         `db:"bucket"`
	AccessKey string         `db:"access_key"`
	SecretKey string         `db:"secret_key"` // decrypted on read
	// Shared offers an admin-global destination to every organization.
	// Meaningful only when OrgID is NULL; an org's own destination is already
	// scoped to that org. Off by default: a destination carries the credentials
	// the archive is written with, so handing it out is a decision.
	Shared    bool      `db:"shared"`
	CreatedAt time.Time `db:"created_at"`
}

// Global reports whether the destination is admin-owned rather than an org's.
func (d *BackupDestination) Global() bool { return !d.OrgID.Valid || d.OrgID.String == "" }

// VisibleTo reports whether an org may use this destination: its own, or a
// global one the admin has shared. An unshared global is "not found" to a
// tenant, never "forbidden": ids must not leak across orgs.
func (d *BackupDestination) VisibleTo(orgID string) bool {
	if d.Global() {
		return d.Shared
	}
	return d.OrgID.String == orgID
}

// Backup is one schedule: what to back up, where to, how often, how many to
// keep. TileID is NULL only for kind "stackr".
type Backup struct {
	ID            string         `db:"id"`
	TileID        sql.NullString `db:"tile_id"`
	DestinationID string         `db:"destination_id"`
	Kind          string         `db:"kind"`
	ContainerMode string         `db:"container_mode"`
	Cron          string         `db:"cron"`
	Timezone      string         `db:"timezone"`
	KeepLatest    int            `db:"keep_latest"`
	Enabled       bool           `db:"enabled"`
	CreatedAt     time.Time      `db:"created_at"`
}

// Schedule is the cron expression as the scheduler sees it: robfig/cron reads
// the timezone off a CRON_TZ= prefix, so the zone is composed in, not parsed.
func (b *Backup) Schedule() string {
	if b.Timezone == "" {
		return b.Cron
	}
	return "CRON_TZ=" + b.Timezone + " " + b.Cron
}

// BackupRun is one execution of a Backup.
type BackupRun struct {
	ID         string       `db:"id"`
	BackupID   string       `db:"backup_id"`
	Trigger    string       `db:"trigger"` // schedule | manual | pre-restore
	Status     string       `db:"status"`  // running | done | error
	ObjectKey  string       `db:"object_key"`
	SizeBytes  int64        `db:"size_bytes"`
	Error      string       `db:"error"`
	CreatedAt  time.Time    `db:"created_at"`
	FinishedAt sql.NullTime `db:"finished_at"`
}

// Metric is one CPU/mem/network sample for a managed container or the host.
type Metric struct {
	Ref      string    `db:"ref"`
	TS       time.Time `db:"ts"`
	CPUPct   float64   `db:"cpu_pct"`
	MemBytes int64     `db:"mem_bytes"`
	RxBps    float64   `db:"rx_bps"` // ingress, bytes/second
	TxBps    float64   `db:"tx_bps"` // egress, bytes/second
}

// NodePosition is a persisted canvas position for one graph node. OwnerID is
// scope-prefixed ("env:<stackID>", every environment of a stack shares one
// layout, "stack:<stackID>", "org:<orgID>"). Node ids are slug-based
// ("app:<slug>") so they survive a redeploy.
type NodePosition struct {
	OwnerID string  `db:"owner_id"`
	NodeID  string  `db:"node_id"`
	X       float64 `db:"x"`
	Y       float64 `db:"y"`
}

// Annotation is a free-floating note on a canvas: a text label or a box drawn
// behind the cards. Shared per canvas, OwnerID uses the same scope-prefixed
// keys as NodePosition, so every viewer of a canvas sees the same board.
type Annotation struct {
	ID        string    `db:"id" json:"id"`
	OwnerID   string    `db:"owner_id" json:"-"`
	Kind      string    `db:"kind" json:"kind"` // "text" | "box"
	Body      string    `db:"body" json:"body"`
	X         float64   `db:"x" json:"x"`
	Y         float64   `db:"y" json:"y"`
	W         float64   `db:"w" json:"w"`
	H         float64   `db:"h" json:"h"`
	Color     string    `db:"color" json:"color,omitempty"`
	CreatedAt time.Time `db:"created_at" json:"-"`
	UpdatedAt time.Time `db:"updated_at" json:"-"`
}

// Annotation limits, same idea as the node-position ones: a hand-rolled
// request must not write junk or unbounded rows.
const (
	MaxAnnotations    = 500
	MaxAnnotationBody = 2000
)

// ValidateAnnotation checks one posted annotation.
func ValidateAnnotation(a *Annotation) error {
	if a.OwnerID == "" || a.ID == "" {
		return errors.New("annotation: id and owner required")
	}
	if a.Kind != "text" && a.Kind != "box" {
		return fmt.Errorf("annotation: unknown kind %q", a.Kind)
	}
	if len(a.Body) > MaxAnnotationBody {
		return fmt.Errorf("annotation: body exceeds %d characters", MaxAnnotationBody)
	}
	for _, v := range [4]float64{a.X, a.Y, a.W, a.H} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < -MaxNodeCoord || v > MaxNodeCoord {
			return errors.New("annotation: impossible coordinate")
		}
	}
	return nil
}

// GraphGroup ties cards and annotations together so the canvas moves them as
// one. Shared per canvas, OwnerID uses the same scope-prefixed keys as
// NodePosition. Members are "node:<nodeID>" / "anno:<annotationID>" keys; a
// group is always replaced whole, so they live as one JSON column.
type GraphGroup struct {
	ID        string       `db:"id" json:"id"`
	OwnerID   string       `db:"owner_id" json:"-"`
	Members   GroupMembers `db:"member_keys" json:"members"`
	CreatedAt time.Time    `db:"created_at" json:"-"`
	UpdatedAt time.Time    `db:"updated_at" json:"-"`
}

// GroupMembers is the JSON string array in graph_groups.member_keys.
type GroupMembers []string

func (m GroupMembers) Value() (driver.Value, error) {
	b, err := json.Marshal(m)
	return string(b), err
}

func (m *GroupMembers) Scan(src any) error {
	switch v := src.(type) {
	case string:
		return json.Unmarshal([]byte(v), m)
	case []byte:
		return json.Unmarshal(v, m)
	}
	return fmt.Errorf("graph group members: unexpected column type %T", src)
}

// Graph group limits, same idea as the annotation ones.
const (
	MaxGraphGroups    = 200
	MaxGroupMembers   = 200
	MaxGroupMemberKey = 200
)

// ValidateGraphGroup checks one posted group.
func ValidateGraphGroup(g *GraphGroup) error {
	if g.OwnerID == "" || g.ID == "" {
		return errors.New("graph group: id and owner required")
	}
	if len(g.Members) < 2 {
		return errors.New("graph group: at least two members required")
	}
	if len(g.Members) > MaxGroupMembers {
		return fmt.Errorf("graph group: exceeds %d members", MaxGroupMembers)
	}
	for _, k := range g.Members {
		if len(k) == 0 || len(k) > MaxGroupMemberKey {
			return errors.New("graph group: bad member key")
		}
	}
	return nil
}

// Graph position scopes. Use with GraphOwner to build a NodePosition.OwnerID.
const (
	ScopeEnv   = "env"
	ScopeStack = "stack"
	ScopeOrg   = "org"
	// ScopeUser owns the all-orgs canvas at "/". Keyed by user, not by org:
	// that canvas is one person's arrangement of the orgs they belong to, and
	// two members of the same orgs should not fight over its layout.
	ScopeUser = "user"
)

// GraphOwner builds the owner key for a graph's saved positions.
func GraphOwner(scope, id string) string { return scope + ":" + id }

// Notification is one entry in one person's notification center. Events fan
// out to a row per recipient, so read state and preferences are per-user.
type Notification struct {
	ID        string    `db:"id"`
	UserID    string    `db:"user_id"`
	Kind      string    `db:"kind"` // deploy_failed | deploy_done | cron_failed
	Title     string    `db:"title"`
	Body      string    `db:"body"`
	Link      string    `db:"link"`
	Read      bool      `db:"read"`
	CreatedAt time.Time `db:"created_at"`
}

// APIKey authenticates REST API calls. Scopes is a JSON array of capability
// strings ("resource:action"); a request is allowed only if the key carries
// the scope the route requires AND the key's user still has org access.
type APIKey struct {
	ID        string    `db:"id"`
	UserID    string    `db:"user_id"`
	Name      string    `db:"name"`
	TokenHash string    `db:"token_hash"`
	Scopes    string    `db:"scopes"` // JSON array of scope strings
	CreatedAt time.Time `db:"created_at"`
}

// OrgRegistryCredential is a push/pull credential for one organization's
// registry namespace. Hashed like an APIKey: the plaintext is shown once and is
// not recoverable, so a database copy is not a set of push credentials.
//
// System marks the one stackr's own deploys use. It is created with the org and
// cannot be deleted: without it the org's next build has nothing to push with.
type OrgRegistryCredential struct {
	ID         string       `db:"id"`
	OrgID      string       `db:"org_id"`
	Name       string       `db:"name"`
	SecretHash string       `db:"secret_hash"`
	Prefix     string       `db:"prefix"` // visible half, for telling two apart
	System     bool         `db:"system"`
	CreatedAt  time.Time    `db:"created_at"`
	LastUsedAt sql.NullTime `db:"last_used_at"`
}

// ScopeList decodes the key's JSON scope array ([] when empty/invalid).
func (k *APIKey) ScopeList() []string {
	if k.Scopes == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(k.Scopes), &out)
	return out
}

// HasScope reports whether the key was granted a capability.
func (k *APIKey) HasScope(scope string) bool {
	for _, s := range k.ScopeList() {
		if s == scope {
			return true
		}
	}
	return false
}

// Canvas layout limits. A real canvas holds tens of cards; these exist so a
// hand-rolled request can't write junk or a million rows.
const (
	MaxNodePositions = 2000
	MaxNodeCoord     = 1e6
)

// ValidateNodePositions checks a posted layout. Handlers call it to answer 400
// rather than 500; the store calls it too, so the rules hold for every writer.
func ValidateNodePositions(ownerID string, ps []NodePosition) error {
	if ownerID == "" {
		return errors.New("node positions: owner required")
	}
	if len(ps) > MaxNodePositions {
		return fmt.Errorf("node positions: %d exceeds the %d-card limit", len(ps), MaxNodePositions)
	}
	for _, p := range ps {
		if p.NodeID == "" {
			return errors.New("node positions: node_id required")
		}
		for _, v := range [2]float64{p.X, p.Y} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < -MaxNodeCoord || v > MaxNodeCoord {
				return fmt.Errorf("node positions: %s is at an impossible coordinate", p.NodeID)
			}
		}
	}
	return nil
}
