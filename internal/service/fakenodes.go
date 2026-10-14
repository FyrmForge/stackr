package service

import (
	"context"
	"encoding/json"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
)

// FakeJoinNode is the fake host's join bootstrap: it mints a token, posts
// the request and confirms it in one go, as the admin and the new machine
// would over several calls, then points the node at addr (its fake worker on
// localhost) and marks it online. Local development only: it refuses unless
// STACKR_FAKE_HOST is on, and the security log shows every step as the
// "fake-host" actor.
func (o *Orchestrator) FakeJoinNode(ctx context.Context, name string, tags []string, addr, csrPEM, wgPub string, facts json.RawMessage) (JoinStatus, error) {
	if o.cfg.FakeHost == "" {
		return JoinStatus{}, errs.Refusedf("Fake nodes exist only on the fake host.")
	}
	by := &Principal{User: User{ID: "fake-host"}, Access: authz.User{ID: "fake-host", Active: true, Admin: true}}
	tok, err := o.CreateJoinToken(ctx, by, JoinTokenSpec{NodeName: name, Tags: tags})
	if err != nil {
		return JoinStatus{}, err
	}
	st, err := o.RequestJoin(ctx, JoinMeta{IP: "127.0.0.1", UserAgent: "fake-host", HTTPS: true},
		JoinRequest{Token: tok.Token, CSR: csrPEM, WGPublicKey: wgPub, Facts: facts})
	if err != nil {
		return JoinStatus{}, err
	}
	if _, err := o.ConfirmJoin(ctx, by, st.NodeID); err != nil {
		return JoinStatus{}, err
	}
	n, err := o.store.Nodes.Get(ctx, st.NodeID)
	if err != nil {
		return JoinStatus{}, err
	}
	// A real worker is reached on its wg address and goes online on first
	// contact; the fake one is on localhost from the start.
	n.WGAddress, n.Status = addr, node.StatusOnline
	if err := o.store.Nodes.Update(ctx, n); err != nil {
		return JoinStatus{}, err
	}
	out, err := o.JoinStatus(ctx, tok.Token)
	if err != nil {
		return out, err
	}
	return out, o.refreshNodeClients(ctx)
}
