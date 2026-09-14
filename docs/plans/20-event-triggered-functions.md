# Event-triggered functions

Status: discussion only, 2026-09-05. Nothing agreed, nothing built. The shape
below is where the conversation got to; the open questions at the end are
what has to be settled before this becomes a real plan.

## The idea

Function tiles: a container that runs on an event rather than on a clock.
Two triggers wanted, with room for more later:

- a Postgres `NOTIFY` on a named channel
- an object created, changed or deleted in an S3 bucket

## What already exists

There is no function runtime to build. `infra/jobs/jobs.go` already is one:
it runs a one-shot container from an app's image, from a custom image, or
`exec`s in a running container, with a timeout, the right network attached,
output capture into a `CronRun` row, and an overlap guard. Tile kind is
already `service | cron` (`store/repo/models.go:240`).

So the feature is not a runtime, it is **a second trigger source**. Three
parts:

1. a trigger definition on the existing job model (cron expression, or event)
2. listeners that turn an event into a call into the same run path
3. the event payload delivered into the container, env var being the cheap way

Open: whether a function is a new tile kind on the canvas, or just a second
trigger type on the existing cron job. A new kind drags in lifecycle, panel,
plan/apply serialization and config-as-code schema. A trigger type is a
field plus listeners, and it appears in the UI where jobs already do.

## The network problem

For a Postgres listener something has to hold an open connection to the
tile's container, which means being on that stack's network.

stackr already does this. `reachInstance()`
(`infra/managedtiles/provision_s3.go:296`) connects stackrd's own container
to the tile's shared network and dials the tile by its alias. It is called
from four places, two of them hot:

- `s3browse.go:21`, every bucket listing in the UI
- `reconcile.go:96`, the periodic reconcile loop
- `provision_s3.go:35` and `:220`, provision and drop

Nothing disconnects afterwards. The only two `NetworkDisconnect` call sites
in the repo are both in `infra/runtime/runtime.go`: line 148 inside
`RemoveNetwork` (detaching members so the network can be deleted) and line
345 in the wipe path (which also removes every container first). So the
membership is permanent, not momentary, and it accumulates one network per
stack that has ever provisioned a bucket.

The cost is not stackrd reaching in. Docker networks are bidirectional, so
every container on that network can reach stackrd on port 8080, the web UI
and the API that control the whole box. stackrd holds the Docker socket, so
joining a stack network grants it no new power, but it makes the most
privileged process on the machine addressable from the least trusted
containers on it.

Options considered:

1. Leave it connected, bind or firewall the control plane so containers on
   shared networks cannot reach 8080. One change, no churn, no races. Worth
   doing regardless of this feature.
2. Refcount the membership, disconnect when the last user leaves. Correct,
   but new state to get wrong, and reconcile re-acquires it every cycle so in
   practice it stays connected anyway.
3. Idle sweep: drop shared-net memberships with no in-flight work. No
   per-call races, but stackrd flaps and the next browse pays a reconnect.
4. An agent container (below).

A naive `defer disconnect` in `reachInstance` is not an option: it would
churn on every file-browser page view, and two concurrent callers would race,
one disconnecting while the other is mid-request.

## How other platforms handle it

Kubernetes does not put the API server on pod networks. Pods reach it inbound
via a ClusterIP. Where the reverse direction was genuinely needed, they built
konnectivity, an egress-selector proxy: an agent inside the cluster network
with an outbound tunnel to the control plane. A whole component whose reason
for existing is refusing to make the API server routable from workload
networks. `exec` and `logs` go via the kubelet on the node, not the pod
network.

Nomad and Swarm are the same shape: the node agent touches workloads, the
servers do not. Neon, Supabase and PlanetScale never dial tenant databases
from the control plane; a per-tenant proxy or pooler sits in the data path
holding only that tenant's credentials. Every managed platform's event
triggers are inbound, S3 to Lambda included.

The pattern is not "never touch the data plane". It is: split the thing that
touches the data plane from the thing that holds authority.

## Proposed shape: an event agent container

Precedent is already in the repo. `cmd/proxyrelay` is a separate `main`, one
static binary on `FROM scratch`, one job, exits when idle, with a comment
saying a relay container must be structurally unable to boot server code.
Same reasoning applies here.

The agent joins the stack network, holds only that stack's credentials, does
the `LISTEN`, and posts events outbound to stackrd, which triggers the job.
stackrd joins nothing. It is konnectivity scaled down to roughly 150 lines.

Cost: one more container to build, ship, version and supervise.

For S3 the direction is already right without an agent: MinIO-compatible
servers push bucket events to a webhook, so stackrd exposes an endpoint and
the tile calls it. Outbound from the tile, no network join.

Deferred deliberately: moving provisioning itself into a vanishing container.
Provision, drop, browse and reconcile all return structured results, so that
is a request/response protocol plus spawn-wait-read-kill lifecycle on every
UI file browse. It buys the same isolation option 1 buys for free. If the
event agent lands and the appetite is still there, revisit.

## Open questions

- **Tile kind or trigger type.** Decides how much of the config, plan and UI
  surface this touches. Nothing else can be scoped until this is settled.
- **Delivery guarantees.** `LISTEN`/`NOTIFY` is fire and forget: no queue, no
  replay, 8000-byte payload cap. An event fired while stackrd or the agent is
  restarting is gone with no record it existed. If "this will definitely run"
  is required, the answer is an outbox table and polling, which is a
  different feature.
- **Burst semantics.** `begin(ref, allowOverlap)` in `jobs.go:69` skips a run
  that is already in flight. Correct for cron, wrong for events: fifty
  uploads would drop forty-nine. Queue, coalesce or drop is a real fork and
  the existing guard does not answer it.
- **Does RustFS implement bucket notifications?** Unverified. The check is
  `PutBucketNotificationConfiguration` against a dev instance with the client
  `provision_s3.go` already builds; a 501 answers it. If absent, the fallback
  is a cron job polling `ListObjectsV2`, which needs no new code at all.
- **Agent credential.** If the agent authenticates with a general API token,
  a compromised app container that pops the agent gets a control-plane
  credential and the hole has moved rather than closed. It needs a capability
  scoped to one verb: trigger jobs belonging to stack X.
- **One agent per stack or per host.** Per host means it joins every stack
  network and a compromise yields every credential. Per stack contains the
  blast radius at the price of more containers.
- **Who issues the `NOTIFY`.** The user's app or a user-authored SQL trigger.
  stackr cannot make it happen, so the docs and the UI have to say so.
- **Cold start.** Roughly a second per event from docker create plus start.
  A ceiling to name, not to design around yet.
