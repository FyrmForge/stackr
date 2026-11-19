package service

import (
	"context"
	"log/slog"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// keyConnectorPin marks the one-time connector pin as done.
const keyConnectorPin = "compat.connector_pin"

// pinConnectors runs once, at the first start of the version that stopped
// picking a connector by host. It writes today's host pick into the empty
// connector id of each bound org and stack, so nothing clones through another
// connector at the upgrade. Only empty ids, only that column (no plan is
// queued), DB only. An org plan then shows a stack whose file names none and
// whose pin differs from what it inherits as a connector rebind. A failure is
// logged and start goes on; the marker is set only after a clean pass.
func (o *Orchestrator) pinConnectors(ctx context.Context) {
	if _, done, err := o.store.Settings.Get(ctx, keyConnectorPin); err != nil || done {
		return
	}
	if err := o.pinConnectorsPass(ctx); err != nil {
		slog.Warn("connectors: compat pin skipped", "err", err)
		return
	}
	if err := o.store.Settings.Set(ctx, keyConnectorPin, "1"); err != nil {
		slog.Warn("connectors: compat pin marker not set", "err", err)
	}
}

func (o *Orchestrator) pinConnectorsPass(ctx context.Context) error {
	ogs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, og := range ogs {
		conns, err := o.conns.ListConnected(ctx, og.ID)
		if err != nil {
			return err
		}
		if og.ConfigRepo != "" && og.ConfigConnectorID == "" {
			// ambiguous or none: left empty, it did not clone before either
			if c, err := connector.LegacyByHost(conns, connector.Host(og.ConfigRepo)); err == nil {
				og.ConfigConnectorID = c.ID
				if err := o.store.Orgs.Update(ctx, og); err != nil {
					return err
				}
			}
		}
		sts, err := o.stacks.List(ctx, og.ID)
		if err != nil {
			return err
		}
		for _, st := range sts {
			if st.ConfigRepo != "" && st.ConfigConnectorID == "" {
				if c, err := connector.LegacyByHost(conns, connector.Host(st.ConfigRepo)); err == nil {
					st.ConfigConnectorID = c.ID
					if err := o.store.Stacks.Update(ctx, st); err != nil {
						return err
					}
				}
			}
			if err := o.warnTileSwitch(ctx, og, st, conns); err != nil {
				return err
			}
		}
	}
	return nil
}

// warnTileSwitch logs each tile of st that now clones through another
// connector than the host pick it used before. A tile has no connector to pin.
func (o *Orchestrator) warnTileSwitch(ctx context.Context, og store.Org, st store.Stack, conns []store.Connector) error {
	ts, err := o.tiles.ListByStack(ctx, st.ID)
	if err != nil {
		return err
	}
	for _, t := range ts {
		if t.GitURL == "" {
			continue
		}
		was, werr := connector.LegacyByHost(conns, connector.Host(t.GitURL))
		now, nerr := connector.ForTile(conns, og, st, t.GitURL)
		if werr == nil && nerr == nil && was.ID != now.ID {
			slog.Warn("connectors: a tile now clones through another connector",
				"tile", t.ID, "was", was.Name, "now", now.Name)
		}
	}
	return nil
}

// repoWarning is the plan and Used by line for a repo the connector in use
// does not list; "" when it is listed or its list is not known (memory only,
// never a provider call per render).
func (o *Orchestrator) repoWarning(c store.Connector, repoURL string) string {
	if listed, known := o.conns.Lists(c.ID, repoURL); !known || listed {
		return ""
	}
	return "connector " + c.Name + " does not list " + repoAt(repoURL, "") + "; the clone works only if the repo is public"
}

// repoWarningFor is repoWarning for the connector a clone of repo of stack st
// uses: the stack's own repo, or a tile's.
func (o *Orchestrator) repoWarningFor(ctx context.Context, st store.Stack, repo string, tile bool) string {
	og, cs, ok := o.connectorsOf(ctx, st.OrgID)
	if !ok {
		return ""
	}
	pick := connector.ForStack
	if tile {
		pick = func(cs []store.Connector, og store.Org, st store.Stack) (store.Connector, error) {
			return connector.ForTile(cs, og, st, repo)
		}
	}
	c, err := pick(cs, og, st)
	if err != nil {
		return ""
	}
	return o.repoWarning(c, repo)
}
