Source: config/runpolicy/runpolicy.go (1 file, 42 lines, no tests)
Commit: c2423f0
Taken: the registry of runnable tile kinds and its per-kind gates (schedule, command, ingress).
Cut: package prose; the start/health-gate behaviour `KeepAlive` drives.
Cuts belong to: service/internal/flow/deploy. Source makes no store, Docker or auth call.

```go
type Policy struct { // one runnable tile kind; the base fields are shared, only these differ
	KeepAlive        bool // extract: dropped "start + health-gate a long-lived container after the build", belongs in flow/deploy
	RequiresSchedule bool // gates schedule:
	AllowsCommand    bool // gates command:
	AllowsIngress    bool // gates port: + domains:
}
var Policies = map[string]Policy{
	"service":  {KeepAlive: true, AllowsIngress: true, AllowsCommand: true},
	"cron":     {RequiresSchedule: true, AllowsCommand: true},
	"function": {AllowsCommand: true},
}
func For(kind string) (Policy, bool) { p, ok := Policies[kind]; return p, ok } // false = not runnable (managed, volume, slice)
```

| kind | allowed fields | refused fields | v1 |
|---|---|---|---|
| service | command, port, domains | schedule* | keep |
| cron | command, schedule | port, domains | later — cron scheduler triggers the run |
| function | command | schedule, port, domains | later — `Tile.RunOnDeploy` triggers the run |
| image | absent: build-vs-image is a base field, not a kind here | — | new in v1 |
| managed | none, `For` returns false | all of the above | keep |
| base, all kinds | source (build/image), env, refs, limits | — | not gated in source |

## Notes for the builder
- Spec building is **not** in this package. The `flow/deploy` half of this row has no source here; it needs a different row.
- The registry gates three features, not fields: volumes, replicas, healthcheck, env are shared base, accepted by every kind with no per-kind check. B26's "one whitelist of what a tile kind may carry" is therefore new structure, not a port. (*`RequiresSchedule` is a required-ness flag, so `service` refusing `schedule` is inferred, not stated by the source.)
- `KeepAlive` is the only lifecycle bit: false means the build stops at the artifact and something outside the deploy flow triggers the run.

Size: source 42 lines, extract 36 lines
