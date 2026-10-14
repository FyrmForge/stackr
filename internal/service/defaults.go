package service

import (
	"context"
	"slices"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The cluster-wide default ingress and default exit are install settings
// holding a node id. Unset (or a node that is gone or lost the tag) means the
// controller for ingress and the own uplink (no ladder) for exits.
const (
	settingDefaultIngress = "default_ingress"
	settingDefaultExit    = "default_exit"

	auditDefaultIngress = "node.default-ingress"
	auditDefaultExit    = "node.default-exit"
)

// defaultNode is the node id the setting names when it is still a live node
// with the tag, else "".
func defaultNode(ctx context.Context, st *store.Store, key, tag string) string {
	id, set, err := st.Settings.Get(ctx, key)
	if err != nil || !set || id == "" {
		return ""
	}
	n, err := st.Nodes.Get(ctx, id)
	if err != nil || n.Status == node.StatusPending || !slices.Contains(node.Tags(n), tag) {
		return ""
	}
	return n.ID
}

// defaultExitIDs is exits.Resolve's def: the default exit node, or nil.
func defaultExitIDs(ctx context.Context, st *store.Store) []string {
	if id := defaultNode(ctx, st, settingDefaultExit, node.TagEgress); id != "" {
		return []string{id}
	}
	return nil
}

// DefaultIngress is the node id the panel route and unpicked routes enter by.
func (o *Orchestrator) DefaultIngress(ctx context.Context) (string, error) {
	if id := defaultNode(ctx, o.store, settingDefaultIngress, node.TagIngress); id != "" {
		return id, nil
	}
	return o.controllerID(ctx)
}

// DefaultExit is the default exit node id, "" = each node's own uplink.
func (o *Orchestrator) DefaultExit(ctx context.Context) string {
	return defaultNode(ctx, o.store, settingDefaultExit, node.TagEgress)
}

// DefaultsOut is the cluster defaults, by node name ("" = none set).
type DefaultsOut struct {
	Ingress string `json:"ingress"`
	Exit    string `json:"exit"`
}

func (o *Orchestrator) Defaults(ctx context.Context) (DefaultsOut, error) {
	ing, err := o.DefaultIngress(ctx)
	if err != nil {
		return DefaultsOut{}, err
	}
	out := DefaultsOut{Ingress: o.nodeName(ctx, ing)}
	if x := o.DefaultExit(ctx); x != "" {
		out.Exit = o.nodeName(ctx, x)
	}
	return out, nil
}

// SetDefaultExit moves the default exit to a node tagged egress; ref "" clears
// it (every node goes out its own uplink again).
func (o *Orchestrator) SetDefaultExit(ctx context.Context, by *Principal, ref string) (DefaultsOut, error) {
	return o.setDefault(ctx, by, ref, node.TagEgress, settingDefaultExit, auditDefaultExit)
}

func (o *Orchestrator) setDefault(ctx context.Context, by *Principal, ref, tag, key, action string) (DefaultsOut, error) {
	if err := nodeAdmin(by); err != nil {
		return DefaultsOut{}, err
	}
	id := ""
	if ref != "" {
		n, err := o.nodeByRef(ctx, ref)
		if err != nil {
			return DefaultsOut{}, err
		}
		if n.Status == node.StatusPending {
			return DefaultsOut{}, errs.ErrNotFound
		}
		if !slices.Contains(node.Tags(n), tag) {
			return DefaultsOut{}, errs.Invalidf("node", "%s is not tagged %s", n.Name, tag)
		}
		id = n.ID
	} else if key == settingDefaultIngress {
		return DefaultsOut{}, errs.Invalidf("node", "name the node to make the default ingress")
	}
	err := o.store.Tx(ctx, func(tx store.Tx) error {
		var err error
		if id == "" {
			err = tx.Settings.Delete(ctx, key)
		} else {
			err = tx.Settings.Set(ctx, key, id)
		}
		if err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: action, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: "node", TargetID: id, Detail: "node " + ref,
		})
	})
	if err != nil {
		return DefaultsOut{}, err
	}
	o.kickMesh()
	if o.sync != nil {
		_ = o.sync.Sync(ctx) // ponytail: best effort; the periodic sync repairs a miss
	}
	return o.Defaults(ctx)
}
