package serverconfig_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const v1 = "version: 1\n"

func ptr[T any](v T) *T { return &v }

// live is a server with the things each block diffs against: an org acme
// bound to its repo through the shared connector gh-main, globex unbound,
// the instance domain example.com with 3 hosts under it, two routes, the
// local destination and the S3 destination offsite (keys in server params,
// the panel backs up to it), the cascade rung cpu_limit 1 with 4 tiles.
func live() serverconfig.Live {
	return serverconfig.Live{
		Settings: map[string]string{
			"panel_domain":      "panel.example.com",
			"root_domain":       "example.com",
			"workers":           "4",
			"cleanup_enabled":   "true",
			"panel_backup_dest": "d-off",
			"acme_email":        "ops@example.com",
		},
		Defaults: settings.Settings{CPULimit: ptr(1.0)},
		Tiles:    4,
		Params: map[string]params.Value{
			"s3.region":                  {V: "eu"},
			"backup_offsite.access_key":  {V: "AK", Secret: true},
			"backup_offsite.secret_key":  {V: "SK", Secret: true},
			"backup_offsite.archive_key": {V: "ARC", Secret: true},
			"smtp.password":              {V: "pw", Secret: true},
		},
		Routes: []store.Route{
			{ID: "r-old", Host: "old.io", Mode: "https", Target: "10.0.0.5:443"},
			{ID: "r-raw", Host: "raw.example.net", Mode: "passthrough", Target: "10.0.0.6:443"},
		},
		Dests: []store.BackupDest{
			{ID: "d-loc", Kind: "local", Name: "local", Shared: true},
			{
				ID: "d-off", Kind: "s3", Name: "offsite", Endpoint: "https://s3.example.com", Region: "eu",
				Bucket: "bk", AccessKey: "AK", SecretKey: "SK", ArchiveKey: "ARC", Shared: true,
			},
		},
		DestSchedules: map[string]int{},
		Domains: []store.DomainResource{
			{ID: "dr1", Level: "instance", Host: "example.com"},
			{ID: "dr2", Level: "org", OrgID: ptr("o1"), Host: "acme.example.com"},
		},
		DomainHosts: map[string]int{"dr1": 3},
		Taken:       []string{"example.com", "acme.example.com", "web.acme.example.com"},
		Connectors: []serverconfig.ConnectorLive{
			{Connector: store.Connector{ID: "c1", Name: "gh-main"}, OrgIDs: []string{"o1"}},
			{Connector: store.Connector{ID: "c2", Name: "gh-all", ShareAll: true}},
		},
		Orgs: []store.Org{
			{
				ID: "o1", Slug: "acme", Name: "Acme", ConfigRepo: "https://github.com/acme/infra",
				ConfigBranch: "main", ConfigConnectorID: "c1", ConfigAuto: true,
			},
			{ID: "o2", Slug: "globex", Name: "Globex"},
		},
		Claims: []org.Claim{{Host: "initech.example.com", OrgID: "o2"}},
	}
}

func parse(t *testing.T, body string) *serverconfig.File {
	t.Helper()
	f, err := serverconfig.Parse([]byte(v1 + body))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	return f
}

// diff parses body (under version: 1) and diffs it against live with mut applied.
func diff(t *testing.T, body string, mut ...func(*serverconfig.Live)) serverconfig.Plan {
	t.Helper()
	l := live()
	for _, m := range mut {
		m(&l)
	}
	return serverconfig.Diff(parse(t, body), l)
}

// kinds lists "kind:field-or-tile" of every change, in plan order.
func kinds(p serverconfig.Plan) []string {
	var out []string
	for _, c := range p.Changes {
		k := c.Kind
		switch {
		case c.Field != "":
			k += ":" + c.Field
		case c.Tile != "":
			k += ":" + c.Tile
		}
		out = append(out, k)
	}
	return out
}

func wantKinds(t *testing.T, p serverconfig.Plan, want ...string) {
	t.Helper()
	got := kinds(p)
	if !slices.Equal(got, want) {
		t.Errorf("changes = %q\nwant      %q\nblockers %q notes %q", got, want, p.Blockers, p.Notes)
	}
}

func has(t *testing.T, what string, got []string, want string) {
	t.Helper()
	if want == "" {
		if len(got) != 0 {
			t.Errorf("%s = %q, want none", what, got)
		}
		return
	}
	if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, want) }) {
		t.Errorf("%s = %q, want one with %q", what, got, want)
	}
}

// ---- parse ----

func TestParseRefuses(t *testing.T) {
	for _, c := range []struct{ name, file, want string }{
		{"version", "version: 2\n", "unsupported version"},
		{"unknown key", v1 + "stacks: {}\n", "stacks"},
		{"unknown setting", v1 + "settings:\n  nope: 1\n", "not an install setting"},
		{"cascade knob is not a setting", v1 + "settings:\n  cpu_limit: 1\n", "not an install setting"},
		{"binding knob", v1 + "settings:\n  server_config_repo: x/y\n", "own binding"},
		{"bad int", v1 + "settings:\n  workers: lots\n", "whole number"},
		{"list value", v1 + "settings:\n  workers: [1]\n", "single value"},
		{"bad email", v1 + "settings:\n  acme_email: nope\n", "email"},
		{"provider not built", v1 + "settings:\n  dns_provider: route53\n", "cloudflare"},
		{"bad cidr", v1 + "settings:\n  trusted_proxies: 10.0.0.0/99\n", "not an IP or CIDR"},
		{"proxy_custom not an array", v1 + "settings:\n  proxy_custom: '{}'\n", "JSON array"},
		{"wildcard panel", v1 + "settings:\n  panel_domain: '*.example.com'\n", "wildcard"},
		{"half a basic auth pair", v1 + "defaults:\n  protect_user: bob\n", "password"},
		{"negative limit", v1 + "defaults:\n  mem_limit_mb: -1\n", "negative"},
		{"secret with a value", v1 + "params:\n  a:\n    b:\n      type: secret\n      value: x\n", "never goes in the file"},
		{"route mode", v1 + "routes:\n  - host: a.example.com\n    mode: tcp\n    target: x\n", "passthrough, http or https"},
		{"route host with port", v1 + "routes:\n  - host: 'a.example.com:81'\n    mode: http\n    target: x\n", "bare hostname"},
		{"route twice", v1 + "routes:\n  - {host: a.example.com, mode: http, target: x}\n  - {host: a.example.com, mode: http, target: y}\n", "twice"},
		{"route target", v1 + "routes:\n  - {host: a.example.com, mode: http, target: 'x/y'}\n", "host or host:port"},
		{"insecure on http", v1 + "routes:\n  - {host: a.example.com, mode: http, target: x, insecure: true}\n", "https route"},
		{"local dest", v1 + "backup_dests:\n  - {name: local, bucket: b, access_key: x, secret_key: y}\n", "install's own"},
		{"dest kind", v1 + "backup_dests:\n  - {name: a, kind: gcs, bucket: b, access_key: x, secret_key: y}\n", "kind is s3"},
		{"literal secret key", v1 + "backup_dests:\n  - {name: a, bucket: b, access_key: '${{ server.params.a.k }}', secret_key: hunter2}\n", "never the value"},
		{"org ref in a server file", v1 + "backup_dests:\n  - {name: a, bucket: b, access_key: '${{ server.params.a.k }}', secret_key: '${{ org.params.a.k }}'}\n", "never the value"},
		{"literal archive key", v1 + "backup_dests:\n  - {name: a, bucket: b, access_key: '${{ server.params.a.k }}', secret_key: '${{ server.params.a.s }}', archive_key: abc}\n", "archive_key"},
		{"dest endpoint", v1 + "backup_dests:\n  - {name: a, bucket: b, endpoint: s3.example.com, access_key: '${{ server.params.a.k }}', secret_key: '${{ server.params.a.s }}'}\n", "http(s) URL"},
		{"domain renames to itself", v1 + "domains:\n  - {host: a.example.com, from: a.example.com}\n", "itself"},
		{"domain renamed and declared", v1 + "domains:\n  - {host: b.example.com, from: a.example.com}\n  - {host: a.example.com}\n", "renamed away and declared"},
		{"share word", v1 + "connectors:\n  - {name: a, share: some}\n", "share is all"},
		{"share slug", v1 + "connectors:\n  - {name: a, share: [Bad]}\n", "not an org slug"},
		{"org slug", v1 + "orgs:\n  - {slug: Acme}\n", "not an org slug"},
		{"org twice", v1 + "orgs:\n  - {slug: acme}\n  - {slug: acme}\n", "twice"},
		{"org branch without repo", v1 + "orgs:\n  - {slug: acme, branch: main}\n", "go with repo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := serverconfig.Parse([]byte(c.file))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// A block that is absent says nothing; one that is present, even empty, is
// the whole list. Scalars decode as their type, null resets.
func TestParseShape(t *testing.T) {
	f := parse(t, "settings:\n  workers: 3\n  cleanup_enabled: true\n  acme_email:\nconnectors:\n  - {name: a, share: all}\n  - {name: b, share: [x, y]}\n  - {name: c, share: []}\n  - {name: d}\n")
	if f.Routes != nil || f.BackupDests != nil || f.Domains != nil || f.Orgs != nil || f.Defaults != nil {
		t.Errorf("absent blocks are nil: %+v", f)
	}
	for k, want := range map[string]string{"workers": "3", "cleanup_enabled": "true", "acme_email": ""} {
		if v, ok := f.Settings[k].(string); !ok || v != want {
			t.Errorf("settings.%s = %#v, want %q", k, f.Settings[k], want)
		}
	}
	shares := []*serverconfig.Share{{All: true}, {Orgs: []string{"x", "y"}}, {Orgs: []string{}}, nil}
	for i, c := range f.Connectors {
		if !shareEq(c.Share, shares[i]) {
			t.Errorf("connector %s share = %+v, want %+v", c.Name, c.Share, shares[i])
		}
	}
	g := parse(t, "routes: []\nbackup_dests: []\ndomains: []\norgs: []\n")
	if g.Routes == nil || g.BackupDests == nil || g.Domains == nil || g.Orgs == nil {
		t.Errorf("present empty blocks are not nil: %+v", g)
	}
}

func shareEq(a, b *serverconfig.Share) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.All == b.All && (a.Orgs == nil) == (b.Orgs == nil) && slices.Equal(a.Orgs, b.Orgs)
}

func TestParseNormalises(t *testing.T) {
	f := parse(t, "settings:\n  panel_domain: HTTPS://Panel.Example.com/\nroutes:\n  - {host: A.Example.com, mode: http, target: '10.0.0.1'}\n  - {host: b.example.com, mode: https, target: 'h:8443'}\ndomains:\n  - {host: Example.COM}\norgs:\n  - {slug: acme, repo: acme/infra}\n")
	if v, _ := f.Settings["panel_domain"].(string); v != "panel.example.com" {
		t.Errorf("panel_domain = %q", v)
	}
	if r := f.Routes[0]; r.Host != "a.example.com" || r.Target != "10.0.0.1:80" {
		t.Errorf("route = %+v", r)
	}
	if r := f.Routes[1]; r.Target != "h:8443" {
		t.Errorf("route = %+v", r)
	}
	if f.Domains[0].Host != "example.com" || f.Orgs[0].Repo != "https://github.com/acme/infra" {
		t.Errorf("domain %+v org %+v", f.Domains[0], f.Orgs[0])
	}
}

// ---- settings ----

func TestDiffSettings(t *testing.T) {
	for _, c := range []struct {
		name    string
		file    string
		mut     func(*serverconfig.Live)
		changes []string
		impact  string
		blocker string
		note    string
	}{
		{name: "nothing named says nothing", file: ""},
		{name: "same value, typed", file: "settings:\n  workers: 4\n  cleanup_enabled: true\n  acme_email: ops@example.com\n"},
		{name: "int change", file: "settings:\n  workers: 3\n", changes: []string{"settings:workers"}},
		{name: "default is the live value when unset", file: "settings:\n  orphans_enabled: true\n"},
		{name: "reset to default", file: "settings:\n  workers:\n", changes: []string{"settings:workers"}},
		{name: "reset when already default", file: "settings:\n  orphan_retention_days:\n"},
		{
			name: "panel domain", file: "settings:\n  panel_domain: new.example.com\n",
			changes: []string{"settings:panel_domain"}, impact: "panel moves to new.example.com and stops answering on panel.example.com at once; point DNS at this box first",
		},
		{name: "root domain cannot be cleared", file: "settings:\n  root_domain:\n", blocker: "cannot be cleared"},
		{name: "panel domain onto a tile domain", file: "settings:\n  panel_domain: web.acme.example.com\n", changes: []string{"settings:panel_domain"}, impact: "panel moves to", blocker: "the panel would shadow it"},
		{
			name: "root domain", file: "settings:\n  root_domain: new.com\n",
			changes: []string{"settings:root_domain"}, impact: "moves 3 hosts, issues 3 certificates; point DNS first",
		},
		{
			name: "dns provider", file: "settings:\n  dns_provider: cloudflare\n",
			changes: []string{"settings:dns_provider"}, impact: "DNS token on the proxy",
		},
		{
			name: "proxy_custom", file: "settings:\n  proxy_custom: '[]'\n",
			changes: []string{"settings:proxy_custom"}, impact: "replaces the extra Caddy routes",
		},
		{
			name: "trusted proxies", file: "settings:\n  trusted_proxies: 10.0.0.0/8, 127.0.0.1\n",
			changes: []string{"settings:trusted_proxies"}, impact: "spoof",
		},
		{
			name: "acme email", file: "settings:\n  acme_email: new@example.com\n",
			changes: []string{"settings:acme_email"}, impact: "contact address",
		},
		{name: "trusted proxies spelling is not a change", file: "settings:\n  trusted_proxies: '10.0.0.0/8,127.0.0.1'\n", mut: func(l *serverconfig.Live) {
			l.Settings["trusted_proxies"] = "10.0.0.0/8 127.0.0.1"
		}},
		{name: "panel dest by name is the live id", file: "settings:\n  panel_backup_dest: offsite\n"},
		{name: "panel dest back to local", file: "settings:\n  panel_backup_dest: local\n", changes: []string{"settings:panel_backup_dest"}},
		{name: "panel dest unknown", file: "settings:\n  panel_backup_dest: nowhere\n", blocker: "no backup destination named"},
		{name: "panel dest the file creates", file: "settings:\n  panel_backup_dest: cold\nbackup_dests:\n  - {name: cold, bucket: b, access_key: '${{ server.params.s3.access }}', secret_key: '${{ server.params.s3.secret }}'}\n", mut: func(l *serverconfig.Live) {
			l.Params["s3.access"] = params.Value{V: "a", Secret: true}
			l.Params["s3.secret"] = params.Value{V: "s", Secret: true}
		}, changes: []string{"settings:panel_backup_dest", "dest:cold", "dest-delete:offsite"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mut []func(*serverconfig.Live)
			if c.mut != nil {
				mut = append(mut, c.mut)
			}
			p := diff(t, c.file, mut...)
			wantKinds(t, p, c.changes...)
			has(t, "impacts", p.Impacts(), c.impact)
			has(t, "blockers", p.Blockers, c.blocker)
			has(t, "notes", p.Notes, c.note)
			if c.impact == "" && p.Risky() {
				t.Errorf("risky without a reason: %q", p.Impacts())
			}
		})
	}
}

// The panel domain change carries no secrets and an empty one is allowed.
func TestDiffSettingsLines(t *testing.T) {
	p := diff(t, "settings:\n  workers: 3\n  panel_backup_dest: local\n")
	c := p.Changes[0]
	if c.Field != "panel_backup_dest" || c.Old != "offsite" || c.New != "local" && c.New != "" {
		t.Errorf("panel dest line = %+v", c)
	}
	if w := p.Changes[1]; w.Old != "4" || w.New != "3" {
		t.Errorf("workers line = %+v", w)
	}
}

// ---- defaults ----

func TestDiffDefaults(t *testing.T) {
	for _, c := range []struct {
		name    string
		file    string
		mut     func(*serverconfig.Live)
		changes []string
		impact  string
	}{
		{name: "absent says nothing", file: ""},
		{name: "same", file: "defaults:\n  cpu_limit: 1\n"},
		{
			name: "cpu redeploys every tile", file: "defaults:\n  cpu_limit: 2\n",
			changes: []string{"defaults:cpu_limit"}, impact: "redeploys 4 tiles",
		},
		{
			name: "block is the whole rung: a field it drops clears", file: "defaults:\n  mem_limit_mb: 512\n",
			changes: []string{"defaults:cpu_limit", "defaults:mem_limit_mb"}, impact: "redeploys 4 tiles",
		},
		{
			name: "one tile", file: "defaults:\n  cpu_limit: 2\n", mut: func(l *serverconfig.Live) { l.Tiles = 1 },
			changes: []string{"defaults:cpu_limit"}, impact: "redeploys 1 tile",
		},
		{
			name: "no tiles, no redeploy: a plain change", file: "defaults:\n  cpu_limit: 2\n", mut: func(l *serverconfig.Live) { l.Tiles = 0 },
			changes: []string{"defaults:cpu_limit"},
		},
		{
			name: "protect changes basic auth", file: "defaults:\n  cpu_limit: 1\n  protect: true\n  protect_user: bob\n  protect_password: s3cret\n",
			changes: []string{"defaults:protect", "defaults:protect_user", "defaults:protect_password"}, impact: "basic auth in front of every URL",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mut []func(*serverconfig.Live)
			if c.mut != nil {
				mut = append(mut, c.mut)
			}
			p := diff(t, c.file, mut...)
			wantKinds(t, p, c.changes...)
			has(t, "impacts", p.Impacts(), c.impact)
			if len(p.Impacts()) > 1 {
				t.Errorf("one impact line per rung change, got %q", p.Impacts())
			}
			for _, ch := range p.Changes {
				if ch.Field == "protect_password" && (ch.Old != "" || ch.New != "") {
					t.Errorf("a password is never shown: %+v", ch)
				}
			}
		})
	}
}

// ---- params ----

func TestDiffParams(t *testing.T) {
	for _, c := range []struct {
		name    string
		file    string
		changes []string
		blocker string
		note    string
	}{
		{name: "same", file: "params:\n  s3:\n    region: {type: param, value: eu}\n    key: {type: secret}\n", note: "s3.key is declared and not set"},
		{name: "new param", file: "params:\n  s3:\n    bucket: {type: param, value: b}\n", changes: []string{"param:s3.bucket"}},
		{name: "changed param", file: "params:\n  s3:\n    region: {type: param, value: us}\n", changes: []string{"param-update:s3.region"}},
		{name: "param becomes secret", file: "params:\n  s3:\n    region: {type: secret}\n", changes: []string{"param-update:s3.region"}},
		{name: "secret never back to a param", file: "params:\n  smtp:\n    password: {type: param, value: x}\n", blocker: "never turned back into a param"},
		{name: "secret set is quiet", file: "params:\n  smtp:\n    password: {type: secret}\n"},
		{name: "never removed", file: "params:\n  other:\n    a: {type: param, value: 1}\n", changes: []string{"param:other.a"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := diff(t, c.file)
			wantKinds(t, p, c.changes...)
			has(t, "blockers", p.Blockers, c.blocker)
			has(t, "notes", p.Notes, c.note)
			if len(p.Removals()) != 0 {
				t.Errorf("a param is never a removal row: %+v", p.Removals())
			}
		})
	}
}

// ---- backup dests ----

const offsite = "  - name: offsite\n    endpoint: https://s3.example.com\n    region: eu\n    bucket: bk\n    shared: true\n" +
	"    access_key: ${{ server.params.backup_offsite.access_key }}\n" +
	"    secret_key: ${{ server.params.backup_offsite.secret_key }}\n"

const offsiteArchive = "    archive_key: ${{ server.params.backup_offsite.archive_key }}\n"

func TestDiffDests(t *testing.T) {
	cold := func(extra string) string {
		return "backup_dests:\n" + offsite + offsiteArchive +
			"  - {name: cold, bucket: cb, access_key: '${{ server.params.cold.ak }}', secret_key: '${{ server.params.cold.sk }}'" + extra + "}\n"
	}
	withCold := func(l *serverconfig.Live) {
		l.Params["cold.ak"] = params.Value{V: "a", Secret: true}
		l.Params["cold.sk"] = params.Value{V: "s", Secret: true}
	}
	for _, c := range []struct {
		name    string
		file    string
		mut     func(*serverconfig.Live)
		changes []string
		blocker string
		note    string
	}{
		{name: "absent block says nothing", file: ""},
		{name: "same, archive key included", file: "backup_dests:\n" + offsite + offsiteArchive},
		{name: "same, trailing slash on the endpoint", file: "backup_dests:\n" + strings.Replace(offsite, "s3.example.com", "s3.example.com/", 1)},
		{name: "new dest", file: cold(""), mut: withCold, changes: []string{"dest:cold"}},
		{
			name: "new dest, missing secret", file: cold(""), mut: func(l *serverconfig.Live) {
				l.Params["cold.ak"] = params.Value{V: "a", Secret: true}
			},
			note: "backup_dests.cold waits for server.params.cold.sk",
		},
		{
			name: "new dest, declared but empty secret", file: cold(""), mut: func(l *serverconfig.Live) {
				withCold(l)
				l.Params["cold.sk"] = params.Value{Secret: true}
			},
			note: "backup_dests.cold waits for server.params.cold.sk",
		},
		{
			name: "field changes", file: "backup_dests:\n" + strings.Replace(offsite, "bucket: bk", "bucket: other", 1),
			changes: []string{"dest-update:bucket"}, note: "",
		},
		{
			name: "sharing changes", file: "backup_dests:\n" + strings.Replace(offsite, "shared: true", "shared: false", 1),
			changes: []string{"dest-update:shared"},
		},
		{
			name: "key rotated: changed, never shown", file: "backup_dests:\n" + offsite, mut: func(l *serverconfig.Live) {
				l.Params["backup_offsite.secret_key"] = params.Value{V: "NEW", Secret: true}
			},
			changes: []string{"dest-update:secret_key"},
		},
		{
			name: "archive key of an existing dest cannot change", file: "backup_dests:\n" + offsite + offsiteArchive, mut: func(l *serverconfig.Live) {
				l.Params["backup_offsite.archive_key"] = params.Value{V: "OTHER", Secret: true}
			},
			blocker: "archive key of an existing destination cannot change",
		},
		{name: "dropped dest is a removal row", file: "backup_dests: []\n", changes: []string{"dest-delete:offsite"}, mut: func(l *serverconfig.Live) {
			l.Settings["panel_backup_dest"] = ""
		}},
		{name: "a dest the panel backup uses stays", file: "backup_dests: []\n", note: "the panel backup uses it"},
		{
			name: "a dest a schedule uses stays", file: "backup_dests: []\n", mut: func(l *serverconfig.Live) {
				l.Settings["panel_backup_dest"] = ""
				l.DestSchedules["d-off"] = 2
			},
			note: "2 volume schedules use it",
		},
		{
			name: "dropped dest the file moves the panel off is a row", file: "settings:\n  panel_backup_dest: local\nbackup_dests: []\n",
			changes: []string{"settings:panel_backup_dest", "dest-delete:offsite"},
		},
		{name: "local is never a removal row", file: "backup_dests:\n" + offsite + offsiteArchive},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mut []func(*serverconfig.Live)
			if c.mut != nil {
				mut = append(mut, c.mut)
			}
			p := diff(t, c.file, mut...)
			wantKinds(t, p, c.changes...)
			has(t, "blockers", p.Blockers, c.blocker)
			has(t, "notes", p.Notes, c.note)
			for _, ch := range p.Changes {
				if ch.Field == "secret_key" || ch.Field == "access_key" {
					if ch.Old != "" || ch.New != "" {
						t.Errorf("a key is never shown: %+v", ch)
					}
				}
			}
		})
	}
}

// ---- domains ----

func TestDiffDomains(t *testing.T) {
	for _, c := range []struct {
		name    string
		file    string
		mut     func(*serverconfig.Live)
		changes []string
		impact  string
		blocker string
		note    string
	}{
		{name: "absent says nothing", file: ""},
		{name: "same", file: "domains:\n  - {host: example.com}\n"},
		{name: "new", file: "domains:\n  - {host: example.com}\n  - {host: apps.example.net}\n", changes: []string{"domain"}},
		{
			name: "update", file: "domains:\n  - {host: example.com, include_env_on_default: true, acme_email: a@example.com}\n",
			changes: []string{"domain-update:include_env_on_default", "domain-update:acme_email"},
		},
		{name: "bad email", file: "domains:\n  - {host: example.com, acme_email: nope}\n", blocker: "not an email address"},
		{name: "squats an org slug", file: "domains:\n  - {host: example.com}\n  - {host: globex.example.net}\n", blocker: "another organization's slug"},
		{
			name: "an org level resource is not the file's", file: "domains:\n  - {host: example.com}\n  - {host: tools.net}\n", blocker: "already a domain resource",
			mut: func(l *serverconfig.Live) {
				l.Domains = append(l.Domains, store.DomainResource{ID: "dr4", Level: "org", OrgID: ptr("o1"), Host: "tools.net"})
			},
		},
		{
			name: "rename", file: "domains:\n  - {host: new.com, from: example.com}\n",
			changes: []string{"domain-rename:new.com"}, impact: "moves 3 hosts, issues 3 certificates; point DNS first",
		},
		{
			name: "rename with a field change", file: "domains:\n  - {host: new.com, from: example.com, acme_email: a@example.com}\n",
			changes: []string{"domain-rename:new.com", "domain-update:acme_email"}, impact: "moves 3 hosts",
		},
		{name: "already moved, from may stay", file: "domains:\n  - {host: new.com, from: example.com}\n", mut: func(l *serverconfig.Live) {
			l.Domains[0].Host = "new.com"
		}},
		{
			name: "rename onto an existing host", file: "domains:\n  - {host: apps.example.net, from: example.com}\n", blocker: "both exist",
			mut: func(l *serverconfig.Live) {
				l.Domains = append(l.Domains, store.DomainResource{ID: "dr3", Level: "instance", Host: "apps.example.net"})
			},
		},
		{name: "rename of nothing", file: "domains:\n  - {host: a.com, from: b.com}\n", blocker: "neither b.com nor a.com exists", note: "domains.example.com stays"},
		{
			name: "past fifty hosts without a wildcard", file: "domains:\n  - {host: new.com, from: example.com}\n",
			mut:     func(l *serverconfig.Live) { l.DomainHosts["dr1"] = 80 },
			changes: []string{"domain-rename:new.com"}, impact: "Let's Encrypt allows about 50 certificates a week",
		},
		{
			name: "past fifty hosts with a wildcard provider", file: "settings:\n  dns_provider: cloudflare\ndomains:\n  - {host: new.com, from: example.com}\n",
			mut:     func(l *serverconfig.Live) { l.DomainHosts["dr1"] = 80 },
			changes: []string{"settings:dns_provider", "domain-rename:new.com"}, impact: "moves 80 hosts",
		},
		{name: "dropped, with no tiles under it: a row", file: "domains: []\n", changes: []string{"domain-delete:example.com"}, mut: func(l *serverconfig.Live) {
			l.DomainHosts = nil
		}},
		{name: "dropped, tiles under it: a note, no row", file: "domains: []\n", note: "3 tile domains sit under it"},
		{name: "external route holds the host", file: "domains:\n  - {host: example.com}\n  - {host: old.io}\n", blocker: "external route"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mut []func(*serverconfig.Live)
			if c.mut != nil {
				mut = append(mut, c.mut)
			}
			p := diff(t, c.file, mut...)
			wantKinds(t, p, c.changes...)
			has(t, "impacts", p.Impacts(), c.impact)
			has(t, "blockers", p.Blockers, c.blocker)
			has(t, "notes", p.Notes, c.note)
		})
	}
}

// root_domain moves the instance resource by itself; a domains: entry for
// the new root is that resource, so the plan holds one rename line, one
// impact line, no create for the new host and no removal for the old.
func TestDiffRootDomainIsOneRename(t *testing.T) {
	for name, file := range map[string]string{
		"no domains block":      "settings:\n  root_domain: new.com\n",
		"entry for the new one": "settings:\n  root_domain: new.com\ndomains:\n  - {host: new.com}\n",
		"entry with from":       "settings:\n  root_domain: new.com\ndomains:\n  - {host: new.com, from: example.com}\n",
		"block without it":      "settings:\n  root_domain: new.com\ndomains:\n  - {host: other.net}\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := diff(t, file)
			if len(p.Impacts()) != 1 || !strings.Contains(p.Impacts()[0], "moves 3 hosts") {
				t.Errorf("impacts = %q, want the one rename line", p.Impacts())
			}
			for _, c := range p.Changes {
				if c.Kind == "domain-rename" || c.Kind == "domain-delete" || (c.Kind == "domain" && c.New == "new.com") {
					t.Errorf("%s is the rename root_domain already does", c.Line())
				}
			}
			if p.Blocked() {
				t.Errorf("blockers = %q", p.Blockers)
			}
		})
	}
	p := diff(t, "settings:\n  root_domain: new.com\ndomains:\n  - {host: new.com, acme_email: a@example.com}\n")
	wantKinds(t, p, "settings:root_domain", "domain-update:acme_email")
}

// ---- routes ----

func TestDiffRoutes(t *testing.T) {
	const old = "  - {host: old.io, mode: https, target: '10.0.0.5'}\n"
	const raw = "  - {host: raw.example.net, mode: passthrough, target: '10.0.0.6'}\n"
	for _, c := range []struct {
		name    string
		file    string
		mut     func(*serverconfig.Live)
		changes []string
		blocker string
	}{
		{name: "absent says nothing", file: ""},
		{name: "same, port filled in", file: "routes:\n" + old + raw},
		{name: "new", file: "routes:\n" + old + raw + "  - {host: new.io, mode: http, target: 'h'}\n", changes: []string{"route:new.io"}},
		{
			name: "changed target and mode", file: "routes:\n  - {host: old.io, mode: http, target: 'h2'}\n" + raw,
			changes: []string{"route-update:mode", "route-update:target"},
		},
		{name: "dropped is a removal row", file: "routes:\n" + old, changes: []string{"route-delete:raw.example.net"}},
		{name: "empty block drops all", file: "routes: []\n", changes: []string{"route-delete:old.io", "route-delete:raw.example.net"}},
		{
			name: "wildcard needs a provider", file: "routes:\n" + old + raw + "  - {host: '*.apps.io', mode: http, target: h}\n",
			changes: []string{"route:*.apps.io"}, blocker: "needs a DNS provider",
		},
		{
			name: "wildcard with the provider the file sets", file: "settings:\n  dns_provider: cloudflare\nroutes:\n" + old + raw + "  - {host: '*.apps.io', mode: http, target: h}\n",
			changes: []string{"settings:dns_provider", "route:*.apps.io"},
		},
		{
			name: "wildcard pass-through needs none", file: "routes:\n" + old + raw + "  - {host: '*.apps.io', mode: passthrough, target: h}\n",
			changes: []string{"route:*.apps.io"},
		},
		{
			name: "pass-through needs tls", file: "routes:\n" + old + raw + "  - {host: p.io, mode: passthrough, target: h}\n",
			mut:     func(l *serverconfig.Live) { l.TLSOff = true },
			changes: []string{"route:p.io"}, blocker: "needs HTTPS on",
		},
		{
			name: "overlaps a tile domain", file: "routes:\n" + old + raw + "  - {host: web.acme.example.com, mode: http, target: h}\n",
			changes: []string{"route:web.acme.example.com"}, blocker: "already a tile domain or domain resource",
		},
		{
			name: "wildcard over a domain resource apex", file: "settings:\n  dns_provider: cloudflare\nroutes:\n" + old + raw + "  - {host: '*.example.com', mode: http, target: h}\n",
			changes: []string{"settings:dns_provider", "route:*.example.com"}, blocker: "already a tile domain or domain resource",
		},
		{
			name: "overlaps another route of the file", file: "routes:\n" + old + raw + "  - {host: 'a.b.io', mode: http, target: h}\n  - {host: '*.b.io', mode: passthrough, target: h}\n",
			changes: []string{"route:a.b.io", "route:*.b.io"}, blocker: "overlaps",
		},
		{
			name: "overlaps a live route the file keeps", file: "routes:\n" + old + raw + "  - {host: '*.example.net', mode: passthrough, target: h}\n",
			changes: []string{"route:*.example.net"}, blocker: "overlaps the route for raw.example.net",
		},
		{
			name: "pass-through over the panel domain", file: "routes:\n" + old + raw + "  - {host: panel.example.com, mode: passthrough, target: h}\n",
			changes: []string{"route:panel.example.com"}, blocker: "covers the panel domain",
		},
		{
			name: "the panel moves under a pass-through route", file: "settings:\n  panel_domain: raw.example.net\nroutes:\n" + old + raw,
			changes: []string{"settings:panel_domain"}, blocker: "covers the panel domain",
		},
		{
			name: "an http route on the panel host is shadowed by it", file: "routes:\n" + old + raw + "  - {host: panel.example.com, mode: http, target: h}\n",
			changes: []string{"route:panel.example.com"}, blocker: "shadowed by the panel domain",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mut []func(*serverconfig.Live)
			if c.mut != nil {
				mut = append(mut, c.mut)
			}
			p := diff(t, c.file, mut...)
			wantKinds(t, p, c.changes...)
			has(t, "blockers", p.Blockers, c.blocker)
		})
	}
}

// ---- connectors ----

func TestDiffConnectors(t *testing.T) {
	for _, c := range []struct {
		name    string
		file    string
		mut     func(*serverconfig.Live)
		changes []string
		impact  string
		blocker string
		note    string
	}{
		{name: "named without share says nothing", file: "connectors:\n  - {name: gh-main}\n"},
		{name: "same list", file: "connectors:\n  - {name: gh-main, share: [acme]}\n"},
		{name: "never created", file: "connectors:\n  - {name: nope, share: all}\n", blocker: "make it in the panel first"},
		{
			name: "all orgs carries the warning", file: "connectors:\n  - {name: gh-main, share: all}\n",
			changes: []string{"connector-share:share"}, impact: "every org owner can clone every repo",
		},
		{name: "all, already", file: "connectors:\n  - {name: gh-all, share: all}\n"},
		{name: "add an org", file: "connectors:\n  - {name: gh-main, share: [acme, globex]}\n", changes: []string{"connector-share:org"}},
		{
			name: "an org the file creates", file: "connectors:\n  - {name: gh-main, share: [acme, newco]}\norgs:\n  - {slug: newco}\n",
			changes: []string{"connector-share:org", "org-create:newco", "org-unbind:acme"}, impact: "creates org newco",
		},
		{name: "unknown org", file: "connectors:\n  - {name: gh-main, share: [acme, ghost]}\n", blocker: "no organization \"ghost\""},
		{
			name: "revoke is a removal row", file: "connectors:\n  - {name: gh-main, share: [globex]}\norgs:\n  - {slug: acme}\n  - {slug: globex}\n",
			changes: []string{"connector-share:org", "connector-share-delete:org"},
			mut:     func(l *serverconfig.Live) { l.Orgs[0].ConfigRepo = "" },
		},
		{name: "none is a removal row", file: "connectors:\n  - {name: gh-main, share: []}\n", changes: []string{"connector-share-delete:org"}, mut: func(l *serverconfig.Live) {
			l.Orgs[0].ConfigRepo = ""
			l.Orgs[0].ConfigConnectorID = ""
		}},
		{
			name: "revoke while a binding names it stays", file: "connectors:\n  - {name: gh-main, share: []}\n",
			mut:  func(l *serverconfig.Live) { l.Connectors[0].Bound = []string{"o1"} },
			note: "stays shared with acme",
		},
		{
			name: "revoke while the file's own org entry names it stays", file: "connectors:\n  - {name: gh-main, share: []}\norgs:\n  - {slug: acme, repo: https://github.com/acme/infra, branch: main, connector: gh-main, auto: true}\n",
			note: "stays shared with acme",
		},
		{
			name: "all to a list: the all share is a removal row", file: "connectors:\n  - {name: gh-all, share: [acme]}\n",
			changes: []string{"connector-share:org", "connector-share-delete:share"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mut []func(*serverconfig.Live)
			if c.mut != nil {
				mut = append(mut, c.mut)
			}
			p := diff(t, c.file, mut...)
			wantKinds(t, p, c.changes...)
			has(t, "impacts", p.Impacts(), c.impact)
			has(t, "blockers", p.Blockers, c.blocker)
			has(t, "notes", p.Notes, c.note)
		})
	}
	// the keys the approve contract ticks
	p := diff(t, "connectors:\n  - {name: gh-all, share: [acme]}\n")
	if r := p.Removals(); len(r) != 1 || r[0].Key != "connector-share:gh-all/all" {
		t.Errorf("removals = %+v", r)
	}
	p = diff(t, "connectors:\n  - {name: gh-main, share: []}\n", func(l *serverconfig.Live) {
		l.Orgs[0].ConfigRepo, l.Orgs[0].ConfigConnectorID = "", ""
	})
	if r := p.Removals(); len(r) != 1 || r[0].Key != "connector-share:gh-main/org:acme" {
		t.Errorf("removals = %+v", r)
	}
}

// ---- orgs ----

func TestDiffOrgs(t *testing.T) {
	const acme = "  - {slug: acme, repo: https://github.com/acme/infra, branch: main, connector: gh-main, auto: true}\n"
	for _, c := range []struct {
		name    string
		file    string
		mut     func(*serverconfig.Live)
		changes []string
		impact  string
		blocker string
		note    string
	}{
		{name: "absent says nothing", file: ""},
		{name: "same", file: "orgs:\n" + acme},
		{name: "same, file path spelled out", file: "orgs:\n  - {slug: acme, repo: acme/infra, branch: main, path: stackr-org.yml, connector: gh-main, auto: true}\n"},
		{
			name: "create", file: "orgs:\n" + acme + "  - {slug: initech2, name: Initech2}\n",
			changes: []string{"org-create:initech2"}, impact: "creates org initech2; you become its owner",
		},
		{
			name: "create bound through a shared connector", file: "connectors:\n  - {name: gh-main, share: [acme, newco]}\norgs:\n" + acme + "  - {slug: newco, name: NewCo, repo: newco/infra, connector: gh-main}\n",
			changes: []string{"connector-share:org", "org-create:newco"}, impact: "creates org newco",
		},
		{name: "slug and name disagree", file: "orgs:\n" + acme + "  - {slug: newco, name: Other Co}\n", blocker: "makes the slug \"other-co\""},
		{name: "a foreign domain leads with the slug", file: "orgs:\n" + acme + "  - {slug: initech}\n", changes: []string{"org-create:initech"}, impact: "creates org initech", blocker: "another organization's domain initech.example.com"},
		{name: "unknown connector", file: "orgs:\n  - {slug: acme, repo: https://github.com/acme/infra, connector: nope}\n", changes: []string{"org-bind:branch", "org-bind:connector", "org-bind:auto"}, blocker: "no server connector named nope"},
		{
			name: "connector not shared with the org", file: "orgs:\n  - {slug: globex, repo: g/infra, connector: gh-main}\n",
			changes: []string{"org-bind:repo", "org-bind:connector", "org-unbind:acme"}, blocker: "not shared with it",
		},
		{
			name: "connector shared with all", file: "orgs:\n  - {slug: globex, repo: g/infra, connector: gh-all}\n",
			changes: []string{"org-bind:repo", "org-bind:connector", "org-unbind:acme"},
		},
		{
			name: "rebind fields", file: "orgs:\n  - {slug: acme, repo: https://github.com/acme/other, branch: dev, path: x.yml, connector: gh-all}\n",
			changes: []string{"org-bind:repo", "org-bind:branch", "org-bind:path", "org-bind:connector", "org-bind:auto"},
			note:    "",
		},
		{name: "listed without a repo: the binding is a removal row", file: "orgs:\n  - {slug: acme}\n", changes: []string{"org-unbind:acme"}},
		{name: "not listed while the block is: a removal row", file: "orgs:\n  - {slug: globex}\n", changes: []string{"org-unbind:acme"}},
		{name: "empty block unbinds every bound org", file: "orgs: []\n", changes: []string{"org-unbind:acme"}},
		{name: "name only applies at creation", file: "orgs:\n" + strings.Replace(acme, "{slug: acme", "{slug: acme, name: ACME", 1), note: "only applies at creation"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mut []func(*serverconfig.Live)
			if c.mut != nil {
				mut = append(mut, c.mut)
			}
			p := diff(t, c.file, mut...)
			wantKinds(t, p, c.changes...)
			has(t, "impacts", p.Impacts(), c.impact)
			has(t, "blockers", p.Blockers, c.blocker)
			has(t, "notes", p.Notes, c.note)
		})
	}
	p := diff(t, "orgs: []\n")
	if r := p.Removals(); len(r) != 1 || r[0].Key != "org-binding:acme" {
		t.Errorf("removals = %+v", r)
	}
}

// ---- the plan as a whole ----

// The file never deletes: every removal row is Optional with a Key, none is
// a blocker, removals come last, and a removal-only plan needs an approver.
func TestPlanOrderAndApproval(t *testing.T) {
	p := diff(t, ""+
		"settings:\n  workers: 3\n  panel_backup_dest: local\n"+
		"params:\n  s3:\n    bucket: {type: param, value: b}\n"+
		"routes:\n  - {host: new.io, mode: http, target: h}\n"+
		"backup_dests: []\n"+
		"connectors:\n  - {name: gh-main, share: [acme, globex]}\n"+
		"orgs:\n  - {slug: acme, repo: https://github.com/acme/infra, branch: main, connector: gh-main, auto: true}\n  - {slug: newco}\n"+
		"domains:\n  - {host: example.com, acme_email: a@example.com}\n")
	got := kinds(p)
	var order []string
	for _, k := range got {
		order = append(order, strings.SplitN(k, ":", 2)[0])
	}
	rank := map[string]int{
		"param": 0, "connector-share": 1, "settings": 2, "defaults": 3, "dest": 4, "domain-update": 5,
		"route": 6, "org-create": 7, "route-delete": 8, "dest-delete": 8,
	}
	last := -1
	for _, k := range order {
		r, ok := rank[k]
		if !ok {
			t.Fatalf("unranked kind %q in %q", k, got)
		}
		if r < last {
			t.Errorf("walk order broken at %q: %q", k, got)
		}
		last = r
	}
	rem := p.Removals()
	if len(rem) == 0 || len(p.Changes)-len(rem) < 1 {
		t.Fatalf("changes = %q", got)
	}
	for i, c := range p.Changes[len(p.Changes)-len(rem):] {
		if !c.Optional || c.Key == "" {
			t.Errorf("tail change %d is not a removal row: %+v", i, c)
		}
	}
	for _, c := range rem {
		if c.Key == "" || !strings.Contains(c.Key, ":") {
			t.Errorf("removal without a <kind>:<key> key: %+v", c)
		}
	}
	if p.Blocked() {
		t.Errorf("a removal is never a blocker: %q", p.Blockers)
	}
	if got := p.OnlyTicked(nil).Removals(); len(got) != 0 {
		t.Errorf("an unticked apply walks removals: %+v", got)
	}
}

func TestAutoOK(t *testing.T) {
	if p := diff(t, ""); !p.AutoOK() {
		t.Error("an empty plan applies by itself")
	}
	if p := diff(t, "settings:\n  workers: 3\n"); !p.AutoOK() || p.Blocked() {
		t.Errorf("a plain change applies by itself: %+v", p)
	}
	if p := diff(t, "routes: []\n"); p.AutoOK() || len(p.Removals()) != 2 {
		t.Errorf("a removal-only plan needs an approver: %+v", p)
	}
	if p := diff(t, "settings:\n  panel_domain: x.example.com\n"); p.AutoOK() {
		t.Error("a risky plan needs an approver")
	}
	if p := diff(t, "orgs:\n  - {slug: newco}\n"); p.AutoOK() {
		t.Error("creating an org needs an approver")
	}
	if p := diff(t, "connectors:\n  - {name: nope, share: all}\n"); p.AutoOK() || !p.Blocked() {
		t.Error("a blocked plan needs an approver")
	}
}

func TestSummary(t *testing.T) {
	p := diff(t, "settings:\n  workers: 3\nroutes: []\nbackup_dests:\n"+offsite+"  - {name: cold, bucket: b, access_key: '${{ server.params.nope.a }}', secret_key: '${{ server.params.nope.b }}'}\nconnectors:\n  - {name: nope1}\n  - {name: nope2}\norgs:\n  - {slug: newco}\n  - {slug: acme, repo: https://github.com/acme/infra, branch: main, connector: gh-main, auto: true}\n")
	want := "1 to add, 1 to change, 2 removals to review, needs confirm, 2 blockers"
	if s := p.Summary(); s != want {
		t.Errorf("summary = %q, want %q (changes %q)", s, want, kinds(p))
	}
}

// ---- export ----

// full is a live with every block populated: it must export, parse and
// diff clean.
func full() serverconfig.Live {
	l := live()
	l.Settings["trusted_proxies"] = "10.0.0.0/8,127.0.0.1"
	l.Settings["dns_provider"] = "cloudflare"
	l.Settings["proxy_custom"] = `[{"handle":[]}]`
	l.Settings["panel_backup_keep"] = "7"
	l.Routes = append(l.Routes, store.Route{ID: "r-wild", Host: "*.ops.net", Mode: "https", Target: "10.0.0.7:443"})
	l.Settings["panel_domain"] = "panel.ops.net"
	l.Defaults = settings.Settings{CPULimit: ptr(1.5), MemLimitMB: ptr(512), Protect: ptr(true), ProtectUser: ptr("bob"), ProtectPassword: ptr("${{ server.params.auth.pw }}")}
	l.Params["auth.pw"] = params.Value{V: "x", Secret: true}
	l.Domains = append(l.Domains, store.DomainResource{ID: "dr3", Level: "instance", Host: "apps.example.net", IncludeEnvOnDefault: true, ACMEEmail: "a@example.net"})
	l.Connectors[0].OrgIDs = []string{"o1", "o2"}
	l.Orgs[1].ConfigRepo = "https://github.com/globex/ops"
	l.Orgs[1].ConfigConnectorID = "c2"
	l.Orgs[1].ConfigPath = "ops/org.yml"
	return l
}

func TestExportThenDiffIsClean(t *testing.T) {
	l := full()
	out, err := serverconfig.Export(l)
	if err != nil {
		t.Fatal(err)
	}
	f, err := serverconfig.Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	p := serverconfig.Diff(f, l)
	if len(p.Changes) != 0 || p.Blocked() || len(p.Notes) != 0 {
		t.Errorf("export diffs against itself: %+v\n%s", p, out)
	}
	for _, want := range []string{
		"workers: 4",
		"cleanup_enabled: true",
		"panel_backup_dest: offsite",
		"panel_domain: panel.ops.net",
		"cpu_limit: 1.5",
		"share: all",
		"- globex\n",
		"${{ server.params.backup_offsite.secret_key }}",
		"slug: globex",
		"path: ops/org.yml",
		"host: apps.example.net",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
	}
	for _, never := range []string{"AK", "SK", "ARC", "server_config", "orphan_retention_days", "hunter2", "local"} {
		if strings.Contains(string(out), never) {
			t.Errorf("export holds %q:\n%s", never, out)
		}
	}
	// a secret is by name and type only
	if !strings.Contains(string(out), "    secret_key:\n      type: secret") {
		t.Errorf("destination keys are declared as secrets:\n%s", out)
	}
}

// An export of a box with nothing set is a bare file that still parses.
func TestExportEmpty(t *testing.T) {
	out, err := serverconfig.Export(serverconfig.Live{})
	if err != nil || strings.TrimSpace(string(out)) != "version: 1" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if _, err := serverconfig.Parse(out); err != nil {
		t.Error(err)
	}
}

// Export declares the destination key params even when Live does not hold
// them yet, so the file parses; the plan then blocks on the missing values
// until the orchestrator (or the admin) sets them.
func TestExportDeclaresDestKeysAndTheDiffBlocksUntilSet(t *testing.T) {
	l := live()
	delete(l.Params, "backup_offsite.access_key")
	delete(l.Params, "backup_offsite.secret_key")
	delete(l.Params, "backup_offsite.archive_key")
	out, err := serverconfig.Export(l)
	if err != nil {
		t.Fatal(err)
	}
	f, err := serverconfig.Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	p := serverconfig.Diff(f, l)
	has(t, "notes", p.Notes, "backup_dests.offsite waits for server.params.backup_offsite.access_key")
	if p.Blocked() {
		t.Errorf("a missing value holds back its item, not the plan: %q", p.Blockers)
	}
	for _, c := range p.Changes {
		t.Errorf("only notes expected, got %s", c.Line())
	}
}

func TestExportDestCollision(t *testing.T) {
	l := live()
	l.Dests = append(l.Dests,
		store.BackupDest{ID: "a", Kind: "s3", Name: "Off-Site", Bucket: "b"},
		store.BackupDest{ID: "b", Kind: "s3", Name: "off_site", Bucket: "b"})
	if _, err := serverconfig.Export(l); err == nil || !strings.Contains(err.Error(), "both map to server params") {
		t.Errorf("err = %v", err)
	}
	if got := serverconfig.DestParamCollection("Off-Site.1"); got != "backup_off_site_1" {
		t.Errorf("collection = %q", got)
	}
}

// An org bound through its own (non-server) connector exports with none,
// and reads clean.
func TestExportOrgOwnConnector(t *testing.T) {
	l := live()
	l.Orgs[0].ConfigConnectorID = "org-own-connector"
	out, err := serverconfig.Export(l)
	if err != nil {
		t.Fatal(err)
	}
	f, err := serverconfig.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if p := serverconfig.Diff(f, l); len(p.Changes) != 0 || p.Blocked() {
		t.Errorf("diff = %+v\n%s", p, out)
	}
}

// ---- the catalogue ----

// Every Flat knob the file accepts round-trips through Parse, and none of
// the binding knobs is accepted.
func TestEveryFlatKnobIsASettingExceptTheBinding(t *testing.T) {
	for _, k := range settings.Catalogue {
		if k.Scopes != settings.Flat {
			continue
		}
		_, err := serverconfig.Parse([]byte(v1 + "settings:\n  " + k.Key + ": \"\"\n"))
		switch {
		case k.ConfigOnly && err == nil:
			t.Errorf("%s is the binding, the file must refuse it", k.Key)
		case !k.ConfigOnly && !k.ReadOnly && err != nil:
			t.Errorf("%s: %v", k.Key, err)
		}
	}
}

// The Let's Encrypt line is for a box with no wildcard provider only.
func TestRenameImpactLetsEncryptLine(t *testing.T) {
	big := func(l *serverconfig.Live) { l.DomainHosts["dr1"] = 80 }
	p := diff(t, "domains:\n  - {host: new.com, from: example.com}\n", big)
	has(t, "impacts", p.Impacts(), "Let's Encrypt")
	p = diff(t, "settings:\n  dns_provider: cloudflare\ndomains:\n  - {host: new.com, from: example.com}\n", big)
	for _, i := range p.Impacts() {
		if strings.Contains(i, "Let's Encrypt") {
			t.Errorf("a wildcard provider has no per-host limit: %q", i)
		}
	}
	p = diff(t, "domains:\n  - {host: new.com, from: example.com}\n", func(l *serverconfig.Live) { l.DomainHosts["dr1"] = 50 })
	for _, i := range p.Impacts() {
		if strings.Contains(i, "Let's Encrypt") {
			t.Errorf("50 is within the limit: %q", i)
		}
	}
}

// A live route over the panel host that the file never touches is not this
// plan's to refuse; one it adds, or any route when the panel moves, is. A
// wildcard over the panel loses one name, not the route.
func TestPanelRouteCheckOnlyLooksAtWhatMoves(t *testing.T) {
	onPanel := func(l *serverconfig.Live) {
		l.Settings["panel_domain"] = "panel.ops.net"
		l.Routes = append(l.Routes,
			store.Route{ID: "r1", Host: "*.ops.net", Mode: "https", Target: "10.0.0.7:443"},
			store.Route{ID: "r2", Host: "panel.ops.net", Mode: "https", Target: "10.0.0.8:443"})
	}
	if p := diff(t, "", onPanel); p.Blocked() || len(p.Changes) != 0 {
		t.Errorf("an empty file on a box with routes over the panel host: %+v", p)
	}
	if p := diff(t, "settings:\n  workers: 3\n", onPanel); p.Blocked() {
		t.Errorf("an unrelated change is blocked: %q", p.Blockers)
	}
	// the panel moves: every route is looked at; the wildcard is fine, the exact host is shadowed
	p := diff(t, "settings:\n  panel_domain: panel.old.net\n", onPanel)
	if p.Blocked() {
		t.Errorf("moving off the host unblocks it: %q", p.Blockers)
	}
	p = diff(t, "settings:\n  panel_domain: raw2.ops.net\n", func(l *serverconfig.Live) {
		onPanel(l)
		l.Routes = append(l.Routes, store.Route{ID: "r3", Host: "raw2.ops.net", Mode: "http", Target: "h:80"})
	})
	has(t, "blockers", p.Blockers, "raw2.ops.net is shadowed by the panel domain")
	if len(p.Blockers) != 1 {
		t.Errorf("only the exact host is shadowed, not the wildcard: %q", p.Blockers)
	}
}
