# CLI reference

Generated from the command definitions: `go test ./cmd/stackr -run TestCLIReference -update`. `stackr <command> --help` prints the same text.

```
Drive a stackr server from the terminal.

Environment (each beats the config file; nothing here is written to disk):
  STACKR_KEY     API key, so CI needs no login
  STACKR_SERVER  server URL
  STACKR_ORG     org slug
  STACKR_PROFILE login to use (see stackr profile ls); --profile beats it
  STACKR_CONFIG  config file path
```

## Flags on every command

```
      --json             print JSON; errors go to stderr as {error, status, code}
      --profile string   the saved login to use (stackr profile ls); login makes it
      --utc              print times in UTC, not this machine's zone
  -y, --yes              skip confirmation prompts (never overrides a server refusal)
```

Exit codes: 0 fine, 1 failed, 2 wrong command line, 3 refused without `--yes` (no terminal, or `--json`).

## `stackr admin`

Server administration (stackr admins)

### `stackr admin admin`

Make a user a stackr admin, or not (--on=false)

```
stackr admin admin <user-id> [flags]
```

```
      --on   admin or not (default true)
```

### `stackr admin alerts`

Alert webhook: node offline, failed backups, expiring certs, low disk (admin)

#### `stackr admin alerts clear`

Turn alerts off: forget the URL and secret

```
stackr admin alerts clear
```

#### `stackr admin alerts set`

Set the alert webhook URL and its signing secret

```
stackr admin alerts set [flags]
```

```
      --no-secret      stop signing alerts
      --secret-stdin   read the HMAC signing secret from stdin
      --url string     where alerts are posted (http or https); empty keeps the current one
```

#### `stackr admin alerts show`

Show the alert webhook and its last delivery

```
stackr admin alerts show
```

#### `stackr admin alerts test`

Send a test alert now

```
stackr admin alerts test
```

### `stackr admin audit`

The security audit log (server admin)

```
stackr admin audit [flags]
```

```
      --action string    an action, or a pattern like cert.*
      --actor string     an email, an address, or system
      --limit int        at most this many events (newest first)
      --outcome string   ok, failed or refused
      --since string     from here on: a duration (24h) or an RFC 3339 time
      --target string    a node name or target id
      --until string     up to here: a duration ago or an RFC 3339 time
```

Example:

```
  stackr admin audit --since 24h
  stackr admin audit --action 'cert.*' --outcome ok
```

### `stackr admin defaults`

Show or change the server's settings defaults

```
stackr admin defaults [flags]
```

```
      --clear stringArray   give a knob back to the built-in default
      --set stringArray     knob=value (stackr knobs lists them)
```

### `stackr admin dest-grant-approve`

Let an org's backup destination reach its private address (id or name)

```
stackr admin dest-grant-approve <dest>
```

### `stackr admin dest-grant-revoke`

Take back an org backup destination's access to its private address (id or name)

```
stackr admin dest-grant-revoke <dest>
```

### `stackr admin dest-grants`

List the org backup destinations on a private address and whether they wait for approval

```
stackr admin dest-grants
```

### `stackr admin disable`

Disable a user; their sessions and keys close

```
stackr admin disable <user-id>
```

### `stackr admin enable`

Enable a user; closed sessions and keys stay closed

```
stackr admin enable <user-id>
```

### `stackr admin host-grants`

List every stack's elevated access and what waits for approval

```
stackr admin host-grants
```

### `stackr admin image-check`

Check every image for a newer tag; a moved tag writes a release

```
stackr admin image-check [flags]
```

```
      --no-wait   print the job and return; it runs server-side
```

### `stackr admin images`

List the images stackr knows

```
stackr admin images
```

### `stackr admin key`

Make an admin key bound to no org; the token is shown once

```
stackr admin key <name>
```

### `stackr admin observe`

Show metrics history and logs: on or off, retention, data size and free disk

```
stackr admin observe
```

### `stackr admin orgs`

List every org

```
stackr admin orgs
```

### `stackr admin panel-backup`

Back up stackr's own database now

```
stackr admin panel-backup [flags]
```

```
      --dest string   a server destination's name, or local (default: the scheduled destination)
      --no-wait       print the job and return; it runs server-side
```

### `stackr admin panel-backups`

List backups of stackr's own database

```
stackr admin panel-backups
```

### `stackr admin password`

Set a user's password

```
stackr admin password <user-id>
```

### `stackr admin placement`

Placements: named places envs belong to, each on one node (server admin)

#### `stackr admin placement add`

Make a placement, on a node or none yet (server admin)

```
stackr admin placement add <name> [flags]
```

```
      --node string   the node it stands on (default: none yet)
      --open string   all, or the org slugs that may use it, separated by commas (default "all")
```

Example:

```
  stackr admin placement add eu-prod --node h4
  stackr admin placement add eu-db --node h3 --open acme,shop
```

#### `stackr admin placement ls`

List placements with their node (server admin)

```
stackr admin placement ls
```

#### `stackr admin placement move`

Repoint a placement to another node, moving its envs (server admin)

```
stackr admin placement move <name> <node> [flags]
```

```
      --no-wait       print the job and return; it runs server-side
      --start-empty   restore even though some volumes have no backup: they start empty
```

Example:

```
  stackr admin placement move eu-prod h5
  stackr admin placement move eu-prod h5 --yes
  stackr admin placement move lab h5 --start-empty --yes
```

#### `stackr admin placement open`

Open a placement to every org or to a list; envs already there stay (server admin)

```
stackr admin placement open <name> <all|org,org,...>
```

#### `stackr admin placement retry`

Move the envs a failed repoint left behind (server admin)

```
stackr admin placement retry <name> [flags]
```

```
      --no-wait   print the job and return; it runs server-side
```

#### `stackr admin placement rm`

Delete a placement no pin or env uses (server admin)

```
stackr admin placement rm <name>
```

### `stackr admin proxy-sync`

Push the routing table to Caddy again

```
stackr admin proxy-sync
```

### `stackr admin registry`

The image registry, server-wide

#### `stackr admin registry clean`

Delete the versions no env runs beyond the newest, then free the disk

```
stackr admin registry clean [flags]
```

```
      --no-wait   print the job and return; it runs server-side
```

#### `stackr admin registry ls`

Show what each org holds and how the last cleanup went

```
stackr admin registry ls
```

### `stackr admin route`

Routes: hosts that go to an address outside stackr (admin)

#### `stackr admin route add`

Add a route

```
stackr admin route add <host> [flags]
```

```
      --insecure      https only: do not verify the target's certificate
      --mode string   passthrough (raw TLS, by name), http or https (stackr terminates TLS)
      --to string     where it goes: host or host:port (443 by default, 80 for http)
```

#### `stackr admin route ls`

List routes

```
stackr admin route ls
```

#### `stackr admin route rm`

Remove a route

```
stackr admin route rm <host>
```

### `stackr admin setting`

Read or write one server setting

```
stackr admin setting <key> [value]
```

### `stackr admin upgrade`

Upgrade stackr to a release

```
stackr admin upgrade [flags]
```

```
      --no-wait      print the job and return; it runs server-side
      --tag string   the release tag (stackr admin upgrade-check names the newest)
```

### `stackr admin upgrade-check`

Is there a newer stackr?

```
stackr admin upgrade-check
```

### `stackr admin upgrade-workers`

Upgrade every worker to this server's version, one at a time

```
stackr admin upgrade-workers [flags]
```

```
      --no-wait   print the job and return; it runs server-side
```

### `stackr admin users`

List users

```
stackr admin users
```

### `stackr admin version`

Show the server's version

```
stackr admin version
```

## `stackr backup`

Volume backups

### `stackr backup create`

Schedule backups of a volume

```
stackr backup create <volume-id> [flags]
```

```
      --dest string       the destination id (default: local)
      --keep int          archives to keep (0: all) (default 7)
      --method string     the backup method (stackr backup methods lists them)
      --mode string       the method's mode
      --schedule string   a cron expression
      --tz string         the cron's timezone (default "UTC")
```

### `stackr backup ls`

List a volume's backup schedules

```
stackr backup ls <volume-id>
```

### `stackr backup methods`

List the ways a volume can be backed up

```
stackr backup methods <volume-id>
```

### `stackr backup restore`

Restore a backup run over a volume

```
stackr backup restore <volume-id> [flags]
```

```
      --no-wait         print the job and return; it runs server-side
      --run string      the backup run to restore (required)
      --target string   restore into this volume instead (default: the source)
```

### `stackr backup rm`

Delete a backup schedule

```
stackr backup rm <schedule-id>
```

### `stackr backup run`

Back a volume up now

```
stackr backup run <volume-id> [flags]
```

```
      --dest string     the destination id (default: local)
      --method string   the backup method
      --mode string     the method's mode
      --no-wait         print the job and return; it runs server-side
```

### `stackr backup runs`

List a volume's backup runs

```
stackr backup runs <volume-id>
```

### `stackr backup set`

Change a backup schedule; only the flags given change

```
stackr backup set <volume-id> <schedule-id> [flags]
```

```
      --dest string       the destination id (default: local)
      --keep int          archives to keep (0: all) (default 7)
      --method string     the backup method (stackr backup methods lists them)
      --mode string       the method's mode
      --schedule string   a cron expression
      --tz string         the cron's timezone (default "UTC")
```

## `stackr cluster`

The cluster at a glance, and its root of trust

### `stackr cluster break-glass`

Wipe the root and every WireGuard key; each node must rejoin (server admin)

```
stackr cluster break-glass
```

### `stackr cluster revoke-controller-cert`

Give the controller a new certificate; workers stop accepting the old one (server admin)

```
stackr cluster revoke-controller-cert
```

### `stackr cluster rotate-root`

Replace the cluster root; every node gets a new certificate, nothing stops. --finish ends a stuck rollover (server admin)

```
stackr cluster rotate-root [flags]
```

```
      --finish   end a running rollover now instead of waiting for offline nodes
```

### `stackr cluster status`

The whole cluster, grouped by network; exits 1 when anything is down

```
stackr cluster status
```

Example:

```
  stackr cluster status
  stackr cluster status --json
```

### `stackr cluster trust`

The cluster root, any rollover and the nodes not rejoined (server admin)

```
stackr cluster trust
```

## `stackr db`

Postgres instances

### `stackr db promote`

Make a standby the primary when the primary is down

```
stackr db promote [flags]
```

```
      --allow-async-target   promote a standby the primary did not wait for, although one it did wait for is usable
      --env string           the environment (default: the linked one)
      --node-gone            confirm the old primary's node is really gone (needed when it does not answer)
      --powered-off          promote without a recorded lease on the old primary: you powered its node off
      --stack string         the stack (default: the linked one)
      --tile string          the tile (default: the linked one)
      --to string            the node of the standby to promote (name or slug)
```

### `stackr db rollback-upgrade`

Return to the postgres major before the last change, with the data it had then; what was written since is lost

```
stackr db rollback-upgrade [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr db settings`

A postgres instance's settings

#### `stackr db settings get`

Show a postgres instance's settings and where each value comes from

```
stackr db settings get [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr db settings set`

Set settings; a reload applies them, or the instance restarts when one needs it

```
stackr db settings set <name=value>... [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr db settings unset`

Remove your override of settings, back to the computed or postgres default

```
stackr db settings unset <name>... [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr db standby`

A postgres instance's standbys

#### `stackr db standby add`

Add a standby on a node; it copies the primary, then streams

```
stackr db standby add [flags]
```

```
      --env string     the environment (default: the linked one)
      --node string    the node the standby runs on (name or slug)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr db standby fence`

Fence a standby: stop it, take it out of the read pool and keep it down

```
stackr db standby fence <node|id> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr db standby list`

List an instance's members: the primary and its standbys, with lag and slot size

```
stackr db standby list [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr db standby rebuild`

Make a standby again on its node (one that needs a rebuild)

```
stackr db standby rebuild <node|id> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr db standby remove`

Remove a standby: its container, volume and slot

```
stackr db standby remove <node|id> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr db standby sync`

Set whether the primary waits for a standby: auto (while it is close), always or never

```
stackr db standby sync <node|id> <auto|always|never> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr db standby unfence`

Unfence a standby: it starts again and rejoins the read pool once it streams

```
stackr db standby unfence <node|id> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr db switchover`

Move the primary to a standby while both run: the primary shuts down cleanly and the standby is promoted

```
stackr db switchover [flags]
```

```
      --abort          give up a switchover stuck before the promote: unfence and restart the old primary
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
      --to string      the standby's node (name or slug); default is the one that replayed the most
```

### `stackr db sync`

Synchronous replication of a postgres instance

#### `stackr db sync off`

Stop waiting for standbys

```
stackr db sync off [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr db sync on`

Make the primary wait for a standby before it confirms a commit

```
stackr db sync on [flags]
```

```
      --env string     the environment (default: the linked one)
      --limit-ms int   the round trip in ms under which an auto standby is waited for (default 10)
      --preferred      fall back to asynchronous when no standby answers (the default)
      --required       writes wait even when no standby answers (they freeze until one does)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr db sync status`

Show whether writes are protected by a standby now

```
stackr db sync status [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr db update`

Move an instance to the current minor of its postgres major; it restarts

```
stackr db update [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr db upgrade`

Move an instance to another postgres major (down too); the old data is kept for 7 days. --check only asks whether it can

```
stackr db upgrade [flags]
```

```
      --check          only check whether the data can move; change nothing
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
      --to int         the postgres major to move to
```

### `stackr db version`

Show the postgres version an instance runs and whether an update is out

```
stackr db version [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

## `stackr deploy`

Deploy the tile and follow it

```
stackr deploy [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

## `stackr dest`

Backup destinations

Flags for it and its subcommands:

```
      --server   the server-wide destinations (admin)
```

### `stackr dest add`

Add an S3 destination

```
stackr dest add <name> [flags]
```

```
      --access-key string   prefer the prompt or STACKR_BACKUP_ACCESS_KEY
      --bucket string       the bucket
      --endpoint string     the S3 endpoint URL
      --region string       the region
      --secret-key string   prefer the prompt or STACKR_BACKUP_SECRET_KEY
      --shared              every org may use it (server destinations)
```

### `stackr dest ls`

List destinations

```
stackr dest ls
```

### `stackr dest rm`

Remove a destination; archives in the bucket are kept

```
stackr dest rm <id> [flags]
```

```
      --force   a deleted org's destination: drop its records without touching the bucket (use when the bucket or credentials are gone)
```

### `stackr dest set`

Rename or share a destination (endpoint, bucket and keys stay: archives were written with them)

```
stackr dest set <id> [flags]
```

```
      --name string   a new name
      --shared        every org may use it
```

## `stackr domain`

Domain resources: the hosts tiles get names under

### `stackr domain add`

Add a host tiles get names under, at instance, org or stack level; one the owner already has is kept

```
stackr domain add [host] [flags]
```

```
      --acme-email string   the ACME account its certificates are issued on (default: the instance's)
      --host string         a bare hostname, e.g. example.com
      --include-env         keep the env label on the default env (api.prod.shop, not api.shop)
      --level string        instance (admin), org or stack (default: org, or stack for an org/stack owner)
      --owner string        the org's slug, or the stack as org/stack (default: the logged-in org)
```

### `stackr domain ls`

List domain resources: the org's and the instance's, or with no --org every one (admin)

```
stackr domain ls [flags]
```

```
      --org string   the org (its slug); without it, every resource on the server (admin)
```

### `stackr domain rename`

Move a domain resource to a new host: its auto hosts follow, and each old host redirects to its new one

```
stackr domain rename <id or host> <new host> [flags]
```

```
      --org string   the org (its slug); without it, every resource on the server (admin)
```

### `stackr domain rm`

Remove a domain resource; refused while tile domains carry its name

```
stackr domain rm <id or host> [flags]
```

```
      --org string   the org (its slug); without it, every resource on the server (admin)
```

## `stackr env`

Environments of the stack

### `stackr env color`

Set the environment's panel colour

```
stackr env color <color> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr env create`

Make an environment

```
stackr env create <name> [flags]
```

```
      --auto               deploy each new release on its own
      --base string        the env id an ephemeral env copies
      --branch string      the branch it builds
      --color string       the panel colour
      --env string         the environment (default: the linked one)
      --from-kind string   branch (builds a branch) or promote (takes releases by hand); default branch for the stack's first env, promote after
      --placement string   run it on this placement (default: the stack's or org's pin, else the default placement)
      --stack string       the stack (default: the linked one)
      --type string        static or ephemeral (default "static")
```

Example:

```
  stackr env create dev --stack shop --branch main
  stackr env create staging --stack shop
```

### `stackr env defaults`

Show or change the environment's settings defaults

```
stackr env defaults [flags]
```

```
      --clear stringArray   give a knob back to the level above
      --env string          the environment (default: the linked one)
      --set stringArray     knob=value (stackr knobs lists them)
      --stack string        the stack (default: the linked one)
```

### `stackr env from`

Set where the env's releases come from

```
stackr env from [flags]
```

```
      --auto               deploy each new release on its own
      --branch string      the branch it builds
      --env string         the environment (default: the linked one)
      --from-kind string   branch (builds a branch) or promote (takes releases by hand) (default "branch")
      --stack string       the stack (default: the linked one)
```

### `stackr env get`

Show the environment

```
stackr env get [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr env ladder`

List environments in promote order

```
stackr env ladder [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr env lock`

Lock the environment: other envs cannot read its params

```
stackr env lock [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr env lost`

List envs whose node was forgotten

```
stackr env lost
```

### `stackr env ls`

List environments

```
stackr env ls [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr env rename`

Rename the environment

```
stackr env rename <name> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr env reorder`

Set the promote order, bottom rung first

```
stackr env reorder <env>... [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr env restore`

Bring a placement's lost envs back from their backups on a live node (server admin)

```
stackr env restore [flags]
```

```
      --env string         the environment (default: the linked one)
      --no-wait            print the job and return; it runs server-side
      --placement string   the placement whose node was forgotten
      --stack string       the stack (default: the linked one)
      --start-empty        restore even though some volumes have no backup: they start empty
      --to string          the live node to restore onto
```

Example:

```
  stackr env restore --placement eu-db --to h5
  stackr env restore --placement eu-db --to h5 --start-empty
```

### `stackr env rm`

Delete the environment

```
stackr env rm [env] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr env sync`

Make --env match another env's setup (tiles, settings, params, volumes): prints the plan, then asks

```
stackr env sync [flags]
```

```
      --dry-run        print the plan and change nothing
      --env string     the environment (default: the linked one)
      --from string    the env of the stack to sync from (slug); required
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile strings   sync only these tiles (slugs, comma list)
```

Example:

```
  stackr env sync --from dev --stack shop --env staging
  stackr env sync --from dev --stack shop --env staging --tile api
```

### `stackr env traffic`

Show tile-to-tile traffic at the last 5 s sample

```
stackr env traffic [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr env unlock`

Unlock the environment: other envs may read its params

```
stackr env unlock [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

## `stackr exit`

The placements traffic leaves the cluster through

### `stackr exit ls`

The exit list in force for a scope

```
stackr exit ls [flags]
```

```
      --env string     the environment
      --org string     the org (default: the logged-in one)
      --stack string   the stack
      --tile string    the tile (needs --stack and --env, or a linked directory)
```

Example:

```
  stackr exit ls --stack shop --env prod
```

### `stackr exit set`

Set the ordered exit list of a scope: placements, or own for the env's own

```
stackr exit set [placement]... [flags]
```

```
      --clear          remove the list: the scope goes out through its own node
      --default        set the cluster-wide default exit to <node> (server admin; no node clears it)
      --env string     the environment
      --org string     the org (default: the logged-in one)
      --stack string   the stack
      --tile string    the tile (needs --stack and --env, or a linked directory)
```

Example:

```
  stackr exit set eu-prod home --stack shop --env prod
  stackr exit set --clear --stack shop --env prod
```

## `stackr forward`

Tunnel a local port to a tile's port

```
stackr forward [tile] [flags]
```

```
      --address string     local address to bind (default "127.0.0.1")
      --container string   the replica (default: a running one)
      --env string         the environment (default: the linked one)
      --port string        [local:]remote port (default: the tile's own)
      --stack string       the stack (default: the linked one)
      --tile string        the tile (default: the linked one)
```

## `stackr host-grant`

Elevated access: what a server admin approved for a stack

### `stackr host-grant approve`

Approve the elevated access the stack's parked jobs ask for (server admin); --only grants part of it. A host network tile can reach every env on its node and the mesh.

```
stackr host-grant approve [flags]
```

```
      --env string         the environment (default: the linked one)
      --only stringArray   grant only this line (repeatable); the rest keeps waiting
      --stack string       the stack (default: the linked one)
```

### `stackr host-grant revoke`

Revoke the stack's elevated access, or one tile's with --tile (server admin); running tiles stay, the next deploy that needs it parks

```
stackr host-grant revoke [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    revoke only this tile's lines
```

### `stackr host-grant show`

Show the stack's approved elevated access and what waits for approval

```
stackr host-grant show [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

## `stackr ingress`

The placement public traffic enters through

### `stackr ingress get`

Which placement public traffic enters through

```
stackr ingress get [flags]
```

```
      --default        show the cluster-wide default ingress and exit (server admin)
      --env string     the environment
      --org string     the org (default: the logged-in one)
      --stack string   the stack
      --tile string    the tile (needs --stack and --env, or a linked directory)
```

Example:

```
  stackr ingress get --stack shop --env prod
  stackr ingress get --default
```

### `stackr ingress set`

Pick the ingress placement of a scope

```
stackr ingress set [placement] [flags]
```

```
      --clear            remove the pick; the scope inherits
      --confirm string   with --default: the node's name, to skip the prompt
      --default          set the cluster-wide default ingress to <node> (server admin); shows where the hosts point first
      --env string       the environment
      --org string       the org (default: the logged-in one)
      --stack string     the stack
      --tile string      the tile (needs --stack and --env, or a linked directory)
```

Example:

```
  stackr ingress set eu-prod --tile admin --stack shop --env prod
  stackr ingress set h4 --default
```

## `stackr job`

Jobs: deploys, promotes, backups, everything queued

Flags for it and its subcommands:

```
      --admin   a job of any org (admin)
```

### `stackr job cancel`

Cancel a queued or running job

```
stackr job cancel <id>
```

### `stackr job get`

Show a job

```
stackr job get <id>
```

### `stackr job log`

Print a job's log; --follow waits for its end

```
stackr job log <id> [flags]
```

```
  -f, --follow   stream until the job ends
```

Example:

```
  stackr job log j1
  stackr job log j1 --follow
```

### `stackr job ls`

List the org's jobs, newest first

```
stackr job ls [flags]
```

```
      --env string      only this env's jobs (with --stack, or the linked stack)
      --limit int       at most this many (0: all) (default 20)
      --stack string    only this stack's jobs
      --state strings   only these states (queued, running, waiting, done, failed, ...)
```

## `stackr key`

Your API keys

### `stackr key add`

Make a key for this org; the token is shown once

```
stackr key add <name> [flags]
```

```
      --role string    viewer, member or owner: the most the key may do (default: your own rights)
      --stack string   limit the key to one stack, by slug (default: the whole org)
```

### `stackr key ls`

List your API keys

```
stackr key ls
```

### `stackr key rm`

Revoke a key

```
stackr key rm <name|id>
```

### `stackr key rotate`

Give a key a new secret; the old one stops working

```
stackr key rotate <name|id>
```

## `stackr knobs`

List the settings knobs every defaults level takes

```
stackr knobs
```

## `stackr link`

Bind this directory to a stack, env and tile

```
stackr link [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

## `stackr login`

Log in to a stackr server

```
stackr login <url> [flags]
```

Log in to a stackr server.

Without --with-key it opens the panel in your browser to approve the login;
set STACKR_NO_BROWSER to print the link only. --with-key (or STACKR_KEY, which
keeps it off argv) saves a key made in the panel instead. --profile saves the
login under a name, to keep several servers.

```
      --name string       the key's name, as the panel lists it (default "cli@<hostname>")
      --org string        the org to work in (its slug)
      --with-key string   an API key made in the panel, instead of the browser flow (or STACKR_KEY, which keeps it off argv)
```

Example:

```
  stackr login https://stackr.example.com
  stackr login https://stackr.example.com --with-key $KEY --org acme
```

## `stackr logout`

Forget the stored key; a browser login's key is revoked

```
stackr logout
```

## `stackr logs`

Print a replica's or a run's log; --follow streams it

### `stackr logs purge`

Delete every kept log line of an environment

```
stackr logs purge [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr logs search`

Search the kept log lines of an environment, or of one --tile, newest first (500 at most)

```
stackr logs search [text] [flags]
```

```
      --env string      the environment (default: the linked one)
      --kind string     only this tile kind (service, image, operated, cron, function)
      --run string      a cron or function run's id
      --since string    how far back, up to the log retention (like 15m, 6h, 24h, 7d) (default "1h")
      --source string   app, access, run or build
      --stack string    the stack (default: the linked one)
      --stream string   stdout or stderr
      --tile string     only this tile
```

Example:

```
  stackr logs search --env prod --tile api --source access 500
```

## `stackr metrics`

Show a tile's CPU, memory, network, disk IO and requests (a cron or function's runs) over --range

```
stackr metrics [tile] [flags]
```

```
      --range string   how far back, up to 24h, or the history retention when on (like 15m, 6h, 7d) (default "1h")
```

Example:

```
  stackr tile metrics api --stack shop --env prod --range 24h
```

## `stackr node`

The cluster's machines: the controller and its workers

### `stackr node add`

Make the join command for a new worker; it always gets egress (server admin)

```
stackr node add <name> [flags]
```

Make the join command for a new worker (server admin). The node always gets egress; add more tags with --tag.
Orgs reach it through a placement: stackr admin placement add <name> --node <node>.

```
      --address string    the node's own DNS name (default: <name>.<panel address>; empty for none)
      --tag stringArray   another capability: ingress, builder or storage (repeatable; egress is always on)
```

Example:

```
  stackr node add vps-fsn1 --tag ingress
  stackr node add vps-fsn1 --tag storage
```

### `stackr node backup`

Back up every volume on a node, one env at a time (server admin)

```
stackr node backup <name>
```

### `stackr node backups`

List a node's backups, newest first (server admin)

```
stackr node backups <name>
```

### `stackr node confirm`

Accept a worker that called back (server admin)

```
stackr node confirm <name>
```

### `stackr node exit`

Set the exits a node without egress sends its traffic out through, in order, up to three (server admin)

```
stackr node exit <name> [<exit>...]
```

Example:

```
  stackr node exit vps-home vps-fsn1 vps-hel1
```

### `stackr node forget`

Write off a dead worker for good (server admin)

```
stackr node forget <name>
```

### `stackr node link-metrics`

Show the tunnel between two nodes: ping, loss, traffic and handshake age over --range (server admin)

```
stackr node link-metrics <node> <peer> [flags]
```

```
      --range string   how far back, up to 24h, or the history retention when on (like 15m, 6h, 7d) (default "1h")
```

Example:

```
  stackr node link-metrics ct110 ct111 --range 6h
```

### `stackr node ls`

List nodes (server admin)

```
stackr node ls [flags]
```

```
      --pending        only nodes waiting for stackr node confirm
      --state string   only nodes in this state (online, pending, ...)
```

Example:

```
  stackr node ls
  stackr node ls --pending
```

### `stackr node metrics`

Show a node's CPU, memory and disk over --range (server admin)

```
stackr node metrics <name> [flags]
```

```
      --range string   how far back, up to 24h, or the history retention when on (like 15m, 6h, 7d) (default "1h")
```

Example:

```
  stackr node metrics ct110 --range 6h
```

### `stackr node reject`

Turn a worker that called back away (server admin)

```
stackr node reject <name>
```

### `stackr node rejoin`

New join command for a broken worker; it keeps its envs (server admin)

```
stackr node rejoin <name>
```

### `stackr node remove`

Take a healthy worker out, once no placement points at it (server admin)

```
stackr node remove <name>
```

Example:

```
  stackr node remove vps-fsn1
```

### `stackr node rotate`

Replace a node's mesh keys without a rejoin; --all for every node, --every to schedule (server admin)

```
stackr node rotate [<name>] [flags]
```

```
      --all         every node, one at a time
      --every int   rotate every N days (0 = off) (default -1)
```

Example:

```
  stackr node rotate h3
  stackr node rotate --all
  stackr node rotate --every 7
```

### `stackr node set`

Set a node's public IP or its own DNS name (server admin)

```
stackr node set <name> [flags]
```

```
      --address string     the node's own DNS name; empty for none
      --public-ip string   a floating IP or load balancer address; empty goes back to the detected one
```

Example:

```
  stackr node set vps-fsn1 --public-ip 203.0.113.10
  stackr node set vps-fsn1 --address fsn1.example.com
```

### `stackr node show`

Show one node: system, tags, certificate, envs

```
stackr node show <name>
```

### `stackr node tag`

Add capability tags (ingress, egress, builder, storage) (server admin)

```
stackr node tag <name> <tag>...
```

Example:

```
  stackr node tag vps-fsn1 egress
```

### `stackr node untag`

Remove capability tags (server admin)

```
stackr node untag <name> <tag>...
```

Example:

```
  stackr node untag vps-fsn1 egress
```

## `stackr operated`

Operated tiles

### `stackr operated create`

Make an operated tile (postgres, s3) in --env

```
stackr operated create <name> [flags]
```

```
      --branch string             git branch
      --build-args string         build args, a JSON object of strings: '{"ARG":"value"}'
      --command string            the command, split like a shell would (quotes work, nothing expands)
      --context string            build context
      --cpus float                CPU limit
      --depends-on string         tiles to start first, one per line: tile[:started|healthy|completed]
      --devices string            devices, one per line: host[:container[:perms]] (needs admin approval)
      --dockerfile string         Dockerfile path
      --engine string             postgres or s3
      --env string                the environment (default: the linked one)
      --env-json string           environment, a JSON object of strings: '{"KEY":"value"}'
      --files string              files from the repo to mount, one per line: repo/path:/container/path[:template]
      --from-dump string          load a pg_dump (custom or plain SQL) into a new slice tile; - reads stdin
      --git string                git URL (service tiles)
      --health-interval int       health check interval, seconds
      --health-path string        HTTP health path
      --health-retries int        health check retries
      --health-start-period int   health check start period, seconds
      --health-timeout int        health check timeout, seconds
      --healthcheck string        health check command, run in the container
      --host-network              use the server's network (needs admin approval)
      --image string              image ref (image tiles)
      --lan string                LAN access, one ip|cidr[:port] per line (needs admin approval)
      --major int                 postgres major, 14 to 18 (default: the newest still supported)
      --memory int                memory limit, MB
      --no-wait                   print the job and return; it runs server-side
      --port int                  container port
      --privileged                run privileged (needs admin approval)
      --protocol string           endpoint protocol
      --published-ports string    ports published on the host, one per line: host:container[/udp]
      --replicas int              replica count
      --restart string            restart policy
      --schedule string           cron expression, CRON_TZ= prefix allowed (cron tiles)
      --shared-net string         shared network
      --shm-size int              shm size, MB
      --slice string              the slice tile the dump loads into (default <name>-db)
      --stack string              the stack (default: the linked one)
      --tag-policy string         tag policy
      --tile string               the tile (default: the linked one)
      --timeout int               a run's timeout, minutes (cron and function tiles; 0 = 30)
      --trigger string            manual or on_deploy (function tiles)
      --update-policy string      update policy
      --user string               run as user
      --volumes string            mounts, one per line: volume:/path[:ro], host:/host/path:/path[:ro] (needs admin approval) or share:name/sub:/path[:ro]
      --watch-paths string        paths whose change triggers a build, one per line
```

### `stackr operated ls`

List the env's operated tiles

```
stackr operated ls [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

## `stackr org`

Orgs, members, invites, credentials, connectors, the org's config file

### `stackr org approve`

Apply a pending config plan: prints it, then asks

```
stackr org approve <plan> [flags]
```

```
      --no-wait              print the job and return; it runs server-side
      --remove stringArray   tick a removal row to apply it (repeatable; the plan lists the keys)
```

### `stackr org config-repo`

Show the org's config repo; --repo binds one and plans it, --unbind drops it

```
stackr org config-repo [flags]
```

```
      --auto               apply a plan with no blocker without an approve
      --branch string      the branch (empty: the repo's default)
      --connector string   the connector id (stackr org connectors ls)
      --path string        the file in the repo (empty: stackr-org.yml)
      --repo string        the repo to bind, owner/name
      --unbind             drop the binding (asks first; -y skips)
```

### `stackr org connectors`

Git connectors and the org's DNS connectors

#### `stackr org connectors domains`

List the account's domains: managed, not managed, or DNS elsewhere

```
stackr org connectors domains <connector> [flags]
```

```
      --org string   the org (default: the logged-in one)
```

#### `stackr org connectors ls`

List git connectors; --dns the org's DNS connectors (owner)

```
stackr org connectors ls [flags]
```

```
      --dns          list the org's DNS connectors
      --org string   the org (default: the logged-in one)
```

#### `stackr org connectors manage`

Let stackr manage a domain's DNS records

```
stackr org connectors manage <connector> <domain> [flags]
```

```
      --adopt          take over the existing records the plan lists as serving a tile (default true)
      --org string     the org (default: the logged-in one)
      --under string   manage only this name and below it, e.g. stackr (default: the whole domain)
```

#### `stackr org connectors plan`

Show the records managing a domain would write, and the existing ones stackr may adopt

```
stackr org connectors plan <connector> <domain> [flags]
```

```
      --org string     the org (default: the logged-in one)
      --server         the connector is a server one (admin)
      --under string   manage only this name and below it, e.g. stackr (default: the whole domain)
```

#### `stackr org connectors records`

Custom DNS records (TXT, MX, CNAME, A, AAAA)

#### `stackr org connectors records add`

Add a custom DNS record: TXT, MX ("10 mx.example.net"), CNAME, A or AAAA

```
stackr org connectors records add <connector> <domain> <type> <value> [flags]
```

```
      --name string   the name below, e.g. _dmarc (default: the name itself)
      --org string    the org (default: the logged-in one)
```

#### `stackr org connectors records ls`

List the custom DNS records and where stackr writes them

```
stackr org connectors records ls <connector> <domain> [flags]
```

```
      --org string   the org (default: the logged-in one)
```

#### `stackr org connectors records rm`

Remove a custom DNS record by id or value; stackr deletes it from the zone

```
stackr org connectors records rm <connector> <domain> <record> [flags]
```

```
      --org string   the org (default: the logged-in one)
```

#### `stackr org connectors rename`

Rename a connector

```
stackr org connectors rename <id> <name>
```

#### `stackr org connectors rescope`

Change what part of a managed domain stackr manages; shows the plan, then asks

```
stackr org connectors rescope <connector> <domain> [flags]
```

```
      --adopt          take over the existing records the plan lists as serving a tile (default true)
      --org string     the org (default: the logged-in one)
      --under string   manage only this name and below it, e.g. stackr (default: the whole domain)
```

#### `stackr org connectors rm`

Remove a connector; --dns a DNS one (owner)

```
stackr org connectors rm <id> [flags]
```

```
      --dns          the connector is a DNS one
      --org string   the org (default: the logged-in one)
```

#### `stackr org connectors takeover`

Switch a domain to its provider's DNS; records at its current DNS host stop applying

```
stackr org connectors takeover <connector> <domain> [flags]
```

```
      --confirm string   the domain, typed out (-y does not skip this)
      --org string       the org (default: the logged-in one)
```

#### `stackr org connectors unmanage`

Stop managing a domain; stackr deletes the records it made, other records stay

```
stackr org connectors unmanage <connector> <domain> [flags]
```

```
      --org string   the org (default: the logged-in one)
```

### `stackr org create`

Make an org and switch to it

```
stackr org create <name>
```

### `stackr org creds`

Registry credentials

#### `stackr org creds add`

Add a registry credential

```
stackr org creds add <name> [flags]
```

```
      --password string   the password (prefer the prompt or STACKR_REGISTRY_PASSWORD)
      --url string        the registry host, e.g. ghcr.io
      --username string   the registry user
```

#### `stackr org creds ls`

List registry credentials

```
stackr org creds ls
```

#### `stackr org creds rm`

Delete a registry credential

```
stackr org creds rm <id>
```

#### `stackr org creds set`

Change a registry credential; unset flags keep their value

```
stackr org creds set <id> [flags]
```

```
      --name string       a new name
      --password string   the password (prefer the prompt or STACKR_REGISTRY_PASSWORD)
      --url string        the registry host, e.g. ghcr.io
      --username string   the registry user
```

### `stackr org domains`

List the managed domains, or one domain's records stackr wrote

```
stackr org domains [domain] [flags]
```

```
      --org string   the org (default: the logged-in one)
```

### `stackr org export`

Write the org as a stackr-org.yml (stdout, or -o FILE)

```
stackr org export [flags]
```

```
      --force           overwrite an existing -o file
  -o, --output string   write here instead of stdout
```

### `stackr org get`

Show this org

```
stackr org get
```

### `stackr org invites`

Pending invites

#### `stackr org invites add`

Invite someone; the link is a credential

```
stackr org invites add [flags]
```

```
      --email string   who it is for (required)
      --role string    viewer, member or owner: viewer sees, member deploys and edits stacks, owner also manages people and settings (default "member")
```

#### `stackr org invites ls`

List pending invites; --all adds used and expired ones

```
stackr org invites ls [flags]
```

```
      --all   add used and expired invites
```

#### `stackr org invites rm`

Revoke an invite; its link stops working

```
stackr org invites rm <id>
```

### `stackr org ls`

List your orgs

```
stackr org ls
```

### `stackr org members`

Org members

#### `stackr org members ls`

List members

```
stackr org members ls
```

#### `stackr org members rm`

Remove a member

```
stackr org members rm <user-id>
```

#### `stackr org members set`

Change a member's role

```
stackr org members set <user-id> [flags]
```

```
      --role string   viewer, member or owner
```

### `stackr org plan`

Plan the bound file at its branch's head now

```
stackr org plan
```

### `stackr org plan-show`

Read a config plan and its changes

```
stackr org plan-show <plan>
```

### `stackr org plans`

List the org's last config plans

```
stackr org plans
```

### `stackr org preview`

Diff a local stackr-org.yml against the org; nothing is stored

```
stackr org preview [flags]
```

```
      --detailed-exitcode   exit 0 no changes, 1 blocked or error, 2 changes
  -f, --file string         the file to preview (default "stackr-org.yml")
```

### `stackr org reject`

Close a pending config plan unapplied

```
stackr org reject <plan>
```

### `stackr org rename`

Rename this org

```
stackr org rename <name>
```

### `stackr org rm`

Delete this org

```
stackr org rm
```

### `stackr org settings`

Show or change the org's settings defaults

```
stackr org settings [flags]
```

```
      --clear stringArray   give a knob back to the level above
      --set stringArray     knob=value (stackr knobs lists them)
```

### `stackr org use`

Work in another org (the key must reach it)

```
stackr org use <slug>
```

## `stackr params`

The param store: org and env levels, a tier's (--tier), the PR blocks (--pr), and the server's own (--level server, admin only)

Flags for it and its subcommands:

```
      --level string   org, env or server (stack too, for volumes); default: the env given or linked, else the org
      --pr             the PR block: the stack's (--stack or linked), else with --level org the org's
      --tier string    an org tier's params
```

### `stackr params export`

Write every param, secrets included, as collection.NAME=value; the one command that puts secrets on disk

```
stackr params export [flags]
```

```
      --env string      the environment (default: the linked one)
      --force           overwrite an existing file
  -o, --output string   the file (0600; default stdout)
      --stack string    the stack (default: the linked one)
```

### `stackr params get`

List params at the level; secrets stay masked unless --reveal

```
stackr params get [collection.NAME] [flags]
```

```
      --env string     the environment (default: the linked one)
      --reveal         show secret values (needs write access; logged)
      --stack string   the stack (default: the linked one)
```

### `stackr params import`

Set a param per KEY=VALUE line of a .env file, in one collection; others at the level stay

```
stackr params import <file> [flags]
```

```
      --collection string   the collection every name lands in
      --dry-run             print the names that would be set, never the values
      --env string          the environment (default: the linked one)
      --scope string        org or env (same as --level)
      --secret              store the values as secrets
      --stack string        the stack (default: the linked one)
      --wait                follow the redeploys to their end; exit 1 if one fails
```

Example:

```
  stackr params import .env --scope env --stack shop --env dev --collection app --secret
```

### `stackr params merge`

Set every collection.NAME=value line of a file (-f, "-" stdin); others stay

```
stackr params merge [flags]
```

```
      --env string     the environment (default: the linked one)
  -f, --file string    the file ("-" for stdin) (default "-")
      --secret         store the values as secrets
      --stack string   the stack (default: the linked one)
      --wait           follow the redeploys to their end; exit 1 if one fails
```

### `stackr params rm`

Delete a param at the level

```
stackr params rm <collection.NAME> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --wait           follow the redeploys to their end; exit 1 if one fails
```

### `stackr params set`

Set params; others at the level stay

```
stackr params set <collection.NAME[=value]>... [flags]
```

```
      --env string     the environment (default: the linked one)
      --generate       set each NAME to a fresh random secret (no =value)
      --length int     length of a generated secret, 16 to 128 (default 32)
      --secret         store the values as secrets
      --stack string   the stack (default: the linked one)
      --wait           follow the redeploys to their end; exit 1 if one fails
```

Example:

```
  stackr params set app.mode=prod --env dev
  stackr params set app.token=s3cret --secret --env dev
```

## `stackr password`

Change your password

```
stackr password
```

Change your password.

Each password comes from its environment variable, else a masked prompt, else
one line of stdin. The current password comes first, then the new one:
  STACKR_PASSWORD      the current password
  STACKR_NEW_PASSWORD  the new password

## `stackr pin`

Pin an org, stack or env to a placement: where its envs run

```
stackr pin [placement] [flags]
```

Pin an org, stack or env to a placement; stackr placements lists the ones open to the org.
Pinning changes where existing envs run: each one on another node moves, after its plan is shown and --yes.

```
      --clear          remove the pin
      --env string     the environment
      --no-wait        print the job and return; it runs server-side
      --org string     the org (default: the logged-in one)
      --stack string   the stack
```

Example:

```
  stackr pin eu-prod --org acme
  stackr pin eu-prod --stack shop
  stackr pin eu-db --stack shop --env prod --yes
  stackr pin --clear --stack shop
```

## `stackr placements`

List the placements open to the org

```
stackr placements [flags]
```

```
      --org string   the org (default: the logged-in one)
```

Example:

```
  stackr placements
  stackr placements --org acme
```

## `stackr profile`

Saved logins, one per server

### `stackr profile ls`

List the saved logins

```
stackr profile ls
```

### `stackr profile use`

Make a saved login the current one

```
stackr profile use <name>
```

## `stackr promote`

Promote a release (number or id) into --env: prints the plan, then asks

```
stackr promote <release> [flags]
```

```
      --dry-run              print the plan and change nothing
      --env string           the environment (default: the linked one)
      --no-wait              print the job and return; it runs server-side
      --remove stringArray   tick a removal row to apply it (repeatable; the plan lists the keys)
      --stack string         the stack (default: the linked one)
```

Example:

```
  stackr promote 7 --stack shop --env prod
  stackr promote 7 --stack shop --env prod --dry-run
```

## `stackr registry`

The stackr image registry

### `stackr registry login`

Let docker push and pull use the stackr registry

```
stackr registry login [flags]
```

```
      --registry string   the registry host (default: registry. plus the server's host)
```

Example:

```
  stackr registry login
  docker push registry.example.com/acme/shop/web:1.4
```

### `stackr registry ls`

List the org's repositories, or one repo's versions

```
stackr registry ls [repo]
```

Example:

```
  stackr registry ls
  stackr registry ls acme/shop/api
```

### `stackr registry rm`

Delete a version and every other tag on its digest

```
stackr registry rm <repo>:<tag> [flags]
```

```
      --force   delete even when an env runs it
```

Example:

```
  stackr registry rm acme/shop/api:v1
```

## `stackr release`

Releases of the stack

### `stackr release get`

Show a release and its pins

```
stackr release get <number> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr release ls`

List releases, newest first

```
stackr release ls [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

## `stackr restart`

Restart the tile's containers

```
stackr restart [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

## `stackr rollback`

Put --env back on an earlier release: prints the plan, then asks

```
stackr rollback [flags]
```

```
      --dry-run        print the plan and change nothing
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tag string     the release to go back to (number or id); required
```

## `stackr server`

The server's config file, stackr-server.yml (admin)

### `stackr server apply`

Plan a local stackr-server.yml (not the repo's) and apply it: prints the plan, then asks

```
stackr server apply [flags]
```

```
  -f, --file string          the file to apply (default "stackr-server.yml")
      --no-wait              print the job and return; it runs server-side
      --remove stringArray   tick a removal row to apply it (repeatable; the plan lists the keys)
```

### `stackr server approve`

Apply a pending server plan: prints it, then asks

```
stackr server approve <plan> [flags]
```

```
      --no-wait              print the job and return; it runs server-side
      --remove stringArray   tick a removal row to apply it (repeatable; the plan lists the keys)
```

### `stackr server bind`

Show the server's config repo; --repo binds one and plans it, --unbind drops it

```
stackr server bind [flags]
```

```
      --auto               apply a plan with no impact line and no removal row without an approve
      --branch string      the branch (empty: the repo's default)
      --connector string   the server connector's id
      --path string        the file in the repo (empty: stackr-server.yml)
      --repo string        the repo to bind, owner/name
      --unbind             drop the binding (asks first; -y skips)
```

### `stackr server connectors`

Git and DNS connectors the server owns (admin)

#### `stackr server connectors domains`

List the account's domains: managed, not managed, or DNS elsewhere

```
stackr server connectors domains <connector>
```

#### `stackr server connectors ls`

List the server's git connectors; --dns its DNS ones

```
stackr server connectors ls [flags]
```

```
      --dns   list the DNS connectors
```

#### `stackr server connectors manage`

Let stackr manage a domain's DNS records; --org limits who may use it (default: every organization)

```
stackr server connectors manage <connector> <domain> [flags]
```

```
      --adopt             take over the existing records the plan lists as serving a tile (default true)
      --org stringArray   available to this organization id only (repeatable)
      --under string      manage only this name and below it, e.g. stackr (default: the whole domain)
```

#### `stackr server connectors plan`

Show the records managing a domain would write, and the existing ones stackr may adopt

```
stackr server connectors plan <connector> <domain> [flags]
```

```
      --under string   manage only this name and below it, e.g. stackr (default: the whole domain)
```

#### `stackr server connectors records`

Custom DNS records (TXT, MX, CNAME, A, AAAA)

#### `stackr server connectors records add`

Add a custom DNS record: TXT, MX ("10 mx.example.net"), CNAME, A or AAAA

```
stackr server connectors records add <connector> <domain> <type> <value> [flags]
```

```
      --name string   the name below, e.g. _dmarc (default: the name itself)
```

#### `stackr server connectors records ls`

List the custom DNS records and where stackr writes them

```
stackr server connectors records ls <connector> <domain>
```

#### `stackr server connectors records rm`

Remove a custom DNS record by id or value; stackr deletes it from the zone

```
stackr server connectors records rm <connector> <domain> <record>
```

#### `stackr server connectors rename`

Rename a server connector

```
stackr server connectors rename <id> <name>
```

#### `stackr server connectors rescope`

Change what part of a managed domain stackr manages; shows the plan, then asks

```
stackr server connectors rescope <connector> <domain> [flags]
```

```
      --adopt          take over the existing records the plan lists as serving a tile (default true)
      --under string   manage only this name and below it, e.g. stackr (default: the whole domain)
```

#### `stackr server connectors rm`

Remove a server connector

```
stackr server connectors rm <id>
```

#### `stackr server connectors share`

Set who may use a server connector: --org ids, --all, or neither to stop sharing

```
stackr server connectors share <id> [flags]
```

```
      --all               share with every organization
      --org stringArray   share with this organization id (repeatable)
```

#### `stackr server connectors takeover`

Switch a domain to its provider's DNS; records at its current DNS host stop applying

```
stackr server connectors takeover <connector> <domain> [flags]
```

```
      --confirm string   the domain, typed out (-y does not skip this)
```

#### `stackr server connectors unmanage`

Stop managing a domain; stackr deletes the records it made, other records stay

```
stackr server connectors unmanage <connector> <domain>
```

### `stackr server domains`

List the managed domains, or one domain's records stackr wrote

```
stackr server domains [domain]
```

### `stackr server export`

Write the server as a stackr-server.yml (stdout, or -o FILE)

```
stackr server export [flags]
```

```
      --force           overwrite an existing -o file
  -o, --output string   write here instead of stdout
```

### `stackr server plan`

Plan the bound file at its branch's head now

```
stackr server plan
```

### `stackr server plan-show`

Read a server plan and its changes

```
stackr server plan-show <plan>
```

### `stackr server plans`

List the server's last config plans

```
stackr server plans
```

### `stackr server preview`

Diff a local stackr-server.yml against the server; nothing is stored

```
stackr server preview [flags]
```

```
      --detailed-exitcode   exit 0 no changes, 1 blocked or error, 2 changes
  -f, --file string         the file to preview (default "stackr-server.yml")
```

### `stackr server reject`

Close a pending server plan unapplied

```
stackr server reject <plan>
```

## `stackr share`

Network shares: NFS or SMB exports tiles mount

### `stackr share add`

Add a share

```
stackr share add <slug> [flags]
```

```
      --kind string       nfs or smb
      --options string    comma-separated mount options, e.g. nfsvers=4
      --password string   smb password as an ${{ org.params.<collection>.<name> }} ref, never the value
      --source string     nfs: host:/export, smb: //host/share
      --user string       smb user, plain or an ${{ org.params.<collection>.<name> }} ref
```

### `stackr share ls`

List the org's shares

```
stackr share ls
```

### `stackr share rm`

Remove a share; refused while a tile mounts it

```
stackr share rm <slug>
```

## `stackr slice`

Slice tiles: a database or bucket cut from an operated tile

### `stackr slice add`

Make a slice tile in --env, cut from --from at its deploy

```
stackr slice add <slug> [flags]
```

```
      --access string   the default access of its consumers, read or write (default write)
      --env string      the environment (default: the linked one)
      --from string     the operated tile, <stack>:<env>:<tile>
      --stack string    the stack (default: the linked one)
      --tile string     the tile (default: the linked one)
```

### `stackr slice bindings`

List the tiles holding a cred on the slice

```
stackr slice bindings [slice] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr slice branch`

Copy a postgres slice as of now, or --at a past time, into a new slice tile <slice>-br-xxxx

```
stackr slice branch <slice> [flags]
```

```
      --at string         copy it as of this time: RFC3339 with an offset, or latest (default: now; needs point-in-time recovery on)
      --env string        the environment (default: the linked one)
      --expires string    delete the branch after this long, e.g. 3d or 36h (default: never)
      --instance string   the postgres tile it lives on: <stack>:<env>:<tile>, or a tile of --to-env (default: the source's)
      --no-wait           print the job and return; it runs server-side
      --stack string      the stack (default: the linked one)
      --tile string       the tile (default: the linked one)
      --to-env string     the env the branch goes to: an env of the stack, or <stack>:<env> (default: the source's)
```

### `stackr slice bucket`

Set a bucket slice's version history, size cap and CORS origins

```
stackr slice bucket <slice> [flags]
```

```
      --cors strings       origins CORS allows, e.g. https://app.example.com; "" clears them
      --env string         the environment (default: the linked one)
      --quota-gb int       hard size cap in GB; 0 = none
      --stack string       the stack (default: the linked one)
      --tile string        the tile (default: the linked one)
      --version-days int   days old versions are kept; 0 turns versioning off (default 7)
```

### `stackr slice forward`

Tunnel a local port to the slice's postgres and print its connection string

```
stackr slice forward <slice> [flags]
```

```
      --env string     the environment (default: the linked one)
  -p, --port int       the local port (default: a free one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr slice get`

Show a slice tile and the instance it resolves to

```
stackr slice get [slice] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr slice on-remove`

Keep or drop the data when the slice tile is removed

```
stackr slice on-remove <slice> <keep|drop> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr slice restore`

Copy a bucket's objects back as they were at --at, as new versions. Never deletes. Runs file by file, not atomically

```
stackr slice restore <slice> [flags]
```

```
      --at string       restore to how the objects were at this time (RFC3339 with an offset)
      --env string      the environment (default: the linked one)
      --key string      only this one object
      --no-wait         print the job and return; it runs server-side
      --prefix string   only keys under this prefix (default: the whole bucket)
      --stack string    the stack (default: the linked one)
      --tile string     the tile (default: the linked one)
```

### `stackr slice url`

Choose whether a postgres slice's url, host and port point at the instance or its pooler

```
stackr slice url <slice> <direct|pooled> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr slice usage`

Show a bucket's current bytes and its old versions' bytes, apart

```
stackr slice usage [slice] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

## `stackr ssh`

Open a shell in a replica

```
stackr ssh [tile] [flags]
```

```
      --container string   the replica (default: a running one)
      --env string         the environment (default: the linked one)
      --shell string       the shell binary (default: bash, else sh)
      --stack string       the stack (default: the linked one)
      --tile string        the tile (default: the linked one)
```

## `stackr stack`

Stacks

### `stackr stack config-repo`

Show the stack's config repo; --repo binds one, --unbind drops it

```
stackr stack config-repo [flags]
```

```
      --branch string      the branch (default "main")
      --connector string   the connector id (stackr org connectors ls)
      --env string         the environment (default: the linked one)
      --path string        the stack file in the repo (default "stackr-compose.yml")
      --repo string        the git URL to bind
      --stack string       the stack (default: the linked one)
      --unbind             drop the binding (asks first; -y skips)
```

### `stackr stack create`

Make a stack

```
stackr stack create <name> [flags]
```

```
      --desc string   a one-line description
```

Example:

```
  stackr stack create shop --desc "The web shop"
```

### `stackr stack defaults`

Show or change the stack's settings defaults

```
stackr stack defaults [flags]
```

```
      --clear stringArray   give a knob back to the level above
      --env string          the environment (default: the linked one)
      --set stringArray     knob=value (stackr knobs lists them)
      --stack string        the stack (default: the linked one)
```

### `stackr stack export`

Write the stack as a stackr-compose.yml (stdout, or -o FILE); --env limits it to one env

```
stackr stack export [flags]
```

```
      --env string      the environment (default: the linked one)
      --force           overwrite an existing -o file
  -o, --output string   write here instead of stdout
      --stack string    the stack (default: the linked one)
```

### `stackr stack get`

Show the stack

```
stackr stack get [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr stack image-check`

Check the stack's images for a newer tag; a moved tag writes a release

```
stackr stack image-check [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
```

### `stackr stack ls`

List stacks

```
stackr stack ls
```

### `stackr stack rename`

Rename the stack

```
stackr stack rename <name> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr stack rm`

Delete the stack; --cascade removes its tiles and environments too

```
stackr stack rm [stack] [flags]
```

```
      --cascade        remove the stack's tiles and environments first
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
```

## `stackr status`

Show the server, the user and this directory's link

```
stackr status
```

## `stackr tier`

The org's env tiers, bottom first

### `stackr tier add`

Add a locked tier on top of the ladder

```
stackr tier add <slug>
```

### `stackr tier lock`

Lock a tier: other envs cannot read its org params

```
stackr tier lock <slug>
```

### `stackr tier ls`

List tiers

```
stackr tier ls
```

### `stackr tier order`

Set the ladder, bottom first: every tier once

```
stackr tier order <slug>...
```

### `stackr tier rename`

Rename a tier; stack envs keep their slugs and leave it

```
stackr tier rename <slug> <new>
```

### `stackr tier rm`

Delete a tier no stack env is in

```
stackr tier rm <slug>
```

### `stackr tier unlock`

Unlock a tier

```
stackr tier unlock <slug>
```

## `stackr tile`

Tiles: the deployed units

### `stackr tile access`

Set the tile's access to a slice tile of its env; a bound tile is re-granted in place

```
stackr tile access [tile] [flags]
```

```
      --access string   read or write
      --env string      the environment (default: the linked one)
      --rm              drop the entry: the slice's default access applies
      --slice string    the slice tile's slug
      --stack string    the stack (default: the linked one)
      --tile string     the tile (default: the linked one)
```

### `stackr tile allow`

Edit who outside the env may cut slices from an operated tile (org:stack:env:tile patterns)

```
stackr tile allow [tile] [flags]
```

```
      --add stringArray   add a pattern, e.g. acme:shop:*
      --env string        the environment (default: the linked one)
      --rm stringArray    remove a pattern
      --set stringArray   replace the list (--set '' empties it)
      --stack string      the stack (default: the linked one)
      --tile string       the tile (default: the linked one)
```

### `stackr tile backups`

Show a postgres instance's point-in-time recovery health

```
stackr tile backups [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile create`

Make a tile in --env

```
stackr tile create <name> [flags]
```

```
      --branch string             git branch
      --build-args string         build args, a JSON object of strings: '{"ARG":"value"}'
      --command string            the command, split like a shell would (quotes work, nothing expands)
      --context string            build context
      --cpus float                CPU limit
      --depends-on string         tiles to start first, one per line: tile[:started|healthy|completed]
      --devices string            devices, one per line: host[:container[:perms]] (needs admin approval)
      --dockerfile string         Dockerfile path
      --env string                the environment (default: the linked one)
      --env-json string           environment, a JSON object of strings: '{"KEY":"value"}'
      --files string              files from the repo to mount, one per line: repo/path:/container/path[:template]
      --git string                git URL (service tiles)
      --health-interval int       health check interval, seconds
      --health-path string        HTTP health path
      --health-retries int        health check retries
      --health-start-period int   health check start period, seconds
      --health-timeout int        health check timeout, seconds
      --healthcheck string        health check command, run in the container
      --host-network              use the server's network (needs admin approval)
      --image string              image ref (image tiles)
      --kind string               image, service (built from git), cron or function (default "image")
      --lan string                LAN access, one ip|cidr[:port] per line (needs admin approval)
      --memory int                memory limit, MB
      --port int                  container port
      --privileged                run privileged (needs admin approval)
      --protocol string           endpoint protocol
      --published-ports string    ports published on the host, one per line: host:container[/udp]
      --replicas int              replica count
      --restart string            restart policy
      --schedule string           cron expression, CRON_TZ= prefix allowed (cron tiles)
      --shared-net string         shared network
      --shm-size int              shm size, MB
      --stack string              the stack (default: the linked one)
      --tag-policy string         tag policy
      --tile string               the tile (default: the linked one)
      --timeout int               a run's timeout, minutes (cron and function tiles; 0 = 30)
      --trigger string            manual or on_deploy (function tiles)
      --update-policy string      update policy
      --user string               run as user
      --volumes string            mounts, one per line: volume:/path[:ro], host:/host/path:/path[:ro] (needs admin approval) or share:name/sub:/path[:ro]
      --watch-paths string        paths whose change triggers a build, one per line
```

Example:

```
  stackr tile create api --stack shop --env dev --image nginx:1.27
```

### `stackr tile deploy`

Deploy the tile and follow it

```
stackr tile deploy [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile domain`

The tile's domains

#### `stackr tile domain add`

Route a host to the tile; --auto names one under the nearest domain resource, --domain one in a managed domain

```
stackr tile domain add [host] [flags]
```

```
      --auto                 take the generated name under the nearest domain resource, like the panel's Add auto domain
      --domain string        a managed domain (stackr tile domain managed lists them); stackr sets its DNS
      --env string           the environment (default: the linked one)
      --force-https          redirect HTTP to HTTPS (default true)
      --https                serve HTTPS (default true)
      --path string          a path prefix
      --port int             the container port (0: the tile's)
      --redirect-to string   redirect every request here
      --replace              take over a DNS record stackr did not create on the name
      --stack string         the stack (default: the linked one)
      --sub string           the subdomain in --domain (none: the domain itself)
      --tile string          the tile (default: the linked one)
```

#### `stackr tile domain caddy`

Set a domain's raw Caddy config (admin); -f - reads stdin, empty clears

```
stackr tile domain caddy <host> [flags]
```

```
      --env string     the environment (default: the linked one)
  -f, --file string    the Caddy JSON ("-" for stdin; empty clears)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr tile domain ls`

List the tile's domains

```
stackr tile domain ls [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr tile domain managed`

List the managed domains the tile's domains may sit in

```
stackr tile domain managed [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr tile domain records`

Custom DNS records (TXT, MX, CNAME, A, AAAA)

#### `stackr tile domain records add`

Add a custom DNS record: TXT, MX ("10 mx.example.net"), CNAME, A or AAAA

```
stackr tile domain records add <host> <type> <value> [flags]
```

```
      --env string     the environment (default: the linked one)
      --name string    the name below, e.g. _dmarc (default: the name itself)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr tile domain records ls`

List the custom DNS records and where stackr writes them

```
stackr tile domain records ls <host> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr tile domain records rm`

Remove a custom DNS record by id or value; stackr deletes it from the zone

```
stackr tile domain records rm <host> <record> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr tile domain rm`

Stop routing a host to the tile

```
stackr tile domain rm <host> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stackr tile domain set`

Change a domain; only the flags given change

```
stackr tile domain set <host> [flags]
```

```
      --env string           the environment (default: the linked one)
      --force-https          redirect HTTP to HTTPS (default true)
      --host string          move the domain to this host
      --https                serve HTTPS (default true)
      --path string          a path prefix
      --port int             the container port (0: the tile's)
      --redirect-to string   redirect every request here
      --replace              take over a DNS record stackr did not create on the new host's name
      --stack string         the stack (default: the linked one)
      --tile string          the tile (default: the linked one)
```

### `stackr tile env-pairs`

Replace an operated tile's env pairs (a consumer's env name to one of this stack's envs); prints the diff, then asks

```
stackr tile env-pairs <tile> [consumer-env=env ...] [flags]
```

```
      --clear          empty the map
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile exec`

Run a command in a replica; piped stdin goes to it (none from a terminal; stackr ssh opens a shell)

```
stackr tile exec [tile] -- <cmd>... [flags]
```

```
      --container string   the replica (default: a running one)
      --env string         the environment (default: the linked one)
      --stack string       the stack (default: the linked one)
      --tile string        the tile (default: the linked one)
```

### `stackr tile forward`

Tunnel a local port to a tile's port

```
stackr tile forward [tile] [flags]
```

```
      --address string     local address to bind (default "127.0.0.1")
      --container string   the replica (default: a running one)
      --env string         the environment (default: the linked one)
      --port string        [local:]remote port (default: the tile's own)
      --stack string       the stack (default: the linked one)
      --tile string        the tile (default: the linked one)
```

### `stackr tile get`

Show a tile

```
stackr tile get [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile image-check`

Check the tile's image for a newer tag; a moved tag writes a release

```
stackr tile image-check [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile jobs`

List the tile's recent jobs

```
stackr tile jobs [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile logs`

Print a replica's or a run's log; --follow streams it

```
stackr tile logs [tile] [flags]
```

```
      --container string   the replica (default: a running one)
      --env string         the environment (default: the linked one)
  -f, --follow             keep streaming
      --run string         a cron or function run's log instead (stackr tile runs lists them)
      --stack string       the stack (default: the linked one)
      --tail int           lines of history (default 200)
      --tile string        the tile (default: the linked one)
```

### `stackr tile ls`

List the env's tiles

```
stackr tile ls [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile metrics`

Show a tile's CPU, memory, network, disk IO and requests (a cron or function's runs) over --range

```
stackr tile metrics [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --range string   how far back, up to 24h, or the history retention when on (like 15m, 6h, 7d) (default "1h")
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

Example:

```
  stackr tile metrics api --stack shop --env prod --range 24h
```

### `stackr tile pause`

Pause a cron tile's schedule

```
stackr tile pause [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile pitr`

Turn point-in-time recovery on (--days N) or off (--days off) for a postgres instance; restarts it once

```
stackr tile pitr [tile] [flags]
```

```
      --days string    the recovery window in days, or off
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile promote-restore`

Make a restored copy the instance it was restored from; the source's apps keep their address and credentials

```
stackr tile promote-restore [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile rebuild`

Build the running commit without the build cache and deploy it

```
stackr tile rebuild [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile rename`

Rename a tile

```
stackr tile rename <tile> <name> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile restart`

Restart the tile's containers

```
stackr tile restart [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile restore`

Restore a postgres instance to a time into a new instance <tile>-restored-YYYYMMDD-HHMM; the source is not touched

```
stackr tile restore [tile] [flags]
```

```
      --at string      the time: RFC3339 with an offset (2026-11-07T10:42:00+01:00, or Z), or latest
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

Example:

```
  stackr tile restore pg --at 2026-11-07T10:42:00Z
  stackr tile restore pg --at latest
```

### `stackr tile resume`

Resume a cron tile's schedule

```
stackr tile resume [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile rm`

Remove a tile

```
stackr tile rm [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile run`

Run a cron or function tile now and follow it

```
stackr tile run [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile runs`

List a cron or function tile's runs, newest first; --run shows one

```
stackr tile runs [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --run string     the run id
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile set`

Change tile settings; only the flags given are sent

```
stackr tile set [tile] [flags]
```

```
      --branch string             git branch
      --build-args string         build args, a JSON object of strings: '{"ARG":"value"}'
      --command string            the command, split like a shell would (quotes work, nothing expands)
      --context string            build context
      --cpus float                CPU limit
      --depends-on string         tiles to start first, one per line: tile[:started|healthy|completed]
      --devices string            devices, one per line: host[:container[:perms]] (needs admin approval)
      --dockerfile string         Dockerfile path
      --env string                the environment (default: the linked one)
      --env-json string           environment, a JSON object of strings: '{"KEY":"value"}'
      --files string              files from the repo to mount, one per line: repo/path:/container/path[:template]
      --git string                git URL (service tiles)
      --health-interval int       health check interval, seconds
      --health-path string        HTTP health path
      --health-retries int        health check retries
      --health-start-period int   health check start period, seconds
      --health-timeout int        health check timeout, seconds
      --healthcheck string        health check command, run in the container
      --host-network              use the server's network (needs admin approval)
      --image string              image ref (image tiles)
      --lan string                LAN access, one ip|cidr[:port] per line (needs admin approval)
      --memory int                memory limit, MB
      --port int                  container port
      --privileged                run privileged (needs admin approval)
      --protocol string           endpoint protocol
      --published-ports string    ports published on the host, one per line: host:container[/udp]
      --replicas int              replica count
      --restart string            restart policy
      --schedule string           cron expression, CRON_TZ= prefix allowed (cron tiles)
      --shared-net string         shared network
      --shm-size int              shm size, MB
      --stack string              the stack (default: the linked one)
      --tag-policy string         tag policy
      --tile string               the tile (default: the linked one)
      --timeout int               a run's timeout, minutes (cron and function tiles; 0 = 30)
      --trigger string            manual or on_deploy (function tiles)
      --update-policy string      update policy
      --user string               run as user
      --volumes string            mounts, one per line: volume:/path[:ro], host:/host/path:/path[:ro] (needs admin approval) or share:name/sub:/path[:ro]
      --wait                      follow the redeploys to their end; exit 1 if one fails
      --watch-paths string        paths whose change triggers a build, one per line
```

Example:

```
  stackr tile set api --stack shop --env dev --image nginx:1.28
```

### `stackr tile ssh`

Open a shell in a replica

```
stackr tile ssh [tile] [flags]
```

```
      --container string   the replica (default: a running one)
      --env string         the environment (default: the linked one)
      --shell string       the shell binary (default: bash, else sh)
      --stack string       the stack (default: the linked one)
      --tile string        the tile (default: the linked one)
```

### `stackr tile start`

Start the tile's containers

```
stackr tile start [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile status`

Show the tile's state and replicas

```
stackr tile status [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stackr tile stop`

Stop the tile's containers; --run stops one run of a cron or function

```
stackr tile stop [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --run string     the run to stop
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

## `stackr unlink`

Drop this directory's link

```
stackr unlink
```

## `stackr volume`

Volumes

Flags for it and its subcommands:

```
      --level string   org, env or server (stack too, for volumes); default: the env given or linked, else the org
```

### `stackr volume add`

Declare a volume at the level

```
stackr volume add <slug> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr volume ls`

List volumes at the level

```
stackr volume ls [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stackr volume rm`

Delete a volume (its data is kept for the retention window, then backed up and removed)

```
stackr volume rm <volume> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

