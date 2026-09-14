package registry

// Garbage collection and the catalog reads behind the org registry page.
//
// The registry runs with REGISTRY_STORAGE_DELETE_ENABLED, so deleting a tag
// unlinks its manifest but leaves the blobs on disk. Reclaiming them is a
// separate sweep inside the registry container, which is why it rides the
// admin maintenance cleanup toggle rather than running on every delete: it
// takes a read lock on the whole store.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// GarbageCollect reclaims blobs no manifest references any more. Runs in the
// registry's own container: the storage driver is local to it.
func GarbageCollect(ctx context.Context, rt *runtime.Runtime) (string, error) {
	cs, err := rt.ListByLabel(ctx, "stackr.registry", "true")
	if err != nil {
		return "", err
	}
	if len(cs) == 0 {
		return "", fmt.Errorf("the managed registry is not running")
	}
	return rt.Exec(ctx, cs[0].ID, []string{
		"/bin/registry", "garbage-collect", "--delete-untagged=true",
		"/etc/docker/registry/config.yml",
	})
}

// Client reads the registry's v2 API as the admin root credential. Only the
// panel uses it: a tenant's view is filtered by namespace before it is
// rendered, never by asking the registry nicely.
type Client struct {
	base   string
	reg    *repo.Registry
	signer *Signer
	http   *http.Client
}

// NewClient talks to the registry over the stkr overlay (PanelAddr), so no TLS
// and no proxy hop. Not the push address: that is localhost:<port>, which is
// the manager's host and not the inside of the panel's own container. The signer is what mints the tokens
// it authenticates with: the registry runs REGISTRY_AUTH=token and no longer
// answers basic auth at all. A nil signer makes every call fail with that
// said, rather than 401ing with nothing to explain it.
func NewClient(reg *repo.Registry, signer *Signer) *Client {
	return &Client{base: "http://" + PanelAddr(reg), reg: reg, signer: signer,
		http: &http.Client{Timeout: 15 * time.Second}}
}

// repoName is docker's repository grammar narrowed to what this install
// actually stores: envnet flattens org, stack and tile into
// <org>_<stack>_<tile>, so a legitimate name has no slash in it. Refusing the
// slash outright is what makes the registry's 301-to-cleaned-path redirect
// unreachable, rather than filtering the ".." that reaches it.
var repoName = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// tagRef is docker's tag grammar. Both checks live here rather than in each
// handler: every path below builds a URL out of these two values, and a caller
// that forgets is a traversal.
var tagRef = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// ValidRepoName is checkRepo for callers that want to refuse a name with
// their own status code rather than surfacing a registry error. The rail below
// stays, as the thing no caller can forget.
func ValidRepoName(name string) bool { return repoName.MatchString(name) }

// ValidTag is ValidRepoName for a tag.
func ValidTag(tag string) bool { return tagRef.MatchString(tag) }

func checkRepo(name string) error {
	if !repoName.MatchString(name) {
		return fmt.Errorf("not a repository name: %q", name)
	}
	return nil
}

func checkRef(name, tag string) error {
	if err := checkRepo(name); err != nil {
		return err
	}
	if !tagRef.MatchString(tag) {
		return fmt.Errorf("not a tag: %q", tag)
	}
	return nil
}

// authz mints a root-scoped bearer token for one call. The subject is the
// managed registry row's own user, which is the credential the token endpoint
// already treats as root (handlers/web/registrytoken.go).
func (c *Client) authz(access []Access) (string, error) {
	if c.signer == nil {
		return "", fmt.Errorf("registry token signing is not configured")
	}
	tok, _, err := c.signer.Sign(c.reg.Username, access)
	if err != nil {
		return "", err
	}
	return "Bearer " + tok, nil
}

// repoAccess is the scope for one repository. GrantAll is not a route to it:
// it only keeps repository scopes, so the catalog read builds its own.
func repoAccess(name string, actions ...string) []Access {
	return []Access{{Type: "repository", Name: name, Actions: actions}}
}

func (c *Client) get(ctx context.Context, path string, access []Access, out any) error {
	authz, err := c.authz(access)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authz)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil // an empty registry has no catalog; that is not an error
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("registry %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Images lists the repositories inside one org's namespace, shortest first.
func (c *Client) Images(ctx context.Context, orgSlug string) ([]string, error) {
	var body struct {
		Repositories []string `json:"repositories"`
	}
	// n is the page size; one page of 1000 is every image an install of this
	// size has. add Link-header paging when somebody hits it.
	catalog := []Access{{Type: "registry", Name: "catalog", Actions: []string{"*"}}}
	if err := c.get(ctx, "/v2/_catalog?n=1000", catalog, &body); err != nil {
		return nil, err
	}
	ns := Namespace(orgSlug)
	var out []string
	for _, r := range body.Repositories {
		if strings.HasPrefix(r, ns) {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Tags lists one repository's tags. name is the full repository path, which
// the caller must already have checked belongs to the org it is rendering.
func (c *Client) Tags(ctx context.Context, name string) ([]string, error) {
	if err := checkRepo(name); err != nil {
		return nil, err
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := c.get(ctx, "/v2/"+name+"/tags/list", repoAccess(name, "pull"), &body); err != nil {
		return nil, err
	}
	sort.Strings(body.Tags)
	return body.Tags, nil
}

// TagInfo is what the panel shows per tag.
type TagInfo struct {
	Tag    string    `json:"tag"`
	Digest string    `json:"digest"`
	Size   int64     `json:"size"`
	Pushed time.Time `json:"pushed,omitempty"`
}

// Tag reads one tag's manifest: the digest a delete needs, and the size of the
// layers it accounts for.
func (c *Client) Tag(ctx context.Context, name, tag string) (TagInfo, error) {
	info := TagInfo{Tag: tag}
	if err := checkRef(name, tag); err != nil {
		return info, err
	}
	authz, err := c.authz(repoAccess(name, "pull"))
	if err != nil {
		return info, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/v2/"+name+"/manifests/"+tag, nil)
	if err != nil {
		return info, err
	}
	req.Header.Set("Authorization", authz)
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return info, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("registry manifest %s:%s: %s", name, tag, resp.Status)
	}
	info.Digest = resp.Header.Get("Docker-Content-Digest")
	var m struct {
		Config struct {
			Size int64 `json:"size"`
		} `json:"config"`
		Layers []struct {
			Size int64 `json:"size"`
		} `json:"layers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return info, err
	}
	info.Size = m.Config.Size
	for _, l := range m.Layers {
		info.Size += l.Size
	}
	return info, nil
}

// DeleteTag removes a tag's manifest. The blobs stay until the next garbage
// collection, which rides the admin maintenance cleanup toggle.
func (c *Client) DeleteTag(ctx context.Context, name, tag string) error {
	if err := checkRef(name, tag); err != nil {
		return err
	}
	info, err := c.Tag(ctx, name, tag)
	if err != nil {
		return err
	}
	if info.Digest == "" {
		return fmt.Errorf("registry gave no digest for %s:%s", name, tag)
	}
	// By digest: the registry's delete endpoint does not take a tag, and a
	// name+tag URL there deletes whatever the tag happens to point at now.
	authz, err := c.authz(repoAccess(name, "pull", "delete"))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.base+"/v2/"+name+"/manifests/"+info.Digest, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authz)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("delete %s:%s: %s: %s", name, tag, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
