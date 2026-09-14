# ADR-001: Logical database activity is a card metric, not an edge metric

- **Status**: Accepted
- **Date**: 2026-07-29

## Context

Provisioned slices (a logical database or bucket carved out of a shared
instance) render as their own tiles on the canvas, and the instance hosting
them is hidden — `internal/graph/graph.go`, `KindResource`.

That raised an obvious question: the canvas already animates edges with live
traffic, so why don't the edges into those slices animate too?

Because the traffic overlay measures containers. `internal/metrics/metrics.go`
reads `/proc/net/nf_conntrack` and keys every flow by container IP and port.
A logical database is not a container:

- all databases on one instance share one IP and one port;
- which database a connection is for is chosen inside the PostgreSQL startup
  packet, at the application layer, after the TCP connection exists;
- so two connections from one consumer to two different databases are
  indistinguishable at layer 4.

There is no attribution to recover from packet headers. The information was
never on the wire.

## Decision

Slice cards carry **transactions/second and size on disk**, read from
PostgreSQL itself (`pg_stat_database`, `pg_database_size`) by
`databases.SliceStats`. The reference edges into a busy slice pulse
(`.edge-busy`), but carry **no number**.

Every consumer's edge into a busy database pulses, not just one. Postgres
counts transactions per database and never per client, so which consumer
caused them is unknowable. The edges say "this database is active"; the card
says how much. This is deliberate — it is not a bug to be fixed by picking a
consumer.

## Alternatives rejected

**Attribute container traffic when a consumer has exactly one slice on an
instance.** Exact where it applies, but the gap is invisible: a consumer with
several slices (a cron touching three databases) shows no line at all, which
reads as "no traffic" rather than "withheld". It also disappears the day
someone provisions a second database for an app — adding configuration would
remove a feature.

**Join conntrack's source port to `pg_stat_activity.client_port`.** Would give
exact per-connection attribution in principle. Rejected: connections that open
and close between samples are never attributed, which biases against the
chattiest workloads; two sample sources on two cadences make attribution flap;
it is postgres-shaped and impossible for S3 (the bucket is in the HTTP request
path); and it collapses entirely behind a connection pooler, which is exactly
what a shared instance invites.

**Count active backends per consumer, grouped by `client_addr`.** This was
tested rather than reasoned about, and it does not work. Six samples taken
during genuine application load showed every connection as `idle` and never
once `active` — application queries last milliseconds, so a sampler almost
never lands inside one. Connection *counts* are useless too, because apps hold
long-lived pooled connections that barely change. See "verified facts" below
before proposing this again.

**Give each slice its own network identity** — a proxy per instance listening
on a port per database. This is the only design where conntrack could attribute
flows natively and exactly, for every consumer, with no polling and no engine
specific views. Not rejected on merit: it needs a supervised process per
instance, adds a hop to every query, and forces a pooling-mode decision
(transaction vs session mode changes prepared statements and advisory locks).
Revisit if per-consumer figures ever justify the infrastructure.

## Verified facts

Established empirically on the test VM, not inferred:

- `pg_stat_activity.client_addr` maps exactly to the consumer tile — confirmed
  against `docker inspect` for all four consumers. The sampler already builds
  container-IP → tile refs in `sampleFlows`, so the join is free. This is a
  sound basis for a *declared vs actual* check ("who is connected to this
  database right now" versus what the variables say), but **not** for load.
- Active-backend sampling never observes activity under real load (above).
- A short-lived consumer is invisible to any connection-based method. The cron
  that shares three of the four slices connects for about a second a day.

## Consequences

- Databases show activity as numbers; only services show flow lines. A canvas
  where the databases don't pulse in step with the byte rates is correct.
- Hiding the instance removed its container-level traffic edges along with it.
  Those bytes are still measured, they just have no node to attach to.
- A burst shorter than the 30s sample interval can fall between ticks and never
  appear. The cadence suits "how busy is this database", not spike detection.
- A deploy resets the sampler's in-memory state: sizes return after one tick,
  rates after two.
- Postgres only. MySQL, mongo and S3 each need their own collector branch —
  S3 in particular cannot use SQL at all (bucket size and object count come
  from its API).
