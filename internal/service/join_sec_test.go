package service

import (
	"context"
	"testing"
	"time"
)

// A new rejoin command ends the one before it.
func TestNewRejoinExpiresThePreviousCommand(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	adm := adminP()
	joinW1(t, o, adm)
	first, err := o.RejoinNode(ctx, adm, "w1")
	must(t, err)
	second, err := o.RejoinNode(ctx, adm, "w1")
	must(t, err)
	if _, err := o.RequestJoin(ctx, httpsMeta, joinReq(t, first.Token)); !isNotFound(err) {
		t.Fatalf("the old rejoin command still works: %v", err)
	}
	if _, err := o.RequestJoin(ctx, httpsMeta, joinReq(t, second.Token)); err != nil {
		t.Fatalf("the new rejoin command: %v", err)
	}
}

// A node mid rejoin stays listed and its drawer opens, waiting to rejoin
// both before and after the box called in.
func TestRejoinStaysListed(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	adm := adminP()
	id, _ := joinW1(t, o, adm)
	out, err := o.RejoinNode(ctx, adm, "w1")
	must(t, err)
	shown := func() string {
		t.Helper()
		d, err := o.NodeDetail(ctx, id)
		must(t, err)
		ns, err := o.Nodes(ctx)
		must(t, err)
		for _, n := range ns {
			if n.ID == id && n.Status == d.Status {
				return n.Status
			}
		}
		t.Fatalf("w1 is not listed: %+v", ns)
		return ""
	}
	if s := shown(); s != StateNeedsRejoin {
		t.Errorf("before the call in = %q", s)
	}
	_, err = o.RequestJoin(ctx, httpsMeta, joinReq(t, out.Token))
	must(t, err)
	if s := shown(); s != NodeRejoining {
		t.Errorf("after the call in = %q, want %q", s, NodeRejoining)
	}
}

// A confirmed node not seen within the wait says the join stopped.
func TestJoinedNotSeenStalls(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	id, _ := joinW1(t, o, adminP())
	stalled := func() bool {
		t.Helper()
		d, err := o.NodeDetail(ctx, id)
		must(t, err)
		if d.Status != "joined" {
			t.Fatalf("status = %q, want joined", d.Status)
		}
		return d.Stalled
	}
	if stalled() {
		t.Error("stalled right after the confirm")
	}
	old := joinSeenWait
	joinSeenWait = -time.Minute
	defer func() { joinSeenWait = old }()
	if !stalled() {
		t.Error("not stalled past the wait")
	}
}

// Another node's pending command is left alone.
func TestRejoinKeepsOtherNodesCommands(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	adm := adminP()
	joinW1(t, o, adm)
	other, err := o.CreateJoinToken(ctx, adm, JoinTokenSpec{NodeName: "w2"})
	must(t, err)
	_, err = o.RejoinNode(ctx, adm, "w1")
	must(t, err)
	if _, err := o.RequestJoin(ctx, httpsMeta, joinReq(t, other.Token)); err != nil {
		t.Fatalf("w2's command died with w1's rejoin: %v", err)
	}
}

// The token that joined a node stops answering /join/status once the node had
// time to fetch its cert.
func TestJoinStatusEndsAfterTheGrace(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	adm := adminP()
	out, err := o.CreateJoinToken(ctx, adm, JoinTokenSpec{NodeName: "w1"})
	must(t, err)
	st, err := o.RequestJoin(ctx, httpsMeta, joinReq(t, out.Token))
	must(t, err)
	_, err = o.ConfirmJoin(ctx, adm, st.NodeID)
	must(t, err)
	if s, err := o.JoinStatus(ctx, out.Token); err != nil || s.Status != JoinConfirmed {
		t.Fatalf("right after the confirm: %+v %v", s, err)
	}
	old := joinStatusGrace
	joinStatusGrace = -time.Hour
	defer func() { joinStatusGrace = old }()
	if _, err := o.JoinStatus(ctx, out.Token); !isNotFound(err) {
		t.Fatalf("a confirmed token still answers: %v", err)
	}
}
