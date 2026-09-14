package stackconf

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// BadRefs is the gate that turns an unresolvable reference into a plan error,
// and it runs on the apply path too. The stackr scope has to pass it: the value
// is recorded when the proxy attaches to the environment's network, and for a
// new environment that network does not exist until this plan's own apply
// creates it. Failing here would refuse the first deploy of every env using it.
func TestBadRefsAcceptsThePlatformScope(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	load := func(t *testing.T, val string) *Resolved {
		t.Helper()
		r, err := Load([]byte("version: 1\nstack: s\nenvironments:\n  prod:\n    tiles:\n      w:\n"+
			"        image: nginx\n        env:\n          TRUSTED_PROXIES: \""+val+"\"\n"), nil)
		require.NoError(t, err, "load")
		return r
	}
	pl := Planner{Store: store}

	for _, ok := range []string{"${{ stackr.PROXY_CIDR }}", "${{ stackr.PROXY_IP }}"} {
		bad := pl.BadRefs(ctx, seed.Stack, load(t, ok), "", nil)
		assert.Empty(t, bad, "%s was rejected at plan time: %v", ok, bad)
	}

	// A typo must still be caught here rather than at deploy: the closed name
	// set is the whole reason this scope can be waved through above.
	bad := pl.BadRefs(ctx, seed.Stack, load(t, "${{ stackr.PROXY }}"), "", nil)
	require.Len(t, bad, 1, "bad = %v, want the unknown-name error", bad)
	assert.Contains(t, bad[0], "is not a stackr value", "bad = %v, want the unknown-name error", bad)
}

// A generated secret used to be flagged twice: SecretIssues warned it would be
// minted on apply, and BadRefs errored that nobody had set it. The error blocks
// the apply that would have minted it, so the only way out was to set the value
// by hand, a soft lock on every stack whose file declares its own secrets.
func TestBadRefsAcceptsDeclaredSecrets(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	file := "version: 1\nstack: s\nsecrets:\n  SESSION_SECRET:\n    default: generated\n  API_KEY:\n" +
		"environments:\n  prod:\n    tiles:\n      w:\n        image: nginx\n        env:\n" +
		"          S: \"${{ stack.secrets.SESSION_SECRET }}\"\n          K: \"${{ stack.secrets.API_KEY }}\"\n" +
		"          U: \"${{ stack.vars.NOT_DECLARED }}\"\n"
	r, err := Load([]byte(file), nil)
	require.NoError(t, err, "load")

	pl := Planner{Store: store}
	bad := pl.BadRefs(ctx, seed.Stack, r, "", nil)
	require.Len(t, bad, 1, "bad = %v, want only the undeclared name", bad)
	assert.Contains(t, bad[0], "NOT_DECLARED", "bad = %v, want only the undeclared name", bad)

	// The declared ones do not block, and no longer warn either: each one is
	// a row of the plan now.
	errs, warns, gen := pl.SecretIssues(ctx, seed.Stack, r, "", nil)
	assert.Empty(t, errs, "declared secrets must not error")
	assert.Equal(t, []string{"SESSION_SECRET"}, gen, "generated secret lost its mint")
	assert.Empty(t, warns, "warns = %v, declared secrets are rows, not banners", warns)
}
