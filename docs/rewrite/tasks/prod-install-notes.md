# Prod install walkthrough notes

Started 2026-11-13. darthvader installs prod himself (v0.8.19) and dictates
what he hits. One line per item: kind, what, where. Nothing here is built
until he says go, except clear bugs with a clear fix.

Prod shape: controller = Proxmox VM 103 `stackr` (Debian 13, cloned from
template 998), panel stackr.fyrmforge.dev, root fyrmforge.dev, behind the
home Traefik (192.168.1.100, TCP passthrough to 192.168.1.108). Worker =
privileged CT 104 `stackr-ct` 192.168.1.104 (nesting, NFS, SMB,
/dev/dri/renderD128 mode 0666) for Jellyfin, node name `media`. Worker =
Hetzner CX23 x86, Debian 13, Falkenstein, prod envs only.

## Change requests

1. One-line controller install. Today it is four lines (download installer
   and SHA256SUMS from the GitHub release, check, chmod) then
   `sudo ./stackr-install`. He expected one curl line, like the join line a
   worker gets from the panel. Release assets today: SHA256SUMS,
   stackr-image.digest, stackr-install-linux-amd64. docs/guide/install.md,
   .github/workflows/release.yml.
   BUILT 2026-11-13, uncommitted: scripts/install.sh (stamped with the
   version and attached to each release by release.yml), install guide
   updated. Tested in a debian:13 container against v0.8.19: installs to
   /usr/local/bin/stackr-install and runs it; a tampered download and a
   non-root run are refused. Live at the latest/download URL only after a
   push and the next release. Later: a short address (redirect).
2. Pin by env tier. He wants the Hetzner box for prod envs only; today each
   prod env is pinned by hand. Idea: a default placement per tier (org or
   server level). Not asked for yet, only offered.

3. Default panel name `stkr.<root>` surprised him (he wanted
   `stackr.<root>` and kept the default by accident). Consider `stackr.` as
   the default, or no default. cmd/stackr-install/prompt.go, installspec.

4. Setup wizard step 2 "Certificates" (/-/setup/server). His words: "this
   screen is a bit shit", "it should make it clear that we can either connect
   a DNS provider or do it manually". Today: title says Certificates but the
   step is about a DNS connector; "+ Add Namecheap" is a small button off to
   the right; a big empty "DNS connectors / SERVER / No connectors yet" box;
   Skip is the largest button; the manual path is only a grey footnote under
   Skip. Proposal (not agreed yet): two equal choice cards like step 1's
   "From a server file" card: "Connect a DNS provider" (stackr writes records,
   wildcard certificates) and "I'll point DNS myself" (one certificate per
   host; show the records to add). Drop the empty connectors box until one
   exists. Also seen by me, not raised by him: no Next/Back, three texts
   saying the same thing, the step does not use the root domain it knows.
   AGREED by him: rename the step from "Certificates" to "DNS" (stepper
   label and heading).
   After connecting (his words: "this nested box is also shit"): three
   boxes inside each other (step card > "DNS connectors SERVER" card >
   connector row card). Flatten: the connector row sits straight in the
   step. Also seen: the row says "Namecheap / Namecheap · simpleadmin"
   (name twice), and "+ Add Namecheap" stays primary after one is connected.

5. Namecheap connector setup: link straight to the API access page,
   https://ap.www.namecheap.com/settings/tools/apiaccess/ (his ask). Today
   step 1 only describes the click path in words ("Open Profile, Tools,
   Business & Dev Tools, Namecheap API Access"). connectors.Step has no link
   field: add one (Step.Link label + URL), render it in the connector setup
   steps, set it in internal/service/internal/connectors/namecheap/kind.go.
   The allow-list step can link the same page.

6. Webhook line on connector cards. His words: "why tf do I need this? its
   dumb". The card shows "Webhook <cut url> Copy" (connectors.templ r.Hook).
   It is only for repairing the app on GitHub after a panel address change;
   the manifest sets it at creation. Proposal: drop it from the card, keep it
   in the connector's Settings (settings.templ already shows Webhook URL).
   Later idea: on a panel address change stackr updates the GitHub App's
   webhook itself (GitHub API "update a webhook configuration for an app"),
   so nobody ever copies it.
7. Box nesting, GitHub step: same three nested boxes as the DNS step (his
   words: "same box nesting issue as the last screen"). The connectors list
   card is shared, so flatten it once for every wizard step that lists
   connectors (DNS, GitHub, likely Backups/Observability too: check each).

8. Observability step presents a choice that is already made. He noticed
   stackr-observe running before he enabled anything. observe_enabled
   defaults to "true" (leaf/settings/catalogue.go), so the container starts
   at first boot; the wizard step (With it / Without it, Save and continue,
   Skip) reads as opt-in, and Skip leaves it on. Options: (a) say so: "On by
   default", Skip becomes "Keep defaults", untick to turn off (my pick);
   (b) default off until the wizard is answered. Also: stepper label is cut
   off ("Observabil…").
   DECIDED by him 2026-11-13: no Skip button on this step, the user must
   choose on or off; stackr-observe is off by default and only starts when
   chosen. Compat (prod exists): an install that never stored
   observe_enabled is running with it on today, so a migration must store
   "true" for those before the default flips, or his history stops.

9. Backups step: split in two (his ask). Today one screen holds the S3
   destination form, "Add destination", then the panel backup schedule with
   a Destination dropdown that still says "This box"; it is not clear the
   destination must be added first, then picked below. Wanted: sub-step 1 =
   add the destination; sub-step 2 = panel backup (schedule + destination,
   the new one preselected). Sub-step 1 is tabbed by kind: S3 | NFS/SMB.
   NOTE: backup destinations are S3-only today (plus local). An NFS/SMB
   destination is a new destination kind (store, backup flow, restore,
   installer restore), not only a wizard tab: scope it separately.
   Also his ask: on the destination sub-step a "Test connection" button
   that keeps the form as typed, then "Add and next". (Saving already writes
   and deletes a test object; the test button runs that check without
   saving.) CHECK when building: does a failed "Add destination" clear the
   form today, the secret included? If so that is a bug by itself.
   DECIDED by him 2026-11-13: S3 only for now. Build the two sub-steps; no
   NFS/SMB tab until that destination kind exists (parked).
   Also offered, not answered: default the Region to us-east-1 when the
   endpoint is not AWS (he asked "why us-east-1?").

10. Nodes step: the inline "Add a node" card is ugly (his words: "again
    this card is ugly, make it an overlay dialogue like some of the others").
    Make Add node open the same modal the connector setups use (Name > Run
    the command > Confirm), with the step showing only the nodes and
    placements lists and an "Add node" button. Seen by me: three intro
    paragraphs above it; the Nodes row reads "controller controller Online".

11. Add node tags. DECIDED by him 2026-11-13: storage ticked by default,
    and each tag gets a one-line explanation (he had to ask "whats the
    storage for again?"). Draft hints: ingress "Public traffic can enter
    here. Needs ports 80 and 443."; egress "Outgoing traffic can leave
    here."; builder "Builds run here."; storage "Apps with volumes or
    databases can run here." Same default in `stackr node add` (today
    --tag storage is opt-in): CLI and panel must agree.
    Also seen: Address is prefilled `<name>.<panel host>` for every node; a
    node with no public address (home container) should not get one. Hint
    says only "The node's own name. Optional."

12. Worker join line needs Docker and curl already on the box (joinScript
    in internal/service/join.go fails with "install Docker 26.0 or newer
    first"), while the controller installer offers to install Docker. A
    fresh Debian 13 container has neither. Make the join line offer or do
    the Docker install like the controller path (stackr-install can already
    run get.docker.com: cmd/stackr-install/hostcheck.go), and say so on the
    Add node "Run the command" step.

13. Add node, "Run the command" step: no Copy button on the join command
    (his words: "add a fucking copy command on the url lol"). Add one
    (data-copy, as the connector cards have).

14. Join output, waiting line. He liked the output ("looks nice") but the
    last line "Waiting for an admin to confirm this node in the panel
    (Nodes, pending)..." should tell the user to go back to the panel and
    give the link, e.g. "Go back to the panel and press Add:
    https://<panel>/-/admin/nodes". cmd/stackr-install/join.go.

15. Controller node name. His ask: a step to name the first node, "not just
    have it default to controller" (Nodes list read "controller controller
    Online" next to "home-ct worker Online"). Options: an installer question
    "Node name" (default the hostname) and/or a name field for the
    controller in the wizard's Nodes step. Check whether a node can be
    renamed after install (mesh DNS, placements, certs carry the name).

16. Admin side menu: the group titles (PEOPLE, NODES, WEB, DATA,
    MONITORING, SECURITY, SYSTEM) should be more pronounced (his ask). Today
    small grey caps, barely different from the items. Brighter and a touch
    larger, with more space above each group or a thin divider.

17. Keys page (Admin, Security, Keys): the inline "Your password" fields
    beside "Rotate the master key" and "Rotate the recovery key" are bad UX
    (his words: "should be a nice bright button, then a popup modal").
    Wanted: one clear button per key; pressing it opens a modal that says
    what rotating does, asks for the password there, and (recovery key)
    shows the new key once with "I've saved it". Same pattern for any other
    password-confirmed action. internal/ui/drawer/admin/admin.templ,
    security.templ.

18. "Server only" wording (his words: "what does server only even mean?",
    "that should be worded better then"). In the shared share picker
    (components ShareView: Every org / Only these orgs / Server only) it
    means no org may use it, admins only. Seen on Add a placement and on Add
    a destination. Reword, e.g. "No org (admins only)", with a one-line hint
    per option; check each place the picker is used (placements, backup
    destinations, connectors, managed domains) reads right.

## Bugs

1. False "no certificate (EOF)" banner on a fresh install. First thing the
   panel showed: "stkr.fyrmforge.dev on controller: no certificate (EOF).
   Since Nov 13, 22:45", while the Let's Encrypt certificate was fine (issued
   about 2 minutes after boot). Cause: the cert check ran at boot before the
   proxy had the certificate, the panel's name had no grace (Added was zero),
   and the check is daily, so the critical problem would stay about a day.
   internal/service/certcheck.go, internal/service/jobs.go watchTick.
   FIXED 2026-11-13, uncommitted: the panel's name gets the 3h grace from the
   controller node's age, and the watch tick re-runs the host check while any
   certfail row is open. Test TestPanelCertGraceAndRecheck; service package
   tests pass. Reaches prod with the next release, through the Update tab.
   Same stale row also made the wizard Summary say "Certificates failing,
   1 certificate not issued" (serversetup.go counts problem.certfail rows),
   while the panel certificate was fine. Gone with the same fix. Seen on the
   Summary too: states are grey/purple text ("on", "failing", "skipped"),
   "failing" is not red; rows are not links to their step.

2. "Continue on GitHub" does nothing, no error (setup wizard step 3, Add a
   GitHub connector, step 2 of 3). Cause: the panel's Content-Security-Policy
   has `form-action 'self'` (prod only matters: added in the 2026-11-02
   security batch), and this step is a browser form POST of the app manifest
   to https://github.com/settings/apps/new. The browser drops the submit and
   only logs to the console. Never caught: the rig kept its GitHub connector
   row across wipes, so the create flow was not re-run after the CSP change.
   FIXED 2026-11-13, uncommitted: form-action also names https://github.com.
   internal/web/server.go, test in internal/web/server_test.go. NOT testable
   without a browser on a prod-mode panel: verify on prod after the update.
   Until then the GitHub connector cannot be created from the panel.

3. Closing the Add GitHub dialog leaves a confusing half-made row (his
   words: "I clicked close, tf is this now"): "GitHub / GitHub (connecting...
   8536ccfa) / Every org / pending", with a Webhook URL and Copy button, and
   Settings. The row is made at step 1, before the GitHub round trip, and is
   only swept after pendingTTL = 1 h (internal/service/kinds.go,
   connector.go SweepPendingConnectors). Nothing says what it is or what to
   do. Proposal: a pending row reads "Setup not finished" with Continue and
   Remove, shows no webhook; closing the dialog before the provider answered
   removes the row. internal/ui/connectors/connectors.templ. NOT fixed yet.

4. Setup wizard Nodes step is not live (his words: "we should not have to
   refresh, we need reactivity, we need statuses there on that page, showing
   if its up, running, properly connected etc"). After Add, htz-prod sat at
   "worker Joined, not seen yet" until a manual reload. Breaks the standing
   rule that the panel never shows stale state (live over the existing SSE).
   Wanted: the Nodes list updates by itself, and each row shows real status:
   joined / mesh link up (handshake age) / worker answering / online, with
   the reason when it is stuck (e.g. "no handshake: is UDP 51871 open on
   the box?"). internal/ui/pages/setup/server.templ, the admin Nodes list
   already has live rows to reuse. NOT fixed yet. Also his ask: "some
   colours too": status as coloured chips (green online, amber joining,
   red down), as the connector cards do, not grey text.

5. Security log says "Kept forever." but events are pruned after 90 days
   (internal/service/prune.go securityKeep). Wrong copy:
   internal/ui/drawer/admin/security.templ. Fix the text ("Kept 90 days.").
   His question: "are pages like this actually done properly, do we page
   them, do they not run indefinitely?" This one: paged (fixed page, "Show
   older" re-reads with a larger ?limit=, no upper cap on it; admin only)
   and bounded by the 90-day prune. TODO for the plan: check every other
   growing list the same way (jobs, backup history, alerts, deploy/release
   history, registry, traffic, logs): page size, a cap, and a prune.

6. Cluster map opens tiny (/-/cluster): 3 nodes, 2 networks drawn as a
   speck at the top centre of an empty canvas (his words: "thats the default
   cluster screen wtf, should be more zoomed in, to fit"). The map must open
   fitted to its content. Known open item since October ("Map polish:
   opening zoom" in the multi-node list), never done. Likely the first fit
   runs before the canvas has its size or before nodes are laid out. The
   fit button (bottom right) is the workaround. NOT fixed yet.

7. Cluster map drops a tag: the controller card shows ingress, egress,
   builder; Admin, Nodes shows ingress, egress, builder, storage for the
   same node. Find why the map card omits storage (cut to three, or its own
   list). It made him think storage was not set.
   CONFIRMED by him with the node drawer open ("this is a display bug"):
   Tags tab has storage ticked, the card behind it shows three tags.
   Seen in the same drawer: "controller serves the panel and 1 routes"
   (plural). The Tags tab already has a hint per tag ("Takes public web
   traffic.", "Builds images.", "Holds volumes and operated databases.",
   "Internet traffic may leave from this node's IP."): reuse those exact
   hints in Add node (change request 11).

## Docs

1. Proxmox containers: the README only says "In a container (LXC) the module
   must be loaded on the host". His join on a fresh privileged CT failed with
   "wireguard: the kernel has no module (in a container, load it on the
   host)" after a host reboot dropped a hand-loaded module. Say how, and how
   to make it stick: `modprobe wireguard` and a line in
   /etc/modules-load.d/. Add a short "Proxmox container" section to
   docs/guide/nodes.md: privileged, nesting, NFS/SMB features, host modules,
   GPU device passthrough with mode 0666.

## Backlog (bigger than a walkthrough fix)

- Node system upgrades. His ask 2026-11-14: "on the backlog we should be
  able to see about upgrading system and system pkgs". Stackr would show
  per node what the OS has pending (packages, security updates, Docker
  version, reboot needed) and run the upgrade from the panel. Needs a
  design: which distros (apt first), drain or not before a reboot, order
  across nodes, controller last, unattended or on a click.

## Parked by him

- Panel as a host binary instead of a container (watchdog when Docker is
  down). "Just ignore it for now."
- Pre-release upgrade test against a copy of prod data. "Don't worry about
  it for now."
- Unprivileged LXC support. Not now.

## Day 2: first org and config as code (2026-11-14)

He runs every step himself; these are his remarks and what the steps showed.

### Change requests

- **"Use an existing App" on the GitHub connector, step 1.** A bare link
  with no hint of who it is for. His words: "that sounds dumb for a normal
  setup". It is the side door for an App made on GitHub by hand (reuse
  across installs). Say so in one line, or fold it under "Advanced".
  `internal/service/internal/connectors/github/kind.go` (`Alt`).
- **GitHub org field invites the wrong input.** He typed his personal
  account name into "GitHub org"; the hint "Leave empty for your personal
  account" sits below and was missed. Refuse or warn when the name is a
  user, not an org, or reword the label ("GitHub org, empty for your own
  account").
- **"Set up your organization" gives no basis to choose.** Two cards,
  "Describe it in a config file" and "Fill it in here"; he asked "describe
  by config or what?". The file route needs a repo stackr can read, which
  needs a working git connector first; the card does not say so.
- **stackr's word "preview" for a dry run.** He did not recognise it; "dry
  run" is the term he expected. Consider the wording in CLI help and docs
  (`stackr server preview`, `stackr org preview`).
- **Org wizard, Domain step: no pick from the DNS connector.** A free text
  field with "Point an A record at this server first", although the server
  already manages `fyrmforge.dev` through Namecheap. He asked: "if we have
  access to a dns connector, can we pick from there?". Offer the managed
  domains the org may use (with a name-under-it field), and drop the A
  record hint when a managed domain covers the host.

### Bugs and gaps

- **Connectors cannot be created on v0.8.20** (the dead "Continue on
  GitHub" button, fix on master, unreleased). On a fresh prod this blocks
  the whole config-as-code route at the first screen: no connector, no
  bound repo, no org from a file. Confirmed by him on prod.
- **Placement `media` is open to no org** (`open: []`) on a fresh install
  where no org exists yet. The first org cannot deploy to the home node
  until someone opens it. Unclear whether the wizard should offer this
  when the first org is made.
- **Local CLI build drift.** The laptop's `stackr` (dev build ddaa87e)
  still says `managed` where the server says `operated`, and has no
  placement or node verbs. A release CLI on the laptop is needed for a
  clean walkthrough.
- **SEVERE: the panel's own name stopped resolving (prod, 2026-11-14/15).**
  `stkr.fyrmforge.dev` had no record of its own; it resolved through the
  zone's `*` wildcard. stackr (v0.8.20, Namecheap, fyrmforge.dev managed)
  wrote `htz-prod.stkr.fyrmforge.dev` (the node's own address, under the
  panel's name). A record below `stkr` makes `stkr` an empty name in DNS, so
  the wildcard no longer answers for it: NOERROR with no data. stackr wrote
  no record for the panel itself because the controller is behind NAT and
  has no public address of its own (`nameWants` in
  `internal/service/dnsrecords.go`: "a LAN-only box never gets a record").
  Seen from outside: `stkr` no A; `htz-prod.stkr` A 188.34.182.150.
  Namecheap's record list (his screenshot): `*` and `@` to the home IP,
  `htz-prod.stkr`, and two leftovers of the old rig (`ghdeploy`, `h4.test`
  to 77.42.15.115). No `stkr`, no `homelab`: those resolve by the wildcard. Panel containers were up the whole time;
  a hosts-file line brought it back. Trigger not pinned down: first noticed
  after he added the org domain `homelab.fyrmforge.dev` in the org wizard.
  Fix ideas: never create a name under the panel's host unless the panel's
  own record is written in the same change; treat "panel resolves only via
  a wildcard" as a state to preserve (write the explicit record with the
  wildcard's value); use the NAT address a worker saw for the panel record
  as it is used for tile hosts; a check that the panel name still resolves
  after every DNS write, with a loud problem line.
  **His call on the fix (2026-11-15):** (1) stackr should manage the
  panel's record itself, as an explicit record; (2) when the panel's box is
  behind NAT, ask the user instead of skipping: offer the publicly seen
  address ("this box is behind a router; point the panel at <address seen
  from outside>?"), let him type another, or say he handles that record
  himself.
  **Decided by him (2026-11-15), in this order:**
  1. **Delete the per-node address feature.** "we dont need it". Nothing
     depends on it: the mesh talks by IP; the only readers are the node
     page, the DNS sync (`nameWants`) and the custom-record guard
     (`ownHosts` in `internal/service/dnscustom.go`). Goes: the Address
     field and its default `<name>.<panel address>` on add node and join
     (`internal/service/nodeaddress.go`, `join.go`), the controller's
     `n1.<panel>` default, the node Network tab part, CLI and API fields,
     the server-file key if one exists, the docs. A forward migration
     (the column may stay unused). No cleanup code for records already
     written: prod is the only deployment and he removes `htz-prod.stkr`
     by hand ("we can also cleanup prod manually").
  2. **A dedicated record for the panel's name,** written by stackr, with
     the behind-NAT question above.

### Questions he asked that the product should answer itself

- What a `domains:` entry in the org file does (it claims the domain for
  the org's tiles, writes no DNS). The docs page for config files gives one
  line per key and no example for org domains.
- Whether building by hand first and applying files later is supported
  (yes: export plans clean; removal rows are never applied unticked). Worth
  a short "start in the panel, move to files later" section in
  `docs/guide/config-files.md`.

## Day 3: connectors and moving off the old VM (2026-11-16)

### Change requests

- **Nested boxes on the org Connectors page look bad.** Org settings →
  Connectors: the page card ("Connectors · ORGANIZATION") holds a second
  bordered card per connector (GitHub, name, Manage, Remove). A box in a
  box, with a lot of dead space around the inner one. His words: "mega
  ugly as per usual". He wants the other screens checked for the same
  pattern, not only this one: an audit of card-inside-card across the
  panel. Ties in with the settings screens rework.

- **Tile outputs are not discoverable.** An ordinary tile with a port
  publishes `host`, `port`, `url` (plus `public_url` and `public_domain`
  when it has a domain), so `${{ tile.sonarr.url }}` works in env, commands
  and template files (`internal/service/internal/leaf/params/ref.go`,
  `Endpoint.Outputs`). The guide shows refs only for slices
  (`docs/guide/operated.md`), so the first stack file written for prod
  hard-coded `http://sonarr:8989` everywhere, and Claude told him the
  feature did not exist. Ask: document app-tile outputs in the config
  files guide with a multi-tile example, and list a tile's outputs in the
  panel next to the tile.

- **Volume backup schedules cannot be set in the stack file (TO FIX).**
  `volumes.<name>.backup` is read only to be refused
  (`internal/service/internal/flow/promote/stackfile.go`, `VolumeConf`);
  schedules are panel and CLI only. A setup kept in git is then not
  complete: rebuilt from the files, no volume has a schedule. Ask: a
  `backup:` body on a volume (schedule, destination, retention), exported
  and planned like the rest. He asked about limits and snapshots on
  volumes too: neither exists (only buckets have `quota_gb`).
  Raised again 2026-11-18 when the six media config volumes needed
  schedules by hand: "fuck same we need to fix that as well, yet another
  todo". No longer "later".

- **Deleting the last org: the panel hides the reason, and the rule itself
  is doubtful.** He tried to delete `homelab`, the only org. The panel
  only said it could not; the reason ("The last organization cannot be
  deleted.") showed only in the CLI. Bug: the panel must show the
  service's message. Then the rule
  (`internal/service/internal/leaf/org/org.go`, `Delete`): no technical
  reason is recorded. The only trace is a comment carried over from v0
  that it is "about what the setup wizard needs to leave behind"
  (`docs/rewrite/extracts/org-rules.md`). A fresh install already runs
  with no finished org and shows the wizard, so deleting the last org
  could do the same. Ask: either allow it and land on the create-org
  wizard, or keep the rule and say why in the message.

- **Binding the org's config repo is all hand typing.** Org settings →
  Config as code: Connector is a dropdown, but Repository, Branch and
  Config path are free text, although the connector already lists the
  account's 113 repos. His words: "do i need to go in and manually type
  these? i think we need to make a modal or smth with a walkthrough". The
  Branch placeholder says `main` while his repo's default is `master`
  (empty does use the default, but the placeholder reads as a value).
  Ask: a short guided dialog: pick the repo from the connector's list
  (search), pick the branch from the repo's branches with the default
  preselected, find `stackr-org.yml` in the repo and say whether it is
  there, then show the plan. Same pattern wherever a repo is typed (stack
  config repo, tile build source).

- **A stack with no environments, and nothing says why.** After the org
  plan was approved, stack `infra` showed "This stack has no environments
  yet." and he asked "where is the stack plan?". Two things. (1) There is
  no stack plan at that point: the org apply binds the stack's file and
  queues a push job (`pushBound` in `internal/service/orgconfig.go`), and
  the only trace is that job's log. (2) The file said `prod: {}`; an env
  with no `branch:` or `from:` is skipped with one log line ("the file
  gives it no branch or promote; not made", `ladderEnvs` in
  `internal/service/jobs.go`). The empty stack page shows none of that.
  Ask: the org plan should list what each stack file will make (envs,
  tiles) and block or warn on an env that will not be made; the empty
  stack page should show the last push's result; the config guide needs a
  minimal stack file example with `branch:`.

- **A waiting release is invisible, and there is no plans page.** With
  `auto: false` the push made env `prod` and (expected) a release that
  waits for a promote. The stack canvas showed the env card "0 tiles,
  idle"; the env page "No tiles in this environment yet." Neither says a
  release is waiting or where to promote it; the releases list sits in a
  drawer tab (`internal/web/handler/canvas/releases.go`). His words: "why
  tf do we not have an indicator somewhere or a plans page?". Ask: (1) a
  chip on the env card and a line on the empty env page, "Release N is
  waiting. Promote", linking to the plan; (2) one place per org that
  lists everything waiting on a person: org file plans to approve,
  releases to promote, deploys parked on an unset secret, each with its
  plan one click away.

- **"Promote to prod" is the only way to see a release's plan.** Env
  drawer, Releases tab: the row's only action is a button reading "Promote
  to prod". It does not deploy; it opens a confirm with the "What changes"
  list (`releasesTab` in `internal/web/handler/canvas/releases.go`). He
  would not click it to look: "that is sooo bad wtf", "that should have
  been like 'review plan'", "we need a separate tab or screen for this,
  and see the plan without the wording 'promote to prod'". Ask: the
  release row offers "Review plan"; the plan opens on its own screen or
  tab (terraform style: what is added, changed, removed, blockers), and
  the Promote button lives on that screen only. Goes with the plans page
  item above.

- **Plan rows say too little and cannot be opened.** The promote plan for
  the first Postgres read: `+ create pg  operated`, `~ operated pg allow
  +homelab:*`, `~ operated pg pitr_days 7`. Not shown: the engine, the
  major (18 in the file), the image, the node it lands on, the volumes it
  makes. The two `~` rows are settings of a tile the same plan creates,
  yet read as changes to an existing one. His ask: "i should be able to
  click onto a row and see what the row is, some more details maybe".
  Ask: a row opens to its full detail (for a create: every field with its
  value and where it lands; for a change: old and new); settings of a new
  tile fold under its create row.

- **The operated tile's Overview preselects a downgrade.** Tile `pg`
  (postgres:18.6, newest major): the "Postgres version" picker opens on
  "Downgrade to 15" with a red "Change version" beside it, on the first
  tab, right under the image line. Nothing is wrong, yet the first thing
  the page offers is the most drastic move. Seen by Claude on his
  screenshot, not raised by him. Ask: no preselected target ("Change
  major..." as an empty choice), or move the control out of Overview.
- **Postgres logs `FATAL: database "stackr" does not exist` on every
  readiness check.** `pg_isready` ran with `-U` and no `-d`, so it asked
  for a database named after the admin user. Fixed 2026-11-17 in
  `internal/service/internal/flow/operated/postgres.go` (`Ready` now
  passes `-d <admin db>`), uncommitted.

- **Settings shown as always-open inputs; applied, default and placeholder
  look the same.** Tile `pg`, Backups tab (and, his words, "the rest of
  the screens like this"). The applied value "Days to keep 7" sits in the
  same kind of input as the empty "Schedule backups" form below it, whose
  fields are prefilled with form defaults (UTC, Database dump, local disk,
  7, Pause tiles) that are applied to nothing. "i cant tell from this if
  these settings are applied, or defaults or placeholders". And: "all of
  these inputs are ugly as fuck". His design: show a value as a fact, no
  input; a pencil or a double click swaps it to an input; after editing,
  show clearly what changed before an Apply or Save button. Ask: read
  mode by default across settings screens, edit in place, a pending
  changes summary before save; "add a new one" forms closed behind a
  button so their defaults never read as state. Belongs to the settings
  screens rework.

- **A plan waiting after your own push looks like a fault.** After he
  pushed the org file with the media stack, every page showed the amber
  problem banner with a warning triangle: "The config file of homelab
  differs from the install: 3 to add  Since Nov 17, 21:43", link "Open
  overview". His words: "that kinda looks like smth is wrong tho". It is
  the drift banner, used here for the normal case: a plan made a minute
  ago by a push, waiting for approval. Ask: a neutral notice for a fresh
  plan ("A plan is waiting: 3 to add. Review"), linking to the plan
  itself; keep the warning look for real drift (the install changed
  behind the file, or a plan left unapproved for long).
- **No way to put existing data into a volume.** Moving the media tools
  needs their old config (9 GB) inside stackr's volumes. There is no
  import: no `stkr volume` verb beyond add, ls, rm; a backup restore only
  takes stackr's own runs. The way round is copying into the Docker
  volume (`stackr-vol-<id>`) on the node by hand with the tile stopped.
  Ask: `stkr volume import <volume> <dir or tar>` and a panel upload, with
  an owner option, refusing while a tile that mounts it runs.

- **The org's Config as code tab is one long open form.** Export card,
  then the whole binding form (connector, repository, branch, path, auto,
  Save and plan, Plan now, Unbind), and only under it the plan that waits
  for approval, then the plan history. His words: "again this interface
  looks shit the config as code should be hidden behind an accordeon,
  when configured it should just show in small amounts whats its done".
  Ask: once bound, show a few facts (repo, branch, file, last plan and
  its state); the form and the export fold away; the pending plan is the
  first thing on the tab.

- **BUG: the pending-plan bar stays after the plan is approved.** Org
  canvas, bar "Config plan pending: 3 to add. View". He approved the
  plan; the media stack appeared on the canvas, the bar stayed. "i had to
  refresh for that top bar to dissapear". The panel never shows stale
  state: the bar must go (and the warning banner on the other pages)
  the moment the plan is applied, over the existing live updates. Also
  two looks for one fact: a plain bar on the org canvas, the amber
  warning banner elsewhere.

- **A failed push shows nowhere in the panel.** The media stack's first
  push job failed ("build media-bootstrap: buildx build: exit status 1").
  The stack card said "idle", the env page "No tiles in this environment
  yet", the Releases tab "No releases yet. The first deploy makes one."
  The failure was found only with `stkr job ls --stack media`. His words:
  "if smth is being rejected.....why cant i see it on the main web ui?".
  Ask: a failed push or build is shown on the stack card, the env page
  and the Releases tab, with the error line and a link to the job log.
- **Job ids are full UUIDs in the CLI.** `stkr job ls` prints
  `f0fb3544-92e4-4690-8588-da6c26476b86`, to be pasted into `stkr job
  log`. His ask: "maybe these jobs should have a short id? or we can use
  the short id to resolve them?". Ask: a short id column, and every
  command that takes an id accepts a short id or a unique prefix.

- **`build.dockerfile` is counted from the repo root, not from
  `build.context`.** A tile with `build: {context: stacks/media/bootstrap}`
  and its Dockerfile in that folder failed: "failed to read dockerfile:
  open Dockerfile: no such file or directory". stackr looks for
  `Dockerfile` at the repo root unless `dockerfile:` gives the full
  repo-relative path (`buildTile` in `internal/service/wiring.go`).
  Docker, Compose and every CI default to the Dockerfile inside the
  context. AGREED by him 2026-11-17 (on the list, he picks the order):
  with no `dockerfile:` given, look in the context folder first, then
  fall back to the repo root, so tiles that rely on today's behaviour
  keep building; an explicit `dockerfile:` stays repo-relative; the
  error names the paths it tried.

- **A drawer opened full screen just stretches.** Org settings, People
  tab, expanded with the arrows button: every input, the role picker and
  the cards run the full width of a 2000 px window. His words: "bro this
  is shit ui, we cant be just stretching it lol". Ask: cap the content
  width, or use columns when there is room.
- **The Settings link changes sides between levels.** Stack and env pages:
  "Settings" on the left, after the breadcrumb. Org page: "Settings" on
  the right of the top bar, beside "+ Create stack". "why is this on a
  different side compared to the org level settings? sloppy ui that is".
  Ask: one place on every level.
- **Elevated access waiting for approval is hard to find.** Tiles showed
  red `source` and `device` badges and "not deployed"; nothing said what
  to do. The approval is a Host access section inside the stack drawer's
  Settings tab; the only pointer is a small "host access" chip in the
  drawer header, seen after opening the drawer. He asked "here? where".
  Ask: a line on the env page and stack card ("5 tiles wait for a server
  admin to approve access. Review") that opens the approval, and the same
  item on the waiting-on-you page.
- **To check: release 2 "image watch" is on prod with `auto: false`.**
  Nine minutes after release 1 (git push), the media stack's Releases
  listed "Release 2, image watch" already on prod, and release 1 offered
  "Roll back prod". The env was declared with `auto: false`. Either image
  watch releases are meant to go straight in, or the gate was skipped.

- **The only job list is under Server settings.** A job's log has a page
  (`/<org>/<stack>/<env>/-/jobs/<id>`, `internal/web/handler/env/handler.go`),
  and Server settings → System → Jobs lists jobs for admins (Claude
  first told him there was no list at all; his screenshot of the admin
  menu showed it). Nothing on a stack, env or tile lists or links its
  jobs, so he found a failed promote with `stkr job ls`. A
  promote that failed on its first tile left all eight tiles "Crashed"
  with no reason on the canvas. He asked: "can we get access to these job
  logs on web ui?". Ask: a Jobs list per env and per stack (state, kind,
  error line, link to the log), and the failed job linked from every tile
  it left undeployed. Goes with "a failed push shows nowhere".
- **A promote that fails on one tile marks every tile "Crashed".** Tiles
  the job never reached show the same red "Crashed" as the one that
  failed. Ask: "not deployed: the promote stopped at bazarr" on the
  others, and the error on the one that failed.

- **The error of a failed job cannot be read in full.** Three tries: the
  job log stops at "creating stackr-bazarr-..." with no error line;
  `stkr job ls` cuts the ERROR column to the terminal width; `stkr job
  get <id>` cuts the `error` field the same way (`fit` in
  `cmd/stkr/app.go` trims every cell, also on a single-record view). Only
  `--json` gives the whole text. His words: "what the fuckkk, yeah thats
  another thing we need todo". Ask: the job log ends with the error that
  failed it; `job get` (any single-record view) never trims, it wraps;
  `job ls` keeps the end of an error, not only its start.

- **An NFS share with no options does not mount: "connection refused".**
  Share `media`, `kind: nfs`, `source: 192.168.1.102:/mnt/tank/media`, no
  options. Deploy of the first tile that mounts it failed: "failed to
  mount local volume: mount :/mnt/tank/media:..., data:
  addr=192.168.1.102: connection refused". stackr hands Docker
  `type=nfs, o=addr=<ip>` and nothing else (`ShareMount` in
  `internal/service/internal/leaf/volume/share.go`); the kernel then
  mounts NFS 3, which needs rpcbind and lock services on the node, and a
  stackr node runs neither. Likely cause, not proven on the node.
  Workaround in the org file: `options: nfsvers=4.2`. Ask: default NFS
  shares to version 4 when no `nfsvers`/`vers` is given; test the mount
  on a node when a share is saved or planned; turn "connection refused"
  into a message that says what to try.

- **BUG: "Open overview" on the pending-plan banner loops.** Banner: "The
  config file of homelab differs from the install: 2 to change", link
  "Open overview" → `/-/admin/overview`. That page lists the same line
  under Open problems with its own "Open overview" button, which points
  at the page it is on. "clicking on 'open overview' does literally
  nothing". Ask: the banner and the problem row link to the org's Config
  as code tab, where the plan and its Approve button are.

- **The promote plan says "Blocked" on elevated access and offers no way
  to unblock.** After the shares got an option, the grants on them were
  dropped (by design). The promote dialog showed a red "Blocked: elevated
  access: bazarr share:media@192.168.1.102, ..." box: seven grants as one
  run-on sentence, no Approve button, no link to Host access, and the
  Promote button still lit. Also every tile row reads "~ image bazarr,
  not deployed yet", a change marker for something never deployed. Ask:
  a server admin gets an "Approve access" action in the box (or ticks, as
  the first promote had); others are told who can approve; one line per
  grant, grouped by tile; Promote is disabled or relabelled while
  blocked. Correction after reading `promote/hostaccess.go`: a plan
  blocked only on access is meant to be promoted; the job parks and only
  then does Host access show Approve. The box must say so.

- **"Building" is the label for any deploy in progress.** Tiles read
  "Online · Building (01:22)" while their image was only pulled and
  started (`word` in `internal/ui/graph/cards/view.go`: status
  `building`). He asked: "when they say building, what does it mean? bc
  its not actually building right?", then: "we need to actually reflect
  the real status lol, that building is a plain lie". Ask: each tile
  shows what is happening to it (waiting its turn in the promote,
  pulling the image, starting, online); "Building" only while an image
  is built.
- **BUG: tile chips run out of the card.** The `bazarr` tile carries
  "service", "source" and "new version"; the last chip sticks out past
  the card's right edge. The chips are `shrink-0` in a row that neither
  wraps nor clips (`internal/ui/graph/cards/cards.templ`, the card top).
  Also: the red "source" chip explains nothing; it means approved
  elevated access tied to the image and command
  (`internal/ui/access/access.go`).
- **A declared but unset secret is not listed.** In the env's Variables
  settings the `gate` group shows only `user`; `password` is declared in
  the stack file (`{type: secret}`) and missing from the grid, so its
  name has to be known and typed under "+ Add variable". Ask: list every
  declared name with an empty cell marked unset, and say which tiles
  wait on it.
  AGREED (2026-11-18): "we should show the param/key there, just show
  it empty if its empty".
- **The "+ Add variable" form explains nothing.** A name box, a
  "Secret?" button, and a box labelled "prod" whose placeholder reads
  "unset". Nothing says that box is the value, or that "Secret?" is a
  toggle to press before typing. He sent it with "???". Ask: labels
  "Name" and "Value in prod", a visible secret switch with its state.
- **Browser autofill pops up on the params form.** Typing `password`
  into the name box brought up the browser's "New Email Alias" offer.
  He: "we should probs make it so those browser popups dont happen".
  Ask: autofill off on every params and secrets input (autocomplete off
  plus the password manager ignore attributes);
  `internal/ui/drawer/vars/grid.templ`, the add form's name box has no
  autocomplete attribute at all.
- **Two GitHub connectors for one account.** The org connector made on
  day 3 cannot fetch the server file; Config as code on the server says
  "Create a server connector first" (no link to Connectors). He:
  "ideally i want just one connector". A server connector shared with
  every org covers both, but an org connector cannot be moved up to the
  server; it has to be made again and the org rebound. Ask: "make this
  a server connector" on an org connector for a server admin, and a
  link from the Repository box to Connectors.
- **BUG: an applied server plan still reads as work to do, and the card
  does not update.** After Save and plan with auto apply ticked, the
  line above the plan said `server-apply · done` while the plan card
  still said "pending", "Applying: approved, the changes are being
  applied." and "+ create route" rows, until a reload. He: "i should
  not have to reload page, should be reactive", and on the rows: same
  issue as the connector, "it shows as an addition u do rather then
  'its been added'". Ask: the card updates by itself; a finished plan
  reads as done ("created route", applied when and by whom).
- **Server file applied (2026-11-18):** three routes (vms, s3, pihole on
  vulpe.dev) through a server connector shared with every org.
- **Tile Logs tab: controls touch each other.** The Live / History
  buttons sit right on the search box and level picker, no gap, and the
  log box starts right under them. He: "the ux and design here is shit,
  look how elements are touching at the top". Also seen: each line shows
  the time twice (stackr's stamp, then the app's own).
- **Jellyfin came up on the imported config (2026-11-18):** its log
  shows migrations applied to the old database and the old library
  folders being watched.
- **A failing certificate is invisible in the panel.** After the router
  moved to the controller, `jf.vulpe.dev` gave ERR_SSL_PROTOCOL_ERROR
  for about ten minutes: the proxy had failed five http-01 checks before
  the move and was waiting 600 s to retry. The tile said Online, the
  Proxy card lists routes with a plain "https" chip, and the only place
  to see "challenge failed ... retrying_in 600" was `docker logs
  stackr-proxy` on the controller. Ask: per host certificate state on
  the Proxy card and the tile's domain row (waiting, failed with the
  reason, next try, valid until), a "try again now" button, and the
  proxy's log readable in the panel.
- **Switch-over done (2026-11-18):** router 80/443 to the controller
  (192.168.1.103); certificates for the vulpe.dev hosts issued.
- **Nodes map: the NAS sits outside its own LAN box.** The S3
  destination `nas` is drawn below the 192.168.1.0/24 box although its
  address is in that network. He: "if we know that nas is in the same
  lan, we could put it in the box". Ask: a destination, share or route
  target whose address is inside a drawn network goes inside that box.
- **A secret cannot be shown again in the panel.** The grid offers only
  "Replace"; `stkr params get --reveal` is the only way to read one
  back. He, while checking the gate password: "thats retarded, we need
  to be able to show the pass again". Ask: a reveal control on a secret
  cell for someone with write access, logged like the CLI's.
- **BUG (blocks the media tools): a tile's password gate cannot be
  used with a secret, and its user name is never resolved.** Found
  2026-11-18 on prod, v0.8.26. The five gated media tools (sonarr,
  radarr, prowlarr, bazarr, rdt on vulpe.dev) answer 401 from Caddy
  (`www-authenticate: Basic realm="restricted"`) for the right login.
  Jellyfin, which has no gate, is fine.

  The setup, as the docs ask for it (docs/guide/config-files.md,
  "Secrets": `proxy.basic_auth.password` must be one `${{ }}` ref):

  ```yaml
  params:
    gate:
      prod:
        user: darthvader
        password: {type: secret}
  # on each tile's domain
  proxy:
    basic_auth:
      user: ${{ params.gate.user }}
      password: ${{ params.gate.password }}
  ```

  Two faults:

  1. **A secret password is refused.** A tile's route resolves its refs
     with `params.InDomain`, on a snapshot loaded without secrets
     (`tileRoute` in `internal/service/internal/flow/deploy/proxy.go`).
     `InDomain` means "an address is public, never a secret"
     (`allowed` and `Expand` in
     `internal/service/internal/leaf/params/ref.go`). That rule is for
     the host name, but the gate password goes through the same
     resolver. `credentials` in
     `internal/service/internal/leaf/domain/caddy.go` then puts in the
     lock: user `locked` and a hash of random bytes.
     Proven with a throwaway test on the resolver: a secret gives
     "gate.password is a secret, and a domain is public", an unset one
     gives "param gate.password is not set"; only a plain value passes.
  2. **The user name is never resolved.** `credentials` expands only
     the password; `a.User` is used as written, so a ref stays the
     literal text `${{ params.gate.user }}`. Read from the code, not run.

  What was ruled out on prod: the stored password is right (sent
  straight from `stkr params get --reveal` with curl: 401), a redeploy
  of the tile changes nothing, and the literal user text with the stored
  password is refused too (expected while fault 1 locks the route).

  Open: `docker logs stackr` on the controller has no "basic auth
  password did not resolve; route locked" line, although `credentials`
  logs one with `slog.Warn`. Not explained.

  Also wrong around it:
  - Nothing says the address is locked: not the plan, not the tile, not
    the Proxy card.
  - `tileReads` in `internal/service/params.go` does not count a
    `basic_auth` ref as a reader, so saving the value rebuilds nothing.

  Fix plan (not started, waits for a go):
  - `leaf/params/ref.go`: a place kind for the gate login; plain and
    secret params allowed, nothing else.
  - `flow/deploy/proxy.go`: a second resolver per tile route for the
    gate login, loaded with secrets.
  - `leaf/domain/caddy.go`, `caddy_auth.go`: user and password resolve
    through it; host names keep the strict rule.
  - `service/params.go`: a gate that reads a changed value rebuilds the
    proxy rules.
  - Tests: secret password works, user ref works, a host name still
    refuses a secret.
  - Then a release and a prod upgrade.

  Stopgap, not taken: `user: darthvader` as plain text in the file and
  `gate.password` as a plain variable (readable in the panel, lands in
  an exported stack file).
- **Changing a value should say what it restarts, and ask.** Today a
  save redeploys the tiles that read the value at once, with one line of
  small print above the grid, and misses a gate that reads it (bug
  above). He: "maybe when we change vals like this we should be
  prompted (like railway does) that we need to restart that tile". Ask:
  after a change, list the tiles and gated addresses it touches and
  offer to apply now or keep it staged, Railway style.
- **A domain's basic auth is not in the panel at all.** The tile's
  domain rows and the "+ Add domain" form (`domains`, `addDomain` in
  `internal/ui/drawer/tile/tile.templ`) offer path, redirect, forward
  auth URL and HTTPS; `proxy.basic_auth` from the stack file is neither
  shown nor editable, and the Proxy card lists a gated host with the
  same plain "https" chip as an open one. He asked: "where can i see
  basic auth options in the ui?". The only password gate the panel
  knows is the Protect setting (server, org, stack, env). Ask: show
  "gate: <user>" (and locked, with the reason) on the domain row and
  the Proxy card; edit it in the domain form.
- **Look at Postgres 19 for the operated tile (asked 2026-11-18).** The
  tile takes majors 14 to 18 (`Major` in
  `internal/service/internal/flow/promote/stackfile.go`); the vchord
  recipe is 17 and 18 only
  (`internal/service/internal/flow/operated/pgrecipes.go`). Check
  whether 19 is released, its official image, each built-in recipe, and
  the upgrade from 18. Came up because Immich's docs name a range up to
  below 20.
- **A server file plan should show on the cluster chart.** A
  server-level config as code plan lives only under Server settings,
  Config as code. He (2026-11-18): "config as code plans on the server
  level should come up on the cluster's chart". Ask: a marker on the
  chart that a plan waits or is being applied, and what it adds or
  changes drawn in place (a route, a node, a placement). Goes with the
  "one page of everything waiting on a person" ask.
- **`stkr params import` cannot rename.** The old `.env` names
  (`SONARR_API_KEY`) are not the declared names (`sonarr`), so a wrapper
  script was needed (`scripts/media-secrets.sh` in the homelab repo). A
  name set twice in the file is sent twice without a word; the old file
  had two different values for two keys.
- **The NFS fix worked.** With `options: nfsvers=4.2` on both shares the
  media tiles mounted and came up on the home node (2026-11-17).

### What worked

- GitHub connected with a personal account (organisation field left empty):
  "installed · 113 repos".
- The panel's own name record was written by the upgrade; `stkr.fyrmforge.dev`
  answers with the public address, no hosts file workaround.

### For the new media stack (found while sorting the old disk)

- Fake releases (`.exe`, `.scr` named like episodes) got downloaded by the
  old Sonarr. Block those extensions in Sonarr/Radarr or the download
  client up front.
- Old Radarr looped: about 13 rips of one film, 7 of another, none
  imported (1.2 TB). Find why imports failed before the same settings are
  carried over.

### The move, written up (2026-11-18)

- **Move RFC on fraedi:** "RFC: moving the home lab from compose stacks onto
  stackr", bb11d6b2-68b6-47ac-a538-aba8cbce0cc6, Projects/stackr/rfc. A
  living record: method per stack, order, switch-over, mistakes, what is
  left. Update it as each stack moves.
- **Auth stack moves before Immich (agreed 2026-11-18).** LLDAP and
  Authelia as stack `auth` in the homelab repo (files written, not pushed
  yet). After it: the media tools go from the broken password gate to
  `forward_auth`, and the `auth.vulpe.dev` passthrough route comes out.
- **YAML style:** he wants block style only, no one-line `{...}` or `[...]`.
- **Immich is a major version behind (2026-11-18).** Old stack runs
  v2.7.5, the app offers v3.4.1. Plan: move on v2.7.5 first, then upgrade
  as its own step after reading the v3 release notes (breaking changes not
  checked yet). To add to the board's Immich note at the next board write.
- **Immich backed up to TrueNAS (2026-11-18).** Photos: 59,690 files,
  159 GB, `tank/backups/immich/photos` (file counts match the old VM).
  Database: `pg_dumpall` taken 13:38 on v2.7.5, `immich-db.sql.gz` (64 MB),
  copied by him to `tank/backups/immich/`. The earlier `16tb-old` copy had
  excluded `/stack_volumes`, so this is the first copy of Immich anywhere.
  Checked in Immich's source: v2.7.5 and v3.4.1 both accept Postgres 18 and
  VectorChord below 2; v3 drops pgvecto.rs and refuses plain-http OAuth.
- **Immich upgraded on the old VM first, his call (2026-11-18 14:44).**
  v2.7.5 to v3.4.1 with `stackr immich update` on the old VM. All 38
  migrations succeeded. One schema drift warning on `geodata_places`
  printed while the geodata import was still running; to confirm gone with
  `immich-admin schema-check`. v3 made a new key under
  `/data/backups/yucca` (its new backup feature), so the move must copy
  the photo folder again, whole. Database extensions in use: vchord 0.4.3,
  vector, cube, earthdistance, pg_trgm, unaccent, uuid-ossp (no pgvecto.rs).
- **Old Immich taken down for the move (2026-11-18, his call).** v3 dump
  `immich-db-v3.sql.gz` and a `--delete` top-up of the photo copy to
  `tank/backups/immich/`, then `stackr immich tear-down` on the old VM. He
  reported "done"; I did not see the outputs. `pics.vulpe.dev` is down
  until Immich runs on stackr. Auth stack files pushed (homelab 5dece5d).
- **Backlog: the org chart draws no lines between stacks (asked
  2026-11-18).** Org chart with `media`, `auth`, `infra`: only the proxy
  and connector lines show. `media` has a database slice on `infra`, and
  nothing on the chart says so. Wanted: reference arrows between stacks
  that use each other (slices, later forward auth). Screenshot on the board.
- **Auth data backed up (2026-11-18):** old `auth` stack torn down, both
  data folders (6 files, 8.5 MB) in `tank/backups/auth/`.
- **Worked as it should: a tile cannot take a host a server route holds.**
  Promote of `auth` #1 was blocked: "tile authelia: auth.vulpe.dev is an
  external route". Clear and early. Gap: the dialog does not say where to
  remove the route (Server settings, or `stackr-server.yml`).
- **A plan whose only change is a removal can be "applied" and do
  nothing (2026-11-18).** Server plan e20fe28: one line, `route-delete
  auth.vulpe.dev`. It ended as "applied" with the line marked "kept" and
  the header still reading "1 removal to review". The route stayed, so the
  `auth` promote stayed blocked, with no hint why. Wanted: approving with
  no tick on a removal-only plan should ask ("nothing will change, tick
  the removal?"), an applied plan should not say "to review", and the
  tick box needs to be hard to miss. To check: whether auto apply did this
  (its label says it skips plans with removals) or an approve without the
  tick. Also: the blocked promote dialog shows no button and no way to
  the fix. Board: next write.
- **BUG, fixed locally: Trusted proxies cannot be cleared (2026-11-18).**
  Server settings → Panel and proxy: empty the field, Save says "Saved.",
  the old value is back. Cause: `Set` in
  `internal/service/internal/leaf/settings/flat.go` deletes the row on an
  empty value, and `Get` then falls back to the installer's boot value
  (`CADDY_TRUSTED_PROXIES` in the env file). Fix: `EmptyOff: true` on the
  knob in `catalogue.go`, test `TestTrustedProxiesClear`. Not committed,
  not released. Stopgap on prod: enter `127.0.0.1/32`. Second fault, open:
  the form says "Saved." when nothing changed.
- **A failed deploy card in the tile drawer cannot be opened
  (2026-11-18, follow-up to "The only job list is under Server
  settings").** Tile drawer → Jobs tab, `templ Jobs` in
  `internal/ui/drawer/tile/tile.templ`: each row is a plain `<li>`, no
  link. The job log page exists (`GET /:org/:stack/:env/-/jobs/:job`,
  `internal/web/handler/env/handler.go`) and holds the crashed
  container's last 20 lines, but `JobRow`
  (`internal/ui/drawer/tile/view.go`) carries no href. Fix: an `Href` on
  the row, set in `jobsView` (`internal/web/handler/tile/views.go`), about
  5 lines. Second point: the red line is the LAST log line (`lastWords`
  in `internal/service/internal/leaf/tile/world.go`), which for Authelia
  was an info line, not the reason; and the failed container is removed,
  so `docker logs` has nothing either.
- **Auth stack moved (2026-11-18 16:36).** LLDAP and Authelia Online on
  stackr with the old data; sign-in at `auth.vulpe.dev` works. First try
  failed: Authelia started before LLDAP listened. `depends_on: lldap` is
  order only; `lldap:healthy` waits (homelab ea23b80). The wait itself was
  not exercised: LLDAP was already healthy at the re-deploy. Open: after
  sign-in Authelia sends you to `dashy.vulpe.dev`, which is gone (default
  redirect in `stacks/auth/authelia.yml`); Dashy itself is not moved and
  its OIDC client was dropped.
- **No jobs page in the panel (2026-11-18).** Jobs are listed only on a
  tile's Jobs tab (rows do not open) and as "last job" lines. `stkr job
  ls` has no panel twin for an env, a stack or the server. The earlier
  note "the only job list is under Server settings" was wrong: that page
  is the worker settings. Wanted: a jobs page per env and for the server,
  rows open the log.
- **First-start data swap needs a deploy first.** A volume declared in the
  stack file has no Docker volume until the first deploy, so old data
  cannot be put in before the tiles have started once on empty volumes.
  Wanted with "import data into a volume": make the volume at plan apply.
- **Backlog: investigate cross-stack references (asked 2026-11-18).**
  Came up with Dashy, which links to every stack. A service tile
  publishes `public_url` (`Endpoint.Outputs`,
  `internal/service/internal/leaf/params/ref.go`), but `tile.<slug>` only
  reaches the same env and `stack.<slug>` refs were removed (DECIDE 194).
  To look at: a read-only ref to another stack's public address (not a
  secret), for dashboards and OIDC redirect lists. Until then: written-out
  addresses, or one org variable for the domain. Dashy's `conf.yml` stays
  written out for now (his call).
- **Backlog: keep the "Internet · outbound" box on the env canvas (asked
  2026-11-18).** Today the box and its line to a tile appear only while
  the tile reaches out, and vanish after. Wanted: box and line stay, for
  observability; only the traffic markers (the B/s labels and the moving
  dashes) show while traffic flows. Then a drawer on the box: which tile
  reached out and when (outbound metrics). Screenshot on the board.
- **BUG: a changed mounted file is not deployed (2026-11-18, seen on
  prod).** Push 445195d changed only `stacks/auth/authelia.yml`, the
  source of a `files:` mount on tile `authelia`. Promote and restarts
  later, the container still had the old file (`grep -c 'client_id:
  dashy'` in the container = 0). Cause, read in the code: the promote plan
  marks a tile for redeploy on a changed row, a moved image pin, a new
  slice and a few more (`w.redeploy` in
  `internal/service/internal/flow/promote/plan.go`); the config commit's
  own pin is skipped (`c.Slug == release.ConfigSlug`), and nothing
  compares the content of a tile's `files:` sources between the old and
  the new commit. The file volume is only rebuilt when the tile deploys
  (`fileMounts`, `internal/service/internal/flow/deploy/mount_files.go`).
  A restart reuses the mount. Fix idea: in the plan, for each tile with
  `files:`, compare the sources' content at the env's current config
  commit and the release's; a difference is a plan row ("file changed")
  and a redeploy. Stopgap: Redeploy the tile by hand. Board: next write.
  Confirmed 2026-11-18 17:0x: after `stkr deploy authelia --stack auth
  --env prod` the container had the new file (grep = 1). The promote to
  release #8 before it reported "done" and changed nothing. The tile
  drawer has Restart but, as far as its templ shows, no deploy button.
- **Dashy stack added (homelab 445195d):** own stack `dashy`, config
  written out, OIDC client `dashy` back in Authelia's config.
- **Mounted file bug, fix direction (his call 2026-11-18: backlog).** "A
  mounted file is part of 'config changed', so we should check, maybe
  hold a checksum of each file we mount." Outline agreed: no stored
  checksum, git already has one. Plan compares each `files:` source's git
  object id at the env's commit and the release's commit: a "file
  changed" row plus a redeploy. Second step: compare the file volume the
  replica mounts with the one the release would make, to catch drift.
  Dashy works (2026-11-18).
- **Backlog: external routes need auth and the other proxy options
  (asked 2026-11-18).** A server route is host, mode, target, insecure
  (`Route` in `internal/service/internal/flow/serverconfig/file.go`). A
  tile address has forward auth, password gate, headers, body size,
  timeouts and more (`ProxyConf` in
  `internal/service/internal/flow/promote/stackfile.go`). Wanted: the same
  set on a route, in `stackr-server.yml` and in the panel. Blocks
  `nas.vulpe.dev` (192.168.1.102:80) and `s3-console.vulpe.dev`
  (192.168.1.102:30293), both behind Authelia on the old Traefik. He ruled
  out a proxy tile as a workaround; TrueNAS is reached on the LAN until
  this exists.
- **App data on TrueNAS (decided 2026-11-18):** a parent dataset
  `tank/apps` with one child per app that needs bulk storage
  (`tank/apps/immich`, later `tank/apps/opencloud`), each child its own
  NFS share. `tank/media` and `tank/downloads` stay top-level.
- **Far backlog: a TrueNAS connector (asked 2026-11-18).** While making
  `tank/apps/immich` by hand (dataset, owner, NFS share for the node):
  stackr could ask TrueNAS to allocate these when a stack declares app
  storage. Fits connectors as the plug-in point (Dozzle was the first
  non-git case).
- **Immich move, state 2026-11-18 17:48.** `tank/apps/immich` made
  (owner darthvader:darthvader 3000:3000, other none), NFS share for
  192.168.1.104 only, photo copy from `tank/backups/immich/photos`
  running. Second dump taken for the slice: `pg_dump --no-owner --no-acl
  -Fc` of the old database (v3.4.1), `immich-v3.dump`, 66 MB, 662 items,
  in `tank/backups/immich/`. The first dump was `pg_dumpall`, which must
  not be loaded into the shared Postgres (drops `postgres`, adds a
  superuser). Lesson for the move RFC: for a slice, dump one database
  without owners from the start. Files written, not pushed:
  `stacks/immich/stackr-compose.yml`, share `photos`, seven extensions on
  `infra:prod:pg`. Routes `nas` and `s3-console` forwarded without a
  login (homelab 72e568f), his call.
- **Future: an operated Redis/Valkey (asked 2026-11-18).** Seeing the
  plain `valkey` tile in the Immich stack: "in the future we'll want to
  make an operator for this". An operated engine next to postgres, with
  slices per app.
- **Immich photos verified (2026-11-18):** 59,690 files on the old VM, in
  `tank/backups/immich/photos` and in `tank/apps/immich`.
- **BUG: the extension name `uuid-ossp` is refused (2026-11-18).** Immich
  promote blocked: `slice immich-db: extensions: "uuid-ossp" is not an
  extension name`. Cause: `extName` in
  `internal/service/internal/flow/operated/pgext.go` is
  `^[a-z][a-z0-9_]{0,62}$`, no hyphen, and `uuid-ossp` is a stock
  Postgres contrib extension. The comment says names reach SQL only
  through `quote`, which escapes nothing, so the fix is the pattern plus a
  check that the quoting is safe for a hyphen. Stopgap: left off both
  lists; the dump's own `CREATE EXTENSION IF NOT EXISTS "uuid-ossp"` makes
  it at load.
- **Three fixes pushed for v0.8.28 (2026-11-18, by session stackr-97 on
  his go):** `uuid-ossp` accepted as an extension name, trusted proxies
  can be cleared, `pg_isready -d`. origin/master 4ffd6381. Prod needs the
  upgrade before the Immich promote; `uuid-ossp` stays on both extension
  lists in the homelab repo.
- **Prod upgrade 0.8.26 to 0.8.28 rolled back (2026-11-18 18:47).** Update
  tab: "Rolled back: the new panel did not pass its trial." Old panel's
  log: "swap failed, old panel restored: new panel: the container
  restarted during the health check". The rollback itself worked: prod
  back on 0.8.26, proxy blip of about a minute, red "no certificate"
  banners until the proxy was back. Cause not known: the crashed panel's
  log went with its container. Prod skipped 0.8.27 (node disks, 47
  commits), so that code met prod data for the first time. Session
  stackr-97 told. To fix in the upgrade flow: show the reason and the new
  panel's last output on the Update tab; keep the crashed container's log.
- **My mistake, corrected: the panel container is `stackr-<version>`
  (`stackr-0.8.26`), not `stackr`.** `docker logs stackr | grep` printed
  nothing because the container does not exist and grep hid the error.
  The gate warning thought missing this morning is there: 220 "route
  locked" lines. RFC open point 3 (missing log line) is closed.
- **BUG, release blocker: v0.8.27 and v0.8.28 crash at boot on any
  install with an online worker (found 2026-11-18 19:06).** Second
  upgrade attempt with the new panel's log captured: nil pointer in
  `disk.(*Leaf).OfNode` (`internal/service/internal/leaf/disk/disk.go:59`)
  via `nodeDisk`, `diskWatch` (`alerts.go:449`), `refreshNodeClients`
  (`nodeclients.go:601`), `service.New` (`orchestrator.go:593`). In `New`,
  `refreshNodeClients` runs at line 593 and `orch.diskLeaf` is only set at
  line 625. A node that answers at boot reaches `diskWatch` with a nil
  leaf. A single-node install never does, so tests passed. Sent to session
  stackr-97 for a v0.8.29. The upgrade trial and rollback caught it twice
  and prod stayed on 0.8.26. Capture used: a loop of `docker logs -f
  --timestamps stackr-0.8.28 >> file` plus `docker events` to a file,
  started before pressing Upgrade.
- **Prod on v0.8.29 (2026-11-18, about 19:55).** Panel and both workers
  (home-ct, htz-prod) upgraded, no rollback. v0.8.29 = v0.8.28 plus the
  boot fix (disk leaf built before the first node facts read) and a guard
  for a share reconcile queued before the job runner exists. v0.8.27 and
  v0.8.28 are broken for any install with workers.
- **CLI ids: short uuids or unique prefixes (asked again 2026-11-18,
  adds to "Job ids are full UUIDs in the CLI").** `stkr job log 346602d6`
  answered `no job "346602d6"`; only the full uuid worked. Wanted: either
  short ids, or any unique prefix accepted wherever an id is taken (job,
  release, volume), git-style, with "ambiguous, matches N" when it is not
  unique.
- **Seen: the CLI warns when it is older than the server** ("stkr is
  v0.8.26, the server is v0.8.29. Update: go install ..."). Works as it
  should.
- **BUG/gap: an app on a slice is not the owner of its database, and
  Immich needs to be (2026-11-18).** First start of `immich-server` on
  the slice failed: `PostgresError: must be owner of database
  immich_prod_immich_db` (42501). Immich runs `ALTER DATABASE <db> SET
  vchordrq.probes = 1` on every start (`createExtension` in its
  `database.repository.ts`, v3.4.1). stackr makes the database owned by
  the slice role (`CREATE DATABASE ... OWNER s.User`,
  `internal/service/internal/flow/operated/postgres.go`), and the app logs
  in as a per-consumer role with grants only (`pgGrants`; its comment
  already names "a shared owner role (SET ROLE)" as the upgrade). So the
  Immich example in `docs/guide/postgres-extensions.md` cannot work as
  written. Stopgap on prod, by hand as superuser: `GRANT
  "immich_prod_immich_db" TO "immich_prod_immich_db_immich_server"`
  (membership in the owner role). Fix to design: a writer is a member of
  the slice's owner role, or an `owner: true` on one consumer. Also: the
  failed deploy's shown error was again only the last log line
  ("Detected CPU Cores").
- **Immich database loaded into the slice (2026-11-18 20:12).** Dump
  copied into the pg container, extensions filtered out of the list
  (`pg_restore -l | grep -v " EXTENSION "`), then `pg_restore --clean
  --if-exists --no-owner --no-acl --role=<consumer role> -L list`. No
  errors. 85 tables owned by the consumer role, 21,151 assets, 2 users.
- **BUG (his words: "that crash on deploy should be more visible"): a
  tile whose deploy failed inside a promote shows no trace of it
  (2026-11-18).** `immich-server` crashed on its first start during
  promote 346602d6. Afterwards the tile drawer says "not deployed" and the
  Deployments tab says "No deployments yet. Hit Deploy to build the first
  one." The failed attempt, its reason and its log are only on the promote
  job (CLI: `stkr job log`). Wanted: the tile shows "deploy failed" with
  the reason and the container's last output, the Deployments tab lists
  the attempt (also when it ran as part of a promote), and a Retry. Also
  the env canvas should mark the tile as failed, not grey "not deployed".
  Correction of an earlier note: the tile drawer does have a Deploy button
  (header, top right, at least when the tile is not deployed).
- **BUG: an image watch release buries a waiting config release
  (2026-11-18, seen on prod, stack immich).** Releases: #3 git push
  b653b0c (20:51, a `depends_on` change, not yet promoted), then #4 image
  watch (20:53) at commit 56abcb8. The watch derives its release from the
  env's current release (`Releases.Derive(e.ReleaseID ...)` in
  `internal/service/internal/flow/imagewatch/imagewatch.go`), so #4, the
  newest number, carries the OLD config commit. Promoting "the latest"
  (#4) applied no config change, and #3 now only offers "Roll back prod",
  so a newer config push reads as a rollback. Wanted: a watch release
  builds on the newest release of the env's line (keeps the pending
  config pin), or the pending release is re-based; and the promote dialog
  names the commit and says when a waiting release is skipped.
- **Stack Settings button: the drawer takes about a second to open
  (2026-11-18).** No spinner, feels dead. To measure.
- **During a deploy the outgoing container shows as "replica 2" with a
  red dot (2026-11-18).** Reads as if the tile was scaled. Wanted: show a
  rollout as old and new, not as replicas.
- **Immich is up on stackr (2026-11-18 20:44):** all four tiles Online,
  `pics.vulpe.dev` served by the new tile. App checks by him still open.
- **forward_auth cannot name a tile in a way a stack file can use (found
  reading the code, 2026-11-18).** `checkProxyStrings`
  (`internal/service/internal/leaf/domain/domain.go`) allows a private
  host only if it is one of `OrgNames`, and those are the org's REPLICA
  names (`OrgReplicaNames`, `internal/service/internal/flow/deploy/proxy.go`),
  container names like `stackr-authelia-65dfea49-2f230f-1` that change
  with every deploy. A tile slug (`authelia`) is not accepted, and a name
  would not resolve from an ingress node other than the tile's node
  anyway. So the only usable URL is the provider's public address, which
  sends every check out to the router and back in through the proxy.
  Wanted: `forward_auth` to a tile by reference
  (`${{ tile.authelia.url }}` or cross-stack), dialled over the env
  network or the node link. Trial on prod: prowlarr with
  `https://auth.vulpe.dev/api/authz/forward-auth`.
- **Promote plan rows hide the change (2026-11-18, adds to "Plan rows say
  too little and cannot be opened").** Media release #15, five tools from
  `basic_auth` to `forward_auth`: the plan shows "domain bazarr:
  bazarr.vulpe.dev -> bazarr.vulpe.dev, settings change" five times. Old
  and new read the same, the changed setting is not named, a row does not
  open. His words: "another example of shit ui, idk what the changes are,
  i cant click on them to expand and see details". Screenshot on the
  board.
- **To do as one piece of work: the release, plan and promote flow
  (his call 2026-11-18: "we need to work on this ui").** About a dozen
  separate notes are the same screen and flow. RFC to write: "plans,
  promotes and deploys: what you see and what you can do", with those
  notes as evidence. On the board at the top of the evening column.
- **Media tools on forward auth (homelab 96ffc59, promoted 2026-11-18
  about 21:20).** All five addresses point at
  `https://auth.vulpe.dev/api/authz/forward-auth`. Result of the browser
  test not reported yet.
- **A push makes a release in every stack of the repo, changed or not
  (asked 2026-11-18).** Stack `auth`, Releases: #9 to #13 are pushes that
  only changed the media, immich or server files; each offers "Promote to
  prod" and would do nothing. His rule: "if a stack does not have any
  changes (code or config) then we should not show a new release." Cause:
  a push to the config repo pins the new config commit for every stack
  bound to it (`isConfig` in
  `internal/service/internal/flow/promote/push.go`). Wanted: a release
  only when the stack's own file, its `files:` sources or its build
  inputs changed between the commits. Same content comparison the
  mounted-file fix needs. Screenshot on the board.
- **Media tools: Authelia in front works (2026-11-18 21:32).** `curl -sI
  https://prowlarr.vulpe.dev` answers 302 to auth.vulpe.dev. The return
  address after sign-in is wrong (`rd=https://auth.vulpe.dev/`): the check
  goes out through the router (seen as 192.168.1.1) and back in through
  the proxy, which rewrites X-Forwarded-Host. He left Trusted proxies
  empty (the v0.8.29 clear works on prod). Sonarr opens after Authelia;
  Prowlarr showed its own login; `*__AUTH__METHOD: External` pushed
  (homelab 1d10550).
- **Media stack done (2026-11-18 about 21:50, his word: "all good").**
  After promoting 1d10550 Prowlarr opens straight after Authelia; he
  confirmed the other tools and the checks as good. Left for later: remove
  the unused `gate` params block and its secret, regenerate the Sonarr and
  Radarr API keys (they were visible in a screenshot in the chat), volume
  backups.
- **His spec for a releases view (2026-11-18, part of the plan and
  promote flow redesign).** "We used to have a release window here"
  (bottom left of the env canvas). Wanted: a releases window on every
  canvas, collapsible, showing "pending" when minimised. Scopes: stack
  and env canvas = that stack; org level = the org, two views (org config
  releases only, or org config plus the org's stacks); cluster chart =
  the server config. Plus a separate Releases screen with all those
  scopes. (He said "on the stack level we should have two views"; I read
  the two views as the org level, to confirm.) History: v0.5.0 had a
  per-stack Releases page (`releasesPage` in
  `internal/stackrd/handlers/web/handler/project/releases.templ`: ladder
  strip per env, commits, waiting config plans, promote). The rewrite
  kept only the env pill (`templ compare` in
  `internal/ui/graph/graph.templ`) and the env drawer's Releases tab.

### State at the end of 2026-11-18 (for whoever picks this up)

- **Prod:** v0.8.29, panel and both workers. v0.8.30 is released (node
  disks phases 2 and 3); not upgraded, session stackr-97 says no disk
  action has run on a real host yet. Trusted proxies empty.
- **On stackr:** stacks infra, media, auth, dashy, immich; routes vms, s3,
  pihole, nas, s3-console. `nas` and `s3-console` have NO login in front
  (his call, until routes can carry auth).
- **Old VM:** immich and auth stacks down; its data untouched. Fallback
  copies in `tank/backups/{immich,auth,media-config,owncloud}`.
- **Hand-made state on prod that stackr does not know about:** in the
  shared Postgres, `GRANT "immich_prod_immich_db" TO
  "immich_prod_immich_db_immich_server"`. If Immich stops starting with
  "must be owner of database" after a promote, re-check this grant. The
  extension `uuid-ossp` is on both lists since v0.8.29.
- **Homelab repo (master, last commit 1d10550):** the `gate` params block
  and its secret in `stacks/media` are unused, to remove once forward auth
  is settled. Release with the Immich `depends_on` line (b653b0c) may
  still be unpromoted on stack immich; promote by commit, not number.
- **Open for him:** regenerate the Sonarr and Radarr API keys (seen in a
  screenshot in the chat); volume backups (none set); OpenCloud, Pi-hole,
  huginn and mx5parts; old VM off.
- **Two RFCs being drafted by subagents, to refine with him and hand to
  session stackr-97:** "RFC: prod walkthrough work package (bugs and UI)"
  (items keep his numbers; item 31 dropped, built on ui1) and "RFC:
  releases view (plans, promotes and deploys)". Both must follow the
  design knowledge doc (fraedi 336f7ce3, his personal space,
  web-playbook). Their cards are NOT on the timeline board yet.
- **Chosen package, his numbers:** bugs 1, 2, 3, 4, 6, 13, 14, 18, 23,
  34, 39; UI 8, 19, 22, 33, 36, 41, 42, 43, 44, 46, 47, 50, 51, 52, 53,
  54. Decisions: 13 watch release builds on the newest release; 18 secret
  works, plain variable warns, locked state shown; 23 writers join the
  slice's owner role. Left out: forward auth to a tile, auth on routes,
  backups in the stack file, volume import, settings as facts.
- **Not mine, uncommitted in the main checkout:** promote/plan.go,
  push_pr_test.go, jobs.go, prenvs_test.go, drawer/org/org.templ,
  canvas/org_test.go (a third session). Mine: this notes file only.
- **Work package RFC published (2026-11-18 late):** "RFC: prod
  walkthrough work package (bugs and UI)", fraedi
  8d8ba342-d56c-4513-a8e3-1ac72e0a75a8, draft, 11 open points for him:
  item 1 drift step; item 4 first or last error line; item 13 what "the
  env's line" is; item 14 ask or refuse, and its split across waves; item
  34 one form or all; item 22 next retry time; item 41 outbound history;
  item 43 marker or drawn in place; item 47 what fixed means; item 52
  build the move or link only. Source file:
  scratchpad/timeline/rfc-workpackage.html. Card on the board still to
  add. The drafter did not read the design knowledge doc's c1-c9 files;
  it named sections from the index.
- **Releases view RFC published (2026-11-18 late):** "RFC: releases view
  (plans, promotes and deploys)", fraedi
  e723e413-9f66-461b-bd91-430337ba4185, revision 2, draft, built on ui1
  at 6b36e4cc, 7 mockups, 12 open points for him (two views on org only
  or also stack; window absorbs the env pill; closed by default; review
  in drawer or full page; what pending means for the bottom env; image
  watch builds on the waiting release; rows for releases that change
  nothing; row count; colour of waiting; live updates on the cluster
  chart; Releases screen versus Waiting on you; jobs under release rows).
  Findings in it: ui1 counts a release as waiting only when the env below
  runs it, so a one-env stack with `auto: false` (his prod) still shows
  nothing; a release stores no base or target env, so newest and roll
  back go by number, which needs one forward migration; org and server
  config have plans, not releases, and promote plans are not stored; the
  cluster chart has no page stream. Source file:
  scratchpad/timeline/rfc-releases-view.html. Both RFC cards are on the
  timeline board in a new frame right of 11-18.
- **Both RFCs agreed and handed over (2026-11-18, about 23:40).** He took
  the recommended answer on every open point ("fast"), looked at the
  releases view mockups ("they look good"), approved the schema additions
  (two columns on releases; an outbound last-seen table only if no
  history exists), and kept the accent tint for "waiting". Work package
  8d8ba342 is revision 2, releases view e723e413 is revision 3; item 43
  moved from the first to the second; the releases view's "one theme"
  error is corrected (the panel has light and dark). Links sent to session
  stackr-97 to build. Board: frame "Prod walkthrough RFCs" retitled
  agreed, a note per RFC at the top of the evening column.

## 2026-11-18, late: review of the two hand-over RFCs

Read both RFCs against the code with darthvader before the hand-over. Changes, all agreed:

- Work package item 13 widened: a push builds on what the env runs too (`push.go`, `base := deref(from[0].ReleaseID)`), so a later push leaves out a waiting image update or an earlier push's build. One rule for every release cut; the two new fields on a release (built on, cut for) move into this item.
- Item 2: also looks at files pulled in with `include:`; when the check cannot tell, the release is made.
- Item 1: step 1 only. Step 2 would rename every file volume and redeploy every tile with mounted files once.
- Mocks: no sign-off stop. The releases view's mocks are agreed and built as drawn.
- Release gate added: run in the playground (worker online, upgrade from v0.8.30), small releases, his go each time.
- Releases view: a fixed failure leaves the window's count but stays in the Releases screen's history.
- New to-do, item 55: the panel follows an action live. Approving a plan, a promote or a fix must show in an open tab with no reload: layout, banners, counts. Known gap: the org canvas plan strip (`planBanner` in `internal/web/handler/canvas/orgconfig.go`) is drawn at page load only. Every UI item's check now includes a live check.

fraedi: work package 8d8ba342 revision 4, releases view e723e413 revision 4.

## 2026-11-18, late: how the build is run

Agreed with darthvader before the hand-over; written into the work package RFC (revision 5), section "How the build is run".

- Build board: a frame on the project board (fraedi fed49ad1), one card per item and per releases view phase. Columns: Backlog 1, 2, 3, Working, Testing local (hamr dev), Testing playground, Finished, Hard blocked. Only the lead saves the board.
- Comments: one fraedi comment thread per item on the board, first line "Item N: title".
- Questions go to a Fable agent. Only it can call a hard block for darthvader: against an agreed decision, schema beyond what was approved, an item dropped or cut, risk to data, prod, a push or a release.
- Lanes: A to H for wave 1, J to P for wave 2, R to T for the releases view. One owner for migrations.
- Testing plan: written by another agent, holds his own test list for 11-19 in order of importance.
- Hard rule from darthvader: nothing is pushed to the upstream repo. No push, tag, release or pull request until he lifts it himself.

## 2026-11-19: huginn gets a stackr file in its own repo

- New `stackr-compose.yml` in the root of `~/FyrmForge/huginn` (public repo FyrmForge/huginn): tile `huginn` built from the repo (`cmd/site/Dockerfile`, port 8080, `health_path: /api/health`, volume `uploads`, huginn.vulpe.dev), slice `huginn-db` on `infra:prod:pg`, one secret `params.huginn.oidc_client_secret`. Env `prod` on master, `auto: false`.
- homelab `stackr-org.yml` lists stack `huginn` with `repo: FyrmForge/huginn`. First stack whose file is not in the homelab repo, and the first built app on prod.
- The old proxy's redirect for `/.well-known/caldav` is not carried over: huginn answers that path itself (`internal/caldav/routes.go`).
- Nothing committed or pushed in either repo.
- Open: the connector must be able to read FyrmForge/huginn; `TRUSTED_PROXIES` is the old value and not checked against the stackr proxy; database and uploads still on the old VM; the old GitHub release workflow still builds an image nobody will use.
- 2026-11-19, huginn first build on prod worked: org apply queued the push, image built in about 3 minutes, release #1. Two gaps seen: (1) while the push job builds, the env's Releases tab says "No releases yet. The first deploy makes one." with no sign that a build is running (work package item 54 and the ui1 push banner cover the failed and done cases; the running case must show too); (2) `stkr job ls` has no stack or env column, so a push job cannot be told apart from the others: he asked "how am i supposed to know which one is huginn?". Wanted: a scope column (org/stack/env) in `job ls` and a `--stack` filter.
- `scripts/huginn-secrets.sh` in the homelab repo sets `huginn.oidc_client_secret` from the old `.env` (not committed yet).
- 2026-11-19, his verdict on the invisible build: "thats a no go, we need some form of feedback to know that smth is happening". Requirement sent to the building session (stackr-97): a running push or build shows on the stack card, env page, Releases tab and the releases window, with the job log link, live; and `stkr job ls` gets a scope column and a `--stack` filter. The RFC files are the builder's, so it writes this in.
- 2026-11-19, huginn first promote: the job parked in state `waiting` on the unset secret `huginn.oidc_client_secret` and resumed by itself once the script set it (that part works). Gaps: (1) the promote dialog on v0.8.29 listed the plan with no word that a secret had no value, and Promote was lit; (2) the tile said "not deployed / never deployed" while the job waited, with nothing naming the secret (ui1 has the secret field on the review and the "Waiting on you" page; check that the tile and env also say it); (3) `stkr job ls` shows `WAITS n/a` for a job in state `waiting`: the column should name the param; (4) while parked, the job re-ran the whole plan on every poll: a `git fetch` of the repo from GitHub plus the lines "plan: image huginn (not deployed yet)" and "slice huginn-db: ..." about 100 times in under 10 minutes. The log is unreadable and each poll is a network call. Wanted: a parked job checks only whether the param is set, and logs nothing until it resumes.
- 2026-11-19, huginn data moved. Old stack was already down on `pods`; brought up only `db` (`stackr huginn compose up -d db`), `pg_dump --no-owner --no-acl -Fc` (41 KB, 18 tables), copied as his own user (not sudo: root on pods has no key for TrueNAS) to `tank/backups/huginn/huginn.dump`. Uploads folder on the old VM was empty, nothing to move. Loaded on home-ct through a temporary read-only NFS volume, piped into `pg_restore -U stackr -d huginn_prod_huginn_db --clean --if-exists --no-owner --no-acl --role=huginn_prod_huginn_db_huginn --single-transaction` with the new app running. Counts match the old database: users 2, calendars 7, events 4, app_passwords 3. Left to do: Deploy on the tile to restart the app on the loaded data, sign-in check, old `huginn_db` container down again, commit `scripts/huginn-secrets.sh`.
- 2026-11-19: huginn is live on stackr. Deployed on the loaded data, sign-in through Authelia works (his words: "ok deployed, it works"). First app on prod built by stackr from its own repo. Open: `TRUSTED_PROXIES` value not checked; old `huginn_db` container on pods to tear down; `scripts/huginn-secrets.sh` uncommitted; the old GitHub release workflow in the huginn repo still builds an unused image; the calendar import to-dos (board, huginn column) are unchanged.
- 2026-11-19, lost in the rewrite: the database table viewer. His words, on the huginn-db slice drawer: "what the fuck happened with our db table viewer? we used to have one". v0.5.0 had a data browser: a Data tab on the Postgres instance drawer (superuser, database picker) and on a slice's drawer (the slice's own user, its database only), drilling databases -> tables -> rows, paged, with inline cell edit. Code at tag v0.5.0: `internal/stackrd/handlers/web/handler/db/data.go`, `data.templ`, `internal/stackrd/infra/managedtiles/sqlbrowse.go` (runs `psql --csv` inside the database container, so no driver and no network path). Gone since v0.6.0; today's slice drawer has Overview and Settings only (`internal/ui/drawer/slice`). Not in the rewrite's deferred lists and not in either hand-over RFC. To bring back; the old transport can be reused.
- 2026-11-19: huginn's old GitHub release workflow (`.github/workflows/release.yml`: image to ghcr, version tag, POST to the old stackr `/deploy`) removed in the working tree of `~/FyrmForge/huginn`, not committed. This morning's push ran it and it ended in failure (run 44503861461). CI (`ci.yml`) stays. Not touched: the ghcr package, the repo variable `STACKR_DEPLOY_URL` and secret `STACKR_TOKEN`, `docs/deployment-plan.md` which still describes the old flow.
- 2026-11-19, two tickets from darthvader, both about the database drawers.
  - **Ticket: database viewer, on the operated Postgres tile and on the slice.** Bring back what the rewrite wiped (commit f82febe3, 2026-09-24): a Data tab with databases -> tables -> rows, paged, cell edit. On the operated tile: any of its databases. On a slice: its own database only, as its own user. The package comment of `internal/ui/drawer/instance/instance.templ` says it outright: "v0's Metrics and Data tabs are out of v1". Metrics came back, Data did not. Old code at tag v0.5.0 (`handler/db/data.go`, `managedtiles/sqlbrowse.go`). The old volume and bucket file browser went the same way; PROGRESS.md says it "comes back after the migration".
  - **Ticket: metrics on the slice, with slow queries.** His words: "see the metrics for the base operated work? i wanna see if we can get smth liek that for the slice also, including 'slow queries' ... useful for the aplication specific tunning". What exists: `PGStatsSQL` in `internal/service/internal/flow/operated/pgstats.go` already reads per database: size, commits, rollbacks, cache hits and reads, deadlocks; only size is charted per database, the rest is summed for the instance. `PGTop` in the same file already returns the ten most expensive statements for a given list of databases from pg_stat_statements (called from `internal/service/pgmonitor.go`). Missing: connections per database (pg_stat_activity has `datname`, the sample groups by state only), per-database series kept for the measures that are summed today, a Metrics tab on the slice drawer (`internal/ui/drawer/slice/slice.templ` has Overview and Settings), and a "slowest" order (by mean time) next to "most total time".
- 2026-11-19, the slice metrics ticket is an investigation first, then its own piece of work. darthvader: "that ticket in the backlog is to be more of an investigation, then work (bigger than just do it now in this workpack) so yeah drop this info there then well revisit". Not in the running build. For now the plain charts can stay per database. The set of information proposed for a slice (and the same across all databases on the operated tile), to start the investigation from:
  1. Queries, from pg_stat_statements (already installed per slice): slowest by mean time; most total time; most called; uneven (high stddev, usually an index that only helps some values); reading from disk (shared_blks_read); spilling to temp files (temp_blks_written); an Explain button per query using `EXPLAIN (GENERIC_PLAN)`, which plans a statement with `$1` placeholders without running it (Postgres 16 and up; prod is 18).
  2. Right now, from pg_stat_activity filtered by `datname`: running queries with their age; sessions idle in transaction; who blocks whom (`pg_blocking_pids`); connections in use per app user.
  3. Tables, from pg_stat_user_tables and the size functions: table, index and total size; live and dead rows; sequential scans against index scans; last vacuum and analyze.
  4. Indexes, from pg_stat_user_indexes and pg_index: unused with their size; duplicates on the same columns; invalid.
  5. Things to fix, as plain sentences shown only when true: a big table read in full often; a large unused index; a session idle in a transaction for hours; a table with many dead rows and no recent vacuum; a 32-bit sequence near its limit; a table with no primary key.
  6. Over time: the per-database charts (size, transactions, cache hits, connections, deadlocks).
  How: 1 to 5 read when the tab is opened, nothing stored, read-only, no new extension; only 6 is sampled. 3 and 4 need a connection into the slice's own database, the way `PGTop` already does it.
  Left out of a first cut: query stats history (needs stored snapshots); capturing the real plan of a slow query (auto_explain, needs a restart); cancel or kill from the panel.
  To investigate before building: cost of these reads on a busy instance; what a slice's own user may see against the superuser; whether stats resets should be shown or offered; how the Explain button handles statements that cannot be planned generically.
- 2026-11-19, connectors, found while trying to merge the org and server GitHub connectors into one.
  - Fact: one connector = one GitHub App = one GitHub account. `Token` in `internal/service/internal/connectors/github/api.go` uses the App's first installation only. Repos in two accounts (JamesTiberiusKirk: homelab; FyrmForge: huginn) need two connectors. Agreed end state: two server connectors, one per account, the org connector removed.
  - Gap: the connector is picked by host only (`Resolve` in `internal/service/internal/leaf/connector/connector.go`: named, else the org's own, else the single shared one, else an "ambiguous" error). The repo's owner is never looked at. huginn (FyrmForge) was fetched with the JamesTiberiusKirk App and it worked only because the repo is public; no warning. Pushes to huginn most likely never arrive (no App installed on FyrmForge); not confirmed. Wanted: pick by the account the App is installed on, and say so when a repo is outside the connector's account.
  - Gap: a tile's own `git_url` cannot name a connector; with two server connectors on one host it leans on the stack's connector (`tileConnector` in `internal/service/wiring.go`).
  - Gap, his words: "the default naming is a bit shit". The connector's default name reads like `stkr-fyrmforge-dev-serverconnector`. Wanted default, his words: "the default should be [user|org] Github and not the same name we send to github". So two separate names: the connector in stackr is named after the GitHub user or org it serves ("JamesTiberiusKirk GitHub", "FyrmForge GitHub"); the App name sent to GitHub (`manifest` in `internal/service/internal/connectors/github/api.go`, "stackr-<panel host>-<id>", must be unique across GitHub) stays what it is. Today the connector shows as "GitHub · <App slug>" (`Name` in `internal/service/internal/connectors/github/kind.go`). He renamed his by hand.
  - Same ticket, the name sent to GitHub: today `stackr-<panel host>-<4 chars of the connector id>`. Wanted: panel address plus level, `stackr-<host>-server` or `stackr-<host>-<org slug>`. Limits: 34 characters (cut the host first, keep the level); unique across GitHub, and two server connectors on one panel would both end in `-server`, so the account goes in when there is room, or a short suffix only on a clash.
  - To do on prod: create the FyrmForge server connector, rebind homelab Config as code to the JamesTiberiusKirk server connector, add `connector:` to the huginn entry in `stackr-org.yml`, remove the org connector.
  - Same ticket, connectors have no slug. A file names a connector by its display name (`ConnectorID` in `internal/service/internal/flow/orgconfig/file.go`; no match = taken as an id), so a rename in the panel silently breaks every file that names it. Wanted: a connector slug set once, display name free to change.
  - Same ticket, the "Slug" field misleads. Under Update credentials it is the GitHub App's slug (last part of github.com/apps/...), used only for the install link. He took it for the connector's slug and changed it on both connectors (2026-11-19); Save accepted the wrong values although the form says "stackr checks the new ones first". Nothing else broke (tokens and webhooks use App ID, key and secret). Wanted: label it "GitHub App slug", or do not ask for it at all and read it from GitHub by App ID; Save refuses a slug that is not the App's. To do on prod: put both real slugs back.
  - Same ticket, `repo: owner/name` is shorthand that the first git connector expands (`repoURL` in `internal/service/internal/connectors/github/api.go`). His point: "what happens when in the future we add support for gitlab". With two providers the short form is a guess. Wanted: full address, or refuse the short form once more than one git provider is connected.
  - Same ticket, BUG: the org chart draws a connector line by host, not by use. `buildsFrom` in `internal/service/internal/flow/graph/graph.go` links a stack to every connector on the host its tiles build from, so `media` (its bootstrap tile builds from the homelab repo on github.com) got a line from "FyrmForge GitHub", which it never uses. He: "why is media using FF?". Wanted: a line only from the connector the clone really resolves to (`Resolve` plus `tileConnector`).
  - 2026-11-19: both App slugs put back (`stkr-fyrmforge-dev-serverconnector`, `stkr-fyrmforge-dev`). 16 dead rig Apps deleted on the FyrmForge GitHub org. homelab `b31988c` pushed: huginn entry names `connector: FyrmForge GitHub`. Removing a connector or wiping a rig never removes its App on GitHub (no API for it); wanted: on Remove, say the App still exists and link to its delete page.
  - 2026-11-19: the chart line bug (false connector lines on media and huginn) sent to the builder (stackr-97) as a bug for the running work package, on his word: "this needs to be done as part of this workpack". The other connector gaps stay out of the package.
- 2026-11-19, TICKET: canvas paint cost audit. Sent to the builder (stackr-97) for the running work package on his word: "make a ticket at tell the work agent to pick it up as part of this same package". Origin: a fraedi bug the same day (board stutter in Brave on Linux with Intel graphics at 4K and in Safari, fine in Chrome on macOS; a dot grid drawn per dot per frame with a 16x cost cliff at one zoom level, and a blur per item per frame). Static read of stackr, not profiled:
  1. Dot grid: CSS `radial-gradient` on `graph-canvas`, 22px times zoom, moved by `background-position` on each pan step (`ui/css/input.css` around line 756). Whole background repainted per frame; cost grows with window size and with zooming out.
  2. Edge labels: `filter: url(#edge-label-bg)` on every label (`input.css` around line 843), one filter pass per label on every redraw of the layer.
  3. Pan sets `--px/--py/--s` on the canvas and every card's transform reads them (`ui/ts/graph-canvas.ts` `apply`), so one pan step restyles all cards. Grows with card count.
  4. `backdrop-filter: blur(4px)` (`input.css` around line 579) over a canvas that updates live while a dialog is open.
  Clean: no 2D canvas draw loop, paths rebuilt only on content change (`repath`), metric charts are a library canvas redrawn on data, card shadows are plain box-shadow.
  Wanted: profile pan and zoom at several zoom levels on a big window (4K at 100% scaling, Brave on Linux with Intel graphics, Safari), under 8 ms of drawing per frame, fix what the profile shows. Fixes that worked in fraedi: grid as one cached tile moved by transform; no blur per item.
- 2026-11-19, connectors ticket, his idea: "if not defined, inherit the same connector as the org in stacks etc". Today: a stack whose file is in the org repo (path alone) already takes the org binding's connector (`StackRef.Binding` in `internal/service/internal/flow/orgconfig/file.go`). A stack in its own repo with no `connector:` does not inherit; it is resolved by host (the org's own connector, else the single shared one, else ambiguous). Wanted, to look at: one rule for a stack or tile that names no connector: the connector for the repo's account if one exists, else the org binding's connector, never a host-only pick. Goes with gap 1 (pick by the repo's account).
  - 2026-11-19: the pick rule (connector for the repo's account, else the org binding's, never host only; say so when the connector is not of the repo's account; covers gaps 1, 2 and the inherit idea) sent to the builder (stackr-97) for the running work package, on his word: "yeah thats good, go add to the backlog and tell the other agent to pick it up". Shares one rule with item 60.
  - 2026-11-19, GAP seen on the homelab rebind plan: the org plan lists "rebind auth connector" with two raw connector ids (28123e4b... -> 9f52cb42...) and no names. Wanted: connector names in plan lines, as the picker shows them. Plan itself was right: 5 rebinds (auth, dashy, immich, infra, media) from the org connector to the JamesTiberiusKirk server connector.
  - 2026-11-19, correction to the pick rule, his words: "in the case of huginn, yeah im saying it would be a given that wed need to specifislly define the FF connector". So no automatic pick by the repo's account: a named connector wins, an unnamed stack or tile inherits the org binding's connector, never host only. The warning stays (connector not of the repo's account, or cannot list the repo). Correction sent to stackr-97.
- 2026-11-19, connector move DONE on prod. homelab has two connectors, both server level and shared: "JamesTiberiusKirk GitHub" (org file binding and the five stacks in the homelab repo) and "FyrmForge GitHub" (huginn). The org connector is removed. Proof by two empty test commits at 16:42: homelab `e943c60` gave an org plan "no changes" at 16:42; huginn `4e2193d` gave release 3 at 16:42. Release 2 of huginn (`e55b56c`, the morning push) was only built at 15:27 when the plan with the FyrmForge connector was applied, so that push had never arrived. prod stays on huginn release 1 (2 and 3 hold no app change). Small gap: release 2 shows no commit message. Left: delete the old App `stackr-stkr.fyrmforge.dev-homelab` on his personal GitHub account.
- 2026-11-19, managing all domains through Namecheap (exploring). `fyrmforge.dev` is managed; `vulpe.dev` is on hand records (a `*` wildcard).
  - Gap: app records do not use the home IP stackr already knows. The address a node outside sees the router on (cluster map `Public`, through the tunnel) is used for the panel's own record only (`panelTarget` in `internal/service/panelname.go`) and for the Namecheap allow-list IP (`serverIPv4` in `internal/service/dns.go`). App records (`nodeAddrs` in `internal/service/dnsrecords.go`) read the typed Public IP or the box's own network card, so a home node gives "no public address" and the record is held. He: "why cant it use the public ip from wg?". BACKLOG item (his call 2026-11-19): stackr finds a home node's public IP for app records itself and follows a changed home IP. How is open: the tunnel-seen address is one possibility, "well consider other ways todo it as well". If the tunnel-seen one is used: not with the panel's "only when the name resolves nowhere" rule, app names resolve through his wildcard today.
  - Workaround he did: typed the home IP on `controller` (Nodes, controller, Network tab, Public IP). Side effect: the typed address wins in `panelTarget` too, so the panel's record no longer follows a home IP change until the field is emptied.
  - Gap: a server route (`stackr-server.yml` `routes:`) gets no DNS record on a managed domain. vms, s3, pihole, nas, s3-console on vulpe.dev stay on the wildcard.
  - 2026-11-19, BLOCKED: Manage on `vulpe.dev` (server Namecheap connector, whole domain, plan listed 11 app A records to the home IP, apex and www offered for adoption) was refused: "vulpe.dev is already a domain resource of another level or organization". Cause: the homelab org file declares `domains: - host: vulpe.dev`, an org-level row; a server connector only manages a server-level row or a name nobody declared (`managedResource` in `internal/service/dns.go`). Gaps: (1) a server connector shared with an org cannot manage that org's own domain; (2) the refusal comes after the confirm screen, which showed a full plan as if it would work; (3) the message does not say which org holds it or what to do; (4) no way to move a domain between org and server level.
  - 2026-11-19, TICKET, HIGH IMPORTANCE (his call), his "main problem here". Also wanted: check every other block of the org file and the server file for the same hole. The case: homelab `3b1793d` took `domains: vulpe.dev` out of the org file; the plan at 22:09 said "clean, no changes, Nothing changes." and the domain stayed on the org. The org file's `domains:` only adds and updates (`internal/service/internal/flow/orgconfig/diff.go`); a domain dropped from the file is ignored without a word, so the file and the org disagree and the plan calls that clean. Shares already do it right in the same file (a dropped share is an unticked removal row). Wanted: a dropped domain shows as a removal row in the plan (unticked, like shares), or as a note when apps still use it. He: "if its committed and pushed why tf can i not see the config release?".
  - 2026-11-19, `vulpe.dev` MANAGED on prod: he deleted the org-level `vulpe.dev` in the homelab org drawer (Domains tab), then Manage on the server Namecheap connector went through (whole domain, every org, apex and www not adopted). The `domains:` block is out of the homelab org file (`3b1793d`). Still to see: the Records tab with the 11 app records, apps answering, the wildcard certificate.
