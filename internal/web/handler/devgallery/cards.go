package devgallery

import "github.com/FyrmForge/stackr/internal/ui/graph/cards"

// placed is a sample card and where the fake env canvas puts it; h
// counts 30 px per sub-tile, as the graph service's does.
type placed struct {
	card       cards.CardView
	x, y, w, h string
}

func drawer(id, tab string) string { return "/dev/components/drawer?node=" + id + "&tab=" + tab }

func sample(id, kind, name, detail, tab string, f cards.FooterView) cards.CardView {
	f.Kind = kind
	return cards.CardView{
		ID:     id,
		Kind:   kind,
		Name:   name,
		Detail: detail,
		Drawer: drawer(id, tab),
		Tab:    tab,
		Footer: f,
	}
}

// envCards is one of every env card kind (ui-plan §2), for eyeballing.
func envCards() []placed {
	api := sample("t-api", "service", "api", "ghcr.io/acme/api:1.4", "status", cards.FooterView{
		Status: "degraded",
		Up:     2,
		Want:   3,
		Domain: "api.acme.dev",
		More:   2,
	})
	api.Host = true
	api.NewVersion = true
	api.Subs = []cards.SubView{
		{
			ID:     "v-uploads",
			Kind:   "volume",
			Label:  "uploads",
			Drawer: drawer("v-uploads", "backups"),
			Tab:    "backups",
		},
		{
			ID:     "t-api-2",
			Kind:   "replica",
			Label:  "api-2 · unhealthy",
			Drawer: drawer("t-api", "status"),
			Tab:    "status",
		},
	}
	db := sample("p-shopdb", "slice", "shop", "database on cache-pg", "overview", cards.FooterView{
		Status:    "running",
		Consumers: 2,
	})
	pg := sample("t-pg2", "managed", "cache-pg", "postgres 16", "slices", cards.FooterView{Status: "running"})
	pg.Subs = []cards.SubView{{
		ID:     "v-pgdata",
		Kind:   "volume",
		Label:  "pgdata",
		Drawer: drawer("v-pgdata", "backups"),
		Tab:    "backups",
	}}
	return []placed{
		{
			sample("proxy", "proxy", "proxy", "caddy", "routes", cards.FooterView{Status: "running"}),
			"20",
			"40",
			"180",
			"92",
		},
		{
			sample("internet", "internet", "internet", "outside the map", "", cards.FooterView{}),
			"20",
			"300",
			"180",
			"92",
		},
		{
			api,
			"300",
			"20",
			"220",
			"170",
		},
		{
			sample("t-nightly", "cron", "nightly", "0 3 * * *", "runs", cards.FooterView{
				LastRun: "ok · Jul 24 03:00",
				NextRun: "next Jul 25 03:00",
			}),
			"580",
			"20",
			"220",
			"110",
		},
		{
			sample("t-migrate", "function", "migrate", "on deploy", "runs", cards.FooterView{
				LastRun: "running · since 13:02",
				Trigger: "on deploy",
			}),
			"580",
			"180",
			"220",
			"110",
		},
		{
			sample("t-report", "function", "report", "manual", "runs", cards.FooterView{LastRun: "never run", Trigger: "manual"}),
			"580",
			"340",
			"220",
			"110",
		},
		{
			db,
			"300",
			"220",
			"220",
			"92",
		},
		{
			pg,
			"300",
			"420",
			"220",
			"122",
		},
		{
			sample("t-shared", "ref", "shared-pg", "org · postgres 16", "", cards.FooterView{}),
			"860",
			"220",
			"220",
			"92",
		},
		{
			sample("v-old", "volume", "old-cache", "80 MB · orphaned", "backups", cards.FooterView{}),
			"860",
			"380",
			"220",
			"62",
		},
		{
			sample("vars", "vars", "vars", "env", "params", cards.FooterView{}),
			"860",
			"20",
			"160",
			"72",
		},
		{
			sample("secrets", "secrets", "secrets", "env", "secrets", cards.FooterView{}),
			"860",
			"110",
			"160",
			"72",
		},
	}
}

// envLanes are sample lanes between the cards above.
var envLanes = []cards.Lane{
	{From: "proxy", To: "t-api", BPS: 12_600},
	{From: "t-api", To: "p-shopdb", BPS: 2_300_000},
	{From: "p-shopdb", To: "t-api", BPS: 800},
	{From: "t-nightly", To: "internet", BPS: 40_000},
}
