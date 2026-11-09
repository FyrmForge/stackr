package operated

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Streaming replication of a postgres instance: a stackr-owned role the
// standbys log in as, one physical slot per standby, and the standby's own
// container. The primary is the instance tile's container (flow/deploy); a
// standby is made by the orchestrator (service/members.go) from StandbySpec.

const (
	// ReplUser is the replication role.
	ReplUser = "stackr_repl"
	// ReplPassfile is the standby's passfile, outside the data directory
	// (pg_basebackup wants it empty) and rewritten from the env at every
	// start. primary_conninfo names it, so no secret is in a config file.
	ReplPassfile = "/var/lib/postgresql/stackr.pgpass"
	// LabelMember marks a standby's containers (service.LabelMember).
	LabelMember = "stackr.member"
	// ReadSuffix is added to the instance's alias for its read alias.
	ReadSuffix = "-read"
)

// ReadAlias is the instance's name for reads: on a standby's node, the
// standby; on the primary's, the primary; from any other node a hosts line
// to the primary's VIP.
func ReadAlias(instanceAlias string) string { return instanceAlias + ReadSuffix }

// SlotName is the physical slot of member id: slot names take lowercase
// letters, digits and underscores only, which a uuid's hyphens are not.
func SlotName(memberID string) string {
	return "stackr_" + strings.ReplaceAll(memberID, "-", "_")
}

// replRoleSQL makes the replication role. roleExists picks CREATE or ALTER (a
// replayed CREATE failing would make postgres log the statement, password
// included).
func replRoleSQL(roleExists bool, password string) []string {
	verb := "CREATE"
	if roleExists {
		verb = "ALTER"
	}
	return []string{fmt.Sprintf(`%s ROLE %s REPLICATION LOGIN PASSWORD '%s'`, verb, ReplUser, password)}
}

// slotCreateSQL makes slot unless it exists.
func slotCreateSQL(slot string) string {
	return `SELECT pg_create_physical_replication_slot('` + slot + `') WHERE NOT EXISTS ` +
		`(SELECT FROM pg_replication_slots WHERE slot_name = '` + slot + `')`
}

// slotDropSQL drops slot; a walsender still holding it is ended first.
func slotDropSQL(slot string) []string {
	return []string{
		`SELECT pg_terminate_backend(active_pid) FROM pg_replication_slots WHERE slot_name = '` + slot + `' AND active_pid IS NOT NULL`,
		`SELECT pg_sleep(1)`,
		`SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = '` + slot + `'`,
	}
}

// slotStatesSQL lists stackr's slots with their wal_status (lost = the cap
// took the WAL the standby needs).
const slotStatesSQL = `SELECT slot_name || '|' || coalesce(wal_status, '') FROM pg_replication_slots WHERE slot_name LIKE 'stackr\_%'`

// ParseSlotStates reads slotStatesSQL's output: slot to wal_status.
func ParseSlotStates(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if n, s, ok := strings.Cut(strings.TrimSpace(l), "|"); ok {
			m[n] = s
		}
	}
	return m
}

// replLiveScript makes a running primary take the replication line and the
// owned conf without a restart: the include is added after the file is
// written (it is fatal when the file is missing).
const replLiveScript = `d="$0"; grep -qx "include 'stackr-owned.conf'" "$d/postgresql.conf" || printf "include 'stackr-owned.conf'\n" >> "$d/postgresql.conf"
grep -qxF "$1" "$d/pg_hba.conf" || printf '%s\n' "$1" >> "$d/pg_hba.conf"`

// PrepareStandby is the primary's half of a new standby: the replication
// role (the password is minted once and stored), its pg_hba line and the
// slot cap in a running primary (reloaded), and the standby's slot. The
// primary has to be running. Every step is repeatable.
func (f *Flow) PrepareStandby(ctx context.Context, it store.Tile, slot string) error {
	m, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return err
	}
	if m.ReplPassword == "" {
		if m, err = f.Instances.SetReplPassword(ctx, m, operated.Password()); err != nil {
			return err
		}
		i.ReplPassword = m.ReplPassword
	}
	i = f.withNodeMem(ctx, it, i)
	psql := pg{}.psql(i, i.AdminDB, nil)
	out, err := x.Exec(ctx, append(psql, "-tA", "-c", `SELECT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+ReplUser+`')::int`))
	if err != nil {
		return err
	}
	if err := pgExecSQL(ctx, x, i, i.AdminDB, replRoleSQL(strings.TrimSpace(out) == "1", i.ReplPassword)); err != nil {
		return err
	}
	if _, err := x.Exec(ctx, pgFileWrite(i, "stackr-owned.conf", pgOwnedConf(i))); err != nil {
		return err
	}
	if _, err := x.Exec(ctx, []string{"sh", "-c", replLiveScript, pgDataDir(i.Major), pgReplHBA}); err != nil {
		return err
	}
	if _, err := x.Exec(ctx, append(psql, "-tA", "-c", `SELECT pg_reload_conf()`)); err != nil {
		return err
	}
	return pgExecSQL(ctx, x, i, i.AdminDB, []string{slotCreateSQL(slot)})
}

// DropSlot drops a standby's slot on the primary.
func (f *Flow) DropSlot(ctx context.Context, it store.Tile, slot string) error {
	_, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return err
	}
	return pgExecSQL(ctx, x, i, i.AdminDB, slotDropSQL(slot))
}

// SlotStates reads the primary's stackr slots (slot to wal_status).
func (f *Flow) SlotStates(ctx context.Context, it store.Tile) (map[string]string, error) {
	_, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return nil, err
	}
	out, err := x.Exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", slotStatesSQL))
	if err != nil {
		return nil, err
	}
	return ParseSlotStates(out), nil
}

// standbyProbeSQL answers "1,streaming" on a standby whose WAL receiver is
// streaming.
const standbyProbeSQL = `SELECT pg_is_in_recovery()::int || ',' || coalesce((SELECT status FROM pg_stat_wal_receiver LIMIT 1), '')`

// StandbyProbeArgv is the argv to run in a standby's container; StandbyStreaming
// reads its output.
func (f *Flow) StandbyProbeArgv(ctx context.Context, it store.Tile) ([]string, error) {
	_, i, _, err := f.postgres(ctx, it)
	if err != nil {
		return nil, err
	}
	return append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", standbyProbeSQL), nil
}

// StandbyStreaming says whether the probe's output is a standby in recovery
// with its WAL receiver streaming.
func StandbyStreaming(out string) bool { return strings.TrimSpace(out) == "1,streaming" }

// standbyPrep runs before the shared start tail in a standby's container. An
// empty data directory is filled by pg_basebackup from the primary as the
// replication role, over a slot made beforehand; never -R, which would write
// primary_conninfo (and a password) to postgresql.auto.conf, which beats
// stackr-owned.conf. The passfile holds the password, read from the env; it
// is never on an argv. A failed copy empties the directory again and stops
// the container: falling through to the image's entrypoint would initdb a
// new, independent cluster here. The primary's conf files and auto.conf come
// along in the copy and are dropped, for the tail to write this member's.
const standbyPrep = `(umask 077; printf '%s:%s:*:` + ReplUser + `:%s\n' "$STACKR_PRIMARY" "$STACKR_PORT" "$STACKR_REPL_PASS" > ` + ReplPassfile + `)
chown postgres:postgres ` + ReplPassfile + `
d="$PGDATA"
if [ ! -s "$d/PG_VERSION" ]; then
mkdir -p "$d" && find "$d" -mindepth 1 -delete || exit 1
if ! PGPASSFILE=` + ReplPassfile + ` pg_basebackup -h "$STACKR_PRIMARY" -p "$STACKR_PORT" -U ` + ReplUser +
	` -D "$d" -X stream -S "$STACKR_SLOT" -c fast -w; then
find "$d" -mindepth 1 -delete
exit 1
fi
rm -f "$d/stackr.conf" "$d/stackr-owned.conf" "$d/.stackr.conf.host" "$d/postgresql.auto.conf"
touch "$d/standby.signal"
chown -R postgres:postgres "$d"
chmod 700 "$d"
fi
`

// standbyCommand is a standby container's command: the prep, the shared
// start tail, then postgres with the same owned flags as the primary.
func standbyCommand(i Instance) []string {
	cmd := []string{"sh", "-c", standbyPrep + pgStartTail, "sh", "postgres"}
	for _, f := range pgFlags(i.Major, 0) {
		cmd = append(cmd, "-c", f)
	}
	return cmd
}

// StandbySpec is the container of standby mem of instance tile it: the
// primary's image and major, the same settings (every member gets the same
// ones, so no limit of the standby's is below the primary's), no archiving
// (a standby does not archive), its own data volume and network on its node,
// and restart "no" (the panel starts it). hostLine is the "alias:vip" line
// that makes the primary's alias resolve.
func (f *Flow) StandbySpec(ctx context.Context, it store.Tile, mem store.OperatedMember, hostLine string) (docker.ContainerSpec, error) {
	m, i, _, err := f.postgres(ctx, it)
	if err != nil {
		return docker.ContainerSpec{}, err
	}
	def := Engines["postgres"].Definition()
	if err := checkMajor(def, m, it); err != nil {
		return docker.ContainerSpec{}, err
	}
	if m.ReplPassword == "" {
		return docker.ContainerSpec{}, errs.Conflictf("%s has no replication role yet", it.Slug)
	}
	i = f.withNodeMem(ctx, it, i)
	i.PITRDays = 0
	i.ReplPassword = m.ReplPassword
	i.PrimaryHost, i.Slot = i.Host, SlotName(mem.ID)
	img := def.Image
	if it.ImageRef != "" {
		img = it.ImageRef
	}
	env := append(def.Config(i),
		"STACKR_PRIMARY="+i.PrimaryHost,
		"STACKR_PORT="+strconv.Itoa(i.Port),
		"STACKR_SLOT="+i.Slot,
		"STACKR_REPL_PASS="+i.ReplPassword,
	)
	return docker.ContainerSpec{
		Name:       mem.Container,
		Image:      img,
		Cmd:        standbyCommand(i),
		Env:        env,
		Labels:     map[string]string{LabelMember: mem.ID},
		Volumes:    []string{mem.Volume + ":" + pgMount(i.Major)},
		Networks:   []docker.NetAttach{{Name: operated.Network(m.ID), Aliases: []string{ReadAlias(i.Host)}}},
		ExtraHosts: []string{hostLine},
		MemLimitMB: it.MemLimitMB,
		Restart:    "no",
	}, nil
}
