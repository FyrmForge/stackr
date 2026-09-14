package v1

// Request/response shapes for the v1 API. Path params carry `path:"…"`, query
// params `query:"…"`, and body fields `json:"…"`, the OpenAPI reflector reads
// these tags, so these structs are both the bind target and the spec.

import (
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
)

type idParam struct {
	ID string `path:"id"`
}

// orgOut is one organization the caller belongs to. Role is the caller's own
// role in it, which is what decides whether a write will be refused before the
// caller tries one.
type orgOut struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	Role string `json:"role"`
}

type stackIn struct {
	Name        string `json:"name" required:"true"`
	Description string `json:"description"`
	OrgID       string `json:"org_id"` // required only if you belong to multiple orgs
}

type stackOut struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type envIn struct {
	ID   string `path:"id"` // stack id
	Name string `json:"name" required:"true"`
}

type envOut struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type appIn struct {
	ID             string            `path:"id"` // stack id
	Name           string            `json:"name" required:"true"`
	EnvSlug        string            `json:"env_slug"` // target environment (default: first/production)
	Kind           string            `json:"kind" description:"service (default) | cron"`
	SourceType     string            `json:"source_type"`
	Image          string            `json:"image"`
	GitURL         string            `json:"git_url"`
	GitBranch      string            `json:"git_branch"`
	DockerfilePath string            `json:"dockerfile_path" description:"path to the Dockerfile (default: Dockerfile)"`
	BuildContext   string            `json:"build_context" description:"build context dir (default: .)"`
	Port           int               `json:"port"`
	Env            map[string]string `json:"env" description:"environment variables"`
	Schedule       string            `json:"schedule" description:"cron only: cron expression"`
	Command        string            `json:"command" description:"cron only: command override"`
	TimeoutMinutes int               `json:"timeout_minutes" description:"cron only: run timeout"`
	Connector      string            `json:"connector_id" description:"git connector supplying credentials; must belong to the stack's org"`
}

// appPatch is the settings-owned field set, borrowed wholesale from
// stackconf.TileConf so the API and the config file name the same things the
// same way (decided in docs/plan-parity.md, 2026-08-03).
//
// Only the fields listed here are written, and only when the request body
// actually carries their key, see patchApp, which reads the body twice for
// exactly that reason. Env vars, domains, volumes and provisioned slices are
// deliberately absent: each already has its own verb with its own lifecycle
// (`vars set`, `tile domain add`, `tile volume add`, `tile provision`), and a
// second way in would be a second way to get them wrong.
type appPatch struct {
	ID string `path:"id"` // app id
	stackconf.TileConf
	// GitBranch is the pre-existing key for TileConf's `branch`. Kept so a
	// caller written against the old four-field patch keeps working.
	GitBranch string `json:"git_branch,omitempty"`
}

type appsQuery struct {
	Stack string `query:"stack" description:"filter to a stack id"`
	Env   string `query:"env" description:"filter to an environment id"`
}

type appOut struct {
	ID         string `json:"id"`
	StackID    string `json:"stack_id"`
	EnvID      string `json:"env_id"`
	Name       string `json:"name"`
	SourceType string `json:"source_type"`
	GitURL     string `json:"git_url"`
	GitBranch  string `json:"git_branch"`
	Status     string `json:"status"`

	UpdatePolicy string `json:"update_policy,omitempty"`
	WaitForCI    bool   `json:"wait_for_ci,omitempty"`
	// Digest pair behind the "new version" badge: what the last deploy
	// pulled vs the newest the registry watcher has seen.
	ImageDigest  string `json:"image_digest,omitempty"`
	LatestDigest string `json:"latest_digest,omitempty"`
}

type destinationIn struct {
	Name      string `json:"name" required:"true"`
	Endpoint  string `json:"endpoint" required:"true" description:"S3 endpoint URL"`
	Bucket    string `json:"bucket" required:"true"`
	Region    string `json:"region"`
	AccessKey string `json:"access_key" required:"true"`
	SecretKey string `json:"secret_key" required:"true"`
	OrgID     string `json:"org_id" description:"owning organization; empty makes it server-wide (admins only)"`
	Shared    bool   `json:"shared" description:"server-wide only: offer it to every organization"`
}

// destinationPatch changes what a destination offers. Only `shared`: endpoint,
// bucket and credentials are what the archives already written were written
// with, so changing them in place would silently orphan them.
type destinationPatch struct {
	ID     string `path:"id"`
	Shared *bool  `json:"shared"`
}

type destinationOut struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	Region   string `json:"region"`
	OrgID    string `json:"org_id"`
	Global   bool   `json:"global"`
	Shared   bool   `json:"shared" description:"server-wide and offered to every organization"`
}

type backupIn struct {
	ID            string `path:"id"` // tile id
	DestinationID string `json:"destination_id" required:"true" description:"destination id, or ${{ org.backups.NAME }} / ${{ stackr.backups.NAME }}"`
	Kind          string `json:"kind" description:"dump (database) | volume"`
	ContainerMode string `json:"container_mode" description:"volume only: pause (default) | stop | live"`
	Cron          string `json:"cron" required:"true"`
	Timezone      string `json:"timezone" description:"IANA zone the cron runs in (default: server time)"`
	KeepLatest    int    `json:"keep_latest" description:"archives to keep; 0 keeps everything"`
	Enabled       *bool  `json:"enabled"`
}

type backupPatch struct {
	ID            string `path:"id"` // backup id
	DestinationID string `json:"destination_id"`
	ContainerMode string `json:"container_mode"`
	Cron          string `json:"cron"`
	Timezone      string `json:"timezone"`
	KeepLatest    *int   `json:"keep_latest"`
	Enabled       *bool  `json:"enabled"`
}

type backupOut struct {
	ID            string `json:"id"`
	TileID        string `json:"tile_id"`
	DestinationID string `json:"destination_id"`
	Kind          string `json:"kind"`
	ContainerMode string `json:"container_mode"`
	Cron          string `json:"cron"`
	Timezone      string `json:"timezone"`
	KeepLatest    int    `json:"keep_latest"`
	Enabled       bool   `json:"enabled"`
}

type backupRunOut struct {
	ID         string `json:"id"`
	BackupID   string `json:"backup_id"`
	Trigger    string `json:"trigger"`
	Status     string `json:"status"`
	ObjectKey  string `json:"object_key"`
	SizeBytes  int64  `json:"size_bytes"`
	Error      string `json:"error"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at"`
}

type restoreIn struct {
	ID    string `path:"id"` // backup id
	RunID string `json:"run_id" required:"true" description:"the run whose archive to restore"`
}

type domainIn struct {
	ID            string `path:"id"` // app id
	Host          string `json:"host" required:"true"`
	Path          string `json:"path" description:"path prefix (default /)"`
	ContainerPort int    `json:"container_port" description:"target port (default: the app's port)"`
	HTTPS         *bool  `json:"https" description:"terminate TLS (default true)"`
	ForceHTTPS    *bool  `json:"force_https" description:"bounce plain HTTP onto the TLS router (default true)"`
	RedirectTo    string `json:"redirect_to" description:"301 to this host instead of proxying"`
}

type domainOut struct {
	ID            string `json:"id"`
	Host          string `json:"host"`
	Path          string `json:"path"`
	ContainerPort int    `json:"container_port"`
	HTTPS         bool   `json:"https"`
	// ForceHTTPS bounces plain HTTP onto the TLS router. Distinct from https,
	// which is only "serve TLS here".
	ForceHTTPS bool   `json:"force_https"`
	RedirectTo string `json:"redirect_to,omitempty"`
}

// domainResourceIn creates a domain resource, a host that tiles generate
// per-environment hostnames under (config `auto: true`, panel/CLI ask).
type domainResourceIn struct {
	Host                string `json:"host" required:"true" description:"bare lowercase hostname, e.g. example.com"`
	ACMEEmail           string `json:"acme_email" description:"Let's Encrypt account for certificates under this host; empty uses the instance's"`
	Level               string `json:"level" description:"instance or node (default, admins only) | org | stack"`
	Owner               string `json:"owner" description:"org or stack id the resource belongs to; ignored for instance level"`
	IncludeEnvOnDefault bool   `json:"include_env_on_default" description:"keep the default env's slug in generated names"`
}

type domainResourceOut struct {
	ID                  string `json:"id"`
	Level               string `json:"level"`
	OwnerID             string `json:"owner_id"`
	Host                string `json:"host"`
	IncludeEnvOnDefault bool   `json:"include_env_on_default"`
	ACMEEmail           string `json:"acme_email,omitempty"`
}

type volumeIn struct {
	ID        string `path:"id"` // app id
	Name      string `json:"name" required:"true"`
	MountPath string `json:"mount_path" required:"true" description:"absolute path to mount inside the container"`
	// VolumeName adopts an existing docker volume instead of creating one
	// named after the tile, which is how data from a pre-stackr deployment is
	// kept rather than started over.
	VolumeName string `json:"volume_name"`
	// MaxSizeMB is a warn-only ceiling (0 = none). Docker's local driver does
	// not enforce quotas, so this reports rather than blocks.
	MaxSizeMB int `json:"max_size_mb"`
}

type volumeOut struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	MountPath  string `json:"mount_path"`
	VolumeName string `json:"volume_name"` // underlying docker volume
	AppID      string `json:"app_id"`
	Status     string `json:"status"`
}

// varEntry is one variable. Value may hold ${{ ... }} references, stored as
// written; secret values read back masked without the secrets:read scope.
type varEntry struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Secret bool   `json:"secret,omitempty"`
	// Generate mints the value server-side, with the same generator the config
	// file's `default: generated` uses, and ignores Value. A name that already
	// has a value is left alone: generating over a live secret would break
	// everything reading it, silently.
	Generate bool `json:"generate,omitempty"`
	// Length is the generated length; 0 is 32.
	Length int `json:"length,omitempty"`
}

type varsIn struct {
	ID   string     `path:"id"`
	Vars []varEntry `json:"variables" required:"true"`
}

type varsOut struct {
	Vars []varEntry `json:"variables"`
}

// refOutputOut is one name a source publishes. Metadata only, no values, so
// the catalogue is safe to serve for autocomplete without secrets:read.
type refOutputOut struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"` // endpoint | variable | resource
	Secret bool   `json:"secret,omitempty"`
}

// refSourceOut is one referenceable source. Slug is empty for the scope-wide
// variable forms (${{ stack.NAME }}).
type refSourceOut struct {
	Scope   string         `json:"scope"` // tile | stack | org
	Slug    string         `json:"slug,omitempty"`
	Kind    string         `json:"kind"`
	Outputs []refOutputOut `json:"outputs"`
}

type resourceOut struct {
	ID      string   `json:"id"`
	Slug    string   `json:"slug"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Status  string   `json:"status"`
	Outputs []string `json:"outputs"`
}

type deploymentOut struct {
	ID        string `json:"id"`
	AppID     string `json:"app_id"`
	Status    string `json:"status"`
	Trigger   string `json:"trigger"`
	CommitSHA string `json:"commit_sha"`
	ImageTag  string `json:"image_tag"`
	Error     string `json:"error,omitempty"`
}

type deployAccepted struct {
	Deployment string `json:"deployment"`
}

type runAccepted struct {
	Started bool `json:"started"`
	// Run is the cron_runs id the work was opened under, so a caller can
	// follow or stop it instead of guessing which row is theirs.
	Run string `json:"run"`
}

// runStopIn addresses one run of one tile.
type runStopIn struct {
	ID  string `path:"id"`
	Run string `path:"run"`
}

type runStopped struct {
	Stopped bool `json:"stopped"` // false when the run had already finished
}

type plansQuery struct {
	ID    string `path:"id"` // stack id
	Limit int    `query:"limit" description:"how many plans to return, newest first (default 20, max 100)"`
}

// previewIn is a config bundle posted for a throwaway plan: the main file plus
// every file its include: names, resolved client-side so the preview reflects
// exactly what is on disk. ~1 MB total, enforced by the handler's body cap.
type previewIn struct {
	ID    string            `path:"id"` // stack id
	Main  string            `json:"main" description:"stackr-compose.yml content"`
	Files map[string]string `json:"files,omitempty" description:"included files, path as written in include: → content"`
	Env   string            `json:"env,omitempty" description:"preview only this environment (also lifts the skip on an env pinned to its own branch)"`
}

// orgPreviewIn is the org twin: org files have no include:, so one file.
type orgPreviewIn struct {
	ID   string `path:"id"` // org id
	Main string `json:"main" description:"stackr-org.yml content"`
}

// planOut is a plan's header, what a list shows. The changes live on
// planDetailOut so a listing doesn't carry every diff.
type planOut struct {
	ID        string `json:"id"`
	StackID   string `json:"stack_id"`
	EnvSlug   string `json:"env_slug,omitempty" description:"empty for a stack-scoped plan"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Status    string `json:"status" description:"pending | clean | applied | rejected | superseded | error"`
	Summary   string `json:"summary"`
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"created_at"`
	DecidedAt string `json:"decided_at,omitempty"`
	// Destructive is on the header, not just the detail, because a CI gate
	// reads a listing, and a listing that omitted it decoded to false on a
	// plan that deletes things, which is the worst possible default.
	Destructive bool `json:"destructive"`
}

type planChangeOut struct {
	Kind string `json:"kind" description:"create-env | delete-env | create | delete | update"`
	// Env is an environment slug. Rows for a value the config declares carry
	// Scope (stack | org) instead: the value lives at the scope, not in an env.
	Env   string `json:"env"`
	Scope string `json:"scope,omitempty"`
	Tile  string `json:"tile,omitempty"`
	Field string `json:"field,omitempty"`
	Old   string `json:"old,omitempty"`
	New   string `json:"new,omitempty"`
	Note  string `json:"note,omitempty"`
	// Destroys marks a change that destroys data without deleting a tile, a
	// second thing a CI gate should refuse alongside the plan-level flag.
	Destroys bool `json:"destroys,omitempty"`
}

type planDetailOut struct {
	planOut
	Changes []planChangeOut `json:"changes"`
	Errors  []string        `json:"errors,omitempty"`
	// Warnings do not block the apply, a declared secret with no value is the
	// usual one. Worth surfacing in a PR check, not worth failing it.
	Warnings []string `json:"warnings,omitempty"`
	// GenSecrets are `default: generated` secrets the apply would mint. They
	// make a plan non-empty server-side, so a gate that only counted changes
	// would call "will mint a secret" a no-op.
	GenSecrets []string `json:"gen_secrets,omitempty"`
}

type logsIn struct {
	ID     string `path:"id"`
	Tail   int    `query:"tail" description:"lines to return (default 200, max 5000)"`
	Follow bool   `query:"follow" description:"stream live via SSE (text/event-stream); lines are 'O <text>' (stdout) or 'E <text>' (stderr)"`
}

type forwardIn struct {
	ID   string `path:"id"`
	Port int    `query:"port" description:"container port (default: the tile's declared port, or the engine's default for a database)"`
}

type logsOut struct {
	Lines string `json:"lines"`
}

type dbIn struct {
	ID      string `path:"id"` // stack id
	Name    string `json:"name" required:"true"`
	Engine  string `json:"engine" required:"true"`
	EnvSlug string `json:"env_slug"`
	Scope   string `json:"scope" description:"how widely the instance is shared for provisioning: env (default) | stack | org"`
}

// provisionIn creates a logical database for a consumer app inside a shared
// instance. The instance may be any provisionable db tile the consumer is
// eligible for (env/stack/org scope). Generic by design: a future self-hosted
// S3/analytics instance provisions through the same endpoint.
type provisionIn struct {
	ID         string `path:"id"` // consumer app id
	InstanceID string `json:"instance_id" description:"instance tile id; supply this or infra_path"`
	InfraPath  string `json:"infra_path" description:"colon-path address of a shared instance, e.g. org:stack:env:slug"`
	Name       string `json:"name" description:"slice name (logical db / bucket); blank derives it from the app slug"`
	Public     bool   `json:"public" description:"engines with publishable slices only (s3): give the slice a public-read policy (unsigned CDN-style GETs); requires the instance to have a public domain"`
	EnvVar     string `json:"env_var" description:"env var to set to the connection secret (blank = publish only, no injection)"`
}

// attachIn points a second consumer at an existing logical database (same env),
// so a cron and a service can share one database.
type attachIn struct {
	ID          string `path:"id"` // consumer app id
	ProvisionID string `json:"provision_id" required:"true"`
	EnvVar      string `json:"env_var" description:"env var to set to the connection secret (blank = publish only)"`
}

type provisionOut struct {
	ID         string `json:"id"`
	InstanceID string `json:"instance_id"`
	ConsumerID string `json:"consumer_id"`
	DBName     string `json:"db_name"`
	Secret     string `json:"secret"` // vestigial legacy secret name; reference the resource instead
	Status     string `json:"status"`
}

// sliceIn creates a slice on a shared instance, with no consumer attached.
type sliceIn struct {
	ID     string `path:"id"`
	Name   string `json:"name" required:"true" description:"database or bucket name; uniquified against the instance"`
	Slug   string `json:"slug" description:"addressable resource slug (blank = the name)"`
	EnvID  string `json:"env_id" description:"environment the slice belongs to (blank = the instance's own)"`
	Public bool   `json:"public" description:"publish read-only, for engines that support it"`
}

// forkIn copies a slice into a new one alongside it.
type forkIn struct {
	ID   string `path:"id"` // source provision id
	Name string `json:"name" description:"database or bucket name for the fork (blank = derived from the source)"`
	Slug string `json:"slug" description:"addressable slug for the fork (blank = the source's, suffixed)"`
}

type resolveIn struct {
	Path  string `json:"path" required:"true" description:"infra path: org:stack:env:slug, or relative to env_id"`
	EnvID string `json:"env_id" description:"the caller's linked environment, which lets the path be relative"`
}

type resolveOut struct {
	Kind       string `json:"kind" description:"instance | slice"`
	ID         string `json:"id" description:"instance tile id, or provision id for a slice"`
	InstanceID string `json:"instance_id" description:"the providing instance, same as id for an instance"`
	Name       string `json:"name"`
	Path       string `json:"path" description:"the fully-qualified address"`
}

// sliceOut describes one slice cut from an instance. No password: this is for
// resolving and listing, and credentials are read through the resource
// outputs, which are audited and gated on secrets:read.
type sliceOut struct {
	ID         string   `json:"id"`
	InstanceID string   `json:"instance_id"`
	Name       string   `json:"name" description:"the database or bucket name"`
	Slug       string   `json:"slug" description:"the addressable resource slug"`
	Status     string   `json:"status"`
	Public     bool     `json:"public"`
	Consumers  []string `json:"consumers" description:"tiles that would be unhooked if this slice were removed"`
}

type dbPatch struct {
	ID           string   `path:"id"`
	ExternalPort *int     `json:"external_port" description:"publish the instance on this host port (0 = internal only)"`
	Scope        *string  `json:"scope" description:"how widely the instance is shared for provisioning: env | stack | org"`
	CPULimit     *float64 `json:"cpu_limit" description:"cores, 0 = unlimited"`
	MemLimitMB   *int     `json:"mem_limit_mb" description:"MB, 0 = unlimited; values under 6 are raised to 6"`
	Image        *string  `json:"image" description:"per-instance image override (version pin / wire-compatible build); empty reverts to the engine default; you own upgrade compatibility"`
	ShmSizeMB    *int     `json:"shm_size_mb" description:"/dev/shm size in MB, 0 = docker default"`
	UpdatePolicy *string  `json:"update_policy" description:"registry watcher: off | notify | auto"`
}

type dbOut struct {
	ID           string  `json:"id"`
	StackID      string  `json:"stack_id"`
	Name         string  `json:"name"`
	Engine       string  `json:"engine"`
	Status       string  `json:"status"`
	ExternalPort int     `json:"external_port"`
	Scope        string  `json:"scope"`
	CPULimit     float64 `json:"cpu_limit"`
	MemLimitMB   int     `json:"mem_limit_mb"`
	Image        string  `json:"image"`
	ShmSizeMB    int     `json:"shm_size_mb"`
	UpdatePolicy string  `json:"update_policy,omitempty"`
	ImageDigest  string  `json:"image_digest,omitempty"`
	LatestDigest string  `json:"latest_digest,omitempty"`
	Path         string  `json:"path,omitempty"` // colon-path address
}

// dbDelete carries the force flag, because dropping a shared instance takes
// every slice cut from it with it.
type dbDelete struct {
	ID    string `path:"id"`
	Force bool   `query:"force" description:"drop the instance even while consumers hold slices on it"`
}

// rollbackIn redeploys an image the tile has run before. The tag is required:
// "the last good one" is a judgement, and guessing it here would redeploy
// whatever happened to be second in the list.
type rollbackIn struct {
	ID       string `path:"id"`
	ImageTag string `json:"image_tag" required:"true"`
}

// metricOut is one sample of a tile's resource use.
type metricOut struct {
	At         time.Time `json:"at"`
	CPUPercent float64   `json:"cpu_percent"`
	MemBytes   int64     `json:"mem_bytes"`
	RxBps      float64   `json:"rx_bps"`
	TxBps      float64   `json:"tx_bps"`
}

// domainPatch changes one domain. Every field is a pointer: an absent field is
// left alone, which is what lets a caller flip HTTPS without also clearing a
// certificate it never mentioned.
type domainPatch struct {
	ID         string  `path:"id"`
	HTTPS      *bool   `json:"https"`
	ForceHTTPS *bool   `json:"force_https" description:"bounce plain HTTP onto the TLS router"`
	CertPEM    *string `json:"cert_pem" description:"PEM certificate chain; empty string removes the custom cert"`
	KeyPEM     *string `json:"key_pem"`
}

// detachIn identifies the consumer tile and the provision row to unhook.
type detachIn struct {
	ID  string `path:"id"`  // consumer tile id
	PID string `path:"pid"` // provision id
}

type publicIn struct {
	ID     string `path:"id"`
	Public bool   `json:"public"`
}

// stackPatch renames or re-describes a stack. Pointers again: a rename must not
// blank a description the caller did not send.
type stackPatch struct {
	ID          string  `path:"id"`
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// prEnvIn configures pull-request environments for a stack. The webhook secret
// is never returned, only rotated.
type prEnvIn struct {
	ID           string `path:"id"`
	Enabled      bool   `json:"enabled"`
	Comment      *bool  `json:"comment" description:"post a comment on the PR with the environment's URLs"`
	Status       *bool  `json:"status" description:"report a commit status on the PR"`
	RotateSecret bool   `json:"rotate_secret" description:"mint a new webhook secret; the old one stops validating at once"`
}

type prEnvOut struct {
	Enabled   bool `json:"enabled"`
	Comment   bool `json:"comment"`
	Status    bool `json:"status"`
	SecretSet bool `json:"secret_set"`
}

// registryCredentialIn names a new org registry credential.
type registryCredentialIn struct {
	ID   string `path:"id"` // org id or slug
	Name string `json:"name" required:"true"`
}

// registryCredentialOut is one credential. Secret is filled exactly once, on
// the create that minted it: the row stores only a hash, so there is no second
// chance to read it.
type registryCredentialOut struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	System     bool       `json:"system" description:"stackr's own deploy credential; cannot be deleted"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Secret     string     `json:"secret,omitempty" description:"shown once, at creation"`
}

type registryImageOut struct {
	Name  string `json:"name"`  // full repository path
	Short string `json:"short"` // the part below the org's namespace
}

type registryTagOut struct {
	Tag    string `json:"tag"`
	Digest string `json:"digest,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// registryTagParam addresses one tag. The image name holds slashes, so it
// arrives URL-escaped.
type registryTagParam struct {
	ID   string `path:"id"`
	Name string `path:"name"`
	Tag  string `path:"tag"`
}

type registryImageParam struct {
	ID   string `path:"id"`
	Name string `path:"name"`
}

type registryCredParam struct {
	ID   string `path:"id"`
	Cred string `path:"cred"`
}

// registryIn records an external registry. Password is write-only: it goes in
// here and never comes back out of a read.
type registryIn struct {
	Name     string `json:"name" required:"true"`
	URL      string `json:"url" required:"true" description:"host[:port], no scheme"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type registryPatch struct {
	ID       string  `path:"id"`
	Domain   *string `json:"domain" description:"managed registry only: the TLS hostname traefik routes"`
	Username *string `json:"username"`
	Password *string `json:"password"`
}

type registryOut struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Domain   string `json:"domain,omitempty"`
	Username string `json:"username,omitempty"`
	Managed  bool   `json:"managed"`
}

// releaseOut is one commit this stack has built, and where it stands.
type releaseOut struct {
	Commit   string `json:"commit"`
	Built    bool   `json:"built"`
	Building bool   `json:"building"`
	// RunningOn names the environments whose newest finished deployment is
	// this commit.
	RunningOn []string `json:"running_on"`
	// PendingPlan is a config plan on this commit that nobody has applied. A
	// promote past it would move the images without the config they need.
	PendingPlan string `json:"pending_plan,omitempty"`

	at int64 // newest deployment time, ordering only
}

// promoteIn moves a built commit onto a rung.
type promoteIn struct {
	ID     string `path:"id"`   // stack
	Slug   string `path:"slug"` // target environment
	Commit string `json:"commit" required:"true"`
	// Plan applies a waiting config plan first, in the same job: the images
	// must not move until the config they need has landed.
	Plan string `json:"plan"`
	// Force applies a plan the gate would otherwise refuse. Default false: the
	// panel passes true after its confirmation dialogue, and an API caller has
	// not been shown one.
	Force bool `json:"force"`
}

type promoteOut struct {
	Env    string `json:"env"`
	Commit string `json:"commit"`
	Plan   string `json:"plan,omitempty"`
	Job    string `json:"job,omitempty"`
}

// forceIn is the deliberate act an API caller makes in place of the panel's
// type-the-slug confirmation.
type forceIn struct {
	ID    string `path:"id"`
	Force bool   `json:"force" description:"tear down running tiles as well"`
}

type copyEnvIn struct {
	ID        string `path:"id"` // the environment to copy
	Name      string `json:"name" required:"true"`
	Ephemeral bool   `json:"ephemeral" description:"a disposable clone rather than a rung"`
}

// envPatch changes the per-environment settings. `protected` is deliberately
// absent: it is a config-file key that gates deletes during an apply, not a
// stored property.
type envPatch struct {
	ID          string  `path:"id"`
	Color       *string `json:"color" description:"palette name or #rrggbb; empty inherits"`
	ApplyPolicy *string `json:"apply_policy" description:"auto | manual; empty is the per-rung default"`
}

// settingKnob is one default: what applies here, which level decided it, and
// this level's own override when it has one.
type settingKnob struct {
	Key    string  `json:"key"`
	Value  string  `json:"value" description:"the resolved value at this level"`
	Source string  `json:"source" description:"server | org | stack | env | built-in"`
	Own    *string `json:"own,omitempty" description:"this level's own override; absent means inherited"`
}

type settingsOut struct {
	Level  string        `json:"level"`
	Values []settingKnob `json:"values"`
}

type memberOut struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name,omitempty"`
	Role   string `json:"role" description:"owner | member | viewer"`
}

// inviteIn invites somebody. An unknown role falls back to member: the least
// access that can still do work, so a typo grants less than was meant.
type inviteIn struct {
	ID          string `path:"id"`
	Email       string `json:"email" required:"true"`
	Role        string `json:"role" description:"owner | member | viewer (default member)"`
	ExpiresDays int    `json:"expires_days" description:"link lifetime; 0 is the 7-day default"`
}

type roleIn struct {
	ID   string `path:"id"`
	User string `path:"user"`
	Role string `json:"role" required:"true" description:"owner | member | viewer"`
}

// inviteOut is an open invite. The URL is a credential: anyone holding it can
// join the organization as the role it carries.
type inviteOut struct {
	ID        string    `json:"id"`
	Email     string    `json:"email,omitempty" description:"empty means anyone with the link"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
	Used      bool      `json:"used"`
	URL       string    `json:"url" description:"path to the accept page; a credential"`
}

type inviteParam struct {
	ID     string `path:"id"`
	Invite string `path:"invite"`
}

// domainResourcePatch changes the account, nothing else: the host is the
// resource's identity, and moving it would orphan every generated name nested
// under it.
type domainResourcePatch struct {
	ID        string  `path:"id"`
	ACMEEmail *string `json:"acme_email" description:"empty uses the instance's account"`
}
