package cli

// The tile and deployment lifecycle calls, plus the settings that used to be
// panel-only. Thin wrappers: every one of them is a single request, and the
// interesting decisions (which tile, which tag, whether to prompt) belong to
// the command layer.

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Org is one organization the key's user belongs to. Slug is what every slug
// path starts with, which is the whole reason this listing exists.
type Org struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// Orgs lists the organizations the key can see.
func (c *Client) Orgs(ctx context.Context) ([]Org, error) {
	var out []Org
	return out, c.getJSON(ctx, "/api/v1/orgs", &out)
}

// GetApp reads one tile.
func (c *Client) GetApp(ctx context.Context, appID string) (App, error) {
	var out App
	return out, c.getJSON(ctx, "/api/v1/apps/"+appID, &out)
}

// Deployments lists a tile's recent deployments, newest first.
func (c *Client) Deployments(ctx context.Context, appID string) ([]Deployment, error) {
	var out []Deployment
	return out, c.getJSON(ctx, "/api/v1/apps/"+appID+"/deployments", &out)
}

// DeploymentLogs returns one deployment's build output.
func (c *Client) DeploymentLogs(ctx context.Context, id string) (string, error) {
	var out struct {
		Lines string `json:"lines"`
	}
	return out.Lines, c.getJSON(ctx, "/api/v1/deployments/"+id+"/logs", &out)
}

// UnresolvedVars lists a tile's variables exactly as stored, references
// unexpanded. This is what you edit against; ResolvedVars is what the container
// will see.
func (c *Client) UnresolvedVars(ctx context.Context, appID string) ([]Var, error) {
	var body varsBody
	return body.Vars, c.getJSON(ctx, "/api/v1/apps/"+appID+"/variables/unresolved", &body)
}

// RefOutput is one name a source publishes.
type RefOutput struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Secret bool   `json:"secret,omitempty"`
}

// RefSource is one thing a tile may reference. Carries no values: the
// catalogue has to be safe to show a caller without secrets:read.
type RefSource struct {
	Scope   string      `json:"scope"`
	Slug    string      `json:"slug,omitempty"`
	Kind    string      `json:"kind"`
	Outputs []RefOutput `json:"outputs"`
}

// Expr renders the reference expression for one output, the string you paste
// into a variable.
func (s RefSource) Expr(name string) string {
	if s.Slug == "" {
		return "${{ " + s.Scope + "." + name + " }}"
	}
	return "${{ " + s.Scope + "." + s.Slug + "." + name + " }}"
}

// ReferenceCatalogue lists what a tile may reference.
func (c *Client) ReferenceCatalogue(ctx context.Context, appID string) ([]RefSource, error) {
	var out []RefSource
	return out, c.getJSON(ctx, "/api/v1/apps/"+appID+"/reference-catalogue", &out)
}

// Resource is one managed resource (a logical database or bucket) in a tile's
// environment, with the output names it publishes.
type Resource struct {
	ID      string      `json:"id"`
	Slug    string      `json:"slug"`
	Kind    string      `json:"kind"`
	Status  string      `json:"status"`
	Bound   bool        `json:"bound"`
	Outputs []RefOutput `json:"outputs,omitempty"`
}

// Resources lists the managed resources visible to a tile.
func (c *Client) Resources(ctx context.Context, appID string) ([]Resource, error) {
	var out []Resource
	return out, c.getJSON(ctx, "/api/v1/apps/"+appID+"/resources", &out)
}

// GetDB reads one shared instance.
func (c *Client) GetDB(ctx context.Context, dbID string) (DB, error) {
	var out DB
	return out, c.getJSON(ctx, "/api/v1/dbs/"+dbID, &out)
}

// StopRun stops a cron or function run that is still in flight.
func (c *Client) StopRun(ctx context.Context, appID, runID string) error {
	return c.send(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/runs/"+runID+"/stop", nil, nil)
}

// StopApp scales a tile to zero. The row, its volumes and its replica count
// stay, so RestartApp brings it back as it was.
func (c *Client) StopApp(ctx context.Context, appID string) (App, error) {
	var out App
	return out, c.send(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/stop", nil, &out)
}

// RestartApp bounces a tile in place: no rebuild, no new image.
func (c *Client) RestartApp(ctx context.Context, appID string) (App, error) {
	var out App
	return out, c.send(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/restart", nil, &out)
}

// ToggleCron pauses a running schedule or resumes a paused one. The answer
// carries the state it landed in.
func (c *Client) ToggleCron(ctx context.Context, appID string) (App, error) {
	var out App
	return out, c.send(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/cron/toggle", nil, &out)
}

// Rollback redeploys an image tag the tile has run before.
func (c *Client) Rollback(ctx context.Context, appID, imageTag string) (string, error) {
	var out struct {
		Deployment string `json:"deployment"`
	}
	err := c.send(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/rollback",
		map[string]string{"image_tag": imageTag}, &out)
	return out.Deployment, err
}

// CancelDeployment stops a build or roll-out in flight.
func (c *Client) CancelDeployment(ctx context.Context, id string) (Deployment, error) {
	var out Deployment
	return out, c.send(ctx, http.MethodPost, "/api/v1/deployments/"+id+"/cancel", nil, &out)
}

// Metric is one sample of a tile's resource use.
type Metric struct {
	At         time.Time `json:"at"`
	CPUPercent float64   `json:"cpu_percent"`
	MemBytes   int64     `json:"mem_bytes"`
	RxBps      float64   `json:"rx_bps"`
	TxBps      float64   `json:"tx_bps"`
}

// Metrics reads a tile's sampled resource use. window is "1h", "6h" or "24h".
func (c *Client) Metrics(ctx context.Context, appID, window string) ([]Metric, error) {
	var out []Metric
	return out, c.getJSON(ctx, "/api/v1/apps/"+appID+"/metrics?range="+window, &out)
}

// AutoDomain generates the tile's hostname under whichever domain resource its
// environment nests below.
func (c *Client) AutoDomain(ctx context.Context, appID string) (Domain, error) {
	var out Domain
	return out, c.send(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/domains/auto", nil, &out)
}

// PatchDomain changes one domain's TLS: HTTPS on or off, a custom certificate
// installed or cleared. Only the fields present are touched.
func (c *Client) PatchDomain(ctx context.Context, domainID string, fields map[string]any) (Domain, error) {
	var out Domain
	return out, c.send(ctx, http.MethodPatch, "/api/v1/domains/"+domainID, fields, &out)
}

// DetachProvision unhooks one consumer from a slice and keeps the data.
func (c *Client) DetachProvision(ctx context.Context, appID, provisionID string) error {
	return c.send(ctx, http.MethodPost,
		"/api/v1/apps/"+appID+"/provisions/"+provisionID+"/detach", nil, nil)
}

// SetSlicePublic exposes or hides a slice outside its own network. Every
// consumer of the same bucket moves with it.
func (c *Client) SetSlicePublic(ctx context.Context, provisionID string, public bool) (Slice, error) {
	var out Slice
	return out, c.send(ctx, http.MethodPost, "/api/v1/provisions/"+provisionID+"/public",
		map[string]bool{"public": public}, &out)
}

// ProbeStorage mounts the share on the node that will mount it at deploy time
// and records the outcome on the storage row.
func (c *Client) ProbeStorage(ctx context.Context, id string) (StorageEntry, error) {
	var out StorageEntry
	return out, c.send(ctx, http.MethodPost, "/api/v1/storage/"+id+"/probe", nil, &out)
}

// PatchStack renames or re-describes a stack. The slug moves with the name.
func (c *Client) PatchStack(ctx context.Context, stackID string, fields map[string]any) (Stack, error) {
	var out Stack
	return out, c.send(ctx, http.MethodPatch, "/api/v1/stacks/"+stackID, fields, &out)
}

// PREnv is a stack's pull-request environment settings. The webhook secret is
// never returned, only whether one is set.
type PREnv struct {
	Enabled   bool `json:"enabled"`
	Comment   bool `json:"comment"`
	Status    bool `json:"status"`
	SecretSet bool `json:"secret_set"`
}

// GetPREnv reads a stack's PR environment settings.
func (c *Client) GetPREnv(ctx context.Context, stackID string) (PREnv, error) {
	var out PREnv
	return out, c.getJSON(ctx, "/api/v1/stacks/"+stackID+"/pr-envs", &out)
}

// SetPREnv writes them.
func (c *Client) SetPREnv(ctx context.Context, stackID string, fields map[string]any) (PREnv, error) {
	var out PREnv
	return out, c.send(ctx, http.MethodPut, "/api/v1/stacks/"+stackID+"/pr-envs", fields, &out)
}

// --- org registry ---

// RegistryCredential is one push credential for an org's registry namespace.
// Secret is filled exactly once, by the create that minted it.
type RegistryCredential struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	System     bool       `json:"system"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Secret     string     `json:"secret,omitempty"`
}

func (c *Client) RegistryCredentials(ctx context.Context, org string) ([]RegistryCredential, error) {
	var out []RegistryCredential
	return out, c.getJSON(ctx, "/api/v1/orgs/"+org+"/registry/credentials", &out)
}

func (c *Client) AddRegistryCredential(ctx context.Context, org, name string) (RegistryCredential, error) {
	var out RegistryCredential
	return out, c.send(ctx, http.MethodPost, "/api/v1/orgs/"+org+"/registry/credentials",
		map[string]string{"name": name}, &out)
}

func (c *Client) DeleteRegistryCredential(ctx context.Context, org, id string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/orgs/"+org+"/registry/credentials/"+id, nil, nil)
}

// RegistryImage is one repository in an org's namespace.
type RegistryImage struct {
	Name  string `json:"name"`
	Short string `json:"short"`
}

func (c *Client) RegistryImages(ctx context.Context, org string) ([]RegistryImage, error) {
	var out []RegistryImage
	return out, c.getJSON(ctx, "/api/v1/orgs/"+org+"/registry/images", &out)
}

// RegistryTag is one tag of one image.
type RegistryTag struct {
	Tag    string `json:"tag"`
	Digest string `json:"digest,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

func (c *Client) RegistryTags(ctx context.Context, org, name string) ([]RegistryTag, error) {
	var out []RegistryTag
	return out, c.getJSON(ctx, "/api/v1/orgs/"+org+"/registry/images/"+url.PathEscape(name)+"/tags", &out)
}

func (c *Client) DeleteRegistryTag(ctx context.Context, org, name, tag string) error {
	return c.send(ctx, http.MethodDelete,
		"/api/v1/orgs/"+org+"/registry/images/"+url.PathEscape(name)+"/tags/"+url.PathEscape(tag), nil, nil)
}

// Registry is one registry stackr knows about: the managed one, or an external
// one a tile pulls from. The password is never returned.
type Registry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Domain   string `json:"domain,omitempty"`
	Username string `json:"username,omitempty"`
	Managed  bool   `json:"managed"`
}

func (c *Client) Registries(ctx context.Context) ([]Registry, error) {
	var out []Registry
	return out, c.getJSON(ctx, "/api/v1/registries", &out)
}

func (c *Client) AddRegistry(ctx context.Context, in Registry, password string) (Registry, error) {
	var out Registry
	return out, c.send(ctx, http.MethodPost, "/api/v1/registries", map[string]string{
		"name": in.Name, "url": in.URL, "username": in.Username, "password": password}, &out)
}

func (c *Client) PatchRegistry(ctx context.Context, id string, fields map[string]any) (Registry, error) {
	var out Registry
	return out, c.send(ctx, http.MethodPatch, "/api/v1/registries/"+id, fields, &out)
}

func (c *Client) DeleteRegistry(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/registries/"+id, nil, nil)
}

// --- releases and promotion ---

// Release is one commit this stack has built, and where it stands.
type Release struct {
	Commit      string   `json:"commit"`
	Built       bool     `json:"built"`
	Building    bool     `json:"building"`
	RunningOn   []string `json:"running_on"`
	PendingPlan string   `json:"pending_plan,omitempty"`
}

// Releases lists what a stack can promote, newest first.
func (c *Client) Releases(ctx context.Context, stackID string) ([]Release, error) {
	var out []Release
	return out, c.getJSON(ctx, "/api/v1/stacks/"+stackID+"/releases", &out)
}

// PromoteResult says what the promote started.
type PromoteResult struct {
	Env    string `json:"env"`
	Commit string `json:"commit"`
	Plan   string `json:"plan,omitempty"`
	Job    string `json:"job,omitempty"`
}

// Promote moves a built commit onto a rung. plan applies a waiting config plan
// first, in the same job.
func (c *Client) Promote(ctx context.Context, stackID, envSlug, commit, plan string, force bool) (PromoteResult, error) {
	var out PromoteResult
	return out, c.send(ctx, http.MethodPost,
		"/api/v1/stacks/"+stackID+"/envs/"+envSlug+"/promote",
		map[string]any{"commit": commit, "plan": plan, "force": force}, &out)
}

// --- environment lifecycle ---

// DeleteEnv tears an environment down. force is required once tiles are
// running there.
func (c *Client) DeleteEnv(ctx context.Context, envID string, force bool) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/envs/"+envID,
		map[string]bool{"force": force}, nil)
}

// ResetEnv tears a config-managed environment down so the next apply rebuilds
// it from the file.
func (c *Client) ResetEnv(ctx context.Context, envID string, force bool) error {
	return c.send(ctx, http.MethodPost, "/api/v1/envs/"+envID+"/reset",
		map[string]bool{"force": force}, nil)
}

// CopyEnv creates a new environment from an existing one: same tiles, nothing
// deployed.
func (c *Client) CopyEnv(ctx context.Context, envID, name string, ephemeral bool) (Env, error) {
	var out Env
	return out, c.send(ctx, http.MethodPost, "/api/v1/envs/"+envID+"/copy",
		map[string]any{"name": name, "ephemeral": ephemeral}, &out)
}

// PatchEnv changes an environment's colour or apply policy.
func (c *Client) PatchEnv(ctx context.Context, envID string, fields map[string]any) (Env, error) {
	var out Env
	return out, c.send(ctx, http.MethodPatch, "/api/v1/envs/"+envID, fields, &out)
}

// --- defaults cascade ---

// SettingKnob is one default: what applies at this level, which level decided
// it, and this level's own override when it has one.
type SettingKnob struct {
	Key    string  `json:"key"`
	Value  string  `json:"value"`
	Source string  `json:"source"`
	Own    *string `json:"own,omitempty"`
}

type SettingsLevel struct {
	Level  string        `json:"level"`
	Values []SettingKnob `json:"values"`
}

// settingsPath is the endpoint for one level. An empty id is the server.
func settingsPath(kind, id string) string {
	switch kind {
	case "org":
		return "/api/v1/orgs/" + id + "/settings"
	case "stack":
		return "/api/v1/stacks/" + id + "/settings"
	case "env":
		return "/api/v1/envs/" + id + "/settings"
	}
	return "/api/v1/settings"
}

// Settings reads one level of the defaults cascade.
func (c *Client) Settings(ctx context.Context, kind, id string) (SettingsLevel, error) {
	var out SettingsLevel
	return out, c.getJSON(ctx, settingsPath(kind, id), &out)
}

// SetSettings writes this level's own overrides. A nil value clears one back
// to inherit.
func (c *Client) SetSettings(ctx context.Context, kind, id string, fields map[string]*string) (SettingsLevel, error) {
	var out SettingsLevel
	return out, c.send(ctx, http.MethodPatch, settingsPath(kind, id), fields, &out)
}

// --- config export ---

// ExportStackConfig renders a stack's live state as the file that would
// produce it: the way out of panel-first and into config-as-code.
func (c *Client) ExportStackConfig(ctx context.Context, stackID string) ([]byte, error) {
	return c.getRaw(ctx, "/api/v1/stacks/"+stackID+"/config/export")
}

// ExportOrgConfig does the same for an organization.
func (c *Client) ExportOrgConfig(ctx context.Context, org string) ([]byte, error) {
	return c.getRaw(ctx, "/api/v1/orgs/"+org+"/config/export")
}

// --- members and invites ---

// Member is one person in an organization.
type Member struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name,omitempty"`
	Role   string `json:"role"`
}

// Invite is an open invitation. The URL is a credential: anyone holding it can
// join as the role it carries.
type Invite struct {
	ID        string    `json:"id"`
	Email     string    `json:"email,omitempty"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
	Used      bool      `json:"used"`
	URL       string    `json:"url"`
}

func (c *Client) Members(ctx context.Context, org string) ([]Member, error) {
	var out []Member
	return out, c.getJSON(ctx, "/api/v1/orgs/"+org+"/members", &out)
}

// AddMember invites rather than inserting a membership: a user row comes from
// accepting an invite.
func (c *Client) AddMember(ctx context.Context, org, email, role string, days int) (Invite, error) {
	var out Invite
	return out, c.send(ctx, http.MethodPost, "/api/v1/orgs/"+org+"/members",
		map[string]any{"email": email, "role": role, "expires_days": days}, &out)
}

func (c *Client) SetMemberRole(ctx context.Context, org, userID, role string) (Member, error) {
	var out Member
	return out, c.send(ctx, http.MethodPatch, "/api/v1/orgs/"+org+"/members/"+userID,
		map[string]string{"role": role}, &out)
}

func (c *Client) RemoveMember(ctx context.Context, org, userID string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/orgs/"+org+"/members/"+userID, nil, nil)
}

func (c *Client) Invites(ctx context.Context, org string) ([]Invite, error) {
	var out []Invite
	return out, c.getJSON(ctx, "/api/v1/orgs/"+org+"/invites", &out)
}

func (c *Client) AddInvite(ctx context.Context, org, email, role string, days int) (Invite, error) {
	var out Invite
	return out, c.send(ctx, http.MethodPost, "/api/v1/orgs/"+org+"/invites",
		map[string]any{"email": email, "role": role, "expires_days": days}, &out)
}

func (c *Client) DeleteInvite(ctx context.Context, org, id string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/orgs/"+org+"/invites/"+id, nil, nil)
}
