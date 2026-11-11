package service

import (
	"context"
	"slices"
	"testing"
)

// A standby may go on an online node that holds no member; none is offered
// once one sits there.
func TestStandbyCandidates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	pgOn(t, n)
	got, err := n.orch.StandbyCandidates(ctx, adminP(), n.devT.ID)
	if err != nil || !slices.Equal(got, []string{"w1"}) {
		t.Fatalf("candidates = %v %v, want [w1]", got, err)
	}
	must(t, n.st.Environments.SetSubnet(ctx, n.dev.ID, "10.208.0.0/24"))
	_, err = n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	if got, err = n.orch.StandbyCandidates(ctx, adminP(), n.devT.ID); err != nil || len(got) != 0 {
		t.Errorf("candidates with a standby on w1 = %v %v, want none", got, err)
	}
}
