# CLI reference

Generated from the command definitions: `go test ./cmd/stkr -run TestCLIReference -update`. `stkr <command> --help` prints the same text.

Install the CLI (needs Go): `go install github.com/FyrmForge/stackr/cmd/stkr@latest`, or pin a release with `@vX.Y.Z`. From a checkout, `make installcli` installs the current commit; it does not carry a release version. The CLI does not update itself: when the server runs another version it prints one line to stderr with the `go install` command that matches the server.

```
Drive a stackr server from the terminal.

Environment (each beats the config file; nothing here is written to disk):
  STACKR_KEY     API key, so CI needs no login
  STACKR_SERVER  server URL
  STACKR_ORG     org slug
  STACKR_PROFILE login to use (see stkr profile ls); --profile beats it
  STACKR_CONFIG  config file path
```

## Flags on every command

```
      --json             print JSON; errors go to stderr as {error, status, code}
      --profile string   the saved login to use (stkr profile ls); login makes it
      --utc              print times in UTC, not this machine's zone
  -y, --yes              skip confirmation prompts (never overrides a server refusal)
```

Exit codes: 0 fine, 1 failed, 2 wrong command line, 3 refused without `--yes` (no terminal, or `--json`).

## `stkr admin`

Server administration (stackr admins)

### `stkr admin admin`

Make a user a stackr admin, or not (--on=false)

```
stkr admin admin <user-id> [flags]
```

```
      --on   admin or not (default true)
```

### `stkr admin alerts`

Alert webhook: node offline, failed backups, expiring certs, low disk (admin)

#### `stkr admin alerts clear`

Turn alerts off: forget the URL and secret

```
stkr admin alerts clear
```

#### `stkr admin alerts set`

Set the alert webhook URL and its signing secret

```
stkr admin alerts set [flags]
```

```
      --no-secret      stop signing alerts
      --secret-stdin   read the HMAC signing secret from stdin
      --url string     where alerts are posted (http or https); empty keeps the current one
```

#### `stkr admin alerts show`

Show the alert webhook and its last delivery

```
stkr admin alerts show
```

#### `stkr admin alerts test`

Send a test alert now

```
stkr admin alerts test
```

### `stkr admin audit`

The security audit log (server admin)

```
stkr admin audit [flags]
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
  stkr admin audit --since 24h
  stkr admin audit --action 'cert.*' --outcome ok
```

### `stkr admin defaults`

Show or change the server's settings defaults

```
stkr admin defaults [flags]
```

```
      --clear stringArray   give a knob back to the built-in default
      --set stringArray     knob=value (stkr knobs lists them)
```

### `stkr admin dest-grant-approve`

Let an org's backup destination reach its private address (id or name)

```
stkr admin dest-grant-approve <dest>
```

### `stkr admin dest-grant-revoke`

Take back an org backup destination's access to its private address (id or name)

```
stkr admin dest-grant-revoke <dest>
```

### `stkr admin dest-grants`

List the org backup destinations on a private address and whether they wait for approval

```
stkr admin dest-grants
```

### `stkr admin disable`

Disable a user; their sessions and keys close

```
stkr admin disable <user-id>
```

### `stkr admin enable`

Enable a user; closed sessions and keys stay closed

```
stkr admin enable <user-id>
```

### `stkr admin host-grants`

List every stack's elevated access and what waits for approval

```
stkr admin host-grants
```

### `stkr admin image-check`

Check every image for a newer tag; a moved tag writes a release

```
stkr admin image-check [flags]
```

```
      --no-wait   print the job and return; it runs server-side
```

### `stkr admin images`

List the images stackr knows

```
stkr admin images
```

### `stkr admin key`

Make an admin key bound to no org; the token is shown once

```
stkr admin key <name>
```

### `stkr admin observe`

Show metrics history and logs: on or off, retention, data size and free disk

```
stkr admin observe
```

### `stkr admin orgs`

List every org

```
stkr admin orgs
```

### `stkr admin panel-backup`

Back up stackr's own database now

```
stkr admin panel-backup [flags]
```

```
      --dest string   a server destination's name, or local (default: the scheduled destination)
      --no-wait       print the job and return; it runs server-side
```

### `stkr admin panel-backups`

List backups of stackr's own database

```
stkr admin panel-backups
```

### `stkr admin password`

Set a user's password

```
stkr admin password <user-id>
```

### `stkr admin placement`

Placements: named places envs belong to, each on one node (server admin)

#### `stkr admin placement add`

Make a placement, on a node or none yet (server admin)

```
stkr admin placement add <name> [flags]
```

```
      --node string   the node it stands on (default: none yet)
      --open string   all, none (admin only), or the org slugs that may use it, separated by commas (default "all")
```

Example:

```
  stkr admin placement add eu-prod --node h4
  stkr admin placement add eu-db --node h3 --open acme,shop
  stkr admin placement add standby-h5 --node h5 --open none
```

#### `stkr admin placement ls`

List placements with their node (server admin)

```
stkr admin placement ls
```

#### `stkr admin placement move`

Repoint a placement to another node, moving its envs (server admin)

```
stkr admin placement move <name> <node> [flags]
```

```
      --no-wait       print the job and return; it runs server-side
      --start-empty   restore even though some volumes have no backup: they start empty
```

Example:

```
  stkr admin placement move eu-prod h5
  stkr admin placement move eu-prod h5 --yes
  stkr admin placement move lab h5 --start-empty --yes
```

#### `stkr admin placement open`

Open a placement to every org or to a list; envs already there stay (server admin)

```
stkr admin placement open <name> <all|none|org,org,...>
```

#### `stkr admin placement retry`

Move the envs a failed repoint left behind (server admin)

```
stkr admin placement retry <name> [flags]
```

```
      --no-wait   print the job and return; it runs server-side
```

#### `stkr admin placement rm`

Delete a placement no pin or env uses (server admin)

```
stkr admin placement rm <name>
```

### `stkr admin proxy-sync`

Push the routing table to Caddy again

```
stkr admin proxy-sync
```

### `stkr admin registry`

The image registry, server-wide

#### `stkr admin registry clean`

Delete the versions no env runs beyond the newest, then free the disk

```
stkr admin registry clean [flags]
```

```
      --no-wait   print the job and return; it runs server-side
```

#### `stkr admin registry ls`

Show what each org holds and how the last cleanup went

```
stkr admin registry ls
```

### `stkr admin route`

Routes: hosts that go to an address outside stackr (admin)

#### `stkr admin route add`

Add a route

```
stkr admin route add <host> [flags]
```

```
      --insecure      https only: do not verify the target's certificate
      --mode string   passthrough (raw TLS, by name), http or https (stackr terminates TLS)
      --to string     where it goes: host or host:port (443 by default, 80 for http)
```

#### `stkr admin route ls`

List routes

```
stkr admin route ls
```

#### `stkr admin route rm`

Remove a route

```
stkr admin route rm <host>
```

### `stkr admin setting`

Read or write one server setting

```
stkr admin setting <key> [value]
```

### `stkr admin upgrade`

Upgrade stackr to a release

```
stkr admin upgrade [flags]
```

```
      --no-wait      print the job and return; it runs server-side
      --tag string   the release tag (stkr admin upgrade-check names the newest)
```

### `stkr admin upgrade-check`

Is there a newer stackr?

```
stkr admin upgrade-check
```

### `stkr admin upgrade-workers`

Upgrade every worker to this server's version, one at a time

```
stkr admin upgrade-workers [flags]
```

```
      --no-wait   print the job and return; it runs server-side
```

### `stkr admin users`

List users

```
stkr admin users
```

### `stkr admin version`

Show the server's version

```
stkr admin version
```

## `stkr backup`

Volume backups

### `stkr backup create`

Schedule backups of a volume

```
stkr backup create <volume-id> [flags]
```

```
      --dest string       the destination id (default: local)
      --keep int          archives to keep (0: all) (default 7)
      --method string     the backup method (stkr backup methods lists them)
      --mode string       the method's mode
      --schedule string   a cron expression
      --tz string         the cron's timezone (default "UTC")
```

### `stkr backup ls`

List a volume's backup schedules

```
stkr backup ls <volume-id>
```

### `stkr backup methods`

List the ways a volume can be backed up

```
stkr backup methods <volume-id>
```

### `stkr backup restore`

Restore a backup run over a volume. Instances restore with `operated restore`, slices with `slice restore`

```
stkr backup restore <volume-id> [flags]
```

```
      --no-wait         print the job and return; it runs server-side
      --run string      the backup run to restore (required)
      --target string   restore into this volume instead (default: the source)
```

### `stkr backup rm`

Delete a backup schedule

```
stkr backup rm <schedule-id>
```

### `stkr backup run`

Back a volume up now

```
stkr backup run <volume-id> [flags]
```

```
      --dest string     the destination id (default: local)
      --method string   the backup method
      --mode string     the method's mode
      --no-wait         print the job and return; it runs server-side
```

### `stkr backup runs`

List a volume's backup runs

```
stkr backup runs <volume-id>
```

### `stkr backup set`

Change a backup schedule; only the flags given change

```
stkr backup set <volume-id> <schedule-id> [flags]
```

```
      --dest string       the destination id (default: local)
      --keep int          archives to keep (0: all) (default 7)
      --method string     the backup method (stkr backup methods lists them)
      --mode string       the method's mode
      --schedule string   a cron expression
      --tz string         the cron's timezone (default "UTC")
```

## `stkr cluster`

The cluster at a glance, and its root of trust

### `stkr cluster break-glass`

Wipe the root and every WireGuard key; each node must rejoin (server admin)

```
stkr cluster break-glass
```

### `stkr cluster revoke-controller-cert`

Give the controller a new certificate; workers stop accepting the old one (server admin)

```
stkr cluster revoke-controller-cert
```

### `stkr cluster rotate-root`

Replace the cluster root; every node gets a new certificate, nothing stops. --finish ends a stuck rollover (server admin)

```
stkr cluster rotate-root [flags]
```

```
      --finish   end a running rollover now instead of waiting for offline nodes
```

### `stkr cluster status`

The whole cluster, grouped by network; exits 1 when anything is down

```
stkr cluster status
```

Example:

```
  stkr cluster status
  stkr cluster status --json
```

### `stkr cluster trust`

The cluster root, any rollover and the nodes not rejoined (server admin)

```
stkr cluster trust
```

## `stkr deploy`

Deploy the tile and follow it

```
stkr deploy [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

## `stkr dest`

Backup destinations

Flags for it and its subcommands:

```
      --server   the server-wide destinations (admin)
```

### `stkr dest add`

Add an S3 destination

```
stkr dest add <name> [flags]
```

```
      --access-key string   prefer the prompt or STACKR_BACKUP_ACCESS_KEY
      --bucket string       the bucket
      --endpoint string     the S3 endpoint URL
      --region string       the region
      --secret-key string   prefer the prompt or STACKR_BACKUP_SECRET_KEY
      --shared              every org may use it (server destinations)
```

### `stkr dest ls`

List destinations

```
stkr dest ls
```

### `stkr dest rm`

Remove a destination; archives in the bucket are kept

```
stkr dest rm <id> [flags]
```

```
      --force   a deleted org's destination: drop its records without touching the bucket (use when the bucket or credentials are gone)
```

### `stkr dest set`

Rename or share a destination (endpoint, bucket and keys stay: archives were written with them)

```
stkr dest set <id> [flags]
```

```
      --name string   a new name
      --shared        every org may use it
```

## `stkr domain`

Domain resources: the hosts tiles get names under

### `stkr domain add`

Add a host tiles get names under, at instance, org or stack level; one the owner already has is kept

```
stkr domain add [host] [flags]
```

```
      --acme-email string   the ACME account its certificates are issued on (default: the instance's)
      --host string         a bare hostname, e.g. example.com
      --include-env         keep the env label on the default env (api.prod.shop, not api.shop)
      --level string        instance (admin), org or stack (default: org, or stack for an org/stack owner)
      --owner string        the org's slug, or the stack as org/stack (default: the logged-in org)
```

### `stkr domain ls`

List domain resources: the org's and the instance's, or with no --org every one (admin)

```
stkr domain ls [flags]
```

```
      --org string   the org (its slug); without it, every resource on the server (admin)
```

### `stkr domain rename`

Move a domain resource to a new host: its auto hosts follow, and each old host redirects to its new one

```
stkr domain rename <id or host> <new host> [flags]
```

```
      --org string   the org (its slug); without it, every resource on the server (admin)
```

### `stkr domain rm`

Remove a domain resource; refused while tile domains carry its name

```
stkr domain rm <id or host> [flags]
```

```
      --org string   the org (its slug); without it, every resource on the server (admin)
```

## `stkr env`

Environments of the stack

### `stkr env color`

Set the environment's panel colour

```
stkr env color <color> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr env create`

Make an environment

```
stkr env create <name> [flags]
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
  stkr env create dev --stack shop --branch main
  stkr env create staging --stack shop
```

### `stkr env defaults`

Show or change the environment's settings defaults

```
stkr env defaults [flags]
```

```
      --clear stringArray   give a knob back to the level above
      --env string          the environment (default: the linked one)
      --set stringArray     knob=value (stkr knobs lists them)
      --stack string        the stack (default: the linked one)
```

### `stkr env from`

Set where the env's releases come from

```
stkr env from [flags]
```

```
      --auto               deploy each new release on its own
      --branch string      the branch it builds
      --env string         the environment (default: the linked one)
      --from-kind string   branch (builds a branch) or promote (takes releases by hand) (default "branch")
      --stack string       the stack (default: the linked one)
```

### `stkr env get`

Show the environment

```
stkr env get [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr env ladder`

List environments in promote order

```
stkr env ladder [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr env lock`

Lock the environment: other envs cannot read its params

```
stkr env lock [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr env lost`

List envs whose node was forgotten

```
stkr env lost
```

### `stkr env ls`

List environments

```
stkr env ls [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr env rename`

Rename the environment

```
stkr env rename <name> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr env reorder`

Set the promote order, bottom rung first

```
stkr env reorder <env>... [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr env restore`

Bring a placement's lost envs back from their backups on a live node (server admin)

```
stkr env restore [flags]
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
  stkr env restore --placement eu-db --to h5
  stkr env restore --placement eu-db --to h5 --start-empty
```

### `stkr env rm`

Delete the environment

```
stkr env rm [env] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr env sync`

Make --env match another env's setup (tiles, settings, params, volumes): prints the plan, then asks

```
stkr env sync [flags]
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
  stkr env sync --from dev --stack shop --env staging
  stkr env sync --from dev --stack shop --env staging --tile api
```

### `stkr env traffic`

Show tile-to-tile traffic at the last 5 s sample

```
stkr env traffic [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr env unlock`

Unlock the environment: other envs may read its params

```
stkr env unlock [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

## `stkr exit`

The placements traffic leaves the cluster through

### `stkr exit ls`

The exit list in force for a scope

```
stkr exit ls [flags]
```

```
      --env string     the environment
      --org string     the org (default: the logged-in one)
      --stack string   the stack
      --tile string    the tile (needs --stack and --env, or a linked directory)
```

Example:

```
  stkr exit ls --stack shop --env prod
```

### `stkr exit set`

Set the ordered exit list of a scope: placements, or own for the env's own

```
stkr exit set [placement]... [flags]
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
  stkr exit set eu-prod home --stack shop --env prod
  stkr exit set --clear --stack shop --env prod
```

## `stkr forward`

Tunnel a local port to a tile's port

```
stkr forward [tile] [flags]
```

```
      --address string     local address to bind (default "127.0.0.1")
      --container string   the replica (default: a running one)
      --env string         the environment (default: the linked one)
      --port string        [local:]remote port (default: the tile's own)
      --stack string       the stack (default: the linked one)
      --tile string        the tile (default: the linked one)
```

## `stkr host-grant`

Elevated access: what a server admin approved for a stack

### `stkr host-grant approve`

Approve the elevated access the stack's parked jobs ask for (server admin); --only grants part of it. A host network tile can reach every env on its node and the mesh.

```
stkr host-grant approve [flags]
```

```
      --env string         the environment (default: the linked one)
      --only stringArray   grant only this line (repeatable); the rest keeps waiting
      --stack string       the stack (default: the linked one)
```

### `stkr host-grant revoke`

Revoke the stack's elevated access, or one tile's with --tile (server admin); running tiles stay, the next deploy that needs it parks

```
stkr host-grant revoke [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    revoke only this tile's lines
```

### `stkr host-grant show`

Show the stack's approved elevated access and what waits for approval

```
stkr host-grant show [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

## `stkr ingress`

The placement public traffic enters through

### `stkr ingress get`

Which placement public traffic enters through

```
stkr ingress get [flags]
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
  stkr ingress get --stack shop --env prod
  stkr ingress get --default
```

### `stkr ingress set`

Pick the ingress placement of a scope

```
stkr ingress set [placement] [flags]
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
  stkr ingress set eu-prod --tile admin --stack shop --env prod
  stkr ingress set h4 --default
```

## `stkr job`

Jobs: deploys, promotes, backups, everything queued

Flags for it and its subcommands:

```
      --admin   a job of any org (admin)
```

### `stkr job cancel`

Cancel a queued or running job

```
stkr job cancel <id>
```

### `stkr job get`

Show a job

```
stkr job get <id>
```

### `stkr job log`

Print a job's log; --follow waits for its end

```
stkr job log <id> [flags]
```

```
  -f, --follow   stream until the job ends
```

Example:

```
  stkr job log j1
  stkr job log j1 --follow
```

### `stkr job ls`

List the org's jobs, newest first

```
stkr job ls [flags]
```

```
      --env string      only this env's jobs (with --stack, or the linked stack)
      --limit int       at most this many (0: all) (default 20)
      --stack string    only this stack's jobs
      --state strings   only these states (queued, running, waiting, done, failed, ...)
```

## `stkr key`

Your API keys

### `stkr key add`

Make a key for this org; the token is shown once

```
stkr key add <name> [flags]
```

```
      --role string    viewer, member or owner: the most the key may do (default: your own rights)
      --stack string   limit the key to one stack, by slug (default: the whole org)
```

### `stkr key ls`

List your API keys

```
stkr key ls
```

### `stkr key rm`

Revoke a key

```
stkr key rm <name|id>
```

### `stkr key rotate`

Give a key a new secret; the old one stops working

```
stkr key rotate <name|id>
```

## `stkr knobs`

List the settings knobs every defaults level takes

```
stkr knobs
```

## `stkr link`

Bind this directory to a stack, env and tile

```
stkr link [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

## `stkr login`

Log in to a stackr server

```
stkr login <url> [flags]
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
  stkr login https://stackr.example.com
  stkr login https://stackr.example.com --with-key $KEY --org acme
```

## `stkr logout`

Forget the stored key; a browser login's key is revoked

```
stkr logout
```

## `stkr logs`

Print a replica's or a run's log; --follow streams it

### `stkr logs purge`

Delete every kept log line of an environment

```
stkr logs purge [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr logs search`

Search the kept log lines of an environment, or of one --tile, newest first (500 at most)

```
stkr logs search [text] [flags]
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
  stkr logs search --env prod --tile api --source access 500
```

## `stkr metrics`

Show a tile's CPU, memory, network, disk IO and requests (a cron or function's runs) over --range

```
stkr metrics [tile] [flags]
```

```
      --range string   how far back, up to 24h, or the history retention when on (like 15m, 6h, 7d) (default "1h")
```

Example:

```
  stkr tile metrics api --stack shop --env prod --range 24h
```

## `stkr node`

The cluster's machines: the controller and its workers

### `stkr node add`

Make the join command for a new worker; it gets egress and storage unless --no-storage (server admin)

```
stkr node add <name> [flags]
```

Make the join command for a new worker (server admin). The node gets egress and storage; add more tags with --tag, or leave storage off with --no-storage.
Orgs reach it through a placement: stkr admin placement add <name> --node <node>.

```
      --no-storage        do not give the node the storage tag
      --tag stringArray   another capability: ingress, builder or storage (repeatable; egress is always on, storage is on unless --no-storage)
```

Example:

```
  stkr node add vps-fsn1 --tag ingress
  stkr node add vps-edge --no-storage
```

### `stkr node backup`

Back up every volume on a node, one env at a time (server admin)

```
stkr node backup <name>
```

### `stkr node backups`

List a node's backups, newest first (server admin)

```
stkr node backups <name>
```

### `stkr node confirm`

Accept a worker that called back (server admin)

```
stkr node confirm <name>
```

### `stkr node disk`

A node's disk: the folder its volumes live on

#### `stkr node disk add`

Make a mounted folder on the node its disk; the folder is checked first (server admin)

```
stkr node disk add <node> <path> [flags]
```

```
      --no-wait   print the job and return; it runs server-side
```

Example:

```
  stkr node disk add vps-fsn1 /mnt/data
```

#### `stkr node disk adopt`

Put the disk of a forgotten node on this node, with the envs that lived there. No path lists the disks to adopt (server admin)

```
stkr node disk adopt <node> [<path>] [flags]
```

```
      --confirm string   the word that confirms (the node's name, or the volume's slug with --discard); without it the verb asks
      --discard string   remove this blocking plain copy (a volume slug) first; the folder stays
      --no-wait          print the job and return; it runs server-side
```

Example:

```
  stkr node disk adopt vps-new
  stkr node disk adopt vps-new /mnt/data
```

#### `stkr node disk check`

Check the node's disk again; the way out of missing (server admin)

```
stkr node disk check <node> [flags]
```

```
      --no-wait   print the job and return; it runs server-side
```

#### `stkr node disk forget`

Forget a disk that is missing, half added or left by a forgotten node; its envs are restored from backups (server admin)

```
stkr node disk forget <disk-id> [flags]
```

```
      --confirm string   the word that confirms (the node's name or the disk's id); without it the verb asks
      --no-wait          print the job and return; it runs server-side
```

Example:

```
  stkr node disk forget 3f2a9c1e
```

#### `stkr node disk ls`

Show a node's disk (server admin)

```
stkr node disk ls <node>
```

Example:

```
  stkr node disk ls vps-fsn1
```

#### `stkr node disk move-all`

Copy every volume of the node onto its disk, one env at a time, smallest first (server admin)

```
stkr node disk move-all <node> [flags]
```

```
      --no-wait   print the job and return; it runs server-side
```

Example:

```
  stkr node disk move-all vps-fsn1
```

### `stkr node exit`

Set the exits a node without egress sends its traffic out through, in order, up to three (server admin)

```
stkr node exit <name> [<exit>...]
```

Example:

```
  stkr node exit vps-home vps-fsn1 vps-hel1
```

### `stkr node forget`

Write off a dead worker for good (server admin)

```
stkr node forget <name>
```

### `stkr node link-metrics`

Show the tunnel between two nodes: ping, loss, traffic and handshake age over --range (server admin)

```
stkr node link-metrics <node> <peer> [flags]
```

```
      --range string   how far back, up to 24h, or the history retention when on (like 15m, 6h, 7d) (default "1h")
```

Example:

```
  stkr node link-metrics ct110 ct111 --range 6h
```

### `stkr node ls`

List nodes (server admin)

```
stkr node ls [flags]
```

```
      --pending        only nodes waiting for stkr node confirm
      --state string   only nodes in this state (online, pending, ...)
```

Example:

```
  stkr node ls
  stkr node ls --pending
```

### `stkr node metrics`

Show a node's CPU, memory and disk over --range (server admin)

```
stkr node metrics <name> [flags]
```

```
      --range string   how far back, up to 24h, or the history retention when on (like 15m, 6h, 7d) (default "1h")
```

Example:

```
  stkr node metrics ct110 --range 6h
```

### `stkr node reject`

Turn a worker that called back away (server admin)

```
stkr node reject <name>
```

### `stkr node rejoin`

New join command for a broken worker; it keeps its envs (server admin)

```
stkr node rejoin <name>
```

### `stkr node rm`

Take a healthy worker out, once no placement points at it (server admin)

```
stkr node rm <name>
```

Example:

```
  stkr node rm vps-fsn1
```

### `stkr node rotate`

Replace a node's mesh keys without a rejoin; --all for every node, --every to schedule (server admin)

```
stkr node rotate [<name>] [flags]
```

```
      --all         every node, one at a time
      --every int   rotate every N days (0 = off) (default -1)
```

Example:

```
  stkr node rotate h3
  stkr node rotate --all
  stkr node rotate --every 7
```

### `stkr node set`

Set a node's public IP (server admin)

```
stkr node set <name> [flags]
```

```
      --public-ip string   a floating IP or load balancer address; empty goes back to the detected one
```

Example:

```
  stkr node set vps-fsn1 --public-ip 203.0.113.10
```

### `stkr node show`

Show one node: system, tags, certificate, envs

```
stkr node show <name>
```

### `stkr node tag`

Add capability tags (ingress, egress, builder, storage) (server admin)

```
stkr node tag <name> <tag>...
```

Example:

```
  stkr node tag vps-fsn1 egress
```

### `stkr node untag`

Remove capability tags (server admin)

```
stkr node untag <name> <tag>...
```

Example:

```
  stkr node untag vps-fsn1 egress
```

## `stkr operated`

Operated tiles: postgres and s3 instances

### `stkr operated accept-restore`

(postgres) Make a restored copy the instance it was restored from; the source's apps keep their address and credentials

```
stkr operated accept-restore <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
```

### `stkr operated allow`

Edit who outside the env may cut slices from an operated tile (org:stack:env:tile patterns)

```
stkr operated allow <tile> [flags]
```

```
      --add stringArray   add a pattern, e.g. acme:shop:*
      --env string        the environment (default: the linked one)
      --rm stringArray    remove a pattern
      --set stringArray   replace the list (--set '' empties it)
      --stack string      the stack (default: the linked one)
```

### `stkr operated create`

Make an operated tile (postgres, s3) in --env

```
stkr operated create <name> [flags]
```

```
      --cpus float          CPU limit
      --devices string      devices, one per line: host[:container[:perms]] (needs admin approval)
      --engine string       postgres or s3
      --env string          the environment (default: the linked one)
      --env-json string     environment, a JSON object of strings: '{"KEY":"value"}'
      --from-dump string    load a pg_dump (custom or plain SQL) into a new slice tile; - reads stdin
      --host-network        use the server's network (needs admin approval)
      --image string        image ref (image tiles)
      --lan string          LAN access, one ip|cidr[:port] per line (needs admin approval)
      --major int           postgres major, 14 to 18 (default: the newest still supported)
      --memory int          memory limit, MB
      --no-wait             print the job and return; it runs server-side
      --port int            container port
      --privileged          run privileged (needs admin approval)
      --replicas int        replica count
      --restart string      restart policy
      --shared-net string   shared network
      --shm-size int        shm size, MB
      --slice string        the slice tile the dump loads into (default <name>-db)
      --stack string        the stack (default: the linked one)
      --user string         run as user
      --volumes string      mounts, one per line: volume:/path[:ro], host:/host/path:/path[:ro] (needs admin approval) or share:name/sub:/path[:ro]
```

### `stkr operated env-pairs`

Replace an operated tile's env pairs (a consumer's env name to one of this stack's envs); prints the diff, then asks

```
stkr operated env-pairs <tile> [consumer-env=env ...] [flags]
```

```
      --clear          empty the map
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr operated extensions`

(postgres) An instance's extensions

#### `stkr operated extensions apply`

(postgres) Apply a reviewed extension plan; --yes confirms a plan that restarts the instance

```
stkr operated extensions apply <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --plan string    the plan id that plan printed
      --stack string   the stack (default: the linked one)
```

#### `stkr operated extensions get`

(postgres) Show an instance's image, startup modules and extensions

```
stkr operated extensions get <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

#### `stkr operated extensions plan`

(postgres) Plan an extension change that needs a build; prints the plan and its id for apply

```
stkr operated extensions plan <tile> [name...] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
```

#### `stkr operated extensions set`

(postgres) Set an instance's extensions, startup modules or image; with no names and no flags the list is emptied

```
stkr operated extensions set <tile> [name...] [flags]
```

```
      --env string        the environment (default: the linked one)
      --image string      the image ref
      --no-wait           print the job and return; it runs server-side
      --preload strings   startup modules, comma separated; "" empties the list
      --stack string      the stack (default: the linked one)
```

### `stkr operated failover`

(postgres) Make a standby the primary when the primary is down

```
stkr operated failover <tile> [flags]
```

```
      --allow-async-target   promote a standby the primary did not wait for, although one it did wait for is usable
      --env string           the environment (default: the linked one)
      --no-wait              print the job and return; it runs server-side
      --node-gone            confirm the old primary's node is really gone (needed when it does not answer)
      --powered-off string   promote without a recorded lease on the old primary; type "I powered the node off" to say you powered its node off
      --stack string         the stack (default: the linked one)
      --to string            the node of the standby to promote (name or slug)
```

### `stkr operated ls`

List the env's operated tiles

```
stkr operated ls [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr operated pitr`

(postgres) Point-in-time recovery

#### `stkr operated pitr set`

(postgres) Turn point-in-time recovery on (--days N) or off (--days off); restarts it once

```
stkr operated pitr set <tile> [flags]
```

```
      --days string    the recovery window in days, or off
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
```

#### `stkr operated pitr status`

(postgres) Show point-in-time recovery health

```
stkr operated pitr status <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr operated restore`

(postgres) Restore an instance to a time into a new instance <tile>-restored-YYYYMMDD-HHMM; the source is not touched. Slices restore with `slice restore`, backups with `backup restore`

```
stkr operated restore <tile> [flags]
```

```
      --at string      the time: RFC3339 with an offset (2026-11-07T10:42:00+01:00, or Z), or latest
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
```

Example:

```
  stkr operated restore pg --at 2026-11-07T10:42:00Z
  stkr operated restore pg --at latest
```

### `stkr operated rollback-upgrade`

(postgres) Return to the postgres major before the last change, with the data it had then; what was written since is lost

```
stkr operated rollback-upgrade <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
```

### `stkr operated settings`

(postgres) An instance's settings

#### `stkr operated settings get`

Show an instance's settings and where each value comes from

```
stkr operated settings get <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

#### `stkr operated settings set`

Set settings; a reload applies them, or the instance restarts when one needs it

```
stkr operated settings set <tile> <name=value>... [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
```

#### `stkr operated settings unset`

Remove your override of settings, back to the computed or postgres default

```
stkr operated settings unset <tile> <name>... [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
```

### `stkr operated standby`

(postgres) An instance's standbys

#### `stkr operated standby add`

Add a standby on a node; it copies the primary, then streams

```
stkr operated standby add <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --node string    the node the standby runs on (name or slug)
      --stack string   the stack (default: the linked one)
```

#### `stkr operated standby fence`

Fence a standby: stop it, take it out of the read pool and keep it down

```
stkr operated standby fence <tile> <node|id> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

#### `stkr operated standby ls`

List an instance's members: the primary and its standbys, with lag and slot size

```
stkr operated standby ls <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

#### `stkr operated standby rebuild`

Make a standby again on its node (one that needs a rebuild)

```
stkr operated standby rebuild <tile> <node|id> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

#### `stkr operated standby rm`

Remove a standby: its container, volume and slot

```
stkr operated standby rm <tile> <node|id> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

#### `stkr operated standby sync`

Set whether the primary waits for a standby: auto (while it is close), always or never

```
stkr operated standby sync <tile> <node|id> <auto|always|never> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

#### `stkr operated standby unfence`

Unfence a standby: it starts again and rejoins the read pool once it streams

```
stkr operated standby unfence <tile> <node|id> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr operated status`

(postgres) Show instance health: connections, oldest transaction, standby lag, pooler. Not `tile status`, which shows containers

```
stkr operated status <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --top            also list the costliest statements
```

### `stkr operated switchover`

(postgres) Move the primary to a standby while both run: the primary shuts down cleanly and the standby is promoted

```
stkr operated switchover <tile> [flags]
```

```
      --abort          give up a switchover stuck before the promote: unfence and restart the old primary
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --to string      the standby's node (name or slug); default is the one that replayed the most
```

### `stkr operated sync`

(postgres) Synchronous replication of an instance

#### `stkr operated sync off`

Stop waiting for standbys

```
stkr operated sync off <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

#### `stkr operated sync on`

Make the primary wait for a standby before it confirms a commit

```
stkr operated sync on <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --limit-ms int   the round trip in ms under which an auto standby is waited for (default 10)
      --preferred      fall back to asynchronous when no standby answers (the default)
      --required       writes wait even when no standby answers (they freeze until one does)
      --stack string   the stack (default: the linked one)
```

#### `stkr operated sync status`

Show whether writes are protected by a standby now

```
stkr operated sync status <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr operated update`

(postgres) Move an instance to the current minor of its postgres major; it restarts

```
stkr operated update <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
```

### `stkr operated upgrade`

(postgres) Move an instance to another postgres major (down too); the old data is kept for 7 days. --check only asks whether it can

```
stkr operated upgrade <tile> [flags]
```

```
      --check          only check whether the data can move; change nothing
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --to int         the postgres major to move to
```

### `stkr operated version`

(postgres) Show the version an instance runs and whether an update is out

```
stkr operated version <tile> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

## `stkr org`

Orgs, members, invites, credentials, connectors, the org's config file

### `stkr org approve`

Apply a pending config plan: prints it, then asks

```
stkr org approve <plan> [flags]
```

```
      --no-wait              print the job and return; it runs server-side
      --remove stringArray   tick a removal row to apply it (repeatable; the plan lists the keys)
```

### `stkr org config-repo`

Show the org's config repo; --repo binds one and plans it, --unbind drops it

```
stkr org config-repo [flags]
```

```
      --auto               apply a plan with no blocker without an approve
      --branch string      the branch (empty: the repo's default)
      --connector string   the connector id (stkr org connectors ls)
      --path string        the file in the repo (empty: stackr-org.yml)
      --repo string        the repo to bind, owner/name
      --unbind             drop the binding (asks first; -y skips)
```

### `stkr org connectors`

Git connectors and the org's DNS connectors

#### `stkr org connectors domains`

List the account's domains: managed, not managed, or DNS elsewhere

```
stkr org connectors domains <connector> [flags]
```

```
      --org string   the org (default: the logged-in one)
```

#### `stkr org connectors ls`

List git connectors; --dns the org's DNS connectors (owner)

```
stkr org connectors ls [flags]
```

```
      --dns          list the org's DNS connectors
      --org string   the org (default: the logged-in one)
```

#### `stkr org connectors manage`

Let stackr manage a domain's DNS records

```
stkr org connectors manage <connector> <domain> [flags]
```

```
      --adopt          take over the existing records the plan lists as serving a tile (default true)
      --org string     the org (default: the logged-in one)
      --under string   manage only this name and below it, e.g. stackr (default: the whole domain)
```

#### `stkr org connectors plan`

Show the records managing a domain would write, and the existing ones stackr may adopt

```
stkr org connectors plan <connector> <domain> [flags]
```

```
      --org string     the org (default: the logged-in one)
      --server         the connector is a server one (admin)
      --under string   manage only this name and below it, e.g. stackr (default: the whole domain)
```

#### `stkr org connectors records`

Custom DNS records (TXT, MX, CNAME, A, AAAA)

#### `stkr org connectors records add`

Add a custom DNS record: TXT, MX ("10 mx.example.net"), CNAME, A or AAAA

```
stkr org connectors records add <connector> <domain> <type> <value> [flags]
```

```
      --name string   the name below, e.g. _dmarc (default: the name itself)
      --org string    the org (default: the logged-in one)
```

#### `stkr org connectors records ls`

List the custom DNS records and where stackr writes them

```
stkr org connectors records ls <connector> <domain> [flags]
```

```
      --org string   the org (default: the logged-in one)
```

#### `stkr org connectors records rm`

Remove a custom DNS record by id or value; stackr deletes it from the zone

```
stkr org connectors records rm <connector> <domain> <record> [flags]
```

```
      --org string   the org (default: the logged-in one)
```

#### `stkr org connectors rename`

Rename a connector

```
stkr org connectors rename <id> <name>
```

#### `stkr org connectors rescope`

Change what part of a managed domain stackr manages; shows the plan, then asks

```
stkr org connectors rescope <connector> <domain> [flags]
```

```
      --adopt          take over the existing records the plan lists as serving a tile (default true)
      --org string     the org (default: the logged-in one)
      --under string   manage only this name and below it, e.g. stackr (default: the whole domain)
```

#### `stkr org connectors rm`

Remove a connector; --dns a DNS one (owner)

```
stkr org connectors rm <id> [flags]
```

```
      --dns          the connector is a DNS one
      --org string   the org (default: the logged-in one)
```

#### `stkr org connectors takeover`

Switch a domain to its provider's DNS; records at its current DNS host stop applying

```
stkr org connectors takeover <connector> <domain> [flags]
```

```
      --confirm string   the domain, typed out (-y does not skip this)
      --org string       the org (default: the logged-in one)
```

#### `stkr org connectors unmanage`

Stop managing a domain; stackr deletes the records it made, other records stay

```
stkr org connectors unmanage <connector> <domain> [flags]
```

```
      --org string   the org (default: the logged-in one)
```

### `stkr org create`

Make an org and switch to it

```
stkr org create <name>
```

### `stkr org creds`

Registry credentials

#### `stkr org creds add`

Add a registry credential

```
stkr org creds add <name> [flags]
```

```
      --password string   the password (prefer the prompt or STACKR_REGISTRY_PASSWORD)
      --url string        the registry host, e.g. ghcr.io
      --username string   the registry user
```

#### `stkr org creds ls`

List registry credentials

```
stkr org creds ls
```

#### `stkr org creds rm`

Delete a registry credential

```
stkr org creds rm <id>
```

#### `stkr org creds set`

Change a registry credential; unset flags keep their value

```
stkr org creds set <id> [flags]
```

```
      --name string       a new name
      --password string   the password (prefer the prompt or STACKR_REGISTRY_PASSWORD)
      --url string        the registry host, e.g. ghcr.io
      --username string   the registry user
```

### `stkr org domains`

List the managed domains, or one domain's records stackr wrote

```
stkr org domains [domain] [flags]
```

```
      --org string   the org (default: the logged-in one)
```

### `stkr org export`

Write the org as a stackr-org.yml (stdout, or -o FILE)

```
stkr org export [flags]
```

```
      --force           overwrite an existing -o file
  -o, --output string   write here instead of stdout
```

### `stkr org get`

Show this org

```
stkr org get
```

### `stkr org invites`

Pending invites

#### `stkr org invites add`

Invite someone; the link is a credential

```
stkr org invites add [flags]
```

```
      --email string   who it is for (required)
      --role string    viewer, member or owner: viewer sees, member deploys and edits stacks, owner also manages people and settings (default "member")
```

#### `stkr org invites ls`

List pending invites; --all adds used and expired ones

```
stkr org invites ls [flags]
```

```
      --all   add used and expired invites
```

#### `stkr org invites rm`

Revoke an invite; its link stops working

```
stkr org invites rm <id>
```

### `stkr org ls`

List your orgs

```
stkr org ls
```

### `stkr org members`

Org members

#### `stkr org members ls`

List members

```
stkr org members ls
```

#### `stkr org members rm`

Remove a member

```
stkr org members rm <user-id>
```

#### `stkr org members set`

Change a member's role

```
stkr org members set <user-id> [flags]
```

```
      --role string   viewer, member or owner
```

### `stkr org plan`

Plan the bound file at its branch's head now

```
stkr org plan
```

### `stkr org plan-show`

Read a config plan and its changes

```
stkr org plan-show <plan>
```

### `stkr org plans`

List the org's last config plans

```
stkr org plans
```

### `stkr org preview`

Dry run: show what applying a local stackr-org.yml would change

```
stkr org preview [flags]
```

Dry run: show what applying the file would change, against the org. Nothing is stored and nothing changes. Also called dry-run.

```
      --detailed-exitcode   exit 0 no changes, 1 blocked or error, 2 changes
  -f, --file string         the file to preview (default "stackr-org.yml")
```

### `stkr org reject`

Close a pending config plan unapplied

```
stkr org reject <plan>
```

### `stkr org rename`

Rename this org

```
stkr org rename <name>
```

### `stkr org rm`

Delete this org

```
stkr org rm
```

### `stkr org settings`

Show or change the org's settings defaults

```
stkr org settings [flags]
```

```
      --clear stringArray   give a knob back to the level above
      --set stringArray     knob=value (stkr knobs lists them)
```

### `stkr org use`

Work in another org (the key must reach it)

```
stkr org use <slug>
```

## `stkr params`

The param store: org and env levels, a tier's (--tier), the PR blocks (--pr), and the server's own (--level server, admin only)

Flags for it and its subcommands:

```
      --level string   org, env or server (stack too, for volumes); default: the env given or linked, else the org
      --pr             the PR block: the stack's (--stack or linked), else with --level org the org's
      --tier string    an org tier's params
```

### `stkr params export`

Write every param, secrets included, as collection.NAME=value; the one command that puts secrets on disk

```
stkr params export [flags]
```

```
      --env string      the environment (default: the linked one)
      --force           overwrite an existing file
  -o, --output string   the file (0600; default stdout)
      --stack string    the stack (default: the linked one)
```

### `stkr params get`

List params at the level; secrets stay masked unless --reveal

```
stkr params get [collection.NAME] [flags]
```

```
      --env string     the environment (default: the linked one)
      --reveal         show secret values (needs write access; logged)
      --stack string   the stack (default: the linked one)
```

### `stkr params import`

Set a param per KEY=VALUE line of a .env file, in one collection; others at the level stay

```
stkr params import <file> [flags]
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
  stkr params import .env --scope env --stack shop --env dev --collection app --secret
```

### `stkr params merge`

Set every collection.NAME=value line of a file (-f, "-" stdin); others stay

```
stkr params merge [flags]
```

```
      --env string     the environment (default: the linked one)
  -f, --file string    the file ("-" for stdin) (default "-")
      --secret         store the values as secrets
      --stack string   the stack (default: the linked one)
      --wait           follow the redeploys to their end; exit 1 if one fails
```

### `stkr params rm`

Delete a param at the level

```
stkr params rm <collection.NAME> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --wait           follow the redeploys to their end; exit 1 if one fails
```

### `stkr params set`

Set params; others at the level stay

```
stkr params set <collection.NAME[=value]>... [flags]
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
  stkr params set app.mode=prod --env dev
  stkr params set app.token=s3cret --secret --env dev
```

## `stkr password`

Change your password

```
stkr password
```

Change your password.

Each password comes from its environment variable, else a masked prompt, else
one line of stdin. The current password comes first, then the new one:
  STACKR_PASSWORD      the current password
  STACKR_NEW_PASSWORD  the new password

## `stkr pin`

Pin an org, stack or env to a placement: where its envs run

```
stkr pin [placement] [flags]
```

Pin an org, stack or env to a placement; stkr placements lists the ones open to the org.
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
  stkr pin eu-prod --org acme
  stkr pin eu-prod --stack shop
  stkr pin eu-db --stack shop --env prod --yes
  stkr pin --clear --stack shop
```

## `stkr placements`

List the placements open to the org

```
stkr placements [flags]
```

```
      --org string   the org (default: the logged-in one)
```

Example:

```
  stkr placements
  stkr placements --org acme
```

## `stkr profile`

Saved logins, one per server

### `stkr profile ls`

List the saved logins

```
stkr profile ls
```

### `stkr profile use`

Make a saved login the current one

```
stkr profile use <name>
```

## `stkr promote`

Promote a release (number or id) into --env: prints the plan, then asks

```
stkr promote <release> [flags]
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
  stkr promote 7 --stack shop --env prod
  stkr promote 7 --stack shop --env prod --dry-run
```

## `stkr registry`

The stackr image registry

### `stkr registry login`

Let docker push and pull use the stackr registry

```
stkr registry login [flags]
```

```
      --registry string   the registry host (default: registry. plus the server's host)
```

Example:

```
  stkr registry login
  docker push registry.example.com/acme/shop/web:1.4
```

### `stkr registry ls`

List the org's repositories, or one repo's versions

```
stkr registry ls [repo]
```

Example:

```
  stkr registry ls
  stkr registry ls acme/shop/api
```

### `stkr registry rm`

Delete a version and every other tag on its digest

```
stkr registry rm <repo>:<tag> [flags]
```

```
      --force   delete even when an env runs it
```

Example:

```
  stkr registry rm acme/shop/api:v1
```

## `stkr release`

Releases of the stack

### `stkr release get`

Show a release and its pins

```
stkr release get <number> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr release ls`

List releases, newest first

```
stkr release ls [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

## `stkr restart`

Restart the tile's containers

```
stkr restart [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

## `stkr rollback`

Put --env back on an earlier release: prints the plan, then asks

```
stkr rollback [flags]
```

```
      --dry-run        print the plan and change nothing
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tag string     the release to go back to (number or id); required
```

## `stkr server`

The server's config file, stackr-server.yml (admin)

### `stkr server apply`

Plan a local stackr-server.yml (not the repo's) and apply it: prints the plan, then asks

```
stkr server apply [flags]
```

```
  -f, --file string          the file to apply (default "stackr-server.yml")
      --no-wait              print the job and return; it runs server-side
      --remove stringArray   tick a removal row to apply it (repeatable; the plan lists the keys)
```

### `stkr server approve`

Apply a pending server plan: prints it, then asks

```
stkr server approve <plan> [flags]
```

```
      --no-wait              print the job and return; it runs server-side
      --remove stringArray   tick a removal row to apply it (repeatable; the plan lists the keys)
```

### `stkr server bind`

Show the server's config repo; --repo binds one and plans it, --unbind drops it

```
stkr server bind [flags]
```

```
      --auto               apply a plan with no impact line and no removal row without an approve
      --branch string      the branch (empty: the repo's default)
      --connector string   the server connector's id
      --path string        the file in the repo (empty: stackr-server.yml)
      --repo string        the repo to bind, owner/name
      --unbind             drop the binding (asks first; -y skips)
```

### `stkr server connectors`

Git and DNS connectors the server owns (admin)

#### `stkr server connectors domains`

List the account's domains: managed, not managed, or DNS elsewhere

```
stkr server connectors domains <connector>
```

#### `stkr server connectors ls`

List the server's git connectors; --dns its DNS ones

```
stkr server connectors ls [flags]
```

```
      --dns   list the DNS connectors
```

#### `stkr server connectors manage`

Let stackr manage a domain's DNS records; --org limits who may use it (default: every organization)

```
stkr server connectors manage <connector> <domain> [flags]
```

```
      --adopt             take over the existing records the plan lists as serving a tile (default true)
      --org stringArray   available to this organization id only (repeatable)
      --under string      manage only this name and below it, e.g. stackr (default: the whole domain)
```

#### `stkr server connectors plan`

Show the records managing a domain would write, and the existing ones stackr may adopt

```
stkr server connectors plan <connector> <domain> [flags]
```

```
      --under string   manage only this name and below it, e.g. stackr (default: the whole domain)
```

#### `stkr server connectors records`

Custom DNS records (TXT, MX, CNAME, A, AAAA)

#### `stkr server connectors records add`

Add a custom DNS record: TXT, MX ("10 mx.example.net"), CNAME, A or AAAA

```
stkr server connectors records add <connector> <domain> <type> <value> [flags]
```

```
      --name string   the name below, e.g. _dmarc (default: the name itself)
```

#### `stkr server connectors records ls`

List the custom DNS records and where stackr writes them

```
stkr server connectors records ls <connector> <domain>
```

#### `stkr server connectors records rm`

Remove a custom DNS record by id or value; stackr deletes it from the zone

```
stkr server connectors records rm <connector> <domain> <record>
```

#### `stkr server connectors rename`

Rename a server connector

```
stkr server connectors rename <id> <name>
```

#### `stkr server connectors rescope`

Change what part of a managed domain stackr manages; shows the plan, then asks

```
stkr server connectors rescope <connector> <domain> [flags]
```

```
      --adopt          take over the existing records the plan lists as serving a tile (default true)
      --under string   manage only this name and below it, e.g. stackr (default: the whole domain)
```

#### `stkr server connectors rm`

Remove a server connector

```
stkr server connectors rm <id>
```

#### `stkr server connectors share`

Set who may use a server connector: --org ids, --all, or neither to stop sharing

```
stkr server connectors share <id> [flags]
```

```
      --all               share with every organization
      --org stringArray   share with this organization id (repeatable)
```

#### `stkr server connectors takeover`

Switch a domain to its provider's DNS; records at its current DNS host stop applying

```
stkr server connectors takeover <connector> <domain> [flags]
```

```
      --confirm string   the domain, typed out (-y does not skip this)
```

#### `stkr server connectors unmanage`

Stop managing a domain; stackr deletes the records it made, other records stay

```
stkr server connectors unmanage <connector> <domain>
```

### `stkr server domains`

List the managed domains, or one domain's records stackr wrote

```
stkr server domains [domain]
```

### `stkr server export`

Write the server as a stackr-server.yml (stdout, or -o FILE)

```
stkr server export [flags]
```

```
      --force           overwrite an existing -o file
  -o, --output string   write here instead of stdout
```

### `stkr server pg-extensions`

Postgres extension recipes the server offers (admin)

#### `stkr server pg-extensions list`

List the built-in and added postgres extension recipes

```
stkr server pg-extensions list
```

#### `stkr server pg-extensions rm`

Remove an added recipe, or a built-in's switch-off row

```
stkr server pg-extensions rm <name>
```

#### `stkr server pg-extensions set`

Add or rewrite a recipe, or switch a built-in on or off (--off); --org or --all say who may use it

```
stkr server pg-extensions set <name> [flags]
```

```
      --all                  every organization may use it
      --extension strings    names CREATE EXTENSION creates
      --major strings        postgres majors it supports
      --off                  switch the recipe off
      --on                   switch the recipe on (the default)
      --org stringArray      an organization id that may use it (repeatable)
      --package strings      apt package names, with {major} where the postgres major goes
      --package-url string   download url of a package file
      --preload strings      startup modules it needs
      --requires strings     recipes it needs first
      --sha256 stringArray   arch=hex checksum of the package file (repeatable)
      --version string       the version to pin
```

### `stkr server plan`

Plan the bound file at its branch's head now

```
stkr server plan
```

### `stkr server plan-show`

Read a server plan and its changes

```
stkr server plan-show <plan>
```

### `stkr server plans`

List the server's last config plans

```
stkr server plans
```

### `stkr server preview`

Dry run: show what applying a local stackr-server.yml would change

```
stkr server preview [flags]
```

Dry run: show what applying the file would change, against the server. Nothing is stored and nothing changes. Also called dry-run.

```
      --detailed-exitcode   exit 0 no changes, 1 blocked or error, 2 changes
  -f, --file string         the file to preview (default "stackr-server.yml")
```

### `stkr server reject`

Close a pending server plan unapplied

```
stkr server reject <plan>
```

## `stkr share`

Network shares: NFS or SMB exports tiles mount

### `stkr share add`

Add a share

```
stkr share add <slug> [flags]
```

```
      --kind string       nfs or smb
      --options string    comma-separated mount options, e.g. nfsvers=4
      --password string   smb password as an ${{ org.params.<collection>.<name> }} ref, never the value
      --source string     nfs: host:/export, smb: //host/share
      --user string       smb user, plain or an ${{ org.params.<collection>.<name> }} ref
```

### `stkr share ls`

List the org's shares

```
stkr share ls
```

### `stkr share rm`

Remove a share; refused while a tile mounts it

```
stkr share rm <slug>
```

## `stkr slice`

Slice tiles: a database or bucket cut from an operated tile

### `stkr slice add`

Make a slice tile in --env, cut from --from at its deploy

```
stkr slice add <slug> [flags]
```

```
      --access string        the default access of its consumers, read or write (default write)
      --env string           the environment (default: the linked one)
      --extensions strings   postgres extensions of the slice's database, comma separated
      --from string          the operated tile, <stack>:<env>:<tile>
      --stack string         the stack (default: the linked one)
      --tile string          the tile (default: the linked one)
```

### `stkr slice bindings`

List the tiles holding a cred on the slice

```
stkr slice bindings [slice] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr slice branch`

Copy a postgres slice as of now, or --at a past time, into a new slice tile <slice>-br-xxxx

```
stkr slice branch <slice> [flags]
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

### `stkr slice bucket`

Set a bucket slice's version history, size cap and CORS origins

```
stkr slice bucket <slice> [flags]
```

```
      --cors strings       origins CORS allows, e.g. https://app.example.com; "" clears them
      --env string         the environment (default: the linked one)
      --quota-gb int       hard size cap in GB; 0 = none
      --stack string       the stack (default: the linked one)
      --tile string        the tile (default: the linked one)
      --version-days int   days old versions are kept; 0 turns versioning off (default 7)
```

### `stkr slice extensions`

Extensions of a postgres slice's database

#### `stkr slice extensions get`

Show a slice's extensions against the ones its instance allows

```
stkr slice extensions get <slice> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stkr slice extensions set`

Set a slice's extensions; with no names the list is emptied

```
stkr slice extensions set <slice> [name...] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr slice forward`

Tunnel a local port to the slice's postgres and print its connection string

```
stkr slice forward <slice> [flags]
```

```
      --env string     the environment (default: the linked one)
  -p, --port int       the local port (default: a free one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr slice get`

Show a slice tile and the instance it resolves to

```
stkr slice get [slice] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr slice on-remove`

Keep or drop the data when the slice tile is removed

```
stkr slice on-remove <slice> <keep|drop> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr slice restore`

Copy a bucket's objects back as they were at --at, as new versions. Never deletes. Runs file by file, not atomically. Instances restore with `operated restore`, backups with `backup restore`

```
stkr slice restore <slice> [flags]
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

### `stkr slice url`

Choose whether a postgres slice's url, host and port point at the instance or its pooler

```
stkr slice url <slice> <direct|pooled> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr slice usage`

Show a bucket's current bytes and its old versions' bytes, apart

```
stkr slice usage [slice] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

## `stkr ssh`

Open a shell in a replica

```
stkr ssh [tile] [flags]
```

```
      --container string   the replica (default: a running one)
      --env string         the environment (default: the linked one)
      --shell string       the shell binary (default: bash, else sh)
      --stack string       the stack (default: the linked one)
      --tile string        the tile (default: the linked one)
```

## `stkr stack`

Stacks

### `stkr stack config-repo`

Show the stack's config repo; --repo binds one, --unbind drops it

```
stkr stack config-repo [flags]
```

```
      --branch string      the branch (default "main")
      --connector string   the connector id (stkr org connectors ls)
      --env string         the environment (default: the linked one)
      --path string        the stack file in the repo (default "stackr-compose.yml")
      --repo string        the git URL to bind
      --stack string       the stack (default: the linked one)
      --unbind             drop the binding (asks first; -y skips)
```

### `stkr stack create`

Make a stack

```
stkr stack create <name> [flags]
```

```
      --desc string   a one-line description
```

Example:

```
  stkr stack create shop --desc "The web shop"
```

### `stkr stack defaults`

Show or change the stack's settings defaults

```
stkr stack defaults [flags]
```

```
      --clear stringArray   give a knob back to the level above
      --env string          the environment (default: the linked one)
      --set stringArray     knob=value (stkr knobs lists them)
      --stack string        the stack (default: the linked one)
```

### `stkr stack export`

Write the stack as a stackr-compose.yml (stdout, or -o FILE); --env limits it to one env

```
stkr stack export [flags]
```

```
      --env string      the environment (default: the linked one)
      --force           overwrite an existing -o file
  -o, --output string   write here instead of stdout
      --stack string    the stack (default: the linked one)
```

### `stkr stack get`

Show the stack

```
stkr stack get [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr stack image-check`

Check the stack's images for a newer tag; a moved tag writes a release

```
stkr stack image-check [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
```

### `stkr stack ls`

List stacks

```
stkr stack ls
```

### `stkr stack rename`

Rename the stack

```
stkr stack rename <name> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr stack rm`

Delete the stack; --cascade removes its tiles and environments too

```
stkr stack rm [stack] [flags]
```

```
      --cascade        remove the stack's tiles and environments first
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
```

## `stkr status`

Show the server, the user and this directory's link

```
stkr status
```

## `stkr tier`

The org's env tiers, bottom first

### `stkr tier add`

Add a locked tier on top of the ladder

```
stkr tier add <slug>
```

### `stkr tier lock`

Lock a tier: other envs cannot read its org params

```
stkr tier lock <slug>
```

### `stkr tier ls`

List tiers

```
stkr tier ls
```

### `stkr tier order`

Set the ladder, bottom first: every tier once

```
stkr tier order <slug>...
```

### `stkr tier rename`

Rename a tier; stack envs keep their slugs and leave it

```
stkr tier rename <slug> <new>
```

### `stkr tier rm`

Delete a tier no stack env is in

```
stkr tier rm <slug>
```

### `stkr tier unlock`

Unlock a tier

```
stkr tier unlock <slug>
```

## `stkr tile`

Tiles: the deployed units

### `stkr tile access`

Set the tile's access to a slice tile of its env; a bound tile is re-granted in place

```
stkr tile access [tile] [flags]
```

```
      --access string   read or write
      --env string      the environment (default: the linked one)
      --rm              drop the entry: the slice's default access applies
      --slice string    the slice tile's slug
      --stack string    the stack (default: the linked one)
      --tile string     the tile (default: the linked one)
```

### `stkr tile create`

Make a tile in --env

```
stkr tile create <name> [flags]
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
  stkr tile create api --stack shop --env dev --image nginx:1.27
```

### `stkr tile deploy`

Deploy the tile and follow it

```
stkr tile deploy [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile domain`

The tile's domains

#### `stkr tile domain add`

Route a host to the tile; --auto names one under the nearest domain resource, --domain one in a managed domain

```
stkr tile domain add [host] [flags]
```

```
      --auto                 take the generated name under the nearest domain resource, like the panel's Add auto domain
      --domain string        a managed domain (stkr tile domain managed lists them); stackr sets its DNS
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

#### `stkr tile domain caddy`

Set a domain's raw Caddy config (admin); -f - reads stdin, empty clears

```
stkr tile domain caddy <host> [flags]
```

```
      --env string     the environment (default: the linked one)
  -f, --file string    the Caddy JSON ("-" for stdin; empty clears)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stkr tile domain ls`

List the tile's domains

```
stkr tile domain ls [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stkr tile domain managed`

List the managed domains the tile's domains may sit in

```
stkr tile domain managed [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stkr tile domain records`

Custom DNS records (TXT, MX, CNAME, A, AAAA)

#### `stkr tile domain records add`

Add a custom DNS record: TXT, MX ("10 mx.example.net"), CNAME, A or AAAA

```
stkr tile domain records add <host> <type> <value> [flags]
```

```
      --env string     the environment (default: the linked one)
      --name string    the name below, e.g. _dmarc (default: the name itself)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stkr tile domain records ls`

List the custom DNS records and where stackr writes them

```
stkr tile domain records ls <host> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stkr tile domain records rm`

Remove a custom DNS record by id or value; stackr deletes it from the zone

```
stkr tile domain records rm <host> <record> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stkr tile domain rm`

Stop routing a host to the tile

```
stkr tile domain rm <host> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

#### `stkr tile domain set`

Change a domain; only the flags given change

```
stkr tile domain set <host> [flags]
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

### `stkr tile exec`

Run a command in a replica; stdin is closed unless -i (stkr ssh opens a shell)

```
stkr tile exec [tile] -- <cmd>... [flags]
```

```
      --container string   the replica (default: a running one)
      --env string         the environment (default: the linked one)
      --stack string       the stack (default: the linked one)
  -i, --stdin              send stdin to the command (until its EOF)
      --tile string        the tile (default: the linked one)
      --timeout string     stop the command after this long, e.g. 5m (default 50s, 1h with -i; max 1h)
```

### `stkr tile forward`

Tunnel a local port to a tile's port

```
stkr tile forward [tile] [flags]
```

```
      --address string     local address to bind (default "127.0.0.1")
      --container string   the replica (default: a running one)
      --env string         the environment (default: the linked one)
      --port string        [local:]remote port (default: the tile's own)
      --stack string       the stack (default: the linked one)
      --tile string        the tile (default: the linked one)
```

### `stkr tile get`

Show a tile

```
stkr tile get [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile image-check`

Check the tile's image for a newer tag; a moved tag writes a release

```
stkr tile image-check [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile jobs`

List the tile's recent jobs

```
stkr tile jobs [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile logs`

Print a replica's or a run's log; --follow streams it

```
stkr tile logs [tile] [flags]
```

```
      --container string   the replica (default: a running one)
      --env string         the environment (default: the linked one)
  -f, --follow             keep streaming
      --run string         a cron or function run's log instead (stkr tile runs lists them)
      --stack string       the stack (default: the linked one)
      --tail int           lines of history (default 200)
      --tile string        the tile (default: the linked one)
```

### `stkr tile ls`

List the env's tiles

```
stkr tile ls [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile metrics`

Show a tile's CPU, memory, network, disk IO and requests (a cron or function's runs) over --range

```
stkr tile metrics [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --range string   how far back, up to 24h, or the history retention when on (like 15m, 6h, 7d) (default "1h")
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

Example:

```
  stkr tile metrics api --stack shop --env prod --range 24h
```

### `stkr tile pause`

Pause a cron tile's schedule

```
stkr tile pause [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile rebuild`

Build the running commit without the build cache and deploy it

```
stkr tile rebuild [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile rename`

Rename a tile

```
stkr tile rename <tile> <name> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile restart`

Restart the tile's containers

```
stkr tile restart [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile resume`

Resume a cron tile's schedule

```
stkr tile resume [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile rm`

Remove a tile

```
stkr tile rm [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile run`

Run a cron or function tile now and follow it

```
stkr tile run [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile runs`

List a cron or function tile's runs, newest first; --run shows one

```
stkr tile runs [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --run string     the run id
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile set`

Change tile settings; only the flags given are sent

```
stkr tile set [tile] [flags]
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
  stkr tile set api --stack shop --env dev --image nginx:1.28
```

### `stkr tile ssh`

Open a shell in a replica

```
stkr tile ssh [tile] [flags]
```

```
      --container string   the replica (default: a running one)
      --env string         the environment (default: the linked one)
      --shell string       the shell binary (default: bash, else sh)
      --stack string       the stack (default: the linked one)
      --tile string        the tile (default: the linked one)
```

### `stkr tile start`

Start the tile's containers

```
stkr tile start [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile status`

Show the tile's state and replicas

```
stkr tile status [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

### `stkr tile stop`

Stop the tile's containers; --run stops one run of a cron or function

```
stkr tile stop [tile] [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --run string     the run to stop
      --stack string   the stack (default: the linked one)
      --tile string    the tile (default: the linked one)
```

## `stkr unlink`

Drop this directory's link

```
stkr unlink
```

## `stkr volume`

Volumes

Flags for it and its subcommands:

```
      --level string   org, env or server (stack too, for volumes); default: the env given or linked, else the org
```

### `stkr volume add`

Declare a volume at the level

```
stkr volume add <slug> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr volume discard`

Remove the plain Docker copy of a volume that also has a disk folder; --bound removes an object bound elsewhere. The folders stay

```
stkr volume discard <volume> [flags]
```

```
      --bound            the object is bound somewhere other than the volume's folder
      --confirm string   the word that confirms (the volume's slug); without it the verb asks
      --env string       the environment (default: the linked one)
      --stack string     the stack (default: the linked one)
```

### `stkr volume ls`

List volumes at the level

```
stkr volume ls [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

### `stkr volume move-to-disk`

Copy a volume onto its node's disk; the tiles that mount it stop while it copies

```
stkr volume move-to-disk <volume> [flags]
```

```
      --env string     the environment (default: the linked one)
      --no-wait        print the job and return; it runs server-side
      --stack string   the stack (default: the linked one)
```

### `stkr volume rm`

Delete a volume (its data is kept for the retention window, then backed up and removed)

```
stkr volume rm <volume> [flags]
```

```
      --env string     the environment (default: the linked one)
      --stack string   the stack (default: the linked one)
```

