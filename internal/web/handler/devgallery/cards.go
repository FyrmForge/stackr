package devgallery

import "github.com/FyrmForge/stackr/internal/ui/graph/cards"

// placed is a sample card and where the fake env canvas puts it.
type placed struct {
	card       cards.CardView
	x, y, w, h string
}

func drawer(id, tab string) string { return "/dev/components/drawer?node=" + id + "&tab=" + tab }

func sample(id, kind, name, detail, tab string, f cards.FooterView) cards.CardView {
	return cards.CardView{ID: id, Kind: kind, Name: name, Detail: detail, Drawer: drawer(id, tab), Tab: tab, Footer: f}
}

// envCards is one of every env card kind (ui-plan §2), for eyeballing.
func envCards() []placed {
	api := sample("t-api", "service", "api", "ghcr.io/acme/api:1.4", "status",
		cards.FooterView{Status: "degraded", Up: 2, Want: 3, Domain: "api.acme.dev", More: 2})
	api.Host, api.NewVersion, api.Volumes = true, true, "uploads +1"
	api.Subs = []cards.SubView{
		{ID: "v-uploads", Kind: "volume", Label: "uploads", Drawer: drawer("v-uploads", "backups"), Tab: "backups"},
		{ID: "t-api-2", Kind: "replica", Label: "api-2 · unhealthy", Drawer: drawer("t-api", "status"), Tab: "status"},
	}
	db := sample("p-shopdb", "slice", "shop", "database shop on pg", "bindings", cards.FooterView{Note: "3 bindings"})
	db.Subs = []cards.SubView{{ID: "t-pg", Kind: "instance", Label: "pg · postgres 16", Drawer: drawer("t-pg", "slices"), Tab: "slices"}}
	pg := sample("t-pg2", "managed", "cache-pg", "postgres 16", "slices", cards.FooterView{Status: "running"})
	pg.Subs = []cards.SubView{{ID: "v-pgdata", Kind: "volume", Label: "pgdata", Drawer: drawer("v-pgdata", "backups"), Tab: "backups"}}
	return []placed{
		{sample("proxy", "proxy", "proxy", "caddy", "routes", cards.FooterView{Note: "4 routes"}), "20", "40", "180", "92"},
		{sample("internet", "internet", "internet", "outside the map", "", cards.FooterView{}), "20", "300", "180", "92"},
		{api, "300", "20", "220", "110"},
		{sample("t-nightly", "cron", "nightly", "0 3 * * *", "runs",
			cards.FooterView{LastRun: "ok · Jul 24 03:00", NextRun: "Jul 25 03:00"}), "580", "20", "220", "110"},
		{sample("t-migrate", "function", "migrate", "on deploy", "runs",
			cards.FooterView{LastRun: "running · since 13:02"}), "580", "180", "220", "110"},
		{sample("t-report", "function", "report", "manual", "runs", cards.FooterView{LastRun: "never run"}), "580", "340", "220", "110"},
		{db, "300", "220", "220", "92"},
		{pg, "300", "420", "220", "92"},
		{sample("t-shared", "ref", "shared-pg", "org · postgres 16", "", cards.FooterView{Note: "managed tile, open home"}), "860", "220", "220", "92"},
		{sample("v-old", "volume", "old-cache", "80 MB · orphaned", "backups", cards.FooterView{}), "860", "380", "220", "62"},
		{sample("vars", "vars", "vars", "env", "params", cards.FooterView{Count: 6}), "860", "20", "160", "72"},
		{sample("secrets", "secrets", "secrets", "env", "secrets", cards.FooterView{Count: 2, Waiting: "db.password"}), "860", "110", "160", "72"},
	}
}

// envLanes are sample lanes between the cards above.
var envLanes = []cards.Lane{
	{From: "proxy", To: "t-api", BPS: 12_600},
	{From: "t-api", To: "p-shopdb", BPS: 2_300_000},
	{From: "p-shopdb", To: "t-api", BPS: 800},
	{From: "t-nightly", To: "internet", BPS: 40_000},
}
