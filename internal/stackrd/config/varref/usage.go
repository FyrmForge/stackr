package varref

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ScopeVarsUsed reports which stack- or org-scoped variable names the given
// tiles reference, the bucket forms `${{ stack.vars.NAME }}` and
// `${{ stack.secrets.NAME }}` (and their org counterparts), not a singleton
// reference, which points at a tile rather than a variable. Both buckets count:
// the canvas draws one card per name, secret or not.
//
// Deliberately not a Resolve pass: the canvas only needs to know that a name is
// mentioned, and resolving would decrypt secrets, hit every referenced source
// and fail whenever a reference is broken. Drawing an edge for a reference that
// doesn't resolve is the right behaviour here, the wire is what the user wrote,
// and the deploy path is what refuses it.
func ScopeVarsUsed(ctx context.Context, store repo.Store, scope string, tileIDs []string) map[string]bool {
	used := map[string]bool{}
	for _, id := range tileIDs {
		vars, err := store.ListVariables(ctx, repo.OwnerTile, id)
		if err != nil {
			continue
		}
		for _, v := range vars {
			for _, body := range Refs(v.Value) {
				r, err := Parse(body)
				if err != nil {
					continue // malformed: the deploy path reports it, the canvas skips it
				}
				if r.Scope == scope && (r.Slug == BucketVars || r.Slug == BucketSecrets) {
					used[r.Name] = true
				}
			}
		}
	}
	return used
}
