package volume

import "testing"

// DL: every row of the Seam 3 table, one case each; a not-ready disk is never
// Legacy for a name with a row or no object.
func TestRouteTable(t *testing.T) {
	plain, absent := Object{Found: true}, Object{}
	here, there := Object{Found: true, Device: "/d/x"}, Object{Found: true, Device: "/o/x"}
	for _, c := range []struct {
		name   string
		st     DiskState
		hasRow bool
		obj    Object
		want   Route
	}{
		{"none absent", DiskNone, false, absent, RouteLegacy},
		{"none plain", DiskNone, false, plain, RouteLegacy},
		{"none bound", DiskNone, false, there, RouteLegacy},
		{"ready no-row absent", DiskReady, false, absent, RouteDisk},
		{"ready no-row plain", DiskReady, false, plain, RouteLegacy},
		{"ready no-row bound here", DiskReady, false, here, RouteDisk},
		{"ready no-row bound elsewhere", DiskReady, false, there, RouteGhost},
		{"ready row absent", DiskReady, true, absent, RouteDisk},
		{"ready row plain", DiskReady, true, plain, RouteGhost},
		{"ready row bound here", DiskReady, true, here, RouteDisk},
		{"ready row bound elsewhere", DiskReady, true, there, RouteGhost},
		{"not-ready no-row absent", DiskNotReady, false, absent, RouteMissing},
		{"not-ready no-row plain", DiskNotReady, false, plain, RouteLegacy},
		{"not-ready no-row bound", DiskNotReady, false, here, RouteMissing},
		{"not-ready row absent", DiskNotReady, true, absent, RouteMissing},
		{"not-ready row plain", DiskNotReady, true, plain, RouteMissing},
		{"not-ready row bound", DiskNotReady, true, here, RouteMissing},
	} {
		if got := route(c.st, c.hasRow, c.obj, "/d/x"); got != c.want {
			t.Errorf("%s: route = %d, want %d", c.name, got, c.want)
		}
	}
}
