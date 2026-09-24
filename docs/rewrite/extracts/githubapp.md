# infra/githubapp

Source: `infra/githubapp/` — `githubapp.go` 393, `ci.go` 123, `feedback.go` 416 (tests read: `githubapp_test.go`, `connector_scope_test.go`)
Commit: c2423f0
Taken: app JWT, installation-token mint + cache, manifest build + callback exchange, clone auth env, what the manifest registers for webhooks
Cut: `ci.go`, `feedback.go`, the connector row (create/save/lookup), the org-scope refusal, the nonce/state check, tile→connector resolution
Cuts belong to: `service/internal/leaf/connector`; checks and PR comments to a later wave

Target `service/internal/githubapp`: a wrapper. Every function takes its
credentials as arguments; nothing here touches the store, checks a session, or
decides a status.

## The wrapper

```go
// Package githubapp is the GitHub App wrapper: app JWT, installation tokens,
// the manifest the connect flow POSTs to GitHub, and clone credentials.
package githubapp

const apiBase = "https://api.github.com"

// App is one connector's GitHub App credentials, already decoded.
// extract: dropped Config/ParseConfig/Connected — they decode a connector
// row's config column, belongs in leaf/connector.
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
	baseURL string
	http    *http.Client

	mu     sync.Mutex
	tokens map[string]cachedToken // connector id → installation token
}

func New(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Timeout: 15 * time.Second}, tokens: map[string]cachedToken{}}
}

// extract: dropped New's repo.Store / Connectors / Settings / Deploys fields
// and the UseSettings/UseDeploys late-binding setters — the old client owned
// four tables. Rows belong in leaf/connector; the settings and deployment
// reads went out with feedback.go.
```

## Manifest (also the webhook registration)

```go
// Manifest returns the GitHub form action and the manifest JSON to POST there.
// state is "<connectorID>.<nonce>" and comes back on the callback. A non-empty
// ghOrg creates the app under that GitHub org (needs org admin): private apps
// install only on their owning account, so org repos need an org-owned app.
func (c *Client) Manifest(connectorID, ghOrg, state string) (action, manifest string, err error) {
	host := c.baseURL
	if u, e := url.Parse(c.baseURL); e == nil && u.Host != "" {
		host = u.Host
	}
	// App names are globally unique on GitHub; the suffix avoids clashes
	// between connectors and between instances on the same domain.
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
		"default_permissions": map[string]string{
			"checks": "read", "contents": "read", "metadata": "read",
			"packages": "read", // ghcr.io pulls
			"pull_requests": "write", "statuses": "write",
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
	return action + "?state=" + state, string(b), nil
}

// extract: dropped Begin's crypto/rand nonce and the repo.Connector it created
// ("GitHub (connecting…)", provider "github", config {state: nonce}) —
// belongs in leaf/connector, which hands the state string in.
```

## Callback exchange

```go
// ConvertManifest trades the callback code for the app's credentials. One
// shot: GitHub invalidates the code on use, and answers 201, not 200.
func (c *Client) ConvertManifest(ctx context.Context, code string) (App, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		apiBase+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
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

// extract: dropped Complete's GetConnector, the provider check, the
// cfg.State != nonce comparison, the "GitHub · <slug>" rename and the Save.
// The state check is not optional — it is what stops a stray callback from
// filling in someone else's pending row. leaf/connector owns it.
```

## Installation token

```go
// Token returns a cached installation access token, minting one when missing
// or near expiry. key is the connector id. The lock guards only the map;
// minting runs unlocked so a slow GitHub call never blocks other connectors
// (worst case two goroutines mint at once, harmless). Uses the app's first
// installation — fine for a private, single-account app.
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
	if err := c.api(ctx, "GET", "/app/installations", jwt, nil, &insts); err != nil {
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
	if err := c.api(ctx, "POST", path, jwt, nil, &tok); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.tokens[key] = cachedToken{tok.Token, tok.ExpiresAt}
	c.mu.Unlock()
	return tok.Token, nil
}

// extract: dropped the Connected() short-circuit returning ("", nil) for a
// half-connected connector, belongs in leaf/connector.

// api calls GitHub with a bearer — app JWT or installation token — optionally
// JSON-encoding in and decoding out.
// extract: merged the old api (JWT, no body) with feedback.go's tokAPI
// (token, body). The same function written twice.
func (c *Client) api(ctx context.Context, method, path, bearer string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, body)
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
	if out == nil {
		return nil
	}
	return json.Unmarshal(rb, out)
}
```

## App JWT

```go
// signJWT builds a short-lived RS256 app JWT. Hand-rolled to avoid a jwt dep:
// a JWT is base64url(header).base64url(claims) signed with the app key.
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
```

Carry its test: generate a 2048-bit key, sign, split on `.`, verify the
signature against the public key, assert `iss` is the app id.

## Clone URL with token

```go
// CloneAuth returns GIT_CONFIG_* environment lines that authenticate an HTTPS
// github.com fetch — env, not -c flags, so the token never lands in
// /proc/*/cmdline or in .git/config.
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

// extract: dropped connectorForTile — GetStack, GetConnector,
// ListConnectorsByOrg and the "sole connected github connector" fallback,
// belongs in leaf/connector.
```

Carry the rule inside `connectorForTile` — a fix, not a preference. It refused
a connector whose `OrgID` differed from the stack's and logged that at error
level rather than returning nil silently. A tile's `connector` field comes
straight out of a stack file, so without the check a file naming another org's
connector id clones that org's private repos with that org's token.
`connector_scope_test.go` is the regression test; it belongs beside the
resolution in `leaf/connector`. Ambiguity (two connected github connectors, no
explicit id) returned nil and demanded an explicit id.

## Webhooks — not in this package

`githubapp/` at c2423f0 has no receive, no signature verify, no event parsing.
The row assumed it lived here. What this package does settle:

- delivery URL, fixed at app creation: `<baseURL>/hooks/connectors/<connectorID>`,
  so the receiver identifies the connector from the path, not the payload
- `webhook_secret`, returned by the manifest conversion and stored per
  connector — the HMAC key
- subscribed events: `push` and `pull_request` only

**DECIDE:** the handler behind `/hooks/connectors/{id}` is outside this
directory and was not read. It needs its own extract before the webhook half of
this row can be built.

## Notes for the builder

**Token TTLs.** Installation tokens last an hour; the cache stores GitHub's own
`expires_at` and re-mints under 2 minutes remaining, so no local TTL is
guessed. The app JWT is minted per call, never cached: `iat = now-60` absorbs
clock skew against GitHub, `exp = now+540` is 9 minutes against GitHub's
10-minute ceiling.

**Header names.** Every API call sends `Accept: application/vnd.github+json`
and `Authorization: Bearer <app JWT | installation token>` — GitHub takes
`Bearer` for both, which is why one helper covers them. The manifest conversion
POST sends no `Authorization` and answers `201 Created`. Bodies are read
through `io.LimitReader(res.Body, 1<<20)`; keep that. The receiver will want
`X-Hub-Signature-256` (hex HMAC-SHA256 over the raw body, compared with
`hmac.Equal`, never `==`), `X-GitHub-Event` and `X-GitHub-Delivery` — read the
body once, verify, then parse.

**Event types.** The manifest subscribes `push` and `pull_request`.
`installation` and `installation_repositories` are delivered to an app's hook
regardless of that list, so the receiver needs a default branch that drops
unknown types quietly rather than erroring.

**What a PR env needed from the event.** Evidenced by what the cut feedback
path read back, so the shape is fixed even though the parser is missing: the
ephemeral environment is `type: "ephemeral"`, `slug: "pr-<number>"` — the PR
number is the only join key between GitHub and the env. The tile's `git_url` is
the `https://github.com/owner/repo` form, and `owner/repo` is what every API
path interpolates. `pull_request.head.sha` is the commit a deployment records;
the cut code re-fetched it with `GET /repos/{owner}/{repo}/pulls/{number}`
rather than trusting the payload, which is also what makes a stale
`synchronize` delivery harmless. Actions: `opened`/`reopened`/`synchronize`
create or update the env, `closed` tears it down. A `push` carries a ref and a
head sha and updates the tracked branch's environment.

**Permissions are set once.** The manifest asks for `checks:read`,
`pull_requests:write` and `statuses:write`, which only the cut `ci.go` and
`feedback.go` used. Keep them anyway: permissions are fixed when the app is
created, and widening them later makes every connector owner re-approve the app
on GitHub by hand — `ci.go` carried a named error (`ErrNoChecksPerm`) for
exactly that migration.

Size: source 932 lines, extract 344 lines
