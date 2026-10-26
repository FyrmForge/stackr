# Join and onboarding lane (before v0.7.0)

Status: agreed with darthvader 2026-10-14. Starts after sec-integ lands on multi-node.

## 1. One-line join from the panel

- The panel prints `curl -fsSL https://<panel>/join/<token> | sh`.
- `/join/<token>` serves a shell script only for a valid, unexpired join token.
- The script checks Linux, amd64 and Docker, downloads `stackr-install` from the panel (the binary already in the panel image), and runs `stackr-install join` with the same token and the panel's version.
- The download goes into a `mktemp -d` folder removed by a `trap` on exit, success or fail. The script itself is piped, never written.
- `/usr/local/bin/stackr-install` stays (worker upgrades and node remove use it).
- A panel without a publicly trusted cert also prints the two-step command (download, then join).

## 2. Nodes open to all orgs by default

- New column `nodes.open_to_all`, default true, existing nodes included (no compat).
- `NodeOpenToOrg` (`internal/service/nodeadmin.go`): true if `open_to_all` or the org is in `node_orgs`.
- Add node: the panel form offers "All orgs" (default) or picked orgs. The choice rides on the join token. CLI mirror: `stackr node add <name> [--org X ...]`.
- Node drawer: "Open to all orgs" switch; when off, the per-org list shows. CLI mirror: `stackr node open|close <name> --all`.
- Closing never evicts envs already on the node.

## 3. Continue setup in panel

- When join finishes, the box (and the curl script) prints "Continue setup in the panel:" and a link to the node's drawer.

## 4. Earlier agreed small items

- The installer logs the CLI on the box in as the admin itself (key passed through the environment, never printed) and prints a docs link for laptop login.
- Fix the `stackr node exit` help text.
- Docs use GitHub's `releases/latest/download` URL for the first install.
- `node add` help says egress is always on.

## Docs

- `docs/guide/install.md` and `docs/guide/nodes.md` describe the panel flow first, CLI as the mirror.
