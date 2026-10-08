
## Wave 0 hand-off

Knobs (`leaf/settings/catalogue.go`, all `Flat`):
- `panel_backup_schedule` default `0 3 * * *`; `panel_backup_keep` int, default `14` (0 refused);
  `panel_backup_dest` string, default empty = local; `cleanup_schedule` default `30 4 * * *`;
  `orphans_schedule` default `@daily`. `cleanup_enabled` unchanged (still the switch).
- Cron knobs validate with `cron.ParseStandard` (the scheduler's own parser; `CRON_TZ=` ok). New helpers:
  `settings.Default(key)`, `settings.IsCron(key)`. (`CronOff` is gone since G2: the switches
  `panel_backup_enabled` and `orphans_enabled` turn an entry off; an empty schedule means the default.)
- Admin form labels for the five new keys are not in `ui/components/settings_form.templ` `knobs` (they land in
  "Other"); R1 owns the Backups tab, wave 2 adds labels for cleanup_schedule and orphans_schedule.

Drivers (`flow/schedule/schedule.go`): `PanelBackup func(ctx) error`, `Cleanup func(ctx) error`,
`Settings func(ctx) (map[string]string, error)`. `Entries(d, scheds, crons, set)` takes the settings map
(missing key = catalogue default). Entry names: `orphans`, `panel-backup`, `cleanup`. A nil PanelBackup or
Cleanup driver means no entry. Wired in `orchestrator.go`; `orch.scheduleSettings` (cleanup.go) feeds the
map from `scheduleKnobs` (admin.go: the four schedule keys plus `cleanup_enabled`).

Jobs:
- `kindPanelBackup` ("panel-backup"), lock `panel-backup`; scheduled payload `{"scheduled":true}`
  (`panelBackupJob` in jobs.go); the manual button still sends no payload. The handler ignores it today:
  R1 decodes it with `payload(...)`, marks the run `schedule`, and reads `panel_backup_dest`/`_keep`.
- `kindCleanup` ("cleanup"), lock `cleanup`, nil payload; handler `o.runCleanup(ctx, *jobs.Run) error` in
  `internal/service/cleanup.go` is a stub (R2 fills it, and re-checks `cleanup_enabled` itself).

`SetSetting` (admin.go): a write to any `scheduleKnobs` key calls `o.sched.Reload(ctx)` before the proxy push.
`SetServerSettings` goes through it. Test: `internal/service/schedulesettings_test.go`.

VIP hook: `func (o *Orchestrator) rebuildVIPs(ctx context.Context) error` in `internal/service/vipboot.go`
(stub, nil), called in `service.New` (orchestrator.go) and logged with `slog.Warn` on error.

Deviations:
1. "Empty = off" cannot be stored: `Set("")` removes the row and the default applies again. Superseded by G2:
   `panel_backup_enabled`, `orphans_enabled` and `cleanup_enabled` are the switches; no `off` word.
2. `rebuildVIPs` runs just BEFORE `jobs.Start`, not after: `Start` launches the workers at once, so after
   would let a deploy race the rebuild (the brief says "before the first deploy can run").
3. A bad stored spec is skipped in `Entries` (slog.Warn) rather than at `AddFunc`; the old `load` error path
   still covers unparseable crons/backup rows.

## R8 needs
- Runs list label (flow/backup, admin drawer): a managed Postgres dump now starts with the line `-- stackr dump format: cluster` (all databases and roles). An older admin-only dump lacks it and still restores as before. To label a run "admin only" in the UI, `flow/backup/backup.go` would read the first line of the decrypted dump at backup time and store the format on the run row (new column); the archive itself already carries it, so restore needs nothing more.

## R4 needs
- `internal/service/run.go` `afterDeploy` queues an on_deploy function's run after every deploy. A dependent with
  `fn:completed` now runs that function first (`runFirst`), so the function runs twice per deploy. Wave 2: skip the
  afterDeploy run for a function a dependent already ran in the same deploy, or accept it.
- `depends_on: x:completed` on a non-run tile (image, service) always fails ("did not complete"): only function and
  cron tiles have runs. `check.go` `ParseDep` could refuse it at validation; left alone.

## R7 hand-off (VIP boot rebuild)

- `rebuildVIPs` (`internal/service/vipboot.go`) lists pause containers, runs `tile.Leaf.Route` against a collecting
  VIP, then one `vip.Table.Rebuild`. Unreadable tiles are logged and skipped. A VIP that is not a `Rebuild`er (test
  stub) is a no-op.
- `orchestrator.go`: new `vip tile.VIP` field on `Orchestrator`, set in `New` (two lines; the table was unreachable).
- Stale comment: `leaf/tile/world.go` `Route` doc says "stackrd rebuilds every VIP on boot (vip.Rebuild)". Now true,
  but should read `service.rebuildVIPs`; the owner of world.go may reword it.

## Fix round needs (G1)
- `flow/deploy/deploy.go` (G4): the "was cleaned up; rebuild it" error comes
  from `leaf/image` `Ensure` and names the ref, then deploy wraps it as
  "pull <ref>: ...". To name the tile instead, deploy.go would catch it
  (`errors.Is(err, image.ErrCleanedUp)` is not exported yet; ask G1).

## Fix round needs (G5)
- `flow/deploy/deploy.go` `prepare` (G4): the `stackr.ref` replica label is set in `spec.go` from `resolved.pinRef`, falling back to `resolved.image`. A tile started by tag is only comparable with a digest pin if `pinRef` is the digest ref. In `prepare`, after `digest, err := f.Images.Ensure(...)`, add: `if digest != "" { r.pinRef = RepoOf(ref) + "@" + digest }` (before `r.image = ref`). Until then a tag-started tile is labelled with its tag and `runsOther` still reads it as current (as before).

## Fix round needs (G3)
- Runs list label ("admin only" for an old dump): still needs a `format` on the run row; see leftovers.
