package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Client is a thin HTTP client over the stackr /api/v1 surface. It stays
// deliberately small: enough to prove auth end-to-end. A generated SDK over the
// committed doc/openapi.json can replace the per-endpoint methods later.
type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

// NewClient builds a client from a saved config.
func NewClient(c Config) *Client {
	return &Client{
		BaseURL: strings.TrimRight(c.URL, "/"),
		Key:     c.Key,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.Key)
	return c.HTTP.Do(req)
}

// Stack is a stack visible to the key.
type Stack struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Env is an environment within a stack.
type Env struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// Stacks lists stacks visible to the key. It doubles as the auth check: a
// 401/403 surfaces as an error.
func (c *Client) Stacks(ctx context.Context) ([]Stack, error) {
	var out []Stack
	return out, c.getJSON(ctx, "/api/v1/stacks", &out)
}

// Envs lists the environments of a stack.
func (c *Client) Envs(ctx context.Context, stackID string) ([]Env, error) {
	var out []Env
	return out, c.getJSON(ctx, "/api/v1/stacks/"+stackID+"/envs", &out)
}

// App is a deployable service within a stack/environment.
type App struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	EnvID  string `json:"env_id"`
	Status string `json:"status"`
}

// Apps lists apps, optionally scoped to a stack and/or environment.
func (c *Client) Apps(ctx context.Context, stack, env string) ([]App, error) {
	q := url.Values{}
	if stack != "" {
		q.Set("stack", stack)
	}
	if env != "" {
		q.Set("env", env)
	}
	var out []App
	return out, c.getJSON(ctx, "/api/v1/apps?"+q.Encode(), &out)
}

// Deploy triggers a deployment and returns the new deployment id.
func (c *Client) Deploy(ctx context.Context, appID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/apps/"+appID+"/deploy", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", c.Key)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		Deployment string `json:"deployment"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Deployment, nil
}

// Deployment is one deployment's state, polled by `deploy` while waiting.
type Deployment struct {
	ID        string `json:"id"`
	AppID     string `json:"app_id"`
	Status    string `json:"status"` // waiting_ci queued running done error cancelled
	Trigger   string `json:"trigger"`
	CommitSHA string `json:"commit_sha"`
	ImageTag  string `json:"image_tag"`
	Error     string `json:"error,omitempty"`
}

// GetDeployment reads one deployment's current state.
func (c *Client) GetDeployment(ctx context.Context, id string) (Deployment, error) {
	var out Deployment
	return out, c.getJSON(ctx, "/api/v1/deployments/"+id, &out)
}

// Logs returns the last `tail` log lines as a single string.
func (c *Client) Logs(ctx context.Context, appID string, tail int) (string, error) {
	var out struct {
		Lines string `json:"lines"`
	}
	return out.Lines, c.getJSON(ctx, fmt.Sprintf("/api/v1/apps/%s/logs?tail=%d", appID, tail), &out)
}

// LogEvent is one live log line with its origin stream ("stdout"/"stderr").
type LogEvent struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// FollowLogs streams live logs (SSE), calling fn per line until the context is
// cancelled or the stream closes. A 204 means the app isn't running (nothing
// to follow). Structured events let the caller choose text or NDJSON.
func (c *Client) FollowLogs(ctx context.Context, appID string, tail int, fn func(LogEvent) error) error {
	u := fmt.Sprintf("%s/api/v1/apps/%s/logs?follow=1&tail=%d", c.BaseURL, appID, tail)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.Key)
	resp, err := (&http.Client{}).Do(req) // no timeout: this is a long-lived stream
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return apiError(resp.Status, body)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue // ": ping" heartbeats and blank event separators
		}
		// Payloads are marked "O <text>" (stdout) or "E <text>" (stderr).
		ev := LogEvent{Stream: "stdout", Line: payload}
		if len(payload) >= 2 && (payload[0] == 'O' || payload[0] == 'E') && payload[1] == ' ' {
			if payload[0] == 'E' {
				ev.Stream = "stderr"
			}
			ev.Line = payload[2:]
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return sc.Err()
}

// DB is a provisioned database (shared instance).
type DB struct {
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
	Path         string  `json:"path"` // colon-path address, set on create
}

// AppCreate is the payload for creating a service or cron app.
type AppCreate struct {
	Name           string            `json:"name"`
	EnvSlug        string            `json:"env_slug,omitempty"`
	Kind           string            `json:"kind,omitempty"`
	SourceType     string            `json:"source_type,omitempty"`
	Image          string            `json:"image,omitempty"`
	GitURL         string            `json:"git_url,omitempty"`
	GitBranch      string            `json:"git_branch,omitempty"`
	DockerfilePath string            `json:"dockerfile_path,omitempty"`
	BuildContext   string            `json:"build_context,omitempty"`
	Port           int               `json:"port,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Schedule       string            `json:"schedule,omitempty"`
	Command        string            `json:"command,omitempty"`
	TimeoutMinutes int               `json:"timeout_minutes,omitempty"`
	Connector      string            `json:"connector_id,omitempty"`
}

// CreateStack creates a stack. orgID may be empty unless you belong to many.
func (c *Client) CreateStack(ctx context.Context, name, desc, orgID string) (Stack, error) {
	var out Stack
	return out, c.send(ctx, http.MethodPost, "/api/v1/stacks",
		map[string]string{"name": name, "description": desc, "org_id": orgID}, &out)
}

// CreateEnv creates an environment in a stack.
func (c *Client) CreateEnv(ctx context.Context, stackID, name string) (Env, error) {
	var out Env
	return out, c.send(ctx, http.MethodPost, "/api/v1/stacks/"+stackID+"/envs",
		map[string]string{"name": name}, &out)
}

// CreateApp creates a service or cron app in a stack.
func (c *Client) CreateApp(ctx context.Context, stackID string, in AppCreate) (App, error) {
	var out App
	return out, c.send(ctx, http.MethodPost, "/api/v1/stacks/"+stackID+"/apps", in, &out)
}

// CreateDB provisions a database in a stack. scope ("", env, stack, org) sets
// how widely a shared instance can be provisioned from.
func (c *Client) CreateDB(ctx context.Context, stackID, name, engine, envSlug, scope string) (DB, error) {
	var out DB
	return out, c.send(ctx, http.MethodPost, "/api/v1/stacks/"+stackID+"/dbs",
		map[string]string{"name": name, "engine": engine, "env_slug": envSlug, "scope": scope}, &out)
}

// Provision is one logical database an app holds inside a shared instance.
type Provision struct {
	ID         string `json:"id"`
	InstanceID string `json:"instance_id"`
	ConsumerID string `json:"consumer_id"`
	DBName     string `json:"db_name"`
	Secret     string `json:"secret"`
	Status     string `json:"status"`
}

// AttachProvision points an app at an existing logical database (same env).
func (c *Client) AttachProvision(ctx context.Context, appID, provisionID, envVar string) (Provision, error) {
	var out Provision
	return out, c.send(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/provisions/attach",
		map[string]string{"provision_id": provisionID, "env_var": envVar}, &out)
}

// ListProvisions lists the logical databases an app holds.
func (c *Client) ListProvisions(ctx context.Context, appID string) ([]Provision, error) {
	var out []Provision
	return out, c.getJSON(ctx, "/api/v1/apps/"+appID+"/provisions", &out)
}

// DotenvQuote quotes a value when bare text would not survive a round trip:
// anything with whitespace, quotes, newlines, or shell-significant characters.
func DotenvQuote(v string) string {
	if v == "" {
		return `""`
	}
	if !strings.ContainsAny(v, " \t\n\"'#$`\\") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(v) + `"`
}

// Var is one app variable. Value may hold a ${{ ... }} reference; Secret marks
// it as masked on read without the secrets:read scope.
type Var struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Secret bool   `json:"secret,omitempty"`
	// Generate mints the value on the server, with the same generator the
	// config file's `default: generated` uses. A name that already has a value
	// is left alone: generating over a live secret would break everything
	// reading it.
	Generate bool `json:"generate,omitempty"`
	Length   int  `json:"length,omitempty"`
}

type varsBody struct {
	Vars []Var `json:"variables"`
}

// VarScope names which variable set to act on. Exactly one field is set: an
// app's own variables, an environment's own values, or the stack/org set those
// cascade down from.
type VarScope struct {
	App   string
	Env   string
	Stack string
	Org   string
}

// path is the variables endpoint for the scope.
func (s VarScope) path() (string, error) {
	switch {
	case s.App != "":
		return "/api/v1/apps/" + s.App + "/variables", nil
	case s.Env != "":
		return "/api/v1/envs/" + s.Env + "/variables", nil
	case s.Stack != "":
		return "/api/v1/stacks/" + s.Stack + "/variables", nil
	case s.Org != "":
		return "/api/v1/orgs/" + s.Org + "/variables", nil
	}
	return "", fmt.Errorf("no variable scope given")
}

// Label names the scope for human output.
func (s VarScope) Label() string {
	switch {
	case s.App != "":
		return "app " + s.App
	case s.Env != "":
		return "environment " + s.Env
	case s.Stack != "":
		return "stack " + s.Stack
	default:
		return "org " + s.Org
	}
}

// VarsAt reads one variable set.
func (c *Client) VarsAt(ctx context.Context, scope VarScope) ([]Var, error) {
	path, err := scope.path()
	if err != nil {
		return nil, err
	}
	var out varsBody
	return out.Vars, c.getJSON(ctx, path, &out)
}

// SetVarsAt is SetVars against any scope, an app, a stack, or an org. The
// read-modify-write is the same at every level, because every one of those
// endpoints replaces the whole set.
func (c *Client) SetVarsAt(ctx context.Context, scope VarScope, vars []Var) ([]Var, error) {
	path, err := scope.path()
	if err != nil {
		return nil, err
	}
	cur, err := c.VarsAt(ctx, scope)
	if err != nil {
		return nil, err
	}
	merged := map[string]Var{}
	for _, v := range cur {
		merged[v.Name] = v
	}
	for _, v := range vars {
		merged[v.Name] = v
	}
	names := make([]string, 0, len(merged))
	for n := range merged {
		names = append(names, n)
	}
	sort.Strings(names)
	body := varsBody{}
	for _, n := range names {
		body.Vars = append(body.Vars, merged[n])
	}
	var out varsBody
	err = c.send(ctx, http.MethodPut, path, body, &out)
	return out.Vars, err
}

// ResolvedVars returns the environment a deploy would produce, references
// expanded. Fails rather than omitting when the key can't read a secret.
func (c *Client) ResolvedVars(ctx context.Context, appID string) ([]Var, error) {
	var out varsBody
	return out.Vars, c.getJSON(ctx, "/api/v1/apps/"+appID+"/variables/resolved", &out)
}

// Domain is a host attached to an app.
type Domain struct {
	ID            string `json:"id"`
	Host          string `json:"host"`
	Path          string `json:"path"`
	ContainerPort int    `json:"container_port"`
	HTTPS         bool   `json:"https"`
}

// AddDomain attaches a host to an app and updates the proxy. port 0 = the app's
// own port; https terminates TLS.
func (c *Client) AddDomain(ctx context.Context, appID, host string, port int, https bool) (Domain, error) {
	var out Domain
	return out, c.send(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/domains",
		map[string]any{"host": host, "container_port": port, "https": https}, &out)
}

// Domains lists an app's hosts.
func (c *Client) Domains(ctx context.Context, appID string) ([]Domain, error) {
	var out []Domain
	return out, c.getJSON(ctx, "/api/v1/apps/"+appID+"/domains", &out)
}

// DeleteDomain removes a host by id and re-syncs the proxy.
func (c *Client) DeleteDomain(ctx context.Context, domainID string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/domains/"+domainID, nil, nil)
}

// DomainResource is a host owned at a level (instance/org/stack) that tiles
// generate per-environment hostnames under.
type DomainResource struct {
	ID                  string `json:"id"`
	Level               string `json:"level"`
	OwnerID             string `json:"owner_id"`
	Host                string `json:"host"`
	IncludeEnvOnDefault bool   `json:"include_env_on_default"`
}

// DomainResources lists the resources visible to the key's user.
func (c *Client) DomainResources(ctx context.Context) ([]DomainResource, error) {
	var out []DomainResource
	return out, c.getJSON(ctx, "/api/v1/domain-resources", &out)
}

// AddDomainResource creates a resource at level (owner: org/stack id, ignored
// for instance level).
func (c *Client) AddDomainResource(ctx context.Context, host, level, owner string, includeEnvOnDefault bool) (DomainResource, error) {
	var out DomainResource
	return out, c.send(ctx, http.MethodPost, "/api/v1/domain-resources",
		map[string]any{"host": host, "level": level, "owner": owner, "include_env_on_default": includeEnvOnDefault}, &out)
}

// DeleteDomainResource removes a resource by id.
func (c *Client) DeleteDomainResource(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/domain-resources/"+id, nil, nil)
}

// Volume is a persistent volume mounted into an app.
type Volume struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	MountPath  string `json:"mount_path"`
	VolumeName string `json:"volume_name"`
	Status     string `json:"status"`
}

// VolumeCreate is a volume's full shape. VolumeName adopts an existing docker
// volume instead of creating one named after the tile, which is how data from
// a pre-stackr deployment is kept rather than started over.
type VolumeCreate struct {
	Name       string `json:"name"`
	MountPath  string `json:"mount_path"`
	VolumeName string `json:"volume_name,omitempty"`
	MaxSizeMB  int    `json:"max_size_mb,omitempty"`
}

// AddVolumeWith mounts a volume with every field set.
func (c *Client) AddVolumeWith(ctx context.Context, appID string, in VolumeCreate) (Volume, error) {
	var out Volume
	return out, c.send(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/volumes", in, &out)
}

// AddVolume creates a volume mounted into an app at mountPath and redeploys it.
func (c *Client) AddVolume(ctx context.Context, appID, name, mountPath string) (Volume, error) {
	var out Volume
	return out, c.send(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/volumes",
		map[string]string{"name": name, "mount_path": mountPath}, &out)
}

// Volumes lists an app's volumes.
func (c *Client) Volumes(ctx context.Context, appID string) ([]Volume, error) {
	var out []Volume
	return out, c.getJSON(ctx, "/api/v1/apps/"+appID+"/volumes", &out)
}

// DeleteVolume removes a volume by id (its data is preserved) and redeploys.
func (c *Client) DeleteVolume(ctx context.Context, volumeID string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/volumes/"+volumeID, nil, nil)
}

// send does an authenticated POST/PUT with a JSON body and decodes any 2xx body.
func (c *Client) send(ctx context.Context, method, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(resp.Body)
		return apiError(resp.Status, rb)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// APIError is a non-2xx API response: the human message plus the HTTP status
// and machine code, kept so `--json` can emit a structured error object.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string   { return e.Message }
func (e *APIError) HTTPStatus() int { return e.StatusCode }
func (e *APIError) APICode() string { return e.Code }

// apiError turns an error response into something a person reads. The API
// wraps its message in {"error":{"code":…,"message":…}}, and printing that
// envelope verbatim makes every failure look like a bug in the client. Falls
// back to the raw body when it is not the expected shape.
func apiError(status string, body []byte) error {
	code, _, _ := strings.Cut(status, " ")
	n, _ := strconv.Atoi(code)
	var env struct {
		Error struct {
			Code    json.Number `json:"code"` // number or string on the wire
			Message string      `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		return &APIError{StatusCode: n, Code: env.Error.Code.String(), Message: env.Error.Message}
	}
	if b := strings.TrimSpace(string(body)); b != "" {
		return &APIError{StatusCode: n, Message: fmt.Sprintf("%s: %s", status, b)}
	}
	return &APIError{StatusCode: n, Message: status}
}

// Forward opens one tunnel to a container port on a tile and returns it as a
// net.Conn, one call per local connection the forward command accepts. The
// second return is the container port actually used, which matters when port
// is 0 and the server picked the tile's own.
//
// The shared HTTP client is deliberately not used: its 15s timeout would cut
// the tunnel off mid-session.
func (c *Client) Forward(ctx context.Context, tileID string, port int) (net.Conn, int, error) {
	return c.forward(ctx, tileID, port, false)
}

// ForwardPresence opens the session websocket `stackr forward` holds for its
// whole lifetime: it registers who has a forward open (the canvas card), and
// doubles as the startup probe, a missing scope, stopped tile or bad port
// fails here. Reading from it blocks until the server drops the session; the
// forward command uses that to re-register after a server restart.
func (c *Client) ForwardPresence(ctx context.Context, tileID string, port int) (net.Conn, int, error) {
	return c.forward(ctx, tileID, port, true)
}

func (c *Client) forward(ctx context.Context, tileID string, port int, presence bool) (net.Conn, int, error) {
	u := fmt.Sprintf("%s/api/v1/tiles/%s/forward", c.BaseURL, tileID)
	q := url.Values{}
	if port > 0 {
		q.Set("port", strconv.Itoa(port))
	}
	if presence {
		q.Set("presence", "1")
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	h := http.Header{}
	h.Set("x-api-key", c.Key)
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPHeader: h,
		HTTPClient: &http.Client{}, // no timeout: long-lived
	})
	if err != nil {
		// The server refuses before upgrading (not running, no port, bad
		// scope), so the status body carries the real reason.
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			if msg := strings.TrimSpace(string(body)); msg != "" {
				return nil, 0, fmt.Errorf("%s: %s", resp.Status, msg)
			}
			return nil, 0, fmt.Errorf("%s", resp.Status)
		}
		return nil, 0, err
	}
	used, _ := strconv.Atoi(resp.Header.Get("X-Stackr-Port"))
	if used == 0 {
		// Only reachable if the header is lost in transit. Failing here beats
		// binding a random local port and printing "-> tile:0" as if it worked.
		if port == 0 {
			_ = ws.CloseNow()
			return nil, 0, fmt.Errorf("server did not report the forwarded port; pass --port explicitly")
		}
		used = port
	}
	return websocket.NetConn(ctx, ws, websocket.MessageBinary), used, nil
}

// DBs lists databases visible to the key. Separate from Apps because the API
// splits the two, though both are tiles and both are forwardable.
func (c *Client) DBs(ctx context.Context) ([]DB, error) {
	var out []DB
	err := c.getJSON(ctx, "/api/v1/dbs", &out)
	return out, err
}

// DeleteDB drops a shared instance. force is required once consumers hold
// slices on it, without it the server refuses rather than destroying their data.
func (c *Client) DeleteDB(ctx context.Context, dbID string, force bool) error {
	path := "/api/v1/dbs/" + dbID
	if force {
		path += "?force=true"
	}
	return c.send(ctx, http.MethodDelete, path, nil, nil)
}

// Target is what an infra path names: a shared instance or one slice of one.
type Target struct {
	Kind       string `json:"kind"` // "instance" | "slice"
	ID         string `json:"id"`
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
	Path       string `json:"path"` // fully-qualified, for confirmation prompts
}

// Slice is one logical database or bucket cut from a shared instance.
type Slice struct {
	ID         string   `json:"id"`
	InstanceID string   `json:"instance_id"`
	Name       string   `json:"name"`
	Slug       string   `json:"slug"`
	Status     string   `json:"status"`
	Public     bool     `json:"public"`
	Consumers  []string `json:"consumers"`
}

// Resolve turns an infra path into what it names. envID is the linked
// environment, which is what lets the path be relative ("app-db" rather than
// org:stack:env:app-db); the server completes it, so nothing about the
// hierarchy is cached on this side.
func (c *Client) Resolve(ctx context.Context, path, envID string) (Target, error) {
	var out Target
	err := c.send(ctx, http.MethodPost, "/api/v1/resolve",
		map[string]string{"path": path, "env_id": envID}, &out)
	return out, err
}

// InstanceSlices lists the slices cut from one shared instance.
func (c *Client) InstanceSlices(ctx context.Context, dbID string) ([]Slice, error) {
	var out []Slice
	err := c.getJSON(ctx, "/api/v1/dbs/"+dbID+"/provisions", &out)
	return out, err
}

// ProvisionSlice cuts a slice out of an instance with no consumer attached.
//
// envID says which environment the slice belongs to, which decides what can
// ever consume it, a stack- or org-scoped instance serves several, and a
// slice cut into the wrong one can never be attached.
func (c *Client) ProvisionSlice(ctx context.Context, dbID, name, envID string, public bool) (Slice, error) {
	var out Slice
	err := c.send(ctx, http.MethodPost, "/api/v1/dbs/"+dbID+"/provisions",
		map[string]any{"name": name, "env_id": envID, "public": public}, &out)
	return out, err
}

// ForkSlice copies a slice into a new one on the same instance. The copy runs
// server-side and can take a while on a large database, so this call is the
// one place the CLI's 15s timeout is deliberately not enough, see the longer
// client it builds.
func (c *Client) ForkSlice(ctx context.Context, provisionID, name string) (Slice, error) {
	slow := &Client{BaseURL: c.BaseURL, Key: c.Key, HTTP: &http.Client{Timeout: 30 * time.Minute}}
	var out Slice
	err := slow.send(ctx, http.MethodPost, "/api/v1/provisions/"+provisionID+"/fork",
		map[string]string{"name": name}, &out)
	return out, err
}

// DeleteSlice destroys one logical database or bucket, and with it every
// consumer's link to it.
func (c *Client) DeleteSlice(ctx context.Context, provisionID string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/provisions/"+provisionID, nil, nil)
}

// PatchApp updates tile settings. The body is a map rather than a struct
// because the API distinguishes absent from cleared by which keys arrive, a
// struct with omitempty could never send a deliberate zero.
func (c *Client) PatchApp(ctx context.Context, appID string, fields map[string]any) (App, error) {
	var out App
	return out, c.send(ctx, http.MethodPatch, "/api/v1/apps/"+appID, fields, &out)
}

// DeleteApp removes a tile: its containers, its proxy route and its row.
func (c *Client) DeleteApp(ctx context.Context, appID string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/apps/"+appID, nil, nil)
}

// DeleteStack removes a stack and everything in it.
func (c *Client) DeleteStack(ctx context.Context, stackID string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/stacks/"+stackID, nil, nil)
}

// DBSettings are the instance knobs a PATCH can change. Pointers so "not
// mentioned" stays distinct from "set to zero", 0 is a meaningful value for
// every one of them.
type DBSettings struct {
	ExternalPort *int     `json:"external_port,omitempty"`
	Scope        *string  `json:"scope,omitempty"`
	CPULimit     *float64 `json:"cpu_limit,omitempty"`
	MemLimitMB   *int     `json:"mem_limit_mb,omitempty"`
	Image        *string  `json:"image,omitempty"`
	ShmSizeMB    *int     `json:"shm_size_mb,omitempty"`
}

// Storage is a server-scoped share/pool with its declared sub-paths.
type StorageEntry struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Slug      string        `json:"slug"`
	Backend   string        `json:"backend"`
	Status    string        `json:"status"`
	StatusMsg string        `json:"status_msg"`
	Org       string        `json:"org,omitempty"`
	Paths     []StoragePath `json:"paths"`
}

type StoragePath struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Subpath  string `json:"subpath"`
	ForcedRO bool   `json:"forced_ro"`
	Volume   string `json:"volume"`
}

type StorageCreate struct {
	Name     string `json:"name"`
	Backend  string `json:"backend"`
	Address  string `json:"address,omitempty"`
	Export   string `json:"export,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Opts     string `json:"opts,omitempty"`
	Org      string `json:"org,omitempty"`
}

type StoragePathCreate struct {
	Name     string `json:"name"`
	Subpath  string `json:"subpath,omitempty"`
	ForcedRO bool   `json:"forced_ro,omitempty"`
}

func (c *Client) ListStorage(ctx context.Context) ([]StorageEntry, error) {
	var out []StorageEntry
	return out, c.getJSON(ctx, "/api/v1/storage", &out)
}

func (c *Client) CreateStorage(ctx context.Context, in StorageCreate) (StorageEntry, error) {
	var out StorageEntry
	return out, c.send(ctx, http.MethodPost, "/api/v1/storage", in, &out)
}

func (c *Client) DeleteStorage(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/storage/"+id, nil, nil)
}

func (c *Client) CreateStoragePath(ctx context.Context, storageID string, in StoragePathCreate) (StoragePath, error) {
	var out StoragePath
	return out, c.send(ctx, http.MethodPost, "/api/v1/storage/"+storageID+"/paths", in, &out)
}

func (c *Client) DeleteStoragePath(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/storage/paths/"+id, nil, nil)
}

// ProxyConfig is the traefik escape-hatch state.
type ProxyConfig struct {
	StaticOverride string            `json:"static_override"`
	OverrideActive bool              `json:"override_active"`
	CurrentStatic  string            `json:"current_static"`
	Entries        map[string]string `json:"entries"`
}

func (c *Client) ProxyConfig(ctx context.Context) (ProxyConfig, error) {
	var out ProxyConfig
	return out, c.getJSON(ctx, "/api/v1/proxy/config", &out)
}

func (c *Client) PutProxyOverride(ctx context.Context, yamlBody string) (ProxyConfig, error) {
	var out ProxyConfig
	return out, c.send(ctx, http.MethodPut, "/api/v1/proxy/static-override", map[string]string{"yaml": yamlBody}, &out)
}

func (c *Client) PutProxyEntry(ctx context.Context, name, yamlBody string) (ProxyConfig, error) {
	var out ProxyConfig
	return out, c.send(ctx, http.MethodPut, "/api/v1/proxy/entries/"+name, map[string]string{"yaml": yamlBody}, &out)
}

func (c *Client) DeleteProxyEntry(ctx context.Context, name string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/proxy/entries/"+name, nil, nil)
}

// RunApp fires one immediate run of a cron or function tile.
func (c *Client) RunApp(ctx context.Context, appID string) error {
	return c.send(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/run", nil, nil)
}

// PatchDB updates a shared instance's port, scope, or resource limits.
func (c *Client) PatchDB(ctx context.Context, dbID string, s DBSettings) (DB, error) {
	var out DB
	return out, c.send(ctx, http.MethodPatch, "/api/v1/dbs/"+dbID, s, &out)
}

// Destination is an S3 target backups are written to. Secrets are write-only,
// the server never renders them back, so they are absent here on purpose.
type Destination struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	Region   string `json:"region"`
	OrgID    string `json:"org_id"`
	Global   bool   `json:"global"`
	Shared   bool   `json:"shared"`
}

// DestinationCreate is the create body. OrgID empty makes the destination
// server-wide, which the API allows for admins only.
type DestinationCreate struct {
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Region    string `json:"region,omitempty"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	OrgID     string `json:"org_id,omitempty"`
	// Shared offers a server-wide destination to every organization. The
	// bucket credentials go with it, so it is off unless asked for.
	Shared bool `json:"shared,omitempty"`
}

// Destinations lists the targets the key can use: its own orgs', plus the
// server-wide ones an admin has shared.
func (c *Client) Destinations(ctx context.Context) ([]Destination, error) {
	var out []Destination
	return out, c.getJSON(ctx, "/api/v1/backup-destinations", &out)
}

// AddDestination creates a destination. The server dials the bucket before
// storing it, so bad credentials fail here rather than at the first run.
func (c *Client) AddDestination(ctx context.Context, in DestinationCreate) (Destination, error) {
	var out Destination
	return out, c.send(ctx, http.MethodPost, "/api/v1/backup-destinations", in, &out)
}

// SetDestinationShared offers a server-wide destination to every organization,
// or takes it back. Taking it back is refused while an org still schedules
// against it, and the error names the tiles.
func (c *Client) SetDestinationShared(ctx context.Context, id string, shared bool) (Destination, error) {
	var out Destination
	return out, c.send(ctx, http.MethodPatch, "/api/v1/backup-destinations/"+id,
		map[string]bool{"shared": shared}, &out)
}

// DeleteDestination removes a destination. Archives already in the bucket stay.
func (c *Client) DeleteDestination(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/backup-destinations/"+id, nil, nil)
}

// Backup is a schedule: what to dump, where to, how often, how much to keep.
type Backup struct {
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

// BackupRun is one execution of a schedule, and the unit a restore names.
type BackupRun struct {
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

// Backups lists a tile's schedules. There is no list-all route, a backup is
// only ever addressed through the tile it protects.
func (c *Client) Backups(ctx context.Context, tileID string) ([]Backup, error) {
	var out []Backup
	return out, c.getJSON(ctx, "/api/v1/tiles/"+tileID+"/backups", &out)
}

// CreateBackup adds a schedule to a tile. The body is a map for the same reason
// PatchApp's is: keep_latest 0 and enabled false are meaningful values.
func (c *Client) CreateBackup(ctx context.Context, tileID string, fields map[string]any) (Backup, error) {
	var out Backup
	return out, c.send(ctx, http.MethodPost, "/api/v1/tiles/"+tileID+"/backups", fields, &out)
}

// PatchBackup changes a schedule's destination, cron, retention or enabled flag.
func (c *Client) PatchBackup(ctx context.Context, id string, fields map[string]any) (Backup, error) {
	var out Backup
	return out, c.send(ctx, http.MethodPatch, "/api/v1/backups/"+id, fields, &out)
}

// DeleteBackup removes a schedule. Archives already written are not touched.
func (c *Client) DeleteBackup(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodDelete, "/api/v1/backups/"+id, nil, nil)
}

// BackupRuns lists a schedule's run history, which is also where a restore's
// run id comes from.
func (c *Client) BackupRuns(ctx context.Context, id string) ([]BackupRun, error) {
	var out []BackupRun
	return out, c.getJSON(ctx, "/api/v1/backups/"+id+"/runs", &out)
}

// RunBackup triggers a schedule immediately and returns the run it started.
func (c *Client) RunBackup(ctx context.Context, id string) (BackupRun, error) {
	var out BackupRun
	return out, c.send(ctx, http.MethodPost, "/api/v1/backups/"+id+"/run", nil, &out)
}

// Restore writes an archive back over the live database or volume. Needs the
// backups:restore scope, which is granted separately from backups:write.
func (c *Client) Restore(ctx context.Context, backupID, runID string) error {
	return c.send(ctx, http.MethodPost, "/api/v1/backups/"+backupID+"/restore",
		map[string]string{"run_id": runID}, nil)
}

// PlanChange is one line of a config plan's diff.
type PlanChange struct {
	Kind string `json:"kind"`
	Env  string `json:"env"`
	// Scope is set instead of Env on a row for a value the config declares
	// (stack | org): the value lives at the scope, not in an environment.
	Scope    string `json:"scope,omitempty"`
	Tile     string `json:"tile,omitempty"`
	Field    string `json:"field,omitempty"`
	Old      string `json:"old,omitempty"`
	New      string `json:"new,omitempty"`
	Note     string `json:"note,omitempty"`
	Destroys bool   `json:"destroys,omitempty"`
}

// Plan is a config-as-code plan. Changes/Destructive are populated on the
// single-plan routes; a listing returns headers only.
type Plan struct {
	ID        string       `json:"id"`
	StackID   string       `json:"stack_id"`
	EnvSlug   string       `json:"env_slug,omitempty"`
	CommitSHA string       `json:"commit_sha,omitempty"`
	Status    string       `json:"status"`
	Summary   string       `json:"summary"`
	Error     string       `json:"error,omitempty"`
	CreatedAt string       `json:"created_at"`
	DecidedAt string       `json:"decided_at,omitempty"`
	Changes   []PlanChange `json:"changes,omitempty"`
	Errors    []string     `json:"errors,omitempty"`
	Warnings  []string     `json:"warnings,omitempty"`
	// GenSecrets: secrets an apply would mint, real work, so a preview with
	// only these is not "empty" for --detailed-exitcode.
	GenSecrets  []string `json:"gen_secrets,omitempty"`
	Destructive bool     `json:"destructive"`
}

// Plans lists a stack's recent config plans, newest first.
// Org config-as-code plans (org id in place of stack id, same wire shape).
func (c *Client) OrgPlans(ctx context.Context, orgID string) ([]Plan, error) {
	var out []Plan
	return out, c.getJSON(ctx, "/api/v1/orgs/"+orgID+"/config/plans", &out)
}

func (c *Client) OrgPlanNow(ctx context.Context, orgID string) (Plan, error) {
	var out Plan
	return out, c.send(ctx, http.MethodPost, "/api/v1/orgs/"+orgID+"/config/plan", nil, &out)
}

// OrgPlanPreview posts the org config file and returns the throwaway plan,
// the org twin of PlanPreview. Org files have no include:, so no bundle map.
func (c *Client) OrgPlanPreview(ctx context.Context, orgID, main string) (Plan, error) {
	var out Plan
	return out, c.send(ctx, http.MethodPost, "/api/v1/orgs/"+orgID+"/config/plan-preview",
		map[string]any{"main": main}, &out)
}

func (c *Client) ApproveOrgPlan(ctx context.Context, planID string) (Plan, error) {
	var out Plan
	return out, c.send(ctx, http.MethodPost, "/api/v1/org-config/plans/"+planID+"/approve", nil, &out)
}

func (c *Client) RejectOrgPlan(ctx context.Context, planID string) (Plan, error) {
	var out Plan
	return out, c.send(ctx, http.MethodPost, "/api/v1/org-config/plans/"+planID+"/reject", nil, &out)
}

func (c *Client) Plans(ctx context.Context, stackID string, limit int) ([]Plan, error) {
	path := "/api/v1/stacks/" + stackID + "/config/plans"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var out []Plan
	return out, c.getJSON(ctx, path, &out)
}

// Plan fetches one plan with its full change list.
func (c *Client) Plan(ctx context.Context, planID string) (Plan, error) {
	var out Plan
	return out, c.getJSON(ctx, "/api/v1/config/plans/"+planID, &out)
}

// PlanNow re-plans a stack against its bound config repo. It returns every
// plan the sweep produced, the stack-scoped one first, then one per
// environment pinned to its own config branch.
func (c *Client) PlanNow(ctx context.Context, stackID string) ([]Plan, error) {
	var out []Plan
	return out, c.send(ctx, http.MethodPost, "/api/v1/stacks/"+stackID+"/config/plan", nil, &out)
}

// PlanPreview posts a local config bundle and returns the throwaway plan the
// server computed against live state. Nothing is stored server-side, the
// returned plan has no ID and cannot be approved.
func (c *Client) PlanPreview(ctx context.Context, stackID, main string, files map[string]string, env string) (Plan, error) {
	body := map[string]any{"main": main}
	if len(files) > 0 {
		body["files"] = files
	}
	if env != "" {
		body["env"] = env
	}
	var out Plan
	return out, c.send(ctx, http.MethodPost, "/api/v1/stacks/"+stackID+"/config/plan-preview", body, &out)
}

// ApprovePlan applies a pending plan.
func (c *Client) ApprovePlan(ctx context.Context, planID string) (Plan, error) {
	var out Plan
	return out, c.send(ctx, http.MethodPost, "/api/v1/config/plans/"+planID+"/approve", nil, &out)
}

// RejectPlan closes a pending plan without applying it.
func (c *Client) RejectPlan(ctx context.Context, planID string) (Plan, error) {
	var out Plan
	return out, c.send(ctx, http.MethodPost, "/api/v1/config/plans/"+planID+"/reject", nil, &out)
}

// getJSON does an authenticated GET and decodes a 200 body into v.
func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	resp, err := c.get(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return apiError(resp.Status, body)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// getRaw reads a non-JSON body (the config exports, which are YAML a person
// puts in a repo).
func (c *Client) getRaw(ctx context.Context, path string) ([]byte, error) {
	resp, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp.Status, body)
	}
	return body, nil
}
