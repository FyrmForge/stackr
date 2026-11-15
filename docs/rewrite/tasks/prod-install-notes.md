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
