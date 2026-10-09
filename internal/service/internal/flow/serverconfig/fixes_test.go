package serverconfig_test

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The fix round's scenarios (docs/rewrite/tasks/serverconfig-fixes.md, F2).

// An org slugged "all" is an org, never the share-all switch: the file cannot
// name it, and a row for it (live data from before the slug was reserved)
// is told apart from share-all by Field, in the row and in the removal key.
func TestShareRowsNeverMeanShareAll(t *testing.T) {
	if _, err := serverconfig.Parse([]byte(v1 + "connectors:\n  - {name: gh-main, share: [acme, all]}\n")); err == nil || !strings.Contains(err.Error(), "not an org slug") {
		t.Errorf("share: [all] parsed: %v", err)
	}
	l := live()
	l.Orgs = append(l.Orgs, store.Org{ID: "o9", Slug: "all", Name: "All"})
	f := &serverconfig.File{Version: 1, Connectors: []serverconfig.Connector{{Name: "gh-main", Share: &serverconfig.Share{Orgs: []string{"acme", "all"}}}}}
	p := serverconfig.Diff(f, l)
	if len(p.Changes) != 1 {
		t.Fatalf("changes = %+v", p.Changes)
	}
	if c := p.Changes[0]; c.Kind != "connector-share" || c.Field != "org" || c.New != "all" || c.Impact != "" {
		t.Errorf("an org named all is an org row: %+v", c)
	}
	// dropping that org from the list and share-all (gh-all) are two rows with two keys
	l.Connectors[0].OrgIDs = []string{"o1", "o9"}
	l.Connectors[1].OrgIDs = []string{"o9"}
	f = &serverconfig.File{Version: 1, Connectors: []serverconfig.Connector{
		{Name: "gh-main", Share: &serverconfig.Share{Orgs: []string{"acme"}}},
		{Name: "gh-all", Share: &serverconfig.Share{Orgs: []string{"acme"}}},
	}}
	l.Orgs[0].ConfigRepo = ""
	p = serverconfig.Diff(f, l)
	got := map[string]string{}
	for _, c := range p.Removals() {
		got[c.Key] = c.Field
	}
	want := map[string]string{
		"connector-share:gh-main/org:all": "org",
		"connector-share:gh-all/all":      "share",
		"connector-share:gh-all/org:all":  "org",
	}
	for k, field := range want {
		if got[k] != field {
			t.Errorf("removal %s field = %q, want %q (got %v)", k, got[k], field, got)
		}
	}
	// the all-share row says what it is
	for _, c := range p.Removals() {
		if c.Key == "connector-share:gh-all/all" && (c.Field != "share" || c.Old != "all") {
			t.Errorf("share-all removal row = %+v", c)
		}
	}
}

const acmeOrg = "{slug: acme, repo: https://github.com/acme/infra, branch: main, connector: gh-main, auto: true}\n"

// Rule 5: a missing server param holds back that item only.
func TestMissingParamBlocksOnlyItsItem(t *testing.T) {
	file := "routes:\n  - {host: old.io, mode: https, target: '10.0.0.5'}\n  - {host: raw.example.net, mode: passthrough, target: '10.0.0.6'}\n  - {host: new.io, mode: http, target: h}\n" +
		"backup_dests:\n" + offsite + offsiteArchive +
		"  - {name: cold, bucket: cb, access_key: '${{ server.params.cold.ak }}', secret_key: '${{ server.params.cold.sk }}'}\n" +
		"orgs:\n  - {slug: newco}\n  - " + acmeOrg
	p := diff(t, file)
	if p.Blocked() {
		t.Fatalf("a missing param blocks the plan: %q", p.Blockers)
	}
	wantKinds(t, p, "route:new.io", "org-create:newco")
	has(t, "notes", p.Notes, "backup_dests.cold waits for server.params.cold.ak, server.params.cold.sk")
}

func TestMissingParamDropsThatItemsRowsAndNotes(t *testing.T) {
	cold := "  - {name: cold, bucket: cb, access_key: '${{ server.params.cold.ak }}', secret_key: '${{ server.params.cold.sk }}'}\n"
	file := "settings:\n  workers: 3\n  panel_backup_dest: cold\nroutes:\n  - {host: old.io, mode: https, target: '10.0.0.5'}\n  - {host: raw.example.net, mode: passthrough, target: '10.0.0.6'}\n  - {host: new.io, mode: http, target: h}\n" +
		"backup_dests:\n" + offsite + offsiteArchive + cold + "orgs:\n  - {slug: newco}\n  - " + acmeOrg
	p := diff(t, file, func(l *serverconfig.Live) { l.Params["cold.ak"] = params.Value{V: "a", Secret: true} })
	if p.Blocked() {
		t.Fatalf("a missing param blocks the plan: %q", p.Blockers)
	}
	// the unrelated rows stay; the dest row and the setting that names it wait
	wantKinds(t, p, "settings:workers", "route:new.io", "org-create:newco")
	has(t, "notes", p.Notes, "backup_dests.cold waits for server.params.cold.sk")
	has(t, "notes", p.Notes, "settings.panel_backup_dest waits for backup_dests.cold")
}

// A ref to a param the same file declares resolves against the file: the
// walk stores params first.
func TestRefToParamTheFileDeclares(t *testing.T) {
	file := "params:\n  cold:\n    ak: {type: param, value: AKIA}\n    sk: {type: param, value: SEC}\n" +
		"backup_dests:\n" + offsite + offsiteArchive +
		"  - {name: cold, bucket: cb, access_key: '${{ server.params.cold.ak }}', secret_key: '${{ server.params.cold.sk }}'}\n"
	p := diff(t, file)
	if p.Blocked() || len(p.Notes) != 0 {
		t.Fatalf("blockers %q notes %q", p.Blockers, p.Notes)
	}
	wantKinds(t, p, "param:cold.ak", "param:cold.sk", "dest:cold")
	// a secret the file only names has no value to resolve against
	file = strings.Replace(file, "ak: {type: param, value: AKIA}", "ak: {type: secret}", 1)
	p = diff(t, file)
	has(t, "notes", p.Notes, "backup_dests.cold waits for server.params.cold.ak")
}

// Export then plan reads clean even when a create-only check would refuse
// what already exists.
func TestExportThenPlanKeepsCreateOnlyChecksOffExistingRows(t *testing.T) {
	l := full()
	delete(l.Settings, "dns_provider") // a wildcard http route stays: SetSettings allows clearing it
	l.Orgs = append(l.Orgs, store.Org{ID: "o3", Slug: "example", Name: "Example"})
	l.TLSOff = true
	l.Routes = append(l.Routes, store.Route{ID: "r-pt", Host: "pt.other.net", Mode: "passthrough", Target: "10.0.0.8:443"})
	out, err := serverconfig.Export(l)
	if err != nil {
		t.Fatal(err)
	}
	f, err := serverconfig.Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	p := serverconfig.Diff(f, l)
	if p.Blocked() || len(p.Changes) != 0 {
		t.Errorf("export diffs against itself: blockers %q changes %+v\n%s", p.Blockers, p.Changes, out)
	}
	// a new one still gets them
	p = diff(t, "domains:\n  - {host: example.com}\n  - {host: globex.example.net}\n")
	has(t, "blockers", p.Blockers, "another organization's slug")
}

// The root instance resource and settings.root_domain are one thing: a from:
// rename of it that names another root_domain is a blocker, and with no
// root_domain in the file the rename stands alone (it sets the root).
func TestRootRenameAndRootDomainAgree(t *testing.T) {
	for _, c := range []struct {
		name    string
		file    string
		blocker string
	}{
		{"exported root_domain left alone", "settings:\n  root_domain: example.com\ndomains:\n  - {host: new.com, from: example.com}\n", "settings.root_domain"},
		{"root_domain somewhere else", "settings:\n  root_domain: other.net\ndomains:\n  - {host: new.com, from: example.com}\n", "settings.root_domain"},
		{"two roots: root_domain and a from", "settings:\n  root_domain: new.com\ndomains:\n  - {host: other.com, from: example.com}\n", "settings.root_domain"},
		{"root_domain and from agree", "settings:\n  root_domain: new.com\ndomains:\n  - {host: new.com, from: example.com}\n", ""},
		{"from alone", "domains:\n  - {host: new.com, from: example.com}\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := diff(t, c.file)
			has(t, "blockers", p.Blockers, c.blocker)
		})
	}
}

// The rename target gets the add path's checks, and so does a root_domain move.
func TestRenameTargetGetsTheClashChecks(t *testing.T) {
	for _, c := range []struct {
		name    string
		file    string
		mut     func(*serverconfig.Live)
		blocker string
	}{
		{"external route", "domains:\n  - {host: old.io, from: example.com}\n", nil, "external route"},
		{"panel host", "domains:\n  - {host: panel.example.com, from: example.com}\n", nil, "panel domain"},
		{
			"another resource", "domains:\n  - {host: tools.net, from: example.com}\n", func(l *serverconfig.Live) {
				l.Domains = append(l.Domains, store.DomainResource{ID: "dr4", Level: "org", OrgID: ptr("o1"), Host: "tools.net"})
			}, "already a domain resource",
		},
		{
			"tile domain", "domains:\n  - {host: taken.io, from: example.com}\n", func(l *serverconfig.Live) { l.Taken = append(l.Taken, "taken.io") },
			"tile domain",
		},
		{"root_domain onto a route", "settings:\n  root_domain: old.io\n", nil, "external route"},
		{"root_domain onto the panel", "settings:\n  root_domain: panel.example.com\n", nil, "panel domain"},
		{"root_domain onto the panel the file moves it to", "settings:\n  root_domain: new.io\n  panel_domain: new.io\n", nil, "panel domain"},
		{"a clean rename", "domains:\n  - {host: new.com, from: example.com}\n", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mut []func(*serverconfig.Live)
			if c.mut != nil {
				mut = append(mut, c.mut)
			}
			p := diff(t, c.file, mut...)
			has(t, "blockers", p.Blockers, c.blocker)
		})
	}
}

func TestTwoRenamesFromOneHost(t *testing.T) {
	_, err := serverconfig.Parse([]byte(v1 + "domains:\n  - {host: a.com, from: example.com}\n  - {host: b.com, from: example.com}\n"))
	if err == nil || !strings.Contains(err.Error(), "renamed twice") {
		t.Errorf("err = %v", err)
	}
}

// Leaving share-all would strand a bound org: a note, no row to tick.
func TestShareAllRemovalWaitsForBoundOrgs(t *testing.T) {
	p := diff(t, "connectors:\n  - {name: gh-all, share: [acme]}\n", func(l *serverconfig.Live) { l.Connectors[1].Bound = []string{"o2"} })
	if got := removalKeys(p); len(got) != 0 {
		t.Errorf("removal offered: %q", got)
	}
	has(t, "notes", p.Notes, "gh-all stays shared with all")
	has(t, "notes", p.Notes, "globex")
	// the file's own binding counts too
	p = diff(t, "connectors:\n  - {name: gh-all, share: [acme]}\norgs:\n  - {slug: globex, repo: https://github.com/globex/ops, connector: gh-all}\n")
	if got := removalKeys(p); len(got) != 0 {
		t.Errorf("removal offered: %q", got)
	}
	has(t, "notes", p.Notes, "stays shared with all")
	// a bound org the list keeps does not hold it back
	p = diff(t, "connectors:\n  - {name: gh-all, share: [acme, globex]}\n", func(l *serverconfig.Live) { l.Connectors[1].Bound = []string{"o2"} })
	if got := removalKeys(p); len(got) != 1 {
		t.Errorf("removals = %q", got)
	}
}

// removalKeys are the share removal rows' keys (other removals aside).
func removalKeys(p serverconfig.Plan) []string {
	var out []string
	for _, c := range p.Removals() {
		if c.Kind == "connector-share-delete" {
			out = append(out, c.Key)
		}
	}
	return out
}

// Export writes only what Parse accepts; a value the panel took and Parse
// refuses stays out, so export then plan and the DR path work.
func TestExportSkipsValuesParseRefuses(t *testing.T) {
	l := live()
	l.Settings["acme_email"] = "Ops <ops@example.com>"
	l.Settings["dns_provider"] = "route53"
	out, err := serverconfig.Export(l)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "route53") || strings.Contains(string(out), "Ops <") {
		t.Errorf("export holds a value Parse refuses:\n%s", out)
	}
	f, err := serverconfig.Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if p := serverconfig.Diff(f, l); p.Blocked() || len(p.Changes) != 0 {
		t.Errorf("plan = %+v", p)
	}
}

// A literal protect password never reaches the exported file.
func TestExportLeavesLiteralProtectPassword(t *testing.T) {
	l := live()
	l.Defaults = settings.Settings{ProtectUser: ptr("bob"), ProtectPassword: ptr("hunter2")}
	out, err := serverconfig.Export(l)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "hunter2") || !strings.HasPrefix(string(out), "# warning:") {
		t.Errorf("export:\n%s", out)
	}
	if _, err := serverconfig.Parse(out); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
