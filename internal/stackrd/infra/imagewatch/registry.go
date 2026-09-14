// Package imagewatch polls container registries for new image digests and
// drives the per-tile update policy (notify / auto-redeploy).
package imagewatch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// acceptManifests is the full set docker pull negotiates. Anything less and a
// multi-arch image answers with a per-arch manifest digest that never matches
// what docker recorded locally, a permanent phantom "update available".
const acceptManifests = "application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json"

// ParseRef splits an image reference into its registry host, repository path
// and tag. Digest-pinned refs (name@sha256:...) return ok=false, there is
// nothing to watch.
func ParseRef(ref string) (host, repoPath, tag string, ok bool) {
	if ref == "" || strings.Contains(ref, "@") {
		return "", "", "", false
	}
	host, rest := "registry-1.docker.io", ref
	if i := strings.Index(ref, "/"); i > 0 {
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

// Client asks registries for manifest digests. Zero value is usable.
type Client struct {
	HTTP *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Digest returns the registry's current manifest digest for ref, via a HEAD
// that doesn't count toward Docker Hub pull limits. user/pass may be empty
// (public images); they also authenticate token requests for private ones.
func (c *Client) Digest(ctx context.Context, ref, user, pass string) (string, error) {
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
		resp, err := c.http().Do(req)
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
			tok, err := c.token(ctx, params, repoPath, user, pass)
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
			basic := "Basic " + basicAuth(user, pass)
			if resp, err = head(basic); err != nil {
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

// token runs the Bearer-challenge flow: GET realm?service=&scope= with
// optional basic auth (anonymous works for public Hub/ghcr images).
func (c *Client) token(ctx context.Context, params map[string]string, repoPath, user, pass string) (string, error) {
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
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint %s: %s", realm, resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
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
