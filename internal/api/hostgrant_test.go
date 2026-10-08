package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
)

// Host access: any reader sees a stack's grant, only a server admin
// approves, approving with nothing parked is a conflict, and an approval
// records what the parked deploy asked for.
func TestHostGrant(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	const path = "/orgs/acme/stacks/shop/host-grant"
	read := func(key string) service.HostGrant {
		t.Helper()
		code, out := w.do(t, key, "GET", path, "")
		var g service.HostGrant
		if err := json.Unmarshal([]byte(out), &g); code != 200 || err != nil {
			t.Fatalf("GET = %d %s", code, out)
		}
		return g
	}
	if g := read(w.owner); g.Granted || len(g.Lines) != 0 {
		t.Errorf("fresh stack = %+v", g)
	}
	if code, _ := w.do(t, w.stranger, "GET", path, ""); code != 404 {
		t.Errorf("stranger read = %d, want 404", code)
	}
	if code, _ := w.do(t, w.owner, "POST", path+"/approve", ""); code != 403 {
		t.Errorf("owner approve = %d, want 403", code)
	}
	if code, _ := w.do(t, w.admin, "POST", path+"/approve", `{"pending":[]}`); code != 409 {
		t.Errorf("approve with nothing waiting = %d, want 409", code)
	}

	_, _, err := w.env.Orch.UpdateTile(ctx, w.tile.ID, func(t *service.Tile) error {
		t.Volumes = "host:/var/run/docker.sock:/s"
		t.Devices = "/dev/dri"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.env.Orch.Deploy(ctx, w.tile.ID); err != nil {
		t.Fatal(err)
	}
	for range 500 {
		if len(read(w.owner).Pending) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sock, dev := "api host:/var/run/docker.sock:/s", "api device:/dev/dri"
	if g := read(w.owner); len(g.Pending) != 2 || g.Pending[0] != dev || g.Pending[1] != sock {
		t.Fatalf("pending = %+v", g)
	}
	shown := `"pending":["` + dev + `","` + sock + `"]`
	if code, out := w.do(t, w.admin, "POST", path+"/approve", `{"pending":["x host:/:/h"]}`); code != 409 || !strings.Contains(out, "the ask changed; review it again") {
		t.Fatalf("approve of a set that is not the ask = %d %s", code, out)
	}
	if g := read(w.owner); g.Granted || len(g.Pending) != 2 {
		t.Fatalf("a refused approval granted: %+v", g)
	}
	if code, _ := w.do(t, w.admin, "POST", path+"/approve", `{`+shown+`,"grant":["z privileged"]}`); code != 400 {
		t.Fatalf("approve of a line that was not asked = %d, want 400", code)
	}
	// Narrowing: the socket is granted, the device keeps the job waiting.
	code, out := w.do(t, w.admin, "POST", path+"/approve", `{`+shown+`,"grant":["`+sock+`"]}`)
	var g service.HostGrant
	if err := json.Unmarshal([]byte(out), &g); code != 200 || err != nil || !g.Granted || len(g.Lines) != 1 {
		t.Fatalf("partial approve = %d %s", code, out)
	}
	for range 500 {
		if g = read(w.owner); len(g.Pending) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(g.Pending) != 1 || g.Pending[0] != dev {
		t.Fatalf("after a partial approve pending = %+v", g)
	}
	if code, out := w.do(t, w.admin, "GET", "/admin/host-grants", ""); code != 200 || !strings.Contains(out, sock) {
		t.Fatalf("admin host-grants = %d %s", code, out)
	}
	if code, _ := w.do(t, w.owner, "GET", "/admin/host-grants", ""); code != 403 {
		t.Errorf("owner admin host-grants = %d, want 403", code)
	}
	code, out = w.do(t, w.admin, "POST", path+"/approve", `{"pending":["`+dev+`"]}`)
	if err := json.Unmarshal([]byte(out), &g); code != 200 || err != nil || len(g.Lines) != 2 || len(g.Pending) != 0 {
		t.Fatalf("approve = %d %s", code, out)
	}
	if code, _ := w.do(t, w.owner, "DELETE", path, ""); code != 403 {
		t.Errorf("owner revoke = %d, want 403", code)
	}
	if code, _ := w.do(t, w.admin, "DELETE", path+"?tile="+"api", ""); code != 204 && code != 200 {
		t.Fatalf("admin revoke = %d", code)
	}
	if g := read(w.owner); g.Granted {
		t.Errorf("after revoke = %+v", g)
	}
}
