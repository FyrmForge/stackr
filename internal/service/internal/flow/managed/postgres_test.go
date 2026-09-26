package managed

import (
	"slices"
	"testing"
)

// The statements per access, exactly: what a reviewer checks against the
// postgres docs, and what a change here has to say out loud.
func TestPostgresStatements(t *testing.T) {
	s := Slice{
		Name: "orders",
		User: "orders",
	}
	api := Grant{
		User:   "orders_api",
		Access: "write",
	}
	rep := Grant{
		User:   "orders_reporter",
		Access: "read",
	}
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "write, alone",
			got:  pgGrants(s, api, nil),
			want: []string{
				`GRANT ALL ON DATABASE "orders" TO "orders_api"`,
				`GRANT ALL ON SCHEMA public TO "orders_api"`,
				`GRANT ALL ON ALL TABLES IN SCHEMA public TO "orders_api"`,
				`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public GRANT ALL ON TABLES TO "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public GRANT ALL ON SEQUENCES TO "orders_api"`,
			},
		},
		{
			name: "read, next to a writer",
			got:  pgGrants(s, rep, []Grant{api}),
			want: []string{
				`GRANT CONNECT ON DATABASE "orders" TO "orders_reporter"`,
				`GRANT USAGE ON SCHEMA public TO "orders_reporter"`,
				`GRANT SELECT ON ALL TABLES IN SCHEMA public TO "orders_reporter"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public GRANT SELECT ON TABLES TO "orders_reporter"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders_api" IN SCHEMA public GRANT SELECT ON TABLES TO "orders_reporter"`,
			},
		},
		{
			name: "write, next to a reader",
			got:  pgGrants(s, api, []Grant{rep}),
			want: []string{
				`GRANT ALL ON DATABASE "orders" TO "orders_api"`,
				`GRANT ALL ON SCHEMA public TO "orders_api"`,
				`GRANT ALL ON ALL TABLES IN SCHEMA public TO "orders_api"`,
				`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public GRANT ALL ON TABLES TO "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public GRANT ALL ON SEQUENCES TO "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders_api" IN SCHEMA public GRANT SELECT ON TABLES TO "orders_reporter"`,
			},
		},
		{
			name: "write to read revokes and hands its tables to the owner",
			got:  pgRevokes(s, api, "write", []Grant{rep}),
			want: []string{
				`REASSIGN OWNED BY "orders_api" TO "orders"`,
				`REVOKE ALL ON DATABASE "orders" FROM "orders_api"`,
				`REVOKE ALL ON SCHEMA public FROM "orders_api"`,
				`REVOKE ALL ON ALL TABLES IN SCHEMA public FROM "orders_api"`,
				`REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public REVOKE ALL ON TABLES FROM "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public REVOKE ALL ON SEQUENCES FROM "orders_api"`,
			},
		},
		{
			name: "read to write revokes, keeps nothing to hand over",
			got:  pgRevokes(s, rep, "read", []Grant{api}),
			want: []string{
				`REVOKE ALL ON DATABASE "orders" FROM "orders_reporter"`,
				`REVOKE ALL ON SCHEMA public FROM "orders_reporter"`,
				`REVOKE ALL ON ALL TABLES IN SCHEMA public FROM "orders_reporter"`,
				`REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM "orders_reporter"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public REVOKE ALL ON TABLES FROM "orders_reporter"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public REVOKE ALL ON SEQUENCES FROM "orders_reporter"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders_api" IN SCHEMA public REVOKE ALL ON TABLES FROM "orders_reporter"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders_api" IN SCHEMA public REVOKE ALL ON SEQUENCES FROM "orders_reporter"`,
			},
		},
		{
			name: "unbind",
			got:  pgUnbind(s, rep),
			want: []string{
				`REASSIGN OWNED BY "orders_reporter" TO "orders"`,
				`DROP OWNED BY "orders_reporter"`,
				`DROP ROLE IF EXISTS "orders_reporter"`,
			},
		},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, c.got, c.want)
		}
	}
}
