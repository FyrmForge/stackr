// Package githubapp is the GitHub App wrapper: app JWT, installation tokens,
// the manifest the connect flow POSTs to GitHub, clone credentials, and
// webhook verify + decode. Every function takes its credentials as arguments;
// nothing here touches the store, checks a session, or decides a status.
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const apiBase = "https://api.github.com"

// App is one connector's GitHub App credentials, already decoded.
// extract: decoding a connector row's config column belongs in leaf/connector.
type App struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
}

type cachedToken struct {
	token string
	exp   time.Time
}

type Client struct {
	baseURL string // stackr's own public URL, for the manifest's callback URLs
	apiURL  string // GitHub's API; a field so tests can point it at a fake
	http    *http.Client

	mu     sync.Mutex
	tokens map[string]cachedToken // connector id → installation token
}

func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiURL:  apiBase,
		http:    &http.Client{Timeout: 15 * time.Second},
		tokens:  map[string]cachedToken{},
	}
}

// Manifest returns the GitHub form action and the manifest JSON to POST there.
// state is "<connectorID>.<nonce>" and comes back on the callback. A non-empty
// ghOrg creates the app under that GitHub org (needs org admin): private apps
// install only on their owning account, so org repos need an org-owned app.
// extract: the nonce and the pending connector row belong in leaf/connector,
// which hands state in and checks it on the callback.
func (c *Client) Manifest(connectorID, ghOrg, state string) (action, manifest string, err error) {
	if len(connectorID) < 4 {
		return "", "", fmt.Errorf("githubapp: connector id %q too short", connectorID)
	}
	host := c.baseURL
	if u, e := url.Parse(c.baseURL); e == nil && u.Host != "" {
		host = u.Host
	}
	// App names are globally unique on GitHub; the suffix avoids clashes
	// between connectors and between installs on the same domain.
	name := "stackr-" + strings.SplitN(host, ":", 2)[0]
	if len(name) > 29 {
		name = name[:29]
	}
	name += "-" + connectorID[:4]
	b, err := json.Marshal(map[string]any{
		"name":         name,
		"url":          c.baseURL,
		"public":       false,
		"redirect_url": c.baseURL + "/settings/github/callback",
		"hook_attributes": map[string]any{
			"url": c.baseURL + "/hooks/connectors/" + connectorID,
		},
		// Fixed at app creation: widening later makes every owner re-approve
		// by hand, so checks/pull_requests/statuses are asked for up front.
		"default_permissions": map[string]string{
			"checks":        "read",
			"contents":      "read",
			"metadata":      "read",
			"packages":      "read", // ghcr.io pulls
			"pull_requests": "write",
			"statuses":      "write",
		},
		"default_events": []string{"pull_request", "push"},
	})
	if err != nil {
		return "", "", err
	}
	action = "https://github.com/settings/apps/new"
	if ghOrg != "" {
		action = "https://github.com/organizations/" + url.PathEscape(ghOrg) + "/settings/apps/new"
	}
	return action + "?state=" + url.QueryEscape(state), string(b), nil
}

// ConvertManifest trades the callback code for the app's credentials. One
// shot: GitHub invalidates the code on use, and answers 201, not 200.
func (c *Client) ConvertManifest(ctx context.Context, code string) (App, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiURL+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
	if err != nil {
		return App{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json") // no Authorization: the code is the credential
	res, err := c.http.Do(req)
	if err != nil {
		return App{}, err
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusCreated {
		return App{}, fmt.Errorf("github conversion failed: %s", res.Status)
	}
	var out App
	return out, json.Unmarshal(body, &out)
}

// Token returns a cached installation access token, minting one when missing
// or under 2 minutes from GitHub's own expires_at. key is the connector id.
// The lock guards only the map; minting runs unlocked so a slow GitHub call
// never blocks other connectors (worst case two goroutines mint at once,
// harmless). Uses the app's first installation: a private app installs only
// on its owning account.
func (c *Client) Token(ctx context.Context, key string, app App) (string, error) {
	c.mu.Lock()
	t, ok := c.tokens[key]
	c.mu.Unlock()
	if ok && time.Until(t.exp) > 2*time.Minute {
		return t.token, nil
	}
	jwt, err := signJWT(fmt.Sprint(app.ID), app.PEM)
	if err != nil {
		return "", err
	}
	var insts []struct {
		ID int64 `json:"id"`
	}
	if err := c.api(ctx, http.MethodGet, "/app/installations", jwt, &insts); err != nil {
		return "", err
	}
	if len(insts) == 0 {
		return "", fmt.Errorf("github app %s has no installations; install it on your account first", app.Slug)
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", insts[0].ID)
	if err := c.api(ctx, http.MethodPost, path, jwt, &tok); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.tokens[key] = cachedToken{tok.Token, tok.ExpiresAt}
	c.mu.Unlock()
	return tok.Token, nil
}

// api calls GitHub with a bearer (app JWT or installation token; GitHub takes
// Bearer for both) and decodes the JSON answer into out.
func (c *Client) api(ctx context.Context, method, path, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, nil)
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
	rb, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		return fmt.Errorf("github %s %s: %s", method, path, res.Status)
	}
	return json.Unmarshal(rb, out)
}

// signJWT builds a short-lived RS256 app JWT. Hand-rolled to avoid a jwt dep:
// base64url(header).base64url(claims) signed with the app key. iat = now-60
// absorbs clock skew; exp = now+540 stays under GitHub's 10-minute ceiling.
func signJWT(appID, pemKey string) (string, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return "", errors.New("invalid app private key")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		k, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return "", err
		}
		var ok bool
		if key, ok = k.(*rsa.PrivateKey); !ok {
			return "", errors.New("app key is not RSA")
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

// CloneAuth returns GIT_CONFIG_* environment lines that authenticate an HTTPS
// github.com fetch: env, not -c flags, so the token never lands in
// /proc/*/cmdline or in .git/config.
// extract: which connector a tile clones with (and refusing another org's
// connector named in a stack file) belongs in leaf/connector.
func CloneAuth(gitURL, token string) []string {
	if token == "" || !strings.HasPrefix(gitURL, "https://github.com/") {
		return nil
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}
}
