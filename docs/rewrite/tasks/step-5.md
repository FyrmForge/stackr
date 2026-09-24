# Step 5: installer and self-upgrade

Read first: `docs/rewrite/PROGRESS.md`, then REWRITE.md "Infrastructure"
(proxy, VIP, NET_ADMIN), "Backups" (local destination, recovery passphrase),
"Auth mechanics" (first account, setup URL), "Build order" row 5. Extracts:
`docs/rewrite/extracts/{installer,admin-upgrade}.md`. Test VM: see the
memory note `vm-test-credentials` for the box; it is disposable.

## Tasks

1. **`cmd/stackr-install`.** Host steps from the `installer` extract, no
   `swarm init`: check Docker, data dir, install spec (root/domain grammar),
   pull `stackrd` image, create the proxy's named Caddy volume, start
   `stackrd proxy` container, start `stackrd` with host networking and
   `NET_ADMIN`, create the local backup destination, first-run: print the
   setup URL and the recovery passphrase once. Idempotent re-run.
   Done when: a fresh VM installs from one command and the setup page
   loads over TLS on the panel domain.

2. **Install spec shared with upgrade.** One struct builds the panel
   container both for install and for upgrade, so an env var added once
   reaches both paths (as the old comment insisted).
   Done when: a test builds the spec both ways and compares.

3. **Self-upgrade.** `flow/upgrade` from step 3 wired to the admin verbs:
   check, run; pre-upgrade backup to the local destination; swap the panel
   container; dev builds refuse. Restore script for a failed swap, as today.
   Done when: the VM upgrades from build N to N+1 through the API and comes
   back with its data.

4. **`make release`** builds `stackrd` image + `stackr-install` for linux
   amd64 with the version baked in.

5. **Done gate.** build/lint/test pass; install + upgrade verified on the
   VM; PROGRESS.md ticked; stacked PR.
