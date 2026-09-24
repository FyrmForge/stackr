# infra/managedtiles

- **Source:** `infra/managedtiles/` — `engine_postgres.go`, `provision.go`, `provision_s3.go`, `ready.go`, `sqlnames.go` (+ `engine_test.go`, `provision_test.go`, `restorecmd_test.go`)
- **Commit:** `c2423f0`
- **Taken:** the per-engine shape behind the new `ManagedTile` interface — definition, readiness probe, provision/drop, per-slice bindings, backup/restore methods — with the SQL and CLI strings verbatim, plus the slice-naming rules.
- **Cut:** its own `Deploy`/`Start`/`Stop`/`Remove`, `fork.go` (and `pgFork`/`pgExtensions`/`s3Fork`, which live inside taken files), browse (`sqlbrowse.go`, `s3browse.go`), stats (`stats.go`, `pgSliceStats`), `AttachableDBs`' store walk, the reconcile bookkeeping, every Docker call and every store call.
- **Cuts belong to:** `flow/deploy` (the one deploy path), `leaf/tile.Exec` (running an engine's command), `leaf/managed` (the `managed_instances` and provisions rows, and the endpoint a slice is reached on), dropped outright (fork, browse, stats).

## engine.go

```go
// One interface per engine. The engine produces commands and never holds a
// Docker handle; flow/managed hands each command to leaf/tile.Exec(). argv
// batches, because one provision is several statements in one session.
//
// s3 does not fit this return type — see "Two execution contracts" in the
// notes, which is the one decision left open here.
type ManagedTile interface {
	Definition() Definition
	Ready(i Instance) []string              // argv; nil = nothing to probe, engine passes
	Provision(i Instance, s Slice) [][]string
	Drop(i Instance, s Slice) [][]string
	Bindings(i Instance, s Slice) []Binding
	Backup(method string, i Instance) []string
	Restore(method, target string, i Instance) []string
	// ponytail: no Scale() on the interface yet; add it when an engine can
	// actually scale, not before.
}

// Definition is everything the deploy flow needs to build a normal tile spec
// for the instance container.
type Definition struct {
	Image   string                   // engine default; an instance may pin its own
	Command []string                 // nil = the image's own entrypoint
	Config  func(i Instance) []string // first-boot env, the credentials included
	Port    int
	Volumes []string // data paths that must survive a recreate
	Deps    []string // other tiles this engine needs up first (none in v1)

	// Defaults an instance inherits and a slice reads.
	PrimaryOutput string // the binding a consumer gets when it names none
	InjectAll     bool   // the consumer needs the whole set, not one url
	PublicSlices  bool   // slices can be published read-only
	SliceNoun     string // "logical db", "bucket" — what the UI calls a slice
	Backups       []string // methods this engine offers: "dump", later "pitr"
}

// Instance is the running engine: admin credentials, where it answers, which
// image. Facts as arguments — the engine never loads a row.
type Instance struct {
	Slug     string
	Engine   string
	AdminUser, AdminPassword string
	AdminDB  string // the engine's own database/bucket namespace
	Host     string // the in-network alias, or the IP leaf/managed resolved
	Port     int
	PublicBase string // scheme+host of the instance's public domain, "" if none
}

// Slice is one per-consumer cut of the instance: a logical database for
// postgres, a bucket for s3.
type Slice struct {
	Name     string // db name / bucket name, already uniquified
	User     string
	Password string
	Public   bool
}

// Binding is one published connection detail. RequiresNetwork marks a value
// whose hostname only resolves on the instance's shared network, so the
// resolver has to pull the consumer onto it.
type Binding struct {
	Name            string
	Value           string
	Secret          bool
	RequiresNetwork bool
}

// Registry: one map, one line per engine. Plugins later write into the same
// map. Nothing outside an engine's own file branches on the engine name.
var Engines = map[string]ManagedTile{}

// extract: dropped RecordProvision/UpdateProvision/RemoveProvision and the
// managed_resource sync that every old hook called at the end of its own body,
// belongs in leaf/managed — the flow writes the row once, after the engine's
// commands have run.
```

### Slice naming (`sqlnames.go`, taken whole)

```go
// sqlIdent turns a tile slug into a safe SQL identifier: lowercase, digits and
// underscores, never leading with a digit.
func sqlIdent(slug string) string {
	id := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r == '-':
			return '_'
		}
		return -1
	}, strings.ToLower(slug))
	if id == "" || (id[0] >= '0' && id[0] <= '9') {
		id = "db_" + id
	}
	return id
}

// uniqueSliceName suffixes base until no existing slice answers to it. sep is
// the engine's own joiner: "_" where the name is an SQL identifier, "-" where
// it has to be DNS-safe. ("api", ["api","api_2"], "_") -> "api_3".
func uniqueSliceName(base string, existing []string, sep string) string {
	taken := map[string]bool{}
	for _, n := range existing {
		taken[n] = true
	}
	name := base
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s%s%d", base, sep, i)
	}
	return name
}
// extract: dropped the []repo.Provision argument, belongs in leaf/managed —
// the flow reads the instance's existing slice names and passes them in.
```

## postgres.go

```go
func (pg) Definition() Definition {
	return Definition{
		Image:   "postgres:17",
		Port:    5432,
		Volumes: []string{"/var/lib/postgresql/data"},
		Config: func(i Instance) []string {
			return []string{
				"POSTGRES_DB=" + i.AdminDB,
				"POSTGRES_USER=" + i.AdminUser,
				"POSTGRES_PASSWORD=" + i.AdminPassword,
			}
		},
		PrimaryOutput: "DATABASE_URL",
		SliceNoun:     "logical db",
		Backups:       []string{"dump"},
	}
}

// Ready asks the instance itself whether it is accepting connections — not
// "the container is running".
func (pg) Ready(i Instance) []string { return []string{"pg_isready", "-U", i.AdminUser} }

// Provision: a logical database plus an owner role for the consumer. The slice
// name is sqlIdent(consumer slug or the requested name), uniquified with "_".
func (e pg) Provision(i Instance, s Slice) [][]string {
	return [][]string{e.psql(i, "postgres", []string{
		fmt.Sprintf(`CREATE ROLE %q LOGIN PASSWORD '%s'`, s.User, s.Password),
		fmt.Sprintf(`CREATE DATABASE %q OWNER %q`, s.Name, s.User),
		fmt.Sprintf(`REVOKE CONNECT ON DATABASE %q FROM PUBLIC`, s.Name),
	})}
}

// Drop destroys the logical database and its owner role.
func (e pg) Drop(i Instance, s Slice) [][]string {
	return [][]string{e.psql(i, "postgres", []string{
		fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, s.Name),
		fmt.Sprintf(`DROP ROLE IF EXISTS %q`, s.User),
	})}
}

// psql runs each statement as the instance superuser in one session.
// ON_ERROR_STOP so a failed CREATE surfaces instead of silently continuing.
func (pg) psql(i Instance, db string, stmts []string) []string {
	args := []string{"env", "PGPASSWORD=" + i.AdminPassword, "psql", "-U", i.AdminUser,
		"-d", db, "-v", "ON_ERROR_STOP=1", "-c", noStatementLogging}
	for _, st := range stmts {
		args = append(args, "-c", st)
	}
	return args
}
// extract: docker call dropped, engine returns the command, leaf/tile.Exec runs it

// noStatementLogging keeps failing statements out of the instance's own log
// for the rest of the session. Our statements carry plaintext passwords
// (CREATE/ALTER ROLE), and postgres logs a failing statement verbatim at the
// default log_min_error_statement=ERROR — which is how credentials ended up in
// a container log. Session-local, superuser-only, first statement of every
// provisioning session.
const noStatementLogging = `SET log_min_error_statement = PANIC`

// Re-provisioning the same slice is a check-then-create, never a replayed
// CREATE: a CREATE that fails with "already exists" makes postgres log the
// failing statement, password included, on every consumer deploy. The state
// query, one round trip, both answers:
//   SELECT (EXISTS (SELECT FROM pg_roles    WHERE rolname = $user))::int || ',' ||
//          (EXISTS (SELECT FROM pg_database WHERE datname = $db))::int
// role exists -> ALTER ROLE %q LOGIN PASSWORD '%s' (re-syncs the row's creds,
// succeeds silently, logs nothing); role missing -> CREATE ROLE; database
// missing -> CREATE DATABASE. Run without ON_ERROR_STOP so one expected error
// does not abort the rest.

// Bindings publish the connection url plus the PG* set libpq reads directly.
// Values are derived, never stored: host and port from the instance, the rest
// from the slice. Two things publish this list with the same names and never
// the same values — the instance about itself (admin creds, its own database)
// and a slice about itself — so it is spelled once.
func (pg) Bindings(i Instance, s Slice) []Binding {
	return []Binding{
		{"DATABASE_URL", fmt.Sprintf("postgres://%s:%s@%s:%d/%s",
			s.User, s.Password, i.Host, i.Port, s.Name), true, true},
		{"PGHOST", i.Host, false, true},
		{"PGPORT", strconv.Itoa(i.Port), false, false},
		{"PGDATABASE", s.Name, false, false},
		{"PGUSER", s.User, false, false},
		{"PGPASSWORD", s.Password, true, false},
	}
}
// RequiresNetwork is true exactly for the two entries carrying the hostname:
// DATABASE_URL and PGHOST.

// Backup("dump"): a logical dump streamed to stdout inside the container.
// "pitr" is not offered in v1, so an unknown method returns nil.
func (pg) Backup(method string, i Instance) []string {
	if method != "dump" {
		return nil
	}
	return []string{"env", "PGPASSWORD=" + i.AdminPassword, "pg_dump", "-U", i.AdminUser, "-d", i.AdminDB}
}

// Restore("dump", target): wipe, then restore. A bare `psql < dump` merges —
// pg_dump emits no DROP statements, so CREATE TABLE fails on a table already
// there and the COPY behind it lands on top, duplicating every row. Measured:
// restoring a one-row dump over a two-row table gave three rows and reported
// success, while the confirm dialog promises the data is overwritten.
// ON_ERROR_STOP so a restore that dies halfway is reported instead of leaving
// a half-restored database looking fine.
//
// Ceilings: only the public schema is dropped, so a dump that creates its own
// schemas restores into whatever of them is already there (recreating the
// database itself needs the server to hold no connection to it). And DROP
// SCHEMA takes locks, so a restore now waits on live connections to the
// instance's own database where before it merged into them — slices are
// separate databases and do not hold it.
//
// sh, not bash: the alpine postgres images have no bash. The password travels
// as a positional arg ($0), never inside the script text, so it stays out of
// the command line that gets logged.
func (pg) Restore(method, target string, i Instance) []string {
	script := `PGPASSWORD="$0" psql -v ON_ERROR_STOP=1 -U "$1" -d "$2" ` +
		`-c 'DROP SCHEMA public CASCADE' -c 'CREATE SCHEMA public' >/dev/null && ` +
		`PGPASSWORD="$0" psql -v ON_ERROR_STOP=1 -U "$1" -d "$2" >/dev/null`
	return []string{"sh", "-c", script, i.AdminPassword, i.AdminUser, target}
}

// extract: dropped pgFork/pgExtensions (fork.go's engine half), belongs
// nowhere in v1
// extract: dropped pgSliceStats/parseSliceStats, belongs nowhere in v1
```

## s3.go

```go
// RustFS. s3Region is fixed — RustFS ignores it, the S3 SDK requires one.
const s3Region = "us-east-1"

func (s3e) Definition() Definition {
	return Definition{
		Image:   "rustfs/rustfs:latest",
		Command: []string{"--console-enable", "/data"},
		Port:    9000,
		Volumes: []string{"/data"},
		Config: func(i Instance) []string {
			return []string{"RUSTFS_ACCESS_KEY=" + i.AdminUser, "RUSTFS_SECRET_KEY=" + i.AdminPassword}
		},
		PrimaryOutput: "S3_ENDPOINT",
		InjectAll:     true,  // endpoint + bucket + keys, not one url
		PublicSlices:  true,
		SliceNoun:     "bucket",
		Backups:       nil,   // s3 restores by volume, not by dump
	}
}

// Ready probes the instance from the outside: the container may not even ship
// a shell. ListBuckets with the instance root credentials; any error is "not
// ready yet".
//
// Provision cuts a bucket, uniquified with "-" (DNS-safe), and is idempotent:
// CreateBucket answering BucketAlreadyOwnedByYou or BucketAlreadyExists is
// success, anything else is the error. Shared-root credentials in v1 — the
// slice carries the instance's own access/secret key and one bucket per
// consumer is the whole isolation boundary, until RustFS's admin API is proven
// enough for scoped keys.
//
// A public bucket is refused unless the instance already carries a public
// domain: public read is only useful if browsers can reach the instance, and
// that domain is the source of the public base URL.
// Public read is the standard S3 bucket policy, which RustFS honors:
//   {"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*",
//    "Action":"s3:GetObject","Resource":"arn:aws:s3:::%s/*"}]}
// Flipping it off is DeleteBucketPolicy, which returns the bucket to
// credentials-only access and deletes cleanly on a bucket that never had one.
// Exposure is a setting, not a decision frozen at creation — and the bindings
// are re-published on the flip, because S3_PUBLIC_URL only exists while the
// bucket is public.
//
// Drop empties the bucket and removes it: S3 refuses to delete a bucket that
// still holds objects, and this is the explicit "destroy this bucket" action,
// so its contents go with it.

// Bindings are the published surface of one bucket. The public base URL wins
// when the instance has a public domain; otherwise the in-network endpoint,
// which only resolves on the instance's shared network — hence the flag.
func (s3e) Bindings(i Instance, s Slice) []Binding {
	endpoint, requiresNet := i.PublicBase, false
	if endpoint == "" {
		endpoint, requiresNet = fmt.Sprintf("http://%s:%d", i.Host, i.Port), true
	}
	out := []Binding{
		{"S3_ENDPOINT", endpoint, false, requiresNet},
		{"S3_BUCKET", s.Name, false, false},
		{"S3_REGION", s3Region, false, false},
		{"S3_ACCESS_KEY", s.User, true, false},
		{"S3_SECRET_KEY", s.Password, true, false},
	}
	if s.Public && i.PublicBase != "" { // a public bucket needs a public instance
		out = append(out, Binding{Name: "S3_PUBLIC_URL", Value: i.PublicBase + "/" + s.Name})
	}
	return out
}

// extract: docker call dropped — reachInstance/sharedNetIP/instanceIP/
// joinSharedNet, belongs in leaf/managed, which hands the engine an endpoint
// (see the notes)
// extract: dropped s3Fork (fork.go's engine half), belongs nowhere in v1
// extract: dropped instancePublicBase's domain query, belongs in leaf/tile —
// the flow passes the public base in
```

## Rules as today (spec)

- **Registering an engine is the whole job.** An engine's map entry makes it
  provisionable, bindable, named with its own nouns and offered in the picker.
  If a new engine needs a change outside its own file, the registry has sprung
  a leak. (`engine_test.go` is that contract.)
- **No slice without a hook.** An engine with no provision support answers "not
  supported for engine %q yet" rather than half-cutting a slice; an engine
  without `PublicSlices` refuses a public slice in the registry, not in a
  caller that happens to know.
- **One slice per consumer, named after it.** The base name is the consumer's
  slug unless one is given, normalised by the engine (`sqlIdent` for SQL) and
  uniquified against the instance's existing slices with the engine's own
  separator.
- **A config-declared slice is adopted, not duplicated.** Same name on the same
  instance in the same env: re-stamp it with the config key and revive it if
  orphaned. Same name in a *different* env is a refusal, not a second copy —
  the file names the slice it means.
- **A cloned env gets a fresh slice** under the same reference key, never a
  second consumer of the base env's data.
- **Attaching a second consumer to an existing slice is same-env only**, shares
  the source slice's credentials and its public flag, and creates nothing in
  the engine — just the row. Detaching keeps the slice (orphaned) and revokes
  the binding so references stop resolving.
- **Dropping a slice takes every consumer's row with it.** The rows share one
  slice, so the engine drop runs once.
- **Readiness is per engine and never "the container is running".** Postgres
  asks itself (`pg_isready`), s3 asks over the API. An engine with no probe
  passes, so a new engine never gets a false gate.
- **Waiting for readiness does not pre-check for a container.** It polls (2s)
  until the deadline. One apply creates a shared instance and the slices cut
  from it, and for the first seconds the instance has a service but no running
  task — an up-front check fired instead of the wait, so every slice failed
  with "instance is not running" and only a second apply worked. A container
  that genuinely never arrives still fails, at the deadline.
- **Credentials never reach a log.** Postgres sets `log_min_error_statement =
  PANIC` for the session before any statement carrying a password, re-provision
  is check-then-create so no CREATE fails, and passwords travel as positional
  args rather than inside a script string.
- **A restore replaces, never merges,** and a restore that dies halfway is
  reported. (`restorecmd_test.go` asserts this for every engine that restores.)
- **Bindings whose value carries a hostname are flagged**, so the resolver
  knows it must pull the consumer onto the instance's shared network.

## Notes for the builder

- **Target:** `service/internal/flow/managed` — `engine.go` (interface +
  registry), `postgres.go`, `s3.go`.
- **DECIDE — two execution contracts.** REWRITE.md says engines produce
  commands and `flow/managed` hands them to `leaf/tile.Exec()`. Postgres does
  exactly that. s3 cannot: its image may ship no shell, so its provision, drop
  and readiness are S3 SDK calls against an endpoint, with the instance root
  credentials. The interface above is written the postgres way; the two ways
  out are (a) a second, smaller interface for API engines that the registry
  holds alongside this one, or (b) every method takes the exec/client as an
  argument (`Provision(i, s, exec func([]string) (string, error)) error`), so
  postgres runs argv and s3 runs SDK calls under one signature — the flow still
  holds no Docker handle either way. Do not paper over it by giving s3 a
  command it cannot run.
- **`leaf/managed` owes the engine an endpoint.** The old `reachInstance` is
  the dropped Docker half, and its reason survives: the provisioner dials the
  instance's **IP on the shared overlay**, not the tile alias, because a spec
  change that only adds an alias does not roll the task — a freshly created
  instance does not answer to its alias for seconds, which is exactly when
  provisioning happens. Consumers keep the alias (they deploy later and their
  DNS resolves it); this is the provisioner's own hop only.
- **`Bindings()` here means per-slice credentials**, not the
  `resource_bindings` rows in `extracts/managedinstance.md`. Same word, two
  things, both landing in `flow/managed`.
- **Instance deploy is `flow/deploy`'s**, not this package's: it reads
  `Definition()`, builds a normal tile spec, runs it, then waits on `Ready()`.
  The old `Deploy`/`Start`/`Stop`/`Remove` in this package is the second deploy
  path and does not come over.
- **Who may provision is `can()` in middleware**, as for every other verb;
  nothing in these files checks it today either.
- **Not coming over:** fork (`fork.go` plus `pgFork`/`pgExtensions`/`s3Fork`
  inside the taken files), the data/file browsers, slice stats, and
  `AttachableDBs`' store walk for the "attach to existing" picker.
- **`Backups` is a list, not a bool:** postgres offers `dump` in v1 and `pitr`
  later, s3 offers none (it restores by volume).

Size: source 1225 lines, extract 420 lines
