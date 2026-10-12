package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// Tagging a worker ingress starts the controller's proxy image on it, host
// network, own cert volume; a second ensure changes nothing; untagging
// removes it. The controller's own proxy is never touched.
func TestNodeProxyFollowsIngressTag(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	n.ctlFake.Details = map[string]docker.Detail{installspec.ProxyName: {Image: "ghcr.io/fyrmforge/stackr:9.9.9", Env: []string{"DNS_API_TOKEN=tok"}}}
	setTags := func(tags string) {
		w, err := n.st.Nodes.Get(ctx, "w1")
		must(t, err)
		w.Tags = tags
		must(t, n.st.Nodes.Update(ctx, w))
	}

	setTags(`["ingress"]`)
	must(t, n.orch.ensureNodeProxy(ctx, "w1"))
	if len(n.wFake.Specs) != 1 {
		t.Fatalf("want one proxy run on w1, got %d", len(n.wFake.Specs))
	}
	s := n.wFake.Specs[0]
	want := installspec.NodeProxy("ghcr.io/fyrmforge/stackr:9.9.9", "tok")
	if s.Name != installspec.ProxyName || s.Image != want.Image || !s.HostNetwork || s.Labels[installspec.LabelSpec] != want.Labels[installspec.LabelSpec] {
		t.Fatalf("proxy spec: %+v", s)
	}
	n.wFake.Containers = []docker.Container{{ID: "p1", Name: s.Name, Labels: s.Labels}}
	must(t, n.orch.ensureNodeProxy(ctx, "w1"))
	if len(n.wFake.Specs) != 1 || sawCall(n.wFake, "StopRemove") {
		t.Fatal("an up-to-date proxy was recreated")
	}

	setTags(`[]`)
	must(t, n.orch.ensureNodeProxy(ctx, "w1"))
	if !sawCall(n.wFake, "StopRemove(p1") {
		t.Fatalf("untagged node kept its proxy: %v", callNames(n.wFake))
	}
	must(t, n.orch.ensureNodeProxy(ctx, n.ctl.ID))
	if sawCall(n.ctlFake, "Run(") || sawCall(n.ctlFake, "StopRemove") {
		t.Fatal("the controller's proxy was touched")
	}
}
