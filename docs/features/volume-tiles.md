# Feature: Volume Tiles

## Summary
Volumes become first-class tiles (Railway model): created, attached, detached,
backed up, restored, and (later) moved between hosts — instead of hidden
`name:/path` text lines in a service's settings. On the canvas a volume renders
as a small tile **stacked underneath** the service it's attached to.

## Rules (agreed)
1. A service may mount **many** volumes; a volume attaches to **at most one**
   service (single-writer — shared storage is a corruption factory).
2. DB tiles' data volumes appear as stacked volume cards too, but are
   **auto-managed**: visible and backupable, never detachable.
3. Deleting a service **keeps** its volumes; they become orphan cards on the
   canvas, reattachable or explicitly deletable. Env teardown keeps volumes.
4. Cloned/PR envs get **fresh empty volumes**; optional seed-from-backup later.
5. Stacked card moves with its parent on the canvas; orphans stand alone.
6. Config-as-code: `type: volume` declared in the file; undeclared volume
   tiles are strict-mode deletes (gated like every delete).
7. Existing inline named mounts auto-convert to volume tiles on upgrade
   (bind-mount paths stay as text — they're host paths, not volumes).

## Model
- `tiles.kind = "volume"` — new kind on the existing tiles table.
- New columns: `attached_tile_id` (the service mounting it, "" = detached),
  `mount_path` (container path).
- Docker volume name: `stackr-vol-<id8>` (mirrors `stackr-db-<id8>`).
- Size shown from the docker disk-usage scan (server Volumes page machinery).

## Behaviour
- **Deploy**: engine assembles a service's binds from its attached volume
  tiles + any legacy text lines. Attach/detach redeploys the service.
- **Attach UI**: from the volume panel (pick service + path) and from the
  service settings (attach existing / create new).
- **Backups**: existing backup machinery gains a volume mode — tar the volume
  to S3 on a schedule; restore = stop dependent, untar, restart. This also
  becomes the unplanned-node-loss recovery for file data.
- **Migrate host** (multi-server phase): stop dependent → snapshot → restore
  on target → reattach.

## Config-as-code
```yaml
base:
  tiles:
    uploads:
      type: volume
      attach: web        # tile slug; omit = detached
      path: /app/uploads
```
Diff fields: attach, path. Create/delete/update via the normal plan/apply flow.

## Canvas
- Volume card: half-height tile, disk icon, name + size badge, stacked
  directly beneath its parent (drags as one unit). Orphans park standalone.
- DB data volumes render as auto-managed stacked cards (no DB row; virtual).

## Migration
Boot-time one-shot (settings flag `volumes_migrated`): for every service tile,
each named `name:/path` line becomes a volume tile (attached, docker volume
name kept as-is so data is preserved), and the line is removed from the text
field. Bind paths (`/host:/path`, `./x:/path`) are left in the text field.

## Phases
1. **Model + migration + deploy binds** ✅ DONE — volume tiles exist, legacy
   converts, services mount them.
2. **UI** ✅ DONE — create/attach/detach/delete panels, canvas stacked
   rendering.
3. **Backups/restore** ✅ DONE — volume tar mode in
   `internal/stackrd/infra/backup`.
4. **Host migration** — with multi-server. Not started.
