# Extract: `infra/imagewatch` → `service/internal/registry`

- **Source:** `infra/imagewatch/registry.go`, `infra/imagewatch/registry_test.go`
- **Commit:** `c2423f0`
- **Taken:** ref parsing, the HEAD-manifest digest call, the Accept set, the `Www-Authenticate` challenge flow (bearer token exchange + basic).
- **Cut:** the package doc's policy sentence; the one-field `Client` struct and its nil-check accessor, collapsed to a package var.
- **Cuts belong to:** `flow/imagewatch`.

## Kept code

```go
// Package registry asks a container registry what is behind a tag.
// extract: dropped "and drives the per-tile update policy (notify /
// auto-redeploy)" from the package doc, belongs in flow/imagewatch.
package registry

// The full set docker pull negotiates. Anything less and a multi-arch image
// answers with a per-arch manifest digest that never matches what docker
// recorded locally: a permanent phantom "update available".
const acceptManifests = "application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json"

// HTTP serves every call here; tests swap it.
var HTTP = &http.Client{Timeout: 15 * time.Second}

// ParseRef splits an image reference into registry host, repository path and
// tag. Digest-pinned refs (name@sha256:…) return ok=false: nothing to watch.
func ParseRef(ref string) (host, repoPath, tag string, ok bool) {
	if ref == "" || strings.Contains(ref, "@") {
		return "", "", "", false
	}
	host, rest := "registry-1.docker.io", ref
	if i := strings.Index(ref, "/"); i > 0 {
		// a first segment with a dot or colon, or "localhost", is a host
		if first := ref[:i]; strings.ContainsAny(first, ".:") || first == "localhost" {
			host, rest = first, ref[i+1:]
		}
	}
	if host == "docker.io" || host == "index.docker.io" {
		host = "registry-1.docker.io"
	}
	tag = "latest"
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		rest, tag = rest[:i], rest[i+1:]
	}
	if host == "registry-1.docker.io" && !strings.Contains(rest, "/") {
		rest = "library/" + rest
	}
	if rest == "" || tag == "" {
		return "", "", "", false
	}
	return host, rest, tag, true
}

// Digest returns the registry's current manifest digest for ref, via a HEAD
// that does not count toward Docker Hub pull limits. user/pass may be empty
// (public images); they also authenticate the token request for private ones.
func Digest(ctx context.Context, ref, user, pass string) (string, error) {
	host, repoPath, tag, ok := ParseRef(ref)
	if !ok {
		return "", fmt.Errorf("unwatchable image ref %q", ref)
	}
	manifestURL := "https://" + host + "/v2/" + repoPath + "/manifests/" + tag
	head := func(authz string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, manifestURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", acceptManifests)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		resp, err := HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		_ = resp.Body.Close() // HEAD: headers are the payload
		return resp, nil
	}
	resp, err := head("")
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		scheme, params := parseChallenge(resp.Header.Get("Www-Authenticate"))
		switch scheme {
		case "bearer":
			tok, err := token(ctx, params, repoPath, user, pass)
			if err != nil {
				return "", err
			}
			if resp, err = head("Bearer " + tok); err != nil {
				return "", err
			}
		case "basic":
			if user == "" {
				return "", fmt.Errorf("registry %s: unauthorized", host)
			}
			basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
			if resp, err = head("Basic " + basic); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("registry %s: unauthorized", host)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry %s: %s for %s:%s", host, resp.Status, repoPath, tag)
	}
	dg := resp.Header.Get("Docker-Content-Digest")
	if dg == "" {
		return "", fmt.Errorf("registry %s: no digest header for %s:%s", host, repoPath, tag)
	}
	return dg, nil
}

// token runs the bearer-challenge flow: GET realm?service=&scope= with
// optional basic auth (anonymous works for public Hub/ghcr images).
func token(ctx context.Context, params map[string]string, repoPath, user, pass string) (string, error) {
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("bearer challenge without realm")
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repoPath + ":pull"
	}
	q.Set("scope", scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint %s: %s", realm, resp.Status)
	}
	var body struct {
		Token       string `json:"token"`        // Hub
		AccessToken string `json:"access_token"` // ghcr and friends
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	switch {
	case body.Token != "":
		return body.Token, nil
	case body.AccessToken != "":
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("token endpoint %s: empty token", realm)
}

// parseChallenge splits `Bearer realm="…",service="…"` into scheme + params.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		params[strings.ToLower(k)] = strings.Trim(v, `"`)
	}
	return strings.ToLower(scheme), params
}
```

Keep the old test's table. It pins: `nginx` → `registry-1.docker.io`
`library/nginx` `latest`; `docker.io/nginx` normalises to the same;
`localhost:5000/app` keeps the port as host; `x@sha256:…` and `""` are not ok;
`Bearer realm="…",service="…"` → `bearer` + both params; `Basic …` → `basic`.

## Notes for the builder

- **Credentials are parameters, not a lookup.** The old resolver lived in the
  service row: ghcr.io asked the GitHub App client, everything else got empty
  strings. The per-tile pull credential belongs in `flow/imagewatch`, not here.
- **Mode 2 does not exist.** No tag listing, no `Link`-header pagination, no
  semver sort. New code — but it reuses `ParseRef`, `token` and `parseChallenge`
  unchanged; only `/v2/<repo>/tags/list` and its JSON are new.
- **Arch resolution is a real decision, not an oversight.** This client
  deliberately does *not* resolve a manifest list to one arch: the full Accept
  set gets the *index* digest, which is what docker records locally. Plan step 3
  wants "resolved to this box's arch" — a second GET of the index plus a
  `platform` match, one more call per image, and a digest that will not equal a
  local one. Pick one; keep whatever the flow compares against consistent.
- **Rate-limit handling is the HEAD, and that is all of it.** No 429,
  `Retry-After` or backoff exists. If v1 wants it, it goes here — the status code
  never leaves `Digest`.
- **Errors are bare strings**, so a caller cannot tell unauthorized from
  no-such-tag from registry-down. Plan step 5 (errors land on the image row and
  retry, never surface as an update) needs an auth-vs-transient type, added here.
- `ParseRef` refusing a digest-pinned ref *is* the plan's "fully pinned, no
  policy: nothing happens, by choice".

Size: source 230 lines, extract 209 lines (167 Go — a near-total take; what got filtered was comments, the wrapper struct, and the test table).
