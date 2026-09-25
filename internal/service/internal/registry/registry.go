// Package registry asks a container registry what is behind a tag and which
// tags exist. Credentials are parameters; which image to ask about, and what
// an answer means, is the caller's. One call per image: the caller dedupes.
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The full set docker pull negotiates. Anything less and a multi-arch image
// answers with a per-arch digest that never matches what docker recorded
// locally: a permanent phantom "update available". So Digest is the index
// digest, arch-blind (DECIDE 6).
const acceptManifests = "application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json"

// HTTP serves every call here; tests swap it.
var HTTP = &http.Client{Timeout: 15 * time.Second}

// Typed failures, so a caller never reads an error as "no update".
var (
	ErrBadRef       = errors.New("registry: not a watchable image ref")
	ErrUnauthorized = errors.New("registry: unauthorized")
	ErrNotFound     = errors.New("registry: no such repository or tag")
)

// RateLimited is a 429. RetryAfter is zero when the registry did not say.
type RateLimited struct{ RetryAfter time.Duration }

func (e RateLimited) Error() string {
	return "registry: rate limited, retry after " + e.RetryAfter.String()
}

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
// that does not count toward Docker Hub pull limits. user/pass may be empty.
func Digest(ctx context.Context, ref, user, pass string) (string, error) {
	host, repoPath, tag, ok := ParseRef(ref)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrBadRef, ref)
	}
	resp, err := do(ctx, http.MethodHead, "https://"+host+"/v2/"+repoPath+"/manifests/"+tag, repoPath, user, pass)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close() // HEAD: headers are the payload
	dg := resp.Header.Get("Docker-Content-Digest")
	if dg == "" {
		return "", fmt.Errorf("registry %s: no digest header for %s:%s", host, repoPath, tag)
	}
	return dg, nil
}

// Tags lists every tag of ref's repository (ref's own tag is ignored),
// following Link pagination.
// ponytail: no page cap; add one if a registry ever loops its Link header.
func Tags(ctx context.Context, ref, user, pass string) ([]string, error) {
	host, repoPath, _, ok := ParseRef(ref)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrBadRef, ref)
	}
	next, _ := url.Parse("https://" + host + "/v2/" + repoPath + "/tags/list?n=1000")
	var tags []string
	for next != nil {
		resp, err := do(ctx, http.MethodGet, next.String(), repoPath, user, pass)
		if err != nil {
			return nil, err
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("registry %s: tag list: %w", host, err)
		}
		tags = append(tags, body.Tags...)
		link := nextLink(resp.Header.Get("Link"))
		if link == "" {
			break
		}
		if next, err = next.Parse(link); err != nil {
			return nil, err
		}
	}
	return tags, nil
}

// nextLink pulls the target out of `<url>; rel="next"`.
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		target, params, ok := strings.Cut(part, ";")
		if ok && strings.Contains(params, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(target), "<>")
		}
	}
	return ""
}

// do sends one request, answering a 401 challenge (bearer token exchange or
// basic) once, and maps the status to a typed error. The caller closes the
// body of a returned response.
func do(ctx context.Context, method, rawURL, repoPath, user, pass string) (*http.Response, error) {
	send := func(authz string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", acceptManifests)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		return HTTP.Do(req)
	}
	resp, err := send("")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		scheme, params := parseChallenge(resp.Header.Get("Www-Authenticate"))
		var authz string
		switch {
		case scheme == "bearer":
			tok, err := token(ctx, params, repoPath, user, pass)
			if err != nil {
				return nil, err
			}
			authz = "Bearer " + tok
		case scheme == "basic" && user != "":
			authz = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
		default:
			return nil, ErrUnauthorized
		}
		if resp, err = send(authz); err != nil {
			return nil, err
		}
	}
	if err := statusErr(resp); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func statusErr(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusTooManyRequests:
		secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return RateLimited{RetryAfter: time.Duration(secs) * time.Second}
	}
	return fmt.Errorf("registry %s: %s", resp.Request.URL.Host, resp.Status)
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
	if err := statusErr(resp); err != nil {
		return "", err
	}
	var body struct {
		Token       string `json:"token"`        // Hub
		AccessToken string `json:"access_token"` // ghcr and friends
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
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
