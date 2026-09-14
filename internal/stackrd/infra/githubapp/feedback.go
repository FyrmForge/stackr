package githubapp

// GitHub-facing devex: repo listing for the picker, GHCR pull credentials,
// and PR feedback (commit status + one sticky preview comment per PR).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/gitlog"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Repo is a repository visible to a connector's installation.
type Repo struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// ListRepos lists repositories the connector's app is installed on.
// first 100 only, paginate when someone actually has more.
func (c *Client) ListRepos(ctx context.Context, cn *repo.Connector) ([]Repo, error) {
	tok, err := c.Token(ctx, cn)
	if err != nil || tok == "" {
		return nil, err
	}
	var out struct {
		Repositories []Repo `json:"repositories"`
	}
	if err := c.tokAPI(ctx, tok, "GET", "/installation/repositories?per_page=100", nil, &out); err != nil {
		return nil, err
	}
	return out.Repositories, nil
}

// ConnectorForTile resolves the tile's git connector (explicit connector_id,
// else the org's sole connected github connector). Nil when none applies.
func (c *Client) ConnectorForTile(ctx context.Context, tile *repo.Tile) *repo.Connector {
	return c.connectorForTile(ctx, tile)
}

// RegistryAuth returns docker credentials for ghcr.io pulls through the
// tile's connector ("", "" when unavailable).
func (c *Client) RegistryAuth(ctx context.Context, tile *repo.Tile) (user, pass string) {
	cn := c.connectorForTile(ctx, tile)
	if cn == nil {
		return "", ""
	}
	tok, err := c.Token(ctx, cn)
	if err != nil || tok == "" {
		return "", ""
	}
	return "x-access-token", tok
}

const previewMarker = "<!-- stackr:preview -->"

// DeployFinished posts PR feedback after a deployment reaches a terminal
// status: a commit status on the PR head plus a sticky preview comment.
// Best-effort, no-ops unless the tile is a github-tracked git tile inside a
// pr-N ephemeral environment with a usable connector.
func (c *Client) DeployFinished(ctx context.Context, tile *repo.Tile, d *repo.Deployment) {
	if tile.SourceType != "git" || !strings.HasPrefix(tile.GitURL, "https://github.com/") {
		return
	}
	if d.Status == "cancelled" { // an admin stopping a deploy isn't a CI failure
		return
	}
	env, err := c.store.GetEnvironment(ctx, tile.EnvironmentID)
	if err != nil || env == nil || env.Type != "ephemeral" || !strings.HasPrefix(env.Slug, "pr-") {
		return
	}
	num := strings.TrimPrefix(env.Slug, "pr-")
	cn := c.connectorForTile(ctx, tile)
	if cn == nil {
		return
	}
	prCfg := envops.LoadPRConfig(ctx, c.store, env.StackID)
	if prCfg.NoComment && prCfg.NoStatus {
		return
	}
	tok, err := c.Token(ctx, cn)
	if err != nil || tok == "" {
		return
	}
	full := repoFullName(tile.GitURL)

	// Commit status on the PR's current head.
	var pr struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.tokAPI(ctx, tok, "GET", "/repos/"+full+"/pulls/"+num, nil, &pr); err != nil {
		return
	}
	state, desc := "success", "Preview deployed"
	if d.Status != "done" {
		state, desc = "failure", "Preview deploy failed"
	}
	if !prCfg.NoStatus {
		_ = c.tokAPI(ctx, tok, "POST", "/repos/"+full+"/statuses/"+pr.Head.SHA, map[string]string{
			"state":       state,
			"context":     "stackr/preview",
			"description": desc,
			"target_url":  c.baseURL + "/deployments/" + d.ID,
		}, nil)
	}
	if prCfg.NoComment {
		return
	}

	// One sticky comment per PR: plan section + preview/deploy section.
	c.upsertPRComment(ctx, tok, full, num, c.prComment(ctx, env.StackID, num, env, d))
}

// PlanKey is the settings key holding the rendered plan-preview markdown for
// one PR (written by the webhook handler, cleared on PR close).
func PlanKey(stackID, prNum string) string { return "prplan." + stackID + "." + prNum }

// RefreshPRComment re-renders and upserts the sticky PR comment outside the
// deploy path, used when a push updates the config plan preview. env is the
// PR's ephemeral environment when it exists (nil is fine: plan-only comment).
func (c *Client) RefreshPRComment(ctx context.Context, cn *repo.Connector, repoFull, prNum, stackID string, env *repo.Environment) {
	tok, err := c.Token(ctx, cn)
	if err != nil || tok == "" {
		return
	}
	c.upsertPRComment(ctx, tok, repoFull, prNum, c.prComment(ctx, stackID, prNum, env, nil))
}

func (c *Client) upsertPRComment(ctx context.Context, tok, full, num, body string) {
	payload := map[string]string{"body": body}
	var comments []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	_ = c.tokAPI(ctx, tok, "GET", "/repos/"+full+"/issues/"+num+"/comments?per_page=100", nil, &comments)
	for _, cm := range comments {
		if strings.Contains(cm.Body, previewMarker) {
			_ = c.tokAPI(ctx, tok, "PATCH", fmt.Sprintf("/repos/%s/issues/comments/%d", full, cm.ID), payload, nil)
			return
		}
	}
	_ = c.tokAPI(ctx, tok, "POST", "/repos/"+full+"/issues/"+num+"/comments", payload, nil)
}

// prComment renders the full sticky comment: config plan (when stored) plus
// the preview environment's URLs and deploy state (when the env exists).
func (c *Client) prComment(ctx context.Context, stackID, prNum string, env *repo.Environment, d *repo.Deployment) string {
	var b strings.Builder
	b.WriteString(previewMarker + "\n")
	if plan, _ := c.store.GetSetting(ctx, PlanKey(stackID, prNum)); plan != "" {
		b.WriteString(plan + "\n")
	}
	if env == nil {
		if b.Len() <= len(previewMarker)+1 {
			b.WriteString("### stackr\n_Nothing to report yet._\n")
		}
		return b.String()
	}
	b.WriteString("### stackr preview for " + env.Name + "\n\n")
	tiles, _ := c.store.ListTilesByEnv(ctx, env.ID)
	urls := 0
	for i := range tiles {
		doms, _ := c.store.ListDomainsByTile(ctx, tiles[i].ID)
		for _, dm := range doms {
			scheme := "http"
			if dm.HTTPS {
				scheme = "https"
			}
			b.WriteString("- **" + tiles[i].Name + "**: " + scheme + "://" + dm.Host + "\n")
			urls++
		}
	}
	if urls == 0 {
		b.WriteString("_No preview domains yet._\n")
	}
	if d == nil {
		// Plan-triggered refresh: report the env's latest deploy if any.
		for i := range tiles {
			if ds, err := c.store.ListDeploymentsByTile(ctx, tiles[i].ID, 1); err == nil && len(ds) > 0 {
				if d == nil || ds[0].CreatedAt.After(d.CreatedAt) {
					dd := ds[0]
					d = &dd
				}
			}
		}
	}
	if d != nil {
		b.WriteString("\nLast deploy: **" + d.Status + "**")
		if len(d.CommitSHA) >= 7 {
			b.WriteString(" (`" + d.CommitSHA[:7] + "`)")
		}
	}
	return b.String()
}

// tokAPI calls the GitHub API with an installation token, optionally JSON
// encoding a request body and decoding the response.
// FileContents fetches one file from a repo at a ref via the connector's
// installation token. Returns nil, nil when the file doesn't exist.
func (c *Client) FileContents(ctx context.Context, cn *repo.Connector, repoFull, ref, path string) ([]byte, error) {
	token, err := c.Token(ctx, cn)
	if err != nil {
		return nil, err
	}
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	p := "/repos/" + repoFull + "/contents/" + path
	if ref != "" {
		p += "?ref=" + url.QueryEscape(ref)
	}
	if err := c.tokAPI(ctx, token, "GET", p, nil, &out); err != nil {
		if strings.Contains(err.Error(), "404") {
			return nil, nil
		}
		return nil, err
	}
	if out.Encoding != "base64" {
		return nil, fmt.Errorf("unexpected contents encoding %q", out.Encoding)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
}

// DefaultBranch fetches the repo's default branch.
func (c *Client) DefaultBranch(ctx context.Context, cn *repo.Connector, repoFull string) (string, error) {
	token, err := c.Token(ctx, cn)
	if err != nil {
		return "", err
	}
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.tokAPI(ctx, token, "GET", "/repos/"+repoFull, nil, &out); err != nil {
		return "", err
	}
	return out.DefaultBranch, nil
}

// ListBranches names the branches of one repo, for the config-as-code branch
// field to autocomplete from.
// first 100 only, same as ListRepos, paginate when someone has more.
func (c *Client) ListBranches(ctx context.Context, cn *repo.Connector, repoFull string) ([]string, error) {
	token, err := c.Token(ctx, cn)
	if err != nil {
		return nil, err
	}
	var out []struct {
		Name string `json:"name"`
	}
	if err := c.tokAPI(ctx, token, "GET", "/repos/"+repoFull+"/branches?per_page=100", nil, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out))
	for _, b := range out {
		names = append(names, b.Name)
	}
	return names, nil
}

// HeadSHA resolves a ref (branch or tag) to the commit it points at, so a plan
// records exactly which commit it reviewed.
func (c *Client) HeadSHA(ctx context.Context, cn *repo.Connector, repoFull, ref string) (string, error) {
	token, err := c.Token(ctx, cn)
	if err != nil {
		return "", err
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := c.tokAPI(ctx, token, "GET", "/repos/"+repoFull+"/commits/"+url.PathEscape(ref), nil, &out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

// Commits lists the last n commits on ref, newest first.
func (c *Client) Commits(ctx context.Context, cn *repo.Connector, repoFull, ref string, n int) ([]gitlog.Commit, error) {
	token, err := c.Token(ctx, cn)
	if err != nil {
		return nil, err
	}
	var out []struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
		Commit  struct {
			Message string `json:"message"`
			Author  struct {
				Name string    `json:"name"`
				Date time.Time `json:"date"`
			} `json:"author"`
		} `json:"commit"`
		Author *struct {
			Login     string `json:"login"`
			AvatarURL string `json:"avatar_url"`
		} `json:"author"`
	}
	q := "?per_page=" + strconv.Itoa(n)
	if ref != "" {
		q += "&sha=" + url.QueryEscape(ref)
	}
	if err := c.tokAPI(ctx, token, "GET", "/repos/"+repoFull+"/commits"+q, nil, &out); err != nil {
		return nil, err
	}
	commits := make([]gitlog.Commit, 0, len(out))
	for _, o := range out {
		msg, _, _ := strings.Cut(o.Commit.Message, "\n")
		cm := gitlog.Commit{SHA: o.SHA, Message: msg, Author: o.Commit.Author.Name, When: o.Commit.Author.Date, URL: o.HTMLURL}
		if o.Author != nil {
			cm.Login, cm.AvatarURL = o.Author.Login, o.Author.AvatarURL
		}
		commits = append(commits, cm)
	}
	return commits, nil
}

func (c *Client) tokAPI(ctx context.Context, token, method, path string, in, out any) error {
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
	req.Header.Set("Authorization", "Bearer "+token)
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

// repoFullName extracts "owner/repo" from an https github URL.
func repoFullName(gitURL string) string {
	s := strings.TrimPrefix(gitURL, "https://github.com/")
	s = strings.TrimSuffix(s, ".git")
	return strings.Trim(s, "/")
}

// Commit reads one commit by sha: what the log shows for a commit older than
// the ten it lists.
func (c *Client) Commit(ctx context.Context, cn *repo.Connector, repoFull, sha string) (gitlog.Commit, error) {
	token, err := c.Token(ctx, cn)
	if err != nil {
		return gitlog.Commit{}, err
	}
	var o struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
		Commit  struct {
			Message string `json:"message"`
			Author  struct {
				Name string    `json:"name"`
				Date time.Time `json:"date"`
			} `json:"author"`
		} `json:"commit"`
		Author *struct {
			Login     string `json:"login"`
			AvatarURL string `json:"avatar_url"`
		} `json:"author"`
	}
	if err := c.tokAPI(ctx, token, "GET", "/repos/"+repoFull+"/commits/"+url.PathEscape(sha), nil, &o); err != nil {
		return gitlog.Commit{}, err
	}
	msg, _, _ := strings.Cut(o.Commit.Message, "\n")
	cm := gitlog.Commit{SHA: o.SHA, Message: msg, Author: o.Commit.Author.Name, When: o.Commit.Author.Date, URL: o.HTMLURL}
	if o.Author != nil {
		cm.Login, cm.AvatarURL = o.Author.Login, o.Author.AvatarURL
	}
	return cm, nil
}

// Behind is how many commits on ref sha lacks, through the compare API.
// onBranch is false when the two have diverged: sha is not on ref at all.
func (c *Client) Behind(ctx context.Context, cn *repo.Connector, repoFull, sha, ref string) (behind int, onBranch bool, err error) {
	token, err := c.Token(ctx, cn)
	if err != nil {
		return 0, false, err
	}
	var o struct {
		Status  string `json:"status"` // identical | ahead | behind | diverged
		AheadBy int    `json:"ahead_by"`
	}
	path := "/repos/" + repoFull + "/compare/" + url.PathEscape(sha) + "..." + url.PathEscape(ref)
	if err := c.tokAPI(ctx, token, "GET", path, nil, &o); err != nil {
		return 0, false, err
	}
	switch o.Status {
	case "identical", "ahead":
		return o.AheadBy, true, nil
	}
	return 0, false, nil
}
