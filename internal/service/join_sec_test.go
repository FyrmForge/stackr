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
