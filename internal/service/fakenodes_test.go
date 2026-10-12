package service

import (
	"context"
	"testing"
)

func TestFakeJoinNode(t *testing.T) {
	w, _, _ := joinWorld(t)
	ctx := context.Background()
	if _, err := w.orch.FakeJoinNode(ctx, "fake-1", nil, "127.0.0.1:7451", csrPEM(t), wgKey, nil); err == nil {
		t.Fatal("a panel off the fake host made a fake node")
	}
	w.orch.cfg.FakeHost = t.TempDir()
	st, err := w.orch.FakeJoinNode(ctx, "fake-1", []string{"builder"}, "127.0.0.1:7451", csrPEM(t), wgKey, nil)
	must(t, err)
	n, err := w.orch.store.Nodes.GetBySlug(ctx, "fake-1")
	must(t, err)
	if st.Status != JoinConfirmed || st.CertPEM == "" || n.Status != "online" || nodeAddr(n) != "127.0.0.1:7451" {
		t.Errorf("st=%+v node=%+v", st, n)
	}
}
