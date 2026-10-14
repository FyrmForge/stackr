package service

import (
	"context"
	"errors"
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
	stackrDigests.Store("9.9.9", "sha256:pub")
	t.Cleanup(stackrDigests.Clear)
	n.orch.markStackrPublished()
	must(t, n.orch.ensureNodeProxy(ctx, "w1"))
	if len(n.wFake.Specs) != 1 {
		t.Fatalf("want one proxy run on w1, got %d", len(n.wFake.Specs))
	}
	s := n.wFake.Specs[0]
	want := installspec.NodeProxy("localhost:5000/stackr.system/stackr@sha256:pub") // the registry copy by digest, not ghcr
	if installspec.TokenFrom(s.Env) != "" {
		t.Fatal("the controller's DNS-01 token reached a worker's proxy")
	}
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

// A proxy image that cannot be pulled leaves the running proxy alone, so
// ingress stays up while the registry copy is missing.
func TestNodeProxyPullFailureKeepsOldProxy(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	n.ctlFake.Details = map[string]docker.Detail{installspec.ProxyName: {Image: "ghcr.io/fyrmforge/stackr:9.9.9"}}
	w, err := n.st.Nodes.Get(ctx, "w1")
	must(t, err)
	w.Tags = `["ingress"]`
	must(t, n.st.Nodes.Update(ctx, w))
	img := "localhost:5000/stackr.system/stackr:9.9.9"
	n.wFake.Gone = []string{img}
	n.wFake.Err = map[string]error{"Pull": errors.New("not found")}
	n.wFake.Containers = []docker.Container{{ID: "old", Name: installspec.ProxyName, Labels: map[string]string{installspec.LabelRole: "proxy"}}}
	if err := n.orch.ensureNodeProxy(ctx, "w1"); err == nil {
		t.Fatal("want the pull error")
	}
	if sawCall(n.wFake, "StopRemove") {
		t.Fatal("the old proxy was removed before the new image was there")
	}
}
