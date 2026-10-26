package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/address"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// renameCarrying runs rename, which returns the new slug, and carrySlug in
// one transaction: a rename never commits without its references.
func (o *Orchestrator) renameCarrying(
	ctx context.Context,
	orgID string,
	pos int,
	above []string,
	old string,
	rename func(store.Tx) (string, error),
) ([]string, error) {
	var warn []string
	err := o.store.Tx(ctx, func(tx store.Tx) error {
		nu, err := rename(tx)
		if err != nil {
			return err
		}
		warn, err = o.carrySlug(ctx, tx, orgID, pos, above, old, nu)
		return err
	})
	return warn, err
}

// carrySlug rewrites, inside the rename's own transaction, every stored
// reference in org that names the renamed slug: slice provision_from
// (stack:env:tile), managed allow lists (org:stack:env:tile) and, for an env,
// the env pairs of its own stack's managed tiles. pos is the moved segment
// of an address (0 org, 1 stack, 2 env, 3 tile), above the slugs before it,
// as they are after the rename. A reference in a config-managed stack is
// its stack file's, so it is left alone and comes back as a warning.
// ponytail: a pattern with * above the moved segment (acme:*:qa:*) also
// names other stacks' qa, so it is kept as written.
func (o *Orchestrator) carrySlug(
	ctx context.Context,
	tx store.Tx,
	orgID string,
	pos int,
	above []string,
	old, nu string,
) ([]string, error) {
	if old == nu {
		return nil, nil
	}
	sts, err := stack.New(tx.Stacks).List(ctx, orgID)
	if err != nil {
		return nil, err
	}
	byID := map[string]store.Stack{}
	for _, s := range sts {
		byID[s.ID] = s
	}
	var warn []string
	file := func(st store.Stack, t store.Tile) {
		warn = append(warn, fmt.Sprintf("%s in stack %s comes from its stack file, so change %s to %s there.", t.Name, st.Name, old, nu))
	}
	tiles := tile.New(tx.Tiles, o.docker, o.vip)
	if pos > 0 { // provision_from has no org segment
		ts, err := tiles.ListByKind(ctx, tile.Slice)
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			st, ok := byID[t.StackID]
			if !ok || t.ProvisionFrom == nil {
				continue
			}
			v, ok := address.RenameSeg(*t.ProvisionFrom, pos-1, above[1:], old, nu)
			if !ok {
				continue
			}
			if st.ConfigRepo != "" {
				file(st, t)
				continue
			}
			cur := t
			cur.ProvisionFrom = &v
			if _, _, err := tiles.Update(ctx, t, cur); err != nil {
				return nil, err
			}
		}
	}
	ms := managed.New(tx.ManagedInstances, tx.Provisions, tx.Bindings)
	orgSlug := nu
	if pos > 0 {
		orgSlug = above[0]
	}
	ts, err := tiles.ListByKind(ctx, tile.Managed)
	if err != nil {
		return nil, err
	}
	for _, t := range ts {
		st, ok := byID[t.StackID]
		if !ok {
			continue
		}
		m, err := ms.GetByTile(ctx, t.ID)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		allow, moved := make([]string, len(m.Allow)), false
		for i, a := range m.Allow {
			v, ok := address.RenameSeg(a, pos, above, old, nu)
			allow[i], moved = v, moved || ok
		}
		// An env pair's value is an env of this stack; its key is the env
		// segment a provision_from names, rewritten above for this stack.
		pairs := map[string]string{}
		for k, v := range m.EnvPairs {
			if pos == 2 && st.Slug == above[1] {
				if k == old {
					k, moved = nu, true
				}
				if v == old {
					v, moved = nu, true
				}
			}
			pairs[k] = v
		}
		if !moved {
			continue
		}
		if st.ConfigRepo != "" {
			file(st, t)
			continue
		}
		if m, err = ms.SetAllow(ctx, m, orgSlug, allow); err != nil {
			return nil, err
		}
		if _, err := ms.SetEnvPairs(ctx, m, pairs); err != nil {
			return nil, err
		}
	}
	return warn, nil
}
