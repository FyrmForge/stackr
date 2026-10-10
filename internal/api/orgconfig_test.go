package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// The org config routes: an owner binds, plans, previews, rejects and
// approves; a member only exports. A plan id answers under its own org.
func TestOrgConfig(t *testing.T) {
	g := servicetest.NewGit(t)
	w := newWorld(t, g.Option())
	u := w.env.User(t, "member@x", false)
	w.env.Member(t, w.acme, u, "member")
	member := w.env.APIKey(t, u, w.acme)
	conn := w.env.Connector(t, w.acme, "whsec")
	g.Commit(t, "acme/org", "main", map[string]string{
		"stackr-org.yml": "version: 1\norg: acme\nparams:\n  app:\n    all:\n      region: us\n",
	})
	const base = "/orgs/acme/config"
	want := func(key, method, path, body string, code int) string {
		t.Helper()
		got, out := w.do(t, key, method, path, body)
		if got != code {
			t.Fatalf("%s %s = %d %s, want %d", method, path, got, out, code)
		}
		return out
	}
	plan := func() service.OrgPlan {
		t.Helper()
		var p service.OrgPlan
		if err := json.Unmarshal([]byte(want(w.owner, "POST", base+"/plan", "", 200)), &p); err != nil {
			t.Fatal(err)
		}
		if p.Status != "pending" {
			t.Fatalf("plan = %+v, want pending", p)
		}
		return p
	}

	bind := `{"connector_id":"` + conn + `","repo":"acme/org","auto":false}`
	want(member, "PUT", "/orgs/acme/config-repo", bind, 403)
	var og service.Org
	if err := json.Unmarshal([]byte(want(w.owner, "PUT", "/orgs/acme/config-repo", bind, 200)), &og); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(og.ConfigRepo, "acme/org") || og.ConfigConnectorID != conn {
		t.Errorf("bound org = %+v", og)
	}

	want(member, "POST", base+"/plan", "", 403)
	first := plan()
	if !strings.Contains(first.Plan, "region") {
		t.Errorf("plan body = %s, want the region param", first.Plan)
	}

	var pv service.OrgConfigPlan
	out := want(w.owner, "POST", base+"/plan-preview", `{"file":"version: 1\norg: Acme Two\n"}`, 200)
	if err := json.Unmarshal([]byte(out), &pv); err != nil {
		t.Fatal(err)
	}
	if len(pv.Changes) != 1 || pv.Changes[0].Kind != "org" {
		t.Errorf("preview = %+v, want one org change", pv)
	}
	want(w.owner, "POST", base+"/plan-preview", `{"file":"version: 2\n"}`, 400)
	huge := `{"file":"` + strings.Repeat("x", 2<<20) + `"}`
	want(w.owner, "POST", base+"/plan-preview", huge, 413)
	want(member, "POST", base+"/plan-preview", `{"file":""}`, 403)

	var ps []service.OrgPlan
	if err := json.Unmarshal([]byte(want(w.owner, "GET", base+"/plans", "", 200)), &ps); err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].ID != first.ID {
		t.Errorf("plans = %+v, want the bind's plan and %s newest first", ps, first.ID)
	}
	want(member, "GET", base+"/plans", "", 403)
	want(w.owner, "GET", base+"/plans/"+first.ID, "", 200)
	want(w.stranger, "GET", "/orgs/other/config/plans/"+first.ID, "", 404)

	want(member, "POST", base+"/plans/"+first.ID+"/reject", "", 403)
	var rej service.OrgPlan
	if err := json.Unmarshal([]byte(want(w.owner, "POST", base+"/plans/"+first.ID+"/reject", "", 200)), &rej); err != nil {
		t.Fatal(err)
	}
	if rej.Status != "rejected" {
		t.Errorf("rejected plan = %+v", rej)
	}

	second := plan()
	want(member, "POST", base+"/plans/"+second.ID+"/approve", "", 403)
	var j service.Job
	if err := json.Unmarshal([]byte(want(w.owner, "POST", base+"/plans/"+second.ID+"/approve", "", 202)), &j); err != nil {
		t.Fatal(err)
	}
	if j.Kind != "org-apply" {
		t.Errorf("approve job = %+v, want org-apply", j)
	}

	yml := want(member, "GET", base+"/export", "", 200)
	if !strings.Contains(yml, "org: acme") {
		t.Errorf("export = %s, want the org", yml)
	}
}

// An approve body carries the ticked removals and the confirm; no body is a
// plain approve, a typo in it is a 400, and the service refuses a risky
// plan unconfirmed whatever the client did.
func TestApproveBody(t *testing.T) {
	w := newWorld(t)
	seed := func() string {
		t.Helper()
		p := service.OrgPlan{
			ID: uuid.NewString(), OrgID: w.acme, Status: "pending", Ticked: []string{}, CreatedAt: time.Now(),
			Plan: `{"changes":[{"kind":"defaults","impact":"redeploys 3 tiles"},` +
				`{"kind":"share-delete","key":"share:old","optional":true}]}`,
		}
		if err := w.env.Store.OrgPlans.Create(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		return "/orgs/acme/config/plans/" + p.ID + "/approve"
	}
	for _, c := range []struct {
		name, body string
		code       int
	}{
		{"no body", "", 409},
		{"empty object", `{}`, 409},
		{"a typo", `{"confirmed":true}`, 400},
		{"a tick outside the plan", `{"confirm":true,"ticked":["share:media"]}`, 400},
		{"confirmed and ticked", `{"confirm":true,"ticked":["share:old"]}`, 202},
	} {
		if code, out := w.do(t, w.owner, "POST", seed(), c.body); code != c.code {
			t.Errorf("%s = %d %s, want %d", c.name, code, out, c.code)
		}
	}
}
