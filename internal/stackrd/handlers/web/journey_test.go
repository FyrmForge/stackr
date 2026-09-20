package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/auth"
	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/FyrmForge/hamr/pkg/websocket"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/orgconf"
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Journey tests walk the real router over HTTP: the same URLs a browser asks
// for, the same redirects, the same rendered HTML. They are the feature tests
// the wizard needed, a step that quietly posts to the wrong endpoint, or a
// settings tab that opens mid-onboarding, is invisible to a handler test that
// calls one function with a hand-built context.
//
// No browser and no docker: deploys are out of scope here (deploy.Engine takes
// a concrete docker runtime), and the config source is a fake FileSource, which
// is a three-method interface. What is left out is exactly what the manual pass
// on the test box is for.

// fileSource is the bound repo, as a map of path to contents.
type fileSource struct {
	files  map[string]string
	branch string
	sha    string
}

func (f fileSource) FileContents(_ context.Context, _ *repo.Connector, _, _, path string) ([]byte, error) {
	body, ok := f.files[path]
	if !ok {
		return nil, stackconf.ErrNoFile
	}
	return []byte(body), nil
}
func (f fileSource) DefaultBranch(context.Context, *repo.Connector, string) (string, error) {
	return f.branch, nil
}
func (f fileSource) HeadSHA(context.Context, *repo.Connector, string, string) (string, error) {
	return f.sha, nil
}

type journey struct {
	t      *testing.T
	store  *sqlite.Store
	src    *fileSource
	client *http.Client
	base   string
}

// repoPath resolves a path relative to the repository root from this package.
func repoPath(rel string) string { return filepath.Join("..", "..", "..", "..", rel) }

func newJourney(t *testing.T) *journey {
	t.Helper()
	store := testdb.New(t)
	src := &fileSource{files: map[string]string{}, branch: "main", sha: "abc1234def"}

	// Static paths are relative to the process working directory, which for a
	// test is the package directory. Serving them for real keeps the browser
	// tests (make e2e) looking like the app rather than like raw HTML.
	srv, err := server.New(
		server.WithDevMode(true),
		server.WithStaticDir(repoPath("frontend/static")),
		server.WithStaticDistDir(repoPath("frontend/dist")),
		server.WithGeneratedDir(repoPath("generated")),
	)
	require.NoError(t, err, "server")
	hub := websocket.NewHub()
	applier := stackconf.Applier{Planner: stackconf.Planner{Store: store, Src: src}}
	// A real work queue, because the applies run on it now: an approve that
	// cannot reach the runner is a 503, and the wizard's own plan screen is
	// what waits for the job and then moves on.
	orgRunner := &orgconf.Runner{Store: store, Src: src, Stacks: applier.Planner, Applier: applier}
	work := workqueue.New(store)
	stackconf.RegisterApply(work, applier)
	stackconf.RegisterPromote(work, applier)
	orgconf.RegisterApply(work, orgRunner)
	work.Start(context.Background())
	// The pages reference /static/..., same as the binary does.
	components.StaticBaseURL = "/static"
	web.RegisterRoutes(srv, &web.Deps{
		Store:          store,
		StaticBaseURL:  "/static",
		DevMode:        true, // plain http: session, flash and CSRF cookies stay non-Secure
		SessionManager: auth.NewSessionManager(store),
		AuthService:    service.NewAuthService(store),
		Hub:            hub,
		Notifier:       notify.New(hub, store),
		Applier:        applier,
		// The pages write through services now; their own dependencies are
		// nil-safe, so the rows land and the docker/proxy half is skipped.
		Tiles:        service.NewTileService(store, nil, nil, nil, nil, nil, service.NewGateService(store)),
		Domains:      service.NewDomainService(store, nil, service.NewGateService(store)),
		Resources:    service.NewDomainResourceService(store, nil),
		Lifecycle:    service.NewTileLifecycleService(store, nil, nil, nil, nil, nil),
		Telemetry:    service.NewTileTelemetryService(store, nil),
		Environments: service.NewEnvironmentService(store, nil, nil, nil, service.NewGateService(store)),
		Orgs:         service.NewOrgService(store),
		Members:      service.NewMemberService(store, nil, service.NewRevokeService(store, nil)),
		Stacks: service.NewStackService(store, nil, nil,
			service.NewEnvironmentService(store, nil, nil, nil, service.NewGateService(store)),
			nil, service.NewGateService(store), nil),
		Variables: service.NewVariableService(store, nil, nil, nil),
		OrgConfig: orgRunner,
		Work:      work,
		Access:    service.NewAccessService(store),
	})

	ts := httptest.NewServer(srv.Echo())
	t.Cleanup(ts.Close)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err, "cookie jar")
	c := &http.Client{Jar: jar}
	// Redirects are the assertion in most of these, so follow nothing by
	// default and let each step say where it expected to land.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &journey{t: t, store: store, src: src, client: c, base: ts.URL}
}

// get fetches a page and returns its status and body.
func (j *journey) get(path string) (int, string) {
	j.t.Helper()
	resp, err := j.client.Get(j.base + path)
	require.NoError(j.t, err, "GET %s", path)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(j.t, err, "GET %s body", path)
	if loc := resp.Header.Get("Location"); loc != "" {
		return resp.StatusCode, loc
	}
	return resp.StatusCode, string(body)
}

// page fetches a page, requires 200, and returns the HTML.
func (j *journey) page(path string) string {
	j.t.Helper()
	code, body := j.get(path)
	require.Equal(j.t, http.StatusOK, code, "GET %s: %s", path, first(body, 200))
	return body
}

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// post submits a form the way the page in html would, carrying that page's own
// CSRF token, which also keeps the tests honest about which page a form is on.
func (j *journey) post(html, path string, values url.Values) (int, string) {
	j.t.Helper()
	m := csrfRe.FindStringSubmatch(html)
	require.NotNil(j.t, m, "no CSRF token on the page that posts to %s", path)
	values.Set("csrf_token", m[1])
	req, err := http.NewRequest(http.MethodPost, j.base+path, strings.NewReader(values.Encode()))
	require.NoError(j.t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := j.client.Do(req)
	require.NoError(j.t, err, "POST %s", path)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if loc := resp.Header.Get("Location"); loc != "" {
		return resp.StatusCode, loc
	}
	if loc := resp.Header.Get("HX-Redirect"); loc != "" {
		return resp.StatusCode, loc
	}
	return resp.StatusCode, string(body)
}

func first(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// register creates the first account, which a fresh install makes an admin.
func (j *journey) register(email, password string) {
	j.t.Helper()
	form := j.page("/register")
	code, loc := j.post(form, "/register", url.Values{
		"name": {"Owner"}, "email": {email},
		"password": {password}, "confirm_password": {password},
	})
	require.Contains(j.t, []int{http.StatusOK, http.StatusSeeOther, http.StatusFound}, code, "register: %s", first(loc, 1500))
}

// createDraft answers step 1 (the branch question) and returns the draft
// org's slug, which is a placeholder until the branch names it.
func (j *journey) createDraft(mode string) string {
	j.t.Helper()
	form := j.page("/setup")
	code, loc := j.post(form, "/orgs", url.Values{"mode": {mode}})
	require.Contains(j.t, []int{http.StatusOK, http.StatusSeeOther, http.StatusFound}, code, "create draft: %s", first(loc, 300))
	parts := strings.Split(loc, "/")
	require.Len(j.t, parts, 5, "step 1 lands on the branch's first step, got %q", loc)
	o, err := j.store.GetOrgBySlug(context.Background(), parts[2])
	require.NoError(j.t, err)
	require.NotNil(j.t, o, "no draft org behind %s", loc)
	return o.Slug
}

// createOrg walks step 1 down the "fill it in here" branch and then its name
// step, which is where a hand-built org gets its name. Returns the slug.
func (j *journey) createOrg(name string) string {
	j.t.Helper()
	draft := j.createDraft("ui")
	form := j.page("/orgs/" + draft + "/setup/name")
	code, loc := j.post(form, "/orgs/"+draft+"/setup/name", url.Values{"name": {name}})
	require.Contains(j.t, []int{http.StatusOK, http.StatusSeeOther, http.StatusFound}, code, "name org: %s", first(loc, 300))
	slug := repo.Slugify(name)
	o, err := j.store.GetOrgBySlug(context.Background(), slug)
	require.NoError(j.t, err)
	require.NotNil(j.t, o, "org %q was not named (landed on %s)", slug, first(loc, 200))
	return o.Slug
}

// finishSetup posts the wizard's Finish, for tests whose subject is what an
// org does afterwards. Without it every org-scoped route answers the wizard.
func (j *journey) finishSetup(slug string) {
	j.t.Helper()
	summary := j.page("/orgs/" + slug + "/setup/done")
	code, loc := j.post(summary, "/orgs/"+slug+"/setup/done", url.Values{})
	require.Equal(j.t, "/orgs/"+slug, loc, "Finish, code %d", code)
}
