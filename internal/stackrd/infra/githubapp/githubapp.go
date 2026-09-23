// Package githubapp implements the GitHub App manifest flow for "github"
// connectors: each connector creates its own private GitHub App in one click,
// then mints short-lived installation tokens for cloning private repos and
// validating webhooks.
package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

const apiBase = "https://api.github.com"

// Config is the JSON stored in a github connector's config column.
type Config struct {
	AppID         int64  `json:"app_id"`
	Slug          string `json:"slug"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
	State         string `json:"state,omitempty"` // pending manifest-flow nonce
}

// Connected reports whether the manifest flow finished for this config.
func (c Config) Connected() bool { return c.AppID != 0 }

// ParseConfig decodes a connector's config column.
func ParseConfig(raw string) Config {
	var c Config
	_ = json.Unmarshal([]byte(raw), &c)
	return c
}

type cachedToken struct {
	token string
	exp   time.Time
}

// Connectors is the connectors table's owner. service.ConnectorService
// satisfies it; an interface because service/ is built on this package.
type Connectors interface {
	Create(ctx context.Context, cn *repo.Connector) error
	Save(ctx context.Context, cn *repo.Connector) error
}

type Client struct {
	store repo.Store
	// conns owns the connector row this flow creates and then fills in with
	// the credentials GitHub hands back.
	conns Connectors
	// set owns the settings row the rendered plan preview is parked in
	// between the webhook that renders it and the comment that shows it.
	set Settings
	// deploys owns the deployments table the PR comment reports from.
	deploys Deploys
	baseURL string
	http    *http.Client

	mu     sync.Mutex
	tokens map[string]cachedToken // connector id → installation token
}

func New(store repo.Store, conns Connectors, baseURL string) *Client {
	return &Client{store: store, conns: conns, baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Timeout: 15 * time.Second}, tokens: map[string]cachedToken{}}
}

// Settings is the owner of the settings row this reads the stored plan
// preview back out of. service.SettingsService satisfies it; an interface
// because that package is built on this one.
type Settings interface {
	Value(ctx context.Context, key string) (string, error)
}

// UseSettings hands over that owner. A setter because this client is built
// during boot, before the services are, and the value is not read until a
// pull-request webhook arrives.
func (c *Client) UseSettings(s Settings) { c.set = s }

// Deploys is the read the PR comment needs: what happened to each tile in the
// preview environment. service.Rows satisfies it.
type Deploys interface {
	Latest(ctx context.Context, tileID string) (*repo.Deployment, error)
}

// UseDeploys hands over that owner, for the same reason as UseSettings.
func (c *Client) UseDeploys(d Deploys) { c.deploys = d }

// Begin creates a pending github connector in the org and returns the
// GitHub form action plus the manifest JSON to POST there. The connector id
// rides inside the state so the callback can find it. A non-empty ghOrg
// creates the app under that GitHub organization (requires org admin),
// private apps are only installable on their owning account, so org repos
// need an org-owned app.
func (c *Client) Begin(ctx context.Context, orgID, ghOrg string) (action, manifest string, err error) {
	buf := make([]byte, 16)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	nonce := fmt.Sprintf("%x", buf)
	cn := &repo.Connector{
		ID:        uuid.New().String(),
		OrgID:     orgID,
		Provider:  "github",
		Name:      "GitHub (connecting…)",
		CreatedAt: time.Now().UTC(),
	}
	cfg, _ := json.Marshal(Config{State: nonce})
	cn.Config = string(cfg)
	if err = c.conns.Create(ctx, cn); err != nil {
		return "", "", err
	}

	host := c.baseURL
	if u, e := url.Parse(c.baseURL); e == nil && u.Host != "" {
		host = u.Host
	}
	// App names are globally unique on GitHub, random suffix avoids clashes
	// between connectors (and instances on the same domain).
	name := "stackr-" + strings.SplitN(host, ":", 2)[0]
	if len(name) > 29 {
		name = name[:29]
	}
	name += "-" + cn.ID[:4]
	m := map[string]any{
		"name":         name,
		"url":          c.baseURL,
		"public":       false,
		"redirect_url": c.baseURL + "/settings/github/callback",
		"hook_attributes": map[string]any{
			"url": c.baseURL + "/hooks/connectors/" + cn.ID,
		},
		"default_permissions": map[string]string{
			"checks":        "read", // wait_for_ci reads Actions check runs
			"contents":      "read",
			"metadata":      "read",
			"packages":      "read", // ghcr.io image pulls
			"pull_requests": "write",
			"statuses":      "write",
		},
		"default_events": []string{"pull_request", "push"},
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", "", err
	}
	action = "https://github.com/settings/apps/new"
	if ghOrg != "" {
		action = "https://github.com/organizations/" + url.PathEscape(ghOrg) + "/settings/apps/new"
	}
	return action + "?state=" + cn.ID + "." + nonce, string(b), nil
}

// Complete exchanges the callback code for app credentials and stores them on
// the pending connector identified by state ("<connectorID>.<nonce>"). It
// returns the connector so the caller can send the browser back to the org
// that owns it.
func (c *Client) Complete(ctx context.Context, code, state string) (*repo.Connector, error) {
	id, nonce, ok := strings.Cut(state, ".")
	if !ok {
		return nil, fmt.Errorf("bad state")
	}
	cn, err := c.store.GetConnector(ctx, id)
	if err != nil {
		return nil, err
	}
	if cn == nil || cn.Provider != "github" {
		return nil, fmt.Errorf("unknown connector")
	}
	cfg := ParseConfig(cn.Config)
	if cfg.State == "" || cfg.State != nonce {
		return nil, fmt.Errorf("state mismatch")
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		apiBase+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("github conversion failed: %s", res.Status)
	}
	var out struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(Config{
		AppID: out.ID, Slug: out.Slug, PEM: out.PEM, WebhookSecret: out.WebhookSecret,
	})
	if err != nil {
		return nil, err
	}
	cn.Name = "GitHub · " + out.Slug
	cn.Config = string(raw)
	if err := c.conns.Save(ctx, cn); err != nil {
		return nil, err
	}
	return cn, nil
}

// Token returns a cached installation access token for the connector,
// minting a new one when missing or near expiry. The lock only guards the
// cache map, minting happens unlocked so a slow GitHub call never blocks
// other connectors (worst case two goroutines mint concurrently; harmless).
// uses the app's first installation, fine for a personal app.
func (c *Client) Token(ctx context.Context, cn *repo.Connector) (string, error) {
	cfg := ParseConfig(cn.Config)
	if !cfg.Connected() {
		return "", nil
	}
	c.mu.Lock()
	t, ok := c.tokens[cn.ID]
	c.mu.Unlock()
	if ok && time.Until(t.exp) > 2*time.Minute {
		return t.token, nil
	}
	jwt, err := signJWT(fmt.Sprint(cfg.AppID), cfg.PEM)
	if err != nil {
		return "", err
	}
	var insts []struct {
		ID int64 `json:"id"`
	}
	if err := c.api(ctx, "GET", "/app/installations", jwt, &insts); err != nil {
		return "", err
	}
	if len(insts) == 0 {
		return "", fmt.Errorf("github app %s has no installations; install it on your account first", cfg.Slug)
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", insts[0].ID)
	if err := c.api(ctx, "POST", path, jwt, &tok); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.tokens[cn.ID] = cachedToken{tok.Token, tok.ExpiresAt}
	c.mu.Unlock()
	return tok.Token, nil
}

func (c *Client) api(ctx context.Context, method, path, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		return fmt.Errorf("github %s %s: %s", method, path, res.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// signJWT builds a short-lived RS256 app JWT. Hand-rolled to avoid a jwt dep:
// a JWT is just base64url(header).base64url(claims) signed with the app key.
func signJWT(appID, pemKey string) (string, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return "", fmt.Errorf("invalid app private key")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		k, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return "", err
		}
		var ok bool
		if key, ok = k.(*rsa.PrivateKey); !ok {
			return "", fmt.Errorf("app key is not RSA")
		}
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	now := time.Now().Unix()
	unsigned := enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"iat": now - 60, "exp": now + 540, "iss": appID})
	h := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// CloneAuth returns GIT_CONFIG_* environment lines that authenticate an
// HTTPS github.com fetch for the tile, env, not -c flags, so the token
// never shows in /proc/*/cmdline or .git/config. Resolution: the tile's
// connector, else the org's sole github connector.
// silent nil on any failure, the clone then runs unauthenticated
// and its own error surfaces in the deploy log.
func (c *Client) CloneAuth(ctx context.Context, tile *repo.Tile) []string {
	if !strings.HasPrefix(tile.GitURL, "https://github.com/") {
		return nil
	}
	cn := c.connectorForTile(ctx, tile)
	if cn == nil {
		return nil
	}
	tok, err := c.Token(ctx, cn)
	if err != nil || tok == "" {
		return nil
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}
}

func (c *Client) connectorForTile(ctx context.Context, tile *repo.Tile) *repo.Connector {
	stack, err := c.store.GetStack(ctx, tile.StackID)
	if err != nil || stack == nil {
		return nil
	}
	if tile.ConnectorID != "" {
		cn, _ := c.store.GetConnector(ctx, tile.ConnectorID)
		// The id can come straight out of a stack file (tiles.connector), and
		// this connector mints a GitHub installation token. Without the org
		// check a file naming another org's connector id would clone that
		// org's private repositories. Every other reference in config is
		// scoped; this one was not.
		if cn == nil || cn.OrgID != stack.OrgID {
			// Refused, not missing. Returning nil here makes the clone run
			// unauthenticated, which fails on a private repository with a
			// git error nobody can trace back to this decision — so say so.
			slog.Error("connector refused: not this stack's organisation",
				"tile", tile.ID, "connector", tile.ConnectorID, "org", stack.OrgID)
			return nil
		}
		return cn
	}
	// Fallback until the repo picker sets connector_id: the org's single
	// connected github connector.
	cns, err := c.store.ListConnectorsByOrg(ctx, stack.OrgID)
	if err != nil {
		return nil
	}
	var found *repo.Connector
	for i := range cns {
		if cns[i].Provider == "github" && ParseConfig(cns[i].Config).Connected() {
			if found != nil {
				return nil // ambiguous, require an explicit connector_id
			}
			found = &cns[i]
		}
	}
	return found
}
