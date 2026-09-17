package db_test

import (
	"testing"

	hamrsqlite "github.com/FyrmForge/hamr/pkg/db/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/db"
)

// Migrations run automatically at server start, so a broken one takes the whole
// app down rather than degrading a feature. Nothing else in the suite exercises
// the down direction at all.
func TestMigrationsUpDownUp(t *testing.T) {
	conn, err := hamrsqlite.Connect(":memory:")
	require.NoError(t, err, "open sqlite")
	defer func() { _ = conn.Close() }()

	cfg := db.MigrateConfig()
	require.NoError(t, hamrsqlite.Migrate(conn, cfg), "migrate up")
	require.NoError(t, hamrsqlite.MigrateDown(conn, cfg), "migrate down")
	// Up again: a down that half-succeeds leaves a schema the next up cannot
	// apply, which is the failure mode that only shows on a rollback.
	require.NoError(t, hamrsqlite.Migrate(conn, cfg), "migrate up after down")

	version, dirty, err := hamrsqlite.MigrateVersion(conn, cfg)
	require.NoError(t, err, "version")
	require.False(t, dirty, "schema left dirty at version %d", version)
}

// squashedColumns is every column the old incremental migrations added by ALTER.
// The baseline was squashed from a dumped schema rather than retyped, so a
// missing column is unlikely, but it is also the one way a squash goes wrong,
// and it would surface as a runtime "no such column" on whichever feature owned
// it rather than as a failed migration.
var squashedColumns = map[string][]string{
	"api_keys":      {"scopes"},
	"config_plans":  {"env_slug"},
	"domains":       {"auto", "cert_pem", "key_pem", "redirect_to"},
	"environments":  {"apply_policy", "config_branch"},
	"metrics":       {"rx_bps", "tx_bps"},
	"notifications": {"user_id"},
	"orgs":          {"avatar_path"},
	"provisions":    {"public", "on_remove"},
	"stacks":        {"config_branch", "config_connector_id", "config_path", "config_repo"},
	"tiles": {
		"attached_tile_id", "basic_auth_password", "basic_auth_user", "connector_id",
		"endpoint_port_var", "endpoint_protocol", "max_size_mb", "mount_path",
		"published_ports", "scope_id", "scope_kind", "sec_headers",
		"traefik_override", "volume_name", "watch_paths",
	},
	"users": {"avatar_path", "notify_prefs"},
}

func TestSquashedBaselineKeepsEveryColumn(t *testing.T) {
	conn, err := hamrsqlite.Connect(":memory:")
	require.NoError(t, err, "open sqlite")
	defer func() { _ = conn.Close() }()
	require.NoError(t, hamrsqlite.Migrate(conn, db.MigrateConfig()), "migrate")

	for table, want := range squashedColumns {
		var have []struct {
			Name string `db:"name"`
		}
		if err := conn.Select(&have, `SELECT name FROM pragma_table_info(?)`, table); !assert.NoError(t, err, "%s", table) {
			continue
		}
		if !assert.NotEmpty(t, have, "table %s is missing from the baseline", table) {
			continue
		}
		set := map[string]bool{}
		for _, c := range have {
			set[c.Name] = true
		}
		for _, col := range want {
			assert.True(t, set[col], "%s.%s lost in the squash", table, col)
		}
	}
}

// The baseline seeds one row the code addresses by literal id. A schema-only
// squash drops it silently and nothing fails until the first "create stack"
// hits a foreign key, which is exactly how this was found, on a freshly paved
// VM rather than in the suite. The orgs table deliberately starts empty: the
// first admin creates their organization from the root canvas.
func TestBaselineSeedRows(t *testing.T) {
	conn, err := hamrsqlite.Connect(":memory:")
	require.NoError(t, err, "open sqlite")
	defer func() { _ = conn.Close() }()
	require.NoError(t, hamrsqlite.Migrate(conn, db.MigrateConfig()), "migrate")

	var kind string
	require.NoError(t, conn.Get(&kind, `SELECT kind FROM servers WHERE id = 'local'`),
		"missing the local server (settings cascade from it)")
	assert.Equal(t, "local", kind)

	var orgs int
	require.NoError(t, conn.Get(&orgs, `SELECT count(*) FROM orgs`))
	assert.Zero(t, orgs, "a fresh install must start with no orgs; the setup flow creates the first one")
}

// The baseline has to stand up a real object graph, not just parse. Foreign
// keys, defaults and the unique indexes are what a dumped-and-replayed schema
// can quietly get wrong.
func TestBaselineAcceptsAnObjectGraph(t *testing.T) {
	conn, err := hamrsqlite.Connect(":memory:")
	require.NoError(t, err, "open sqlite")
	defer func() { _ = conn.Close() }()
	require.NoError(t, hamrsqlite.Migrate(conn, db.MigrateConfig()), "migrate")

	for _, q := range []string{
		`INSERT INTO orgs (id, name, slug) VALUES ('o1', 'O', 'o')`,
		`INSERT INTO stacks (id, org_id, name, slug) VALUES ('s1', 'o1', 'S', 's')`,
		`INSERT INTO environments (id, stack_id, name, slug) VALUES ('e1', 's1', 'E', 'e')`,
		`INSERT INTO tiles (id, stack_id, environment_id, name, slug, webhook_token)
		 VALUES ('t1', 's1', 'e1', 'T', 't', 'tok')`,
		`INSERT INTO provisions (id, instance_tile_id, consumer_tile_id, env_id, db_name, db_user, db_password, secret_name, created_at)
		 VALUES ('p1', 't1', 't1', 'e1', 'orders', 'u', 'p', 's', CURRENT_TIMESTAMP)`,
	} {
		_, err := conn.Exec(q)
		require.NoError(t, err, "seed %q", q)
	}

	// A slice defaults to keeping its data; nothing may arm a drop implicitly.
	var onRemove string
	require.NoError(t, conn.Get(&onRemove, `SELECT on_remove FROM provisions WHERE id = 'p1'`), "read on_remove")
	assert.Equal(t, "", onRemove, "on_remove defaults to %q, want the data-preserving default", onRemove)

	// The tile slug is unique per environment; a second one must be refused.
	_, err = conn.Exec(`INSERT INTO tiles (id, stack_id, environment_id, name, slug, webhook_token)
		VALUES ('t2', 's1', 'e1', 'T', 't', 'tok2')`)
	assert.Error(t, err, "the unique index on (environment_id, slug) did not survive the squash")

	// Cascades: dropping the stack takes its environments and tiles with it.
	_, err = conn.Exec(`DELETE FROM stacks WHERE id = 's1'`)
	require.NoError(t, err, "delete stack")
	var n int
	err = conn.Get(&n, `SELECT count(*) FROM tiles`)
	assert.False(t, err != nil || n != 0, "tiles left behind after the stack went (n=%d, err=%v); cascade lost", n, err)
}

// Nothing here rolls back through a numbered migration any more: the chain is
// one baseline (001), squashed 2026-09-14, so there is no intermediate version
// to step to. TestMigrationsUpDownUp covers the only direction left. The test
// this replaced walked back through 008 to prove its compose-tiles archive was
// optional, and 008 no longer exists.
