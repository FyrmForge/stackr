package web_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// An env whose node is offline: its owner is told the placement, never the
// node (D3); a server admin is told the node.
func TestNodeDownBanner(t *testing.T) {
	ctx := context.Background()
	s := webtest.New(t)
	st := s.Store
	w := s.Worker(t, "hidden-h4", "iss")
	adm := &service.Principal{Access: authz.User{ID: "root", Active: true, Admin: true}}
	p, err := s.Orch.CreatePlacement(ctx, adm, service.PlacementSpec{Name: "eu-prod", Node: "hidden-h4", OpenToAll: true})
	must(t, err)
	must(t, st.Environments.SetNode(ctx, s.Tile.Env, w))
	must(t, st.Environments.SetPlacement(ctx, s.Tile.Env, p.ID))
	n, err := st.Nodes.Get(ctx, w)
	must(t, err)
	n.Status = "offline"
	must(t, st.Nodes.Update(ctx, n))

	const page = "/acme/shop/dev"
	body := s.Do(t, "GET", page, nil).Body.String()
	if !strings.Contains(body, "Placement eu-prod is offline") || strings.Contains(body, "hidden-h4") {
		t.Errorf("owner page: want the placement and no node\n%s", body)
	}
	admin := s.Session(t, s.User(t, "root@x.test", true))
	if body := s.As(t, admin, "GET", page, nil).Body.String(); !strings.Contains(body, "hidden-h4 is offline") {
		t.Errorf("admin page: want the node\n%s", body)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
