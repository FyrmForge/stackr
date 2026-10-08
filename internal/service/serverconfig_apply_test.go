package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

type fileRig struct {
	env  *servicetest.Env
	g    *servicetest.Git
	conn string // a server connector
}

func newFileRig(t *testing.T) fileRig {
	t.Helper()
	g := servicetest.NewGit(t)
	env := servicetest.NewWith(t, []service.Option{g.Option()})
	return fileRig{env: env, g: g, conn: env.ServerConnector(t, "gh", "whsec")}
}

// applied waits for the server plan to end applied (or fails with its error).
func (r fileRig) applied(t *testing.T, id string) {
	t.Helper()
	var last service.ServerPlan
	eventually(t, "server plan "+id+" ended", func() bool {
		p, err := r.env.Orch.ServerPlan(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		last = p
		return p.Status == "applied" || p.Status == "error"
	})
	if last.Status != "applied" {
		t.Fatalf("server plan %s = %s: %s", id, last.Status, last.Error)
	}
}

func (r fileRig) routes(t *testing.T) []string {
	t.Helper()
	rs, err := r.env.Orch.ExternalRoutes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, x := range rs {
		hosts = append(hosts, x.Host)
	}
	return hosts
}

const serverFileV1 = `version: 1
settings:
  backup_run_concurrency: 3
params:
  app:
    region:
      type: param
      value: eu
routes:
  - host: old.example.com
    mode: https
    target: backend.internal:8443
`

// A local file plans with source "local" and its bytes on the row, and the
// approve applies it: settings, params and routes. Planned again it reads
// clean.
func TestServerLocalPlanApplies(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	pl, err := r.env.Orch.PlanServerFile(ctx, []byte(serverFileV1))
	if err != nil {
		t.Fatal(err)
	}
	if pl.Status != "pending" || pl.Source != "local" || pl.File != serverFileV1 || pl.Commit != "" {
		t.Fatalf("plan = %+v; want pending, local, with the bytes", pl)
	}
	if !strings.Contains(pl.Summary, "to add") {
		t.Errorf("summary = %q", pl.Summary)
	}
	if _, err := r.env.Orch.PlanServerFile(ctx, []byte("version: 9\n")); err == nil {
		t.Error("a bad local file stored a plan")
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)

	if v, _ := r.env.Orch.Setting(ctx, "backup_run_concurrency"); v != "3" {
		t.Errorf("backup_run_concurrency = %q, want 3", v)
	}
	if got := r.routes(t); len(got) != 1 || got[0] != "old.example.com" {
		t.Errorf("routes = %v", got)
	}
	ps, err := r.env.Orch.Params(ctx, service.ServerParamScope, false)
	if err != nil || len(ps) != 1 || ps[0].Name != "region" || ps[0].Value != "eu" {
		t.Errorf("server params = %+v, %v", ps, err)
	}

	again, err := r.env.Orch.PlanServerFile(ctx, []byte(serverFileV1))
	if err != nil || again.Status != "clean" {
		t.Errorf("replan = %+v, %v; want clean", again, err)
	}
}

// A route the file no longer names is a removal row: unticked it stays,
// ticked it goes.
func TestServerRemovalNeedsATick(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	first, err := r.env.Orch.PlanServerFile(ctx, []byte(serverFileV1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, first.ID, service.ApproveOpts{}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, first.ID)

	dropped := "version: 1\nroutes: []\n"
	pl, err := r.env.Orch.PlanServerFile(ctx, []byte(dropped))
	if err != nil || !strings.Contains(pl.Plan, `"key":"route:old.example.com"`) {
		t.Fatalf("plan = %+v, %v; want the removal row", pl, err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)
	if len(r.routes(t)) != 1 {
		t.Fatal("an unticked removal row deleted the route")
	}

	pl, err = r.env.Orch.PlanServerFile(ctx, []byte(dropped))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{Ticked: []string{"route:old.example.com"}}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)
	if got := r.routes(t); len(got) != 0 {
		t.Errorf("a ticked removal row left %v", got)
	}
}

// A file that names a new org creates it with the approver as owner; the
// plan is risky, so it needs the confirm. An apply with no approver cannot
// own it and ends error.
func TestServerFileCreatesOrg(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	admin := r.env.User(t, "root@example.com", true)
	const file = "version: 1\norgs:\n  - slug: newco\n    name: Newco\n"
	pl, err := r.env.Orch.PlanServerFile(ctx, []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{ApproverID: admin}); !isConflict(err) {
		t.Fatalf("creating an org without the confirm = %v, want the impact refusal", err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{Confirm: true, ApproverID: admin}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)
	og, err := r.env.Store.Orgs.GetBySlug(ctx, "newco")
	if err != nil || og.Name != "Newco" {
		t.Fatalf("org = %+v, %v", og, err)
	}
	ms, err := r.env.Store.OrgMembers.ListByOrg(ctx, og.ID)
	if err != nil || len(ms) != 1 || ms[0].UserID != admin || ms[0].Role != "owner" {
		t.Errorf("newco members = %+v, %v; want the approver as owner", ms, err)
	}

	orphan := "version: 1\norgs:\n  - slug: ghostco\n"
	pl, err = r.env.Orch.PlanServerFile(ctx, []byte(orphan))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{Confirm: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the ownerless apply to fail", func() bool {
		p, _ := r.env.Orch.ServerPlan(ctx, pl.ID)
		return p.Status == "error" && strings.Contains(p.Error, "approver")
	})
	if _, err := r.env.Store.Orgs.GetBySlug(ctx, "ghostco"); err == nil {
		t.Error("an org was created with no owner")
	}
}

// The bound repo's file plans at the branch head with source "repo"; a push
// through the server connector queues the plan; auto applies a quiet plan.
func TestServerRepoPlanAndAuto(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	r.g.Commit(t, "acme/server", "main", map[string]string{"stackr-server.yml": serverFileV1})
	if _, err := r.env.Orch.BindServerConfig(ctx, r.conn, "acme/server", "", "", false); err != nil {
		t.Fatal(err)
	}
	ps, err := r.env.Orch.ServerPlans(ctx, 10)
	if err != nil || len(ps) != 1 || ps[0].Source != "repo" || ps[0].Status != "pending" || ps[0].Commit == "" || ps[0].File != "" {
		t.Fatalf("plans after bind = %+v, %v; want one pending repo plan at a commit", ps, err)
	}

	// auto on: the next quiet plan applies with no call
	if _, err := r.env.Orch.BindServerConfig(ctx, r.conn, "acme/server", "", "", true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the auto apply", func() bool {
		ps, _ := r.env.Orch.ServerPlans(ctx, 10)
		return len(ps) > 0 && ps[0].Status == "applied"
	})
	if got := r.routes(t); len(got) != 1 {
		t.Errorf("routes after the auto apply = %v", got)
	}
	// a risky plan does not apply by itself
	r.g.Commit(t, "acme/server", "main", map[string]string{"stackr-server.yml": serverFileV1 + "orgs:\n  - slug: newco\n"})
	pl, err := r.env.Orch.PlanServerConfig(ctx)
	if err != nil || pl.Status != "pending" || pl.DecidedAt != nil {
		t.Errorf("risky plan with auto on = %+v, %v; want pending and undecided", pl, err)
	}
}

// Export reads back clean: previewing it against the same server shows
// nothing to do.
func TestServerExportPreviewsClean(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	pl, err := r.env.Orch.PlanServerFile(ctx, []byte(serverFileV1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)
	out, err := r.env.Orch.ExportServerConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := r.env.Orch.PreviewServerConfig(ctx, out)
	if err != nil {
		t.Fatalf("export does not preview: %v\n%s", err, out)
	}
	if len(pv.Changes) > 0 || len(pv.Blockers) > 0 {
		t.Errorf("export previews %+v, want clean\n%s", pv, out)
	}
}

// A backup destination takes its keys from server params, and the panel's
// backup can point at it in the same file. A missing key blocks the plan until
// it is set; the archive key of the new destination is the file's.
func TestServerDestsAndPanelDest(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	const file = `version: 1
settings:
  panel_backup_dest: offsite
params:
  backup_offsite:
    access_key:
      type: secret
    secret_key:
      type: secret
    archive_key:
      type: secret
backup_dests:
  - name: offsite
    endpoint: https://s3.example.com
    bucket: bk
    access_key: ${{ server.params.backup_offsite.access_key }}
    secret_key: ${{ server.params.backup_offsite.secret_key }}
    archive_key: ${{ server.params.backup_offsite.archive_key }}
`
	pl, err := r.env.Orch.PlanServerFile(ctx, []byte(file))
	if err != nil || !strings.Contains(pl.Plan, "server.params.backup_offsite.access_key") {
		t.Fatalf("plan = %+v, %v; want a blocker naming the missing key", pl, err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{}); !isConflict(err) {
		t.Fatalf("approve of a blocked plan = %v, want Conflict", err)
	}
	_, err = r.env.Orch.SetParams(ctx, service.ServerParamScope, []service.ParamEntry{
		{Collection: "backup_offsite", Name: "access_key", Kind: "secret", Value: "AK"},
		{Collection: "backup_offsite", Name: "secret_key", Kind: "secret", Value: "SK"},
		{Collection: "backup_offsite", Name: "archive_key", Kind: "secret", Value: "ARCHIVE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pl, err = r.env.Orch.PlanServerFile(ctx, []byte(file)); err != nil || pl.Status != "pending" {
		t.Fatalf("plan = %+v, %v; want pending", pl, err)
	}
	// params are the file's too: it declares the secrets, which the plan carries as notes only
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)

	ds, err := r.env.Orch.GlobalBackupDests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var d service.BackupDest
	for _, x := range ds {
		if x.Name == "offsite" {
			d = x
		}
	}
	if d.ID == "" || d.Bucket != "bk" || d.Endpoint != "https://s3.example.com" {
		t.Fatalf("destinations = %+v; want offsite", ds)
	}
	raw, err := r.env.Store.BackupDests.Get(ctx, d.ID)
	if err != nil || raw.AccessKey != "AK" || raw.SecretKey != "SK" || raw.ArchiveKey != "ARCHIVE" {
		t.Errorf("stored keys = %q %q %q, %v; want the params' values", raw.AccessKey, raw.SecretKey, raw.ArchiveKey, err)
	}
	if v, _ := r.env.Orch.Setting(ctx, "panel_backup_dest"); v != d.ID {
		t.Errorf("panel_backup_dest = %q, want the destination's id %q", v, d.ID)
	}
	if again, err := r.env.Orch.PlanServerFile(ctx, []byte(file)); err != nil || again.Status != "clean" {
		t.Errorf("replan = %+v, %v; want clean", again, err)
	}
}

// Export stores the keys of a destination that has no params yet, so its
// refs resolve and the file reads clean; a param the admin already set stays.
func TestServerExportSeedsDestKeys(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	_, err := r.env.Orch.CreateBackupDest(ctx, nil, service.BackupDestSpec{
		Name: "off-site", Bucket: "bk", AccessKey: "AK", SecretKey: "SK",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.env.Orch.SetParams(ctx, service.ServerParamScope, []service.ParamEntry{
		{Collection: "backup_off_site", Name: "access_key", Kind: "secret", Value: "ROTATED"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.env.Orch.ExportServerConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "${{ server.params.backup_off_site.secret_key }}") {
		t.Errorf("export lacks the dest's refs:\n%s", out)
	}
	pv, err := r.env.Orch.PreviewServerConfig(ctx, out)
	if err != nil {
		t.Fatalf("export does not preview: %v\n%s", err, out)
	}
	if len(pv.Blockers) > 0 {
		t.Errorf("export blocks: %+v\n%s", pv.Blockers, out)
	}
	vals, err := r.env.Orch.Params(ctx, service.ServerParamScope, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range vals {
		got[p.Collection+"."+p.Name] = p.Value
	}
	if got["backup_off_site.access_key"] != "ROTATED" || got["backup_off_site.secret_key"] != "SK" {
		t.Errorf("params = %v; want the rotated key kept and the missing ones seeded", got)
	}
}

// The cascade rung, an instance domain and a connector share apply; an org
// the file binds to its org file takes the shared connector.
func TestServerDefaultsDomainsSharesAndOrgBinding(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	acme := r.env.Org(t, "acme")
	r.g.Commit(t, "acme/org", "main", map[string]string{"stackr-org.yml": "version: 1\norg: acme\n"})
	const file = `version: 1
defaults:
  cpu_limit: 1.5
domains:
  - host: apps.example.com
    include_env_on_default: true
connectors:
  - name: gh
    share:
      - acme
orgs:
  - slug: acme
    repo: acme/org
    connector: gh
`
	pl, err := r.env.Orch.PlanServerFile(ctx, []byte(file))
	if err != nil || pl.Status != "pending" {
		t.Fatalf("plan = %+v, %v", pl, err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)

	d, err := r.env.Orch.SettingDefaults(ctx)
	if err != nil || d.CPULimit == nil || *d.CPULimit != 1.5 {
		t.Errorf("server rung = %+v, %v; want cpu_limit 1.5", d, err)
	}
	rs, err := r.env.Orch.AllDomainResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range rs {
		found = found || (x.Host == "apps.example.com" && x.IncludeEnvOnDefault)
	}
	if !found {
		t.Errorf("domain resources = %+v; want apps.example.com", rs)
	}
	scs, err := r.env.Orch.ServerConnectors(ctx)
	if err != nil || len(scs) != 1 || len(scs[0].OrgIDs) != 1 || scs[0].OrgIDs[0] != acme {
		t.Errorf("server connectors = %+v, %v; want gh shared with acme", scs, err)
	}
	og, err := r.env.Store.Orgs.Get(ctx, acme)
	if err != nil || og.ConfigRepo != "https://github.com/acme/org" || og.ConfigConnectorID != r.conn {
		t.Errorf("org binding = %q via %q, %v; want acme/org through the shared connector", og.ConfigRepo, og.ConfigConnectorID, err)
	}
	if again, err := r.env.Orch.PlanServerFile(ctx, []byte(file)); err != nil || again.Status != "clean" {
		t.Errorf("replan = %+v, %v; want clean", again, err)
	}

	// dropped from the file: unbinding is a removal row, kept unless ticked
	pl, err = r.env.Orch.PlanServerFile(ctx, []byte("version: 1\norgs: []\n"))
	if err != nil || !strings.Contains(pl.Plan, `"key":"org-binding:acme"`) {
		t.Fatalf("plan = %+v, %v; want an org-binding removal row", pl, err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{Ticked: []string{"org-binding:acme"}}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)
	if og, _ = r.env.Store.Orgs.Get(ctx, acme); og.ConfigRepo != "" {
		t.Errorf("org still bound to %q after the ticked removal", og.ConfigRepo)
	}
}

// Caddy checks proxy_custom only at the push. A value it cannot load fails the
// apply step and is taken back, so it does not break every later push.
func TestServerBadProxyCustomIsTakenBack(t *testing.T) {
	ctx := context.Background()
	g := servicetest.NewGit(t)
	push := func(_ context.Context, cfg json.RawMessage) error {
		if strings.Contains(string(cfg), "BADROUTE") {
			return errors.New("caddy: cannot load BADROUTE")
		}
		return nil
	}
	r := fileRig{env: servicetest.NewWith(t, []service.Option{g.Option(), service.WithProxy(push)}), g: g}
	if err := r.env.Orch.SetSetting(ctx, "proxy_custom", `[{"match":[{"host":["good.example.com"]}]}]`); err != nil {
		t.Fatal(err)
	}
	pl, err := r.env.Orch.PlanServerFile(ctx, []byte(`version: 1
settings:
  proxy_custom: '[{"match":[{"host":["BADROUTE.example.com"]}]}]'
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{Confirm: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the apply to fail", func() bool {
		p, _ := r.env.Orch.ServerPlan(ctx, pl.ID)
		return p.Status == "error" && strings.Contains(p.Error, "BADROUTE")
	})
	if v, _ := r.env.Orch.Setting(ctx, "proxy_custom"); !strings.Contains(v, "good.example.com") {
		t.Errorf("proxy_custom = %q, want the working value back", v)
	}
	if err := r.env.Orch.SetSetting(ctx, "backup_run_concurrency", "4"); err != nil {
		t.Errorf("a later push fails: %v", err)
	}
}

// The same through the web form and the settings API: SetSettings takes the
// value back, so one bad save does not break every later push.
func TestSetSettingsTakesBackBadProxyCustom(t *testing.T) {
	ctx := context.Background()
	push := func(_ context.Context, cfg json.RawMessage) error {
		if strings.Contains(string(cfg), "BADROUTE") {
			return errors.New("caddy: cannot load BADROUTE")
		}
		return nil
	}
	env := servicetest.NewWith(t, []service.Option{service.WithProxy(push)})
	good := `[{"match":[{"host":["good.example.com"]}]}]`
	if err := env.Orch.SetSetting(ctx, "proxy_custom", good); err != nil {
		t.Fatal(err)
	}
	err := env.Orch.SetSetting(ctx, "proxy_custom", `[{"match":[{"host":["BADROUTE.example.com"]}]}]`)
	if err == nil || !strings.Contains(err.Error(), "BADROUTE") {
		t.Fatalf("bad route saved: %v", err)
	}
	if v, _ := env.Orch.Setting(ctx, "proxy_custom"); v != good {
		t.Errorf("proxy_custom = %q, want the working value back", v)
	}
	if err := env.Orch.SetSetting(ctx, "backup_run_concurrency", "4"); err != nil {
		t.Errorf("a later push fails: %v", err)
	}
}
