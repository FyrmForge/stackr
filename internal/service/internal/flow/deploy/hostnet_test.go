package deploy_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A granted host-network tile starts with HostNetwork and nothing else on the
// network side, stops its old replica first, makes no pause container, and the
// proxy dials the host.
func TestHostNetworkDeploy(t *testing.T) {
	w := setup(t)
	w.f.HostGrants = hostgrant.New(w.st.HostGrants)
	tl := w.tile
	tl.HostNetwork = true
	tl.PublishedPorts = "8080:80"
	must(t, w.st.Tiles.Update(ctx, tl))
	w.fake.Containers[0].State = "running"

	_, err := w.f.Run(ctx, tl, "nginx:1", io.Discard, nil)
	n, ok := errs.IsNeedsApproval(err)
	if !ok {
		t.Fatalf("run without a grant = %v", err)
	}
	now := time.Now()
	must(t, w.st.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	ask, _ := hostgrant.Parse(n.What)
	if !ask.Has(hostgrant.Line(tl.Slug, hostgrant.NetworkHost)) {
		t.Fatalf("asked for %q", n.What)
	}
	_, err = w.f.HostGrants.Approve(ctx, tl.StackID, "adm", ask)
	must(t, err)
	if _, err = w.f.Run(ctx, tl, "nginx:1", io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	s := w.fake.Specs[len(w.fake.Specs)-1]
	if !s.HostNetwork || len(s.Networks) != 0 || len(s.Ports) != 0 {
		t.Errorf("spec = host %v networks %v ports %v", s.HostNetwork, s.Networks, s.Ports)
	}
	c := w.fake.Calls()
	if stop, start := at(c, "Stop", "old"), at(c, "Start", "new"); stop < 0 || start < stop {
		t.Errorf("want the old replica stopped before the new one starts: %v", c)
	}
	if at(c, "StopRemove", "p") < 0 {
		t.Errorf("the leftover pause container was not removed: %v", c)
	}
	if len(w.fake.Specs) != 1 {
		t.Errorf("%d containers created, want only the replica (no pause): %+v", len(w.fake.Specs), w.fake.Specs)
	}

	w.fake.Containers[0].State = "running"
	must(t, w.st.Domains.Create(ctx, store.Domain{ID: uuid.NewString(), TileID: tl.ID, Host: "a.example.com", ContainerPort: 80, CreatedAt: now}))
	cfg, err := w.f.ProxyConfig(ctx, domain.Install{AdminListen: "127.0.0.1:2019", TLSOff: true}, nil)
	must(t, err)
	// the row says host, the replica is still bridged: the proxy keeps its name
	if strings.Contains(string(cfg), domain.HostUpstream+`:80`) {
		t.Errorf("a bridged replica is dialled as the host: %s", cfg)
	}
	w.fake.Containers[0].HostNetwork = true
	cfg, err = w.f.ProxyConfig(ctx, domain.Install{AdminListen: "127.0.0.1:2019", TLSOff: true}, nil)
	must(t, err)
	if !strings.Contains(string(cfg), `"dial":"`+domain.HostUpstream+`:80"`) {
		t.Errorf("proxy config does not dial the host: %s", cfg)
	}
}
