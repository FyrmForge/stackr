package operated

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The engine's half of a failover. The orchestrator (service/failover.go)
// sequences the steps; these are the container, the SQL and the shell they
// run.

// PromotedSpec is the container of member mem once it is the instance's
// primary: made after pg_promote, on the member's own data volume, in place
// of the standby container. It must never run standbyPrep: a promoted data
// directory has no standby.signal, so that script would read it as a cut-short
// copy and empty it. So it runs the primary's own command, carries the tile's
// labels (every Replicas, exec and stream of the instance finds it there, see
// tile.Leaf.Reroute) and answers both the write and the read alias on the
// instance's network of its node, at ip (pinned, "" = Docker picks): the
// address cross-node consumers dial.
// ponytail: it does not archive. The shipper reads the tile's own archive
// volume on the env's node, so a relocated primary has none to read; PITR
// restarts when the instance is back on its home node.
func (f *Flow) PromotedSpec(ctx context.Context, it store.Tile, mem store.OperatedMember, ip string) (docker.ContainerSpec, error) {
	m, i, _, err := f.postgres(ctx, it)
	if err != nil {
		return docker.ContainerSpec{}, err
	}
	def := Engines["postgres"].Definition()
	if err := checkMajor(def, m, it); err != nil {
		return docker.ContainerSpec{}, err
	}
	i = f.withNodeMem(ctx, it, i)
	i.PITRDays = 0
	img := def.Image
	if it.ImageRef != "" {
		img = it.ImageRef
	}
	env := append(def.Config(i), "STACKR_PG_REWRITE=1") // the files follow the env at every start
	return docker.ContainerSpec{
		Name:  mem.Container,
		Image: img,
		Cmd:   def.Start(i),
		Env:   env,
		Labels: map[string]string{
			tile.LabelTile: it.ID, tile.LabelRole: "replica", LabelMember: mem.ID,
		},
		Volumes:    []string{mem.Volume + ":" + pgMount(i.Major)},
		Networks:   []docker.NetAttach{{Name: operated.Network(m.ID), Aliases: []string{i.Host, ReadAlias(i.Host)}, IPv4: ip}},
		MemLimitMB: it.MemLimitMB,
		Restart:    "no",
	}, nil
}

// PsqlArgv is the argv that runs sql as the admin over the socket in the
// instance's container: one value per line (-tA).
func (f *Flow) PsqlArgv(ctx context.Context, it store.Tile, sql string) ([]string, error) {
	_, i, _, err := f.postgres(ctx, it)
	if err != nil {
		return nil, err
	}
	return append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", sql), nil
}

// The queries a failover asks.
const (
	SQLInRecovery = `SELECT pg_is_in_recovery()::int`
	SQLReplayLSN  = `SELECT coalesce(pg_last_wal_replay_lsn()::text, '')`
	SQLPromote    = `SELECT pg_promote(true, 60)::int`
	SQLSwitchWAL  = `SELECT pg_switch_wal()::text`
	SQLCurrentWAL = `SELECT pg_walfile_name(pg_current_wal_lsn())`
)

// SQLArchivedPast is 1 once the archiver has shipped segment seg or a later
// one.
func SQLArchivedPast(seg string) string {
	return `SELECT (coalesce(last_archived_wal, '') >= '` + strings.ReplaceAll(seg, `'`, "") + `')::int FROM pg_stat_archiver`
}

// ControlDataArgv reads a stopped cluster's control file; run it in a
// container that has the data volume.
func ControlDataArgv(major int) []string { return []string{"pg_controldata", pgDataDir(major)} }

// DataMount is where a member's data volume mounts.
func DataMount(major int) string { return pgMount(major) }

// ControlData is what a stopped cluster's control file says: its state
// ("shut down" is the clean one) and where its last checkpoint is.
type ControlData struct{ State, Checkpoint string }

// ParseControlData reads pg_controldata's output.
func ParseControlData(out string) ControlData {
	var c ControlData
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Database cluster state":
			c.State = strings.TrimSpace(v)
		case "Latest checkpoint location":
			c.Checkpoint = strings.TrimSpace(v)
		}
	}
	return c
}

// ParseLSN is a pg_lsn ("0/3000060") as a number.
func ParseLSN(s string) (uint64, error) {
	hi, lo, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return 0, fmt.Errorf("not an LSN: %q", s)
	}
	h, err := strconv.ParseUint(hi, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("not an LSN: %q", s)
	}
	l, err := strconv.ParseUint(lo, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("not an LSN: %q", s)
	}
	return h<<32 | l, nil
}

// rewindScript runs pg_rewind on the old primary's data directory against the
// new primary, as postgres, then makes the directory a finished standby the
// way standbyPrep does after a copy (the primary's conf files go, the marker
// is written last). It exits non-zero when the rewind did not work, leaving
// no marker: the standby container's own prep then copies the primary afresh,
// which is the rebuild.
const rewindScript = `[ -s ` + ReplPassfile + ` ] && chown postgres:postgres ` + ReplPassfile + ` && chmod 600 ` + ReplPassfile + ` || exit 1
d="$PGDATA"
[ -s "$d/PG_VERSION" ] || exit 1
rm -f "$d/` + standbyDone + `"
as() { if command -v gosu >/dev/null 2>&1; then gosu postgres "$@"; else su-exec postgres "$@"; fi; }
as env PGPASSFILE=` + ReplPassfile + ` pg_rewind -D "$d" --source-server "host=$STACKR_PRIMARY port=$STACKR_PORT user=$STACKR_ADMIN dbname=postgres connect_timeout=10" || exit 1
rm -f "$d/stackr.conf" "$d/stackr-owned.conf" "$d/.stackr.conf.host" "$d/postgresql.auto.conf"
touch "$d/standby.signal"
chown -R postgres:postgres "$d"
chmod 700 "$d"
sync
touch "$d/` + standbyDone + `" && chown postgres:postgres "$d/` + standbyDone + `" && sync || exit 1
`

// RewindSpec is the one-shot container that rewinds the old primary's volume
// (mem's) onto the new primary, reached by hostLine ("alias:ip"). Its login
// is the admin's, in a passfile (RewindPassfile) the caller copies in before
// the start: no secret in the env.
func (f *Flow) RewindSpec(ctx context.Context, it store.Tile, mem store.OperatedMember, name, hostLine string) (docker.ContainerSpec, error) {
	m, i, _, err := f.postgres(ctx, it)
	if err != nil {
		return docker.ContainerSpec{}, err
	}
	def := Engines["postgres"].Definition()
	img := def.Image
	if it.ImageRef != "" {
		img = it.ImageRef
	}
	return docker.ContainerSpec{
		Name:  name,
		Image: img,
		Cmd:   []string{"sh", "-c", rewindScript},
		Env: []string{
			"STACKR_PRIMARY=" + i.Host, "STACKR_PORT=" + strconv.Itoa(i.Port), "STACKR_ADMIN=" + i.AdminUser,
			"PGDATA=" + pgDataDir(i.Major),
		},
		Labels:     map[string]string{LabelMember: mem.ID},
		Volumes:    []string{mem.Volume + ":" + pgMount(i.Major)},
		Networks:   []docker.NetAttach{{Name: operated.Network(m.ID)}},
		ExtraHosts: []string{hostLine},
		Restart:    "no",
	}, nil
}

// RewindPassfile is the admin's passfile content for the new primary's alias.
func (f *Flow) RewindPassfile(ctx context.Context, it store.Tile) ([]byte, error) {
	_, i, _, err := f.postgres(ctx, it)
	if err != nil {
		return nil, err
	}
	if i.AdminPassword == "" {
		return nil, errs.Conflictf("%s has no admin password", it.Slug)
	}
	return []byte(fmt.Sprintf("%s:%d:*:%s:%s\n", i.Host, i.Port, i.AdminUser, i.AdminPassword)), nil
}

// ControlSpec is a one-shot container with member mem's volume that prints
// the cluster's control file.
func (f *Flow) ControlSpec(ctx context.Context, it store.Tile, name, volume string) (docker.ContainerSpec, error) {
	_, i, _, err := f.postgres(ctx, it)
	if err != nil {
		return docker.ContainerSpec{}, err
	}
	def := Engines["postgres"].Definition()
	img := def.Image
	if it.ImageRef != "" {
		img = it.ImageRef
	}
	return docker.ContainerSpec{
		Name:    name,
		Image:   img,
		Cmd:     ControlDataArgv(i.Major),
		Volumes: []string{volume + ":" + pgMount(i.Major) + ":ro"},
		Restart: "no",
	}, nil
}
