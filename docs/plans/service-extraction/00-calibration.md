# Calibration set — known findings

Findings from an unstructured look at the codebase on 2026-09-18. Every line
ref was spot-verified. The sweep reads this first, skips re-deriving it, and
extends it.

## Seed findings

Already verified, folded in so the sweep does not re-derive them.

### Tile settings — class A and B

`api/v1/apps.go:177` `patchApp` vs `web/handler/app/handler.go:1428`
`SaveSettings`.

- `container_port`: API gates on `runpolicy.For(kind).AllowsIngress`
  (`apps.go:242`); web plain `Atoi` (`handler.go:1551`). Web accepts a port on
  a tile with no ingress.
- `replicas` / `node_group`: web only (`handler.go:1536-1549`), including the
  `placement.IsPinned` volume guard. The API has no key. The CLI cannot scale
  a tile.
- `update_policy` / `wait_for_ci` on the wrong source: API returns 400
  (`apps.go:427,436`); web silently coerces to `off`/`false`
  (`handler.go:1557-1562`).
- **Class B bug:** web calls `syncProxy` → `px.WriteApp` after save
  (`handler.go:1600`). The API patch never does. Setting `basic_auth_*`,
  `sec_headers` or `traefik_override` over the API does not take effect until
  a redeploy. Note `basic_auth_password` is hashed when the route is written
  (`stackconf.go:397`), so the API path also stores it unhashed and unused.
- Runnable-source guard (image source needs an image, git needs a `git_url`):
  API only (`apps.go:443-451`).
- Connector org check: inline in web (`handler.go:1483-1494`) vs
  `a.checkConnector` in the API. Two implementations of one rule.

Same in both, not drift: `ParseDevice`, `NormalizeRestart`, `shm_size_mb ≥ 0`,
`ParseFileMount`, basic-auth pairing, `ValidGitURL`.

### The gate — class A

`editGate` and `rejectManaged` are **not** one function with a bug. They
answer different questions, and both surfaces use both:

- `editGate` — config-owned **field edit**. May stage (`ui_edits: stage`).
  web `744, 1138, 1172, 1439, 1612, 1654, 1806, 1883`.
- `rejectManaged` — **structural write** (create/delete a tile, domain, env).
  Always refuses, fails closed. web `906, 936, 994, 1768`; api 13 sites.

Collapsing them would let structural writes stage on a config-managed stack.
Do not.

- **Bug:** `api/v1/apps.go:183` `patchApp` is a field edit but calls
  `rejectManaged`. Its web twin calls `editGate`. A stack set to
  `ui_edits: stage` accepts a browser settings edit and 409s the identical
  CLI edit.
- Inconsistent, flag only: web `CreateDomain` (1654) uses `editGate`,
  web `CreateAutoDomain` (1768) uses `rejectManaged`. Same operation.

### Volumes — class C

No volume concept in the store. A docker volume comes into existence four
ways; one is addressable.

| # | Source | Name | Row? |
|---|---|---|---|
| 1 | Tile volume | `stackr-vol-<tileID[:8]>` or an adopted custom name | yes, `tiles` `kind=volume` |
| 2 | Managed DB data | `stackr-db-<tileID[:8]>` | no |
| 3 | Storage path / org share | `stackr-stor-<pathID[:8]>`, `stackr-stor-<storageID[:8]>-<hash>` | no |
| 4 | Hand-made on the node page | whatever the operator typed | no |

Refs: `store/repo/models.go:685` `DockerVolume()` (custom name is adoption, so
no prefix to parse), `infra/managedtiles/managedtiles.go:328`,
`store/repo/models.go:287,296`, `web/handler/server/handler.go:259`
`CreateVolume` (calls docker through the cluster, writes no row).

The surfaces address different sets, with no join key:

- web `/servers/:id/volumes` — docker volumes by name + node, from a live
  `docker volume ls`. The only place kinds 2, 3 and 4 are visible.
- api `/apps/:id/volumes`, `DELETE /volumes/:id` (`v1.go:325-328`), CLI
  `stackr tile volume` (`internal/cli/cmd/tile.go:614-720`) — kind 1 only,
  by tile id.

So a managed postgres data volume is deletable from the web and invisible to
the CLI; a hand-made volume is invisible to everything but the page that made
it.

"Which volumes does X own" is answered four times, each with its own rules:
`infra/backup/backup.go:305` `VolumeFor`, `infra/volmove/volmove.go:232`
`volumesOf` (has the sibling walk, and the bug comment at `:236`),
`infra/storagetiles/storagetiles.go:88` `DropOrgShareVolumes` (prefix match on
a live `ListVolumes`), `web/graph/graph.go:573` which rebuilds
`"stackr-db-" + d.ID[:8]` inline while its own comment names the function it
is not calling.

Volume → mounting service is answered once: `backup.go:404` `quiesce`,
`:421` `serviceOf` (hops `AttachedTileID`). The only owner-aware stop/start
in the tree.

Below the cluster line is uniform and out of scope: `infra/cluster` twelve
volume methods (`cluster.go:312-476`), the agent's ten endpoints
(`agent/server.go:57-70`), and `runtime`.

### Tile field lists — duplication

`web/handler/app/handler.go:1366` `settingsPatch()` is ~40 hand-written
`p["key"] = a.Field` lines mapping `repo.Tile` → the staging patch. Add a tile
field, forget a line, the staged version silently drops it.

It is the **fourth** copy of the tile field set, after `repo.Tile`, the API's
`appPatch`, and `stackconf.TileConf`. A `Tile → TileConf` serializer would
collapse it; `config/stackconf/serialize.go` is where it would live. Note
`apply.go:1691-1746` goes the other way (`TileConf → Tile`), so this is the
inverse and does not exist yet.

