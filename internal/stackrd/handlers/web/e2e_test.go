//go:build e2e

package web_test

import (
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/e2e"
	"github.com/go-rod/rod"
	"github.com/stretchr/testify/require"
)

// Browser tests, `make e2e`, never part of `make test`. Only what actually
// needs a browser lives here: HTMX swaps, the CSS that turns a button primary
// when a field becomes valid, an accordion opening. Everything a request and a
// response can prove is a journey test instead; those run in a second.
//
// Headed by default, because watching it is the point; E2E_HEADLESS=1 for CI.
// Every page this drives is titled "go-rod e2e; ..." so the window that appears
// on your desktop is obviously a test run and not a browser you opened.

// browserJourney boots the app and a browser pointed at it.
//
// Headed by default. The window is Chromium's own: hamr's SetupBrowser owns the
// launcher flags, so GPU and Wayland belong in an option there rather than in a
// launcher rolled here.
func browserJourney(t *testing.T) (*journey, *rod.Browser) {
	t.Helper()
	j := newJourney(t)
	b := e2e.SetupBrowser(t,
		e2e.WithHeadless(os.Getenv("E2E_HEADLESS") == "1"),
		e2e.WithArtifactDir(repoPath("bin/e2e")),
		e2e.WithTimeout(20*time.Second),
	)
	return j, b
}

// step is the only wait these tests use: an element, with a deadline of its
// own. A bare MustElement inherits no timeout and hangs the whole run when a
// page does not arrive, which is the failure mode you least want from a suite
// you are watching.
func step(t *testing.T, page *rod.Page, selector string) *rod.Element {
	t.Helper()
	el, err := page.Timeout(15 * time.Second).Element(selector)
	require.NoError(t, err, "waiting for %s", selector)
	return el.CancelTimeout()
}

// nav goes to a page and stamps the window title. Chromium takes its window
// title from the document, and an untitled stackr window is indistinguishable
// from a real one; this run is throwaway and should look it.
func nav(t *testing.T, page *rod.Page, url string) *rod.Page {
	t.Helper()
	require.NoError(t, page.Timeout(15*time.Second).Navigate(url), "navigate %s", url)
	require.NoError(t, page.Timeout(15*time.Second).WaitLoad(), "load %s", url)
	page.MustEval(`() => { document.title = "go-rod e2e; " + document.title }`)
	return page
}

// signUp registers the first account through the browser, so the session lives
// in the browser's own cookie jar rather than the journey client's.
func signUp(t *testing.T, j *journey, b *rod.Browser) *rod.Page {
	t.Helper()
	page := nav(t, e2e.NewPage(t, b, "about:blank"), j.base+"/register")
	step(t, page, `input[name="name"]`).MustInput("Owner")
	step(t, page, `input[name="email"]`).MustInput("owner@example.com")
	step(t, page, `input[name="password"]`).MustInput("Correct-Horse9")
	step(t, page, `input[name="confirm_password"]`).MustInput("Correct-Horse9")
	step(t, page, `button[type="submit"]`).MustClick()
	// Registration leaves the form; where it lands is the app's business.
	require.NoError(t,
		page.Timeout(15*time.Second).Wait(rod.Eval(`() => location.pathname !== "/register"`)),
		"registration did not leave the form")
	return page
}

// The wizard end to end, with the stepper and the real HTMX swaps.
func TestE2EWizard(t *testing.T) {
	j, b := browserJourney(t)
	page := signUp(t, j, b)

	createOrgInBrowser(t, j, page, "Acme")
	e2e.AssertURLContains(t, page, "/setup/")

	for _, s := range []string{"name", "connector", "domain", "team", "done"} {
		nav(t, page, j.base+"/orgs/acme/setup/"+s)
		step(t, page, ".panel")
	}

	step(t, page, `form[hx-post$="/setup/done"] button[type="submit"]`).MustClick()
	step(t, page, `[data-canvas], main, .graph-legend`) // the org canvas
	e2e.AssertURLContains(t, page, "/orgs/acme")
	o := mustOrg(t, j, "acme")
	require.NotNil(t, o)
}

// The add button is a ghost until the address is one. It is pure CSS (:has),
// so nothing but a browser can tell whether it works.
func TestE2EAddPersonButtonTurnsPrimary(t *testing.T) {
	j, b := browserJourney(t)
	page := signUp(t, j, b)
	createOrgInBrowser(t, j, page, "Acme")

	nav(t, page, j.base+"/orgs/acme/setup/team")
	btn := step(t, page, ".people-add-submit")
	before := btn.MustEval(`() => getComputedStyle(this).backgroundColor`).String()

	step(t, page, `#people-email`).MustInput("someone@example.com")
	// Wait on the colour itself. Waiting on the :has() selector is not enough:
	// it matches a frame before the recalc reaches the button, and a single
	// read after it comes back with the old colour often enough to be flaky.
	require.NoError(t,
		page.Timeout(15*time.Second).Wait(rod.Eval(
			`(was) => getComputedStyle(document.querySelector(".people-add-submit")).backgroundColor !== was`,
			before)),
		"a valid address must make Add the obvious thing to click")
}

// A plan row opens to show what it creates, and a declared value can be set
// without leaving the page.
func TestE2EPlanPage(t *testing.T) {
	j, b := browserJourney(t)
	page := signUp(t, j, b)
	createOrgInBrowser(t, j, page, "Acme")

	// The journey client seeds the binding; the browser reviews the plan. Same
	// account, two clients: the browser owns the org.
	j.login("owner@example.com", "Correct-Horse9")
	j.bindConfig("acme", "version: 1\norg: Acme\nvars:\n  REGION: eu\nsecrets:\n  STRIPE_KEY:\n")

	nav(t, page, j.base+"/orgs/acme/setup/config/plan")
	// The row asking for a value is closed until it is clicked: the page is a
	// list of one-liners, and the box is what the red row opens to.
	step(t, page, `li:has(input[name="value"]) summary`).MustClick()
	step(t, page, `input[name="value"]`).MustInput("sk_test_1")
	step(t, page, `form[hx-post$="/inputs"] button[type="submit"]`).MustClick()
	// Setting the value re-plans, and the plan that comes back no longer asks
	// for it. Waiting on the box going away, not on the heading, which is on
	// both the old page and the new one.
	require.NoError(t,
		page.Timeout(15*time.Second).Wait(rod.Eval(`() => !document.querySelector('input[name="value"]')`)),
		"the box should go away once the value exists")
}

// createOrgInBrowser answers step 1 with "fill it in here", names the org on
// step 2 and waits for step 3.
func createOrgInBrowser(t *testing.T, j *journey, page *rod.Page, name string) {
	t.Helper()
	nav(t, page, j.base+"/setup")
	step(t, page, `button[value="ui"]`).MustClick()
	require.NoError(t,
		page.Timeout(15*time.Second).Wait(rod.Eval(`() => location.pathname.endsWith("/setup/name")`)),
		"the name step did not load")
	// Step 1 answers with an HX-Redirect. On the document that lands from one,
	// neither a click on Continue nor Enter in the field produces a submit
	// event under rod, though requestSubmit on the same form does and a hand
	// drive in a real browser works. Re-navigating makes the page drivable
	// again; the cause was not chased further than that.
	nav(t, page, page.MustInfo().URL)
	step(t, page, `input[name="name"]`).MustInput(name)
	step(t, page, `form[hx-post$="/setup/name"] button[type="submit"]`).MustClick()
	step(t, page, `form[action$="/setup/connector"], a[href$="/setup/domain"]`) // step 3
}

// login signs the journey client in as an existing account; used when the
// browser made the account and the client is the second party in the room.
func (j *journey) login(email, password string) {
	j.t.Helper()
	form := j.page("/login")
	code, loc := j.post(form, "/login", url.Values{"email": {email}, "password": {password}})
	require.Contains(j.t, []int{http.StatusOK, http.StatusSeeOther, http.StatusFound}, code,
		"login: %s", first(loc, 300))
}
