package service

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func svcTile() *repo.Tile {
	return &repo.Tile{ID: "t1", Kind: "service", SourceType: "image", ImageRef: "nginx:1",
		ContainerPort: 8080, CPULimit: 1.5, MemLimitMB: 512}
}

// The headline point-3 row: a limit, port, healthcheck, command or replica
// edit has to redeploy. The panel and the API both wrote the row and stopped,
// so every one of these sat dormant until the next manual deploy.
func TestAChangedSpecIsNotProxyOnly(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*repo.Tile)
		key  string
	}{
		{"port", func(x *repo.Tile) { x.ContainerPort = 9090 }, "port"},
		{"cpu", func(x *repo.Tile) { x.CPULimit = 2 }, "limits"},
		{"memory", func(x *repo.Tile) { x.MemLimitMB = 1024 }, "limits"},
		{"healthcheck", func(x *repo.Tile) { x.HealthcheckCmd = "curl -f /" }, "healthcheck"},
		{"healthcheck interval", func(x *repo.Tile) { x.HealthcheckIntervalS = 10 }, "healthcheck"},
		{"command", func(x *repo.Tile) { x.Command = "-v" }, "command"},
		{"replicas", func(x *repo.Tile) { x.Replicas = 3 }, "replicas"},
		{"node group", func(x *repo.Tile) { x.NodeGroup = "gpu" }, "node_group"},
		{"volumes", func(x *repo.Tile) { x.Volumes = "data:/var" }, "volumes"},
		{"image", func(x *repo.Tile) { x.ImageRef = "nginx:2" }, "image"},
	} {
		t.Run(c.name, func(t *testing.T) {
			old, cur := svcTile(), svcTile()
			c.edit(cur)
			got := DiffTiles(old, cur)
			if !got[c.key] {
				t.Fatalf("%q not reported changed: %v", c.key, got)
			}
			if got.ProxyOnly() {
				t.Errorf("%q must earn a redeploy, not just a route rewrite", c.key)
			}
		})
	}
}

// The other half: the route-only fields must NOT redeploy, or every basic-auth
// toggle bounces the container.
func TestRouteOnlyFieldsStayProxyOnly(t *testing.T) {
	old, cur := svcTile(), svcTile()
	cur.SecHeaders = true
	cur.BasicAuthUser = "ops"
	cur.BasicAuthPassword = "hunter2"
	cur.TraefikOverride = "http: {}"
	got := DiffTiles(old, cur)
	if !got.Any() || !got.ProxyOnly() {
		t.Errorf("want a proxy-only change, got %v", got)
	}
	// A config apply names each domain it touched; those are route-only too.
	got["domain +api.example.com"] = true
	if !got.ProxyOnly() {
		t.Error("a domain edit must not force a redeploy")
	}
}

// A save that moved nothing earns nothing. Without this every panel visit
// that posted the form unchanged would queue a build.
func TestNoChangeEarnsNothing(t *testing.T) {
	if DiffTiles(svcTile(), svcTile()).Any() {
		t.Error("identical rows reported a change")
	}
}

// A cron reads its schedule off the row at each run, so a schedule change
// reloads the table but must not rebuild the image.
func TestCronScheduleReloadsButDoesNotBuild(t *testing.T) {
	old := &repo.Tile{ID: "c1", Kind: "cron", SourceType: "image", ImageRef: "alpine:3", Cron: "0 3 * * *"}
	cur := &repo.Tile{ID: "c1", Kind: "cron", SourceType: "image", ImageRef: "alpine:3", Cron: "*/5 * * * *"}
	got := DiffTiles(old, cur)
	if !got.NeedsCronReload() {
		t.Error("a changed schedule has to re-register the cron table")
	}
	if got.NeedsBuild() {
		t.Error("a changed schedule must not rebuild the image")
	}
	cur.ImageRef = "alpine:4"
	if !DiffTiles(old, cur).NeedsBuild() {
		t.Error("a changed image has to rebuild")
	}
}

// A row written before the service canonicalised its enums holds the raw
// spelling. Comparing that straight against a validated row reported a change
// nobody made — and so redeployed on a save that touched nothing.
func TestCanonicalFormsAreNotChanges(t *testing.T) {
	old, cur := svcTile(), svcTile()
	old.UpdatePolicy, cur.UpdatePolicy = "", "off"
	old.RestartPolicy, cur.RestartPolicy = "always", ""
	if got := DiffTiles(old, cur); got.Any() {
		t.Errorf("two spellings of the same value reported as a change: %v", got)
	}
}

// A cron switching from an image to a git build has to rebuild. The config
// differ names that change "source_type", which the first cut of NeedsBuild
// did not list.
func TestSourceTypeChangeRebuilds(t *testing.T) {
	for _, key := range []string{"source", "source_type"} {
		if !(Changed{key: true}).NeedsBuild() {
			t.Errorf("%q must rebuild", key)
		}
	}
}
