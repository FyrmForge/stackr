package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// serverRig is two orgs and a server connector over the git fake.
type serverRig struct {
	env         *servicetest.Env
	g           *servicetest.Git
	acme, other string
	srv         string
}

func newServerRig(t *testing.T) serverRig {
	t.Helper()
	g := servicetest.NewGit(t)
	env := servicetest.NewWith(t, []service.Option{g.Option()})
	return serverRig{
		env:   env,
		g:     g,
		acme:  env.Org(t, "acme"),
		other: env.Org(t, "other"),
		srv:   env.ServerConnector(t, "main", "srvsec"),
	}
}

func (r serverRig) push(t *testing.T, repo, sha string) {
	t.Helper()
	body := []byte(`{"ref":"refs/heads/main","after":"` + sha + `",` +
		`"repository":{"clone_url":"https://github.com/` + repo + `.git","default_branch":"main"},` +
		`"commits":[{"modified":["x"]}]}`)
	mac := hmac.New(sha256.New, []byte("srvsec"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if err := r.env.Orch.Webhook(context.Background(), r.srv, "push", sig, body); err != nil {
		t.Fatal(err)
	}
}

// jobs is the kind of every job row with the org it names in its payload.
func (r serverRig) jobs(t *testing.T) []string {
	t.Helper()
	js, err := r.env.Orch.Jobs(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, j := range js {
		switch {
		case strings.Contains(j.Payload, r.acme):
			out = append(out, j.Kind+":acme")
		case strings.Contains(j.Payload, r.other):
			out = append(out, j.Kind+":other")
		default:
			out = append(out, j.Kind)
		}
	}
	slices.Sort(out)
	return out
}

// An org binds its config file to a server connector only once it is
// shared with it, reads it in its picker, and never writes through it.
func TestServerConnectorSharing(t *testing.T) {
	ctx := context.Background()
	r := newServerRig(t)
	o := r.env.Orch
	r.g.Commit(t, "acme/org", "main", map[string]string{"stackr-org.yml": "version: 1\norg: acme\n"})

	if _, err := o.SetOrgConfigRepo(ctx, r.acme, r.srv, "acme/org", "", "", false); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("bind to an unshared server connector = %v, want not found", err)
	}
	if cs, _ := o.ConnectedConnectors(ctx, r.acme); len(cs) != 0 {
		t.Errorf("unshared connector in the picker: %+v", cs)
	}

	sc, err := o.ShareConnector(ctx, r.srv, []string{r.acme}, false)
	if err != nil || !slices.Equal(sc.OrgIDs, []string{r.acme}) || sc.ShareAll || sc.Config != "" {
		t.Fatalf("share = %+v %v", sc, err)
	}
	if cs, _ := o.ConnectedConnectors(ctx, r.acme); len(cs) != 1 || cs[0].ID != r.srv {
		t.Errorf("shared connector not in the picker: %+v", cs)
	}
	if cs, _ := o.ConnectedConnectors(ctx, r.other); len(cs) != 0 {
		t.Errorf("an org it is not shared with sees it: %+v", cs)
	}
	if cs, _ := o.Connectors(ctx, r.acme); len(cs) != 1 || !cs[0].Shared {
		t.Errorf("the org's list lacks the shared connector, marked: %+v", cs)
	}
	if cs, _ := o.Connectors(ctx, r.other); len(cs) != 0 {
		t.Errorf("an org it is not shared with lists it: %+v", cs)
	}
	if _, err := o.SetOrgConfigRepo(ctx, r.other, r.srv, "acme/org", "", "", false); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("another org bound it = %v", err)
	}

	// writes never reach a server connector
	if err := o.DeleteConnector(ctx, r.acme, r.srv); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("org delete = %v", err)
	}
	if _, err := o.RenameConnector(ctx, r.acme, r.srv, "mine"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("org rename = %v", err)
	}

	og, err := o.SetOrgConfigRepo(ctx, r.acme, r.srv, "acme/org", "", "", false)
	if err != nil || og.ConfigConnectorID != r.srv {
		t.Fatalf("bind to a shared server connector = %+v %v", og, err)
	}
	if ps, _ := o.OrgPlans(ctx, r.acme, 5); len(ps) != 1 || ps[0].Status == "error" {
		t.Errorf("the bind did not plan the org: %+v", ps)
	}

	// revoke and delete are refused while the binding names it
	if _, err := o.ShareConnector(ctx, r.srv, nil, false); !isConflict(err) || !strings.Contains(err.Error(), "acme") {
		t.Errorf("revoke while bound = %v", err)
	}
	if _, err := o.ShareConnector(ctx, r.srv, nil, true); err != nil {
		t.Errorf("widening to all orgs = %v", err)
	}
	if _, err := o.ShareConnector(ctx, r.srv, []string{r.acme}, false); err != nil {
		t.Errorf("narrowing to the binding org = %v", err)
	}
	if _, err := o.ShareConnector(ctx, r.srv, []string{r.other}, false); !isConflict(err) {
		t.Errorf("narrowing away from the binding org = %v", err)
	}
	if err := o.DeleteServerConnector(ctx, r.srv); !isConflict(err) {
		t.Errorf("delete while an org binds it = %v", err)
	}
	if _, err := o.SetOrgConfigRepo(ctx, r.acme, "", "", "", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := o.ShareConnector(ctx, r.srv, nil, false); err != nil {
		t.Errorf("revoke after unbind = %v", err)
	}
	// a stack's config repo and the server file's binding count too
	if err := o.SetSettings(ctx, map[string]string{"server_config_connector": r.srv}); err != nil {
		t.Fatal(err)
	}
	if err := o.DeleteServerConnector(ctx, r.srv); !isConflict(err) || !strings.Contains(err.Error(), "server") {
		t.Errorf("delete while the server file binds it = %v", err)
	}
	if err := o.SetSettings(ctx, map[string]string{"server_config_connector": ""}); err != nil {
		t.Fatal(err)
	}
	if err := o.DeleteServerConnector(ctx, r.srv); err != nil {
		t.Errorf("delete = %v", err)
	}
}

func TestShareConnectorRefusals(t *testing.T) {
	ctx := context.Background()
	r := newServerRig(t)
	o := r.env.Orch
	if _, err := o.ShareConnector(ctx, r.srv, []string{"no-such-org"}, false); !errors.As(err, &errs.Invalid{}) {
		t.Errorf("unknown org = %v", err)
	}
	if _, err := o.ShareConnector(ctx, "no-such", nil, true); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown connector = %v", err)
	}
	own := r.env.Connector(t, r.acme, "s")
	if _, err := o.ShareConnector(ctx, own, nil, true); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("sharing an org's own connector = %v", err)
	}
	if err := o.DeleteServerConnector(ctx, own); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("server delete of an org's connector = %v", err)
	}
	sc, err := o.ShareConnector(ctx, r.srv, []string{r.acme, r.acme, r.other}, false)
	if err != nil || len(sc.OrgIDs) != 2 {
		t.Errorf("duplicates = %+v %v", sc, err)
	}
	all, err := o.ServerConnectors(ctx)
	if err != nil || len(all) != 1 || all[0].ID != r.srv || all[0].Config != "" || len(all[0].OrgIDs) != 2 {
		t.Errorf("list = %+v %v", all, err)
	}
	if got, err := o.RenameServerConnector(ctx, r.srv, "primary"); err != nil || got.Name != "primary" || len(got.OrgIDs) != 2 {
		t.Errorf("rename = %+v %v", got, err)
	}
}

// A delivery on a server connector queues the server plan when the push
// lands on the server file's binding, and the org plan and stack pushes of
// each org it is shared with; an org it is not shared with gets nothing.
func TestServerConnectorWebhook(t *testing.T) {
	ctx := context.Background()
	r := newServerRig(t)
	o := r.env.Orch
	r.g.Commit(t, "acme/org", "main", map[string]string{"stackr-org.yml": "version: 1\norg: acme\n"})
	if _, err := o.ShareConnector(ctx, r.srv, []string{r.acme}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := o.SetOrgConfigRepo(ctx, r.acme, r.srv, "acme/org", "", "", false); err != nil {
		t.Fatal(err)
	}
	// other binds the same repo through its own connector: only a delivery
	// of that connector may plan it
	otherConn := r.env.Connector(t, r.other, "othersec")
	if _, err := o.SetOrgConfigRepo(ctx, r.other, otherConn, "acme/org", "", "", false); err != nil {
		t.Fatal(err)
	}
	sha := r.g.Commit(t, "acme/org", "main", map[string]string{"stackr-org.yml": "version: 1\norg: Acme Two\n"})

	r.push(t, "acme/org", sha)
	if got := r.jobs(t); !slices.Equal(got, []string{"org-plan:acme"}) {
		t.Fatalf("jobs after a push to the org repo = %v, want the shared org's plan only", got)
	}

	// the server binding: another repo, so a push to it is the server plan's
	r.g.Commit(t, "acme/server", "main", map[string]string{"stackr-server.yml": "version: 1\n"})
	for k, v := range map[string]string{
		"server_config_connector": r.srv,
		"server_config_repo":      "https://github.com/acme/server",
	} {
		if err := o.SetSettings(ctx, map[string]string{k: v}); err != nil {
			t.Fatal(err)
		}
	}
	r.push(t, "acme/server", r.g.Commit(t, "acme/server", "main", map[string]string{"x": "1"}))
	if got := r.jobs(t); !slices.Equal(got, []string{"org-plan:acme", "server-plan"}) {
		t.Errorf("jobs after a push to the server repo = %v, want the server plan", got)
	}

	// a connector the binding does not name queues no server plan
	if err := o.SetSettings(ctx, map[string]string{"server_config_connector": "somebody-else"}); err != nil {
		t.Fatal(err)
	}
	r.push(t, "acme/server", r.g.Commit(t, "acme/server", "main", map[string]string{"x": "2"}))
	if n := strings.Count(strings.Join(r.jobs(t), " "), "server-plan"); n != 1 {
		t.Errorf("server plans = %d, want the one from before", n)
	}

	// shared with all orgs: both orgs plan
	if _, err := o.ShareConnector(ctx, r.srv, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := o.SetOrgConfigRepo(ctx, r.other, r.srv, "acme/org", "", "", false); err != nil {
		t.Fatal(err)
	}
	r.push(t, "acme/org", sha)
	got := strings.Join(r.jobs(t), " ")
	if strings.Count(got, "org-plan:other") < 1 || strings.Count(got, "org-plan:acme") < 2 {
		t.Errorf("jobs after a share-all push = %s", got)
	}
}

// The server file is read through the binding's server connector, never an
// org's; with none named it says so.
func TestServerFileRead(t *testing.T) {
	ctx := context.Background()
	r := newServerRig(t)
	o := r.env.Orch
	if _, _, err := o.ServerFile(ctx, ""); !isConflict(err) {
		t.Errorf("unbound = %v, want Conflict", err)
	}
	sha := r.g.Commit(t, "acme/server", "main", map[string]string{"stackr-server.yml": "version: 1\n"})
	if err := o.SetSettings(ctx, map[string]string{"server_config_repo": "https://github.com/acme/server"}); err != nil {
		t.Fatal(err)
	}
	data, got, err := o.ServerFile(ctx, "")
	if err != nil || string(data) != "version: 1\n" || got != sha {
		t.Fatalf("file = %q at %s, %v; want the committed file at %s", data, got, err, sha)
	}
	r.g.Commit(t, "acme/server", "main", map[string]string{"infra/server.yml": "version: 1\n# custom\n"})
	if err := o.SetSettings(ctx, map[string]string{"server_config_path": "infra/server.yml"}); err != nil {
		t.Fatal(err)
	}
	if data, _, err = o.ServerFile(ctx, ""); err != nil || !strings.Contains(string(data), "custom") {
		t.Errorf("custom path = %q, %v", data, err)
	}

	// no git fake: the connector lookup runs
	bare := servicetest.New(t)
	if err := bare.Orch.SetSettings(ctx, map[string]string{"server_config_repo": "https://github.com/acme/server"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bare.Orch.ServerFile(ctx, ""); !isConflict(err) {
		t.Errorf("no connector named = %v, want Conflict", err)
	}
	orgConn := bare.Connector(t, bare.Org(t, "acme"), "s")
	if err := bare.Orch.SetSettings(ctx, map[string]string{"server_config_connector": orgConn}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bare.Orch.ServerFile(ctx, ""); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("an org's connector as the server's = %v, want not found", err)
	}
}

// A stack binds its stackr-compose.yml to a shared server connector, and to
// no other org's.
func TestStackConfigRepoSharedConnector(t *testing.T) {
	ctx := context.Background()
	r := newServerRig(t)
	o := r.env.Orch
	tl := r.env.Tile(t, r.acme)

	if _, err := o.SetConfigRepo(ctx, tl.Stack, r.srv, "acme/shop", "", ""); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("bind to an unshared server connector = %v, want not found", err)
	}
	if _, err := o.ShareConnector(ctx, r.srv, []string{r.acme}, false); err != nil {
		t.Fatal(err)
	}
	if st, err := o.SetConfigRepo(ctx, tl.Stack, r.srv, "acme/shop", "", ""); err != nil || st.ConfigConnectorID != r.srv {
		t.Fatalf("bind to a shared server connector = %+v %v", st, err)
	}
}
