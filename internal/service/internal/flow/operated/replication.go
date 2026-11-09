package operated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

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
	// ReplPassfile is the standby's passfile, outside every volume and the
	// data directory. The orchestrator copies it into the created container
	// before it starts (docker.CopyTo), so the password is in no env and no
	// config file; primary_conninfo names the path.
	ReplPassfile = "/etc/stackr.pgpass"
	// standbyDone marks a standby's data directory as completely bootstrapped:
	// written last, after the copy, the cleanup, standby.signal, ownership and
	// a sync. A directory with PG_VERSION and no marker is a copy that was cut
	// short, and is made again.
	standbyDone = ".stackr.standby.done"
	// LabelConf is the hash of the settings file a standby's container was
	// made with; a start with another one makes the container again.
	LabelConf = "stackr.conf"
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

// replLiveScript makes a running primary take the owned conf and the
// replication lines without a restart: the include is added after the file
// is written (it is fatal when the file is missing). The lines ($1, one per
// standby subnet, empty = none) are a marked block of pg_hba.conf, replaced
// as a whole: every postgres major reads it (an include in pg_hba.conf is
// from 16).
const replLiveScript = `d="$0"
grep -qx "include 'stackr-owned.conf'" "$d/postgresql.conf" || printf "include 'stackr-owned.conf'\n" >> "$d/postgresql.conf"
[ -e "$d/stackr-sync.conf" ] || { : > "$d/stackr-sync.conf"; chown postgres:postgres "$d/stackr-sync.conf"; }
grep -qx "include 'stackr-sync.conf'" "$d/postgresql.conf" || printf "include 'stackr-sync.conf'\n" >> "$d/postgresql.conf"
f="$d/pg_hba.conf"
{ sed '/^# stackr replication begin$/,/^# stackr replication end$/d' "$f"
if [ -n "$1" ]; then printf '# stackr replication begin\n%s\n# stackr replication end\n' "$1"; fi; } > "$f.new" &&
chown postgres:postgres "$f.new" && chmod 600 "$f.new" && mv "$f.new" "$f"`

// replHBALines is one replication line per standby network: the replication
// role only, from that member's /24 (the firewall lets only member networks
// reach the primary's VIP). The image's own catch-all line is for "all"
// databases, which never includes replication.
func replHBALines(subnets []string) string {
	var l []string
	for _, s := range subnets {
		l = append(l, "host replication "+ReplUser+" "+s+" scram-sha-256")
	}
	return strings.Join(l, "\n")
}

// walBudgetMB is what the primary's node can give to WAL a standby's slot
// holds back: the free disk less a headroom of 10% of the disk (5 GB at
// least); negative when the disk is short of that.
func walBudgetMB(freeMB, totalMB int) int { return freeMB - max(5<<10, totalMB/10) }

// minWALBudgetMB is the least budget a standby may be added with.
const minWALBudgetMB = 1 << 10

// WALBudget refuses adding a standby while the primary's node has under 1 GB
// to spare for WAL a lagging standby would hold back.
func (f *Flow) WALBudget(ctx context.Context, it store.Tile) error {
	i := f.withNodeMem(ctx, it, Instance{Engine: "postgres"})
	if i.DiskFreeMB > 0 && walBudgetMB(i.DiskFreeMB, i.DiskTotalMB) < minWALBudgetMB {
		return errs.Conflictf("%s's node has too little free disk to keep WAL for a standby. Free some space first.", it.Slug)
	}
	return nil
}

// PrepareStandby is the primary's half of a new standby: the replication
// role (the password is minted once and stored), the replication lines of
// subnets in pg_hba.conf and the slot cap in a running primary (reloaded),
// and the standby's slot. The primary has to be running. Every step is
// repeatable.
func (f *Flow) PrepareStandby(ctx context.Context, it store.Tile, slot string, subnets []string) error {
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
	if _, err := x.Exec(ctx, []string{"sh", "-c", replLiveScript, pgDataDir(i.Major), replHBALines(subnets)}); err != nil {
		return err
	}
	if _, err := x.Exec(ctx, append(psql, "-tA", "-c", `SELECT pg_reload_conf()`)); err != nil {
		return err
	}
	return pgExecSQL(ctx, x, i, i.AdminDB, []string{slotCreateSQL(slot)})
}

// SyncReplHBA makes the primary's replication lines those of subnets and
// reloads. With none left the role is closed: no login, and the stored
// password is dropped, so the next standby gets a new one (PrepareStandby).
func (f *Flow) SyncReplHBA(ctx context.Context, it store.Tile, subnets []string) error {
	m, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return err
	}
	if _, err := x.Exec(ctx, []string{"sh", "-c", replLiveScript, pgDataDir(i.Major), replHBALines(subnets)}); err != nil {
		return err
	}
	if len(subnets) == 0 && m.ReplPassword != "" {
		if err := pgExecSQL(ctx, x, i, i.AdminDB, []string{`ALTER ROLE ` + ReplUser + ` NOLOGIN PASSWORD NULL`}); err != nil {
			return err
		}
		if _, err := f.Instances.SetReplPassword(ctx, m, ""); err != nil {
			return err
		}
	}
	_, err = x.Exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", `SELECT pg_reload_conf()`))
	return err
}

// SyncNoneSlot is a name no standby has: required mode names it when it has no
// standby to wait for, so a commit waits for something that never answers.
const SyncNoneSlot = "stackr_none"

// SyncStandbyNames is the setting that makes the primary wait for any one of
// slots (application_name is the slot name); "" for none = asynchronous.
func SyncStandbyNames(slots []string) string {
	if len(slots) == 0 {
		return ""
	}
	q := make([]string, len(slots))
	for k, s := range slots {
		q[k] = `"` + s + `"`
	}
	return "ANY 1 (" + strings.Join(q, ", ") + ")"
}

// ReplicaState is one row of the primary's pg_stat_replication.
type ReplicaState struct {
	Name      string // application_name = the slot
	Streaming bool
	ReplyAge  float64 // seconds since the standby last replied; -1 = never
}

const replicaStatesSQL = `SELECT application_name || '|' || (state = 'streaming')::int || '|' || ` +
	`coalesce(round(extract(epoch FROM now() - reply_time))::int, -1) FROM pg_stat_replication`

// ReplicaStates reads the primary's pg_stat_replication.
func (f *Flow) ReplicaStates(ctx context.Context, it store.Tile) ([]ReplicaState, error) {
	_, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return nil, err
	}
	out, err := x.Exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", replicaStatesSQL))
	if err != nil {
		return nil, err
	}
	return ParseReplicaStates(out), nil
}

// ParseReplicaStates reads replicaStatesSQL's output.
func ParseReplicaStates(out string) []ReplicaState {
	var rs []ReplicaState
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		p := strings.Split(strings.TrimSpace(l), "|")
		if len(p) != 3 {
			continue
		}
		age, err := strconv.Atoi(p[2])
		if err != nil {
			continue
		}
		rs = append(rs, ReplicaState{Name: p[0], Streaming: p[1] == "1", ReplyAge: float64(age)})
	}
	return rs
}

// SyncOwned makes the running primary hold what it should: stackr-owned.conf
// (the slot cap follows the node's free disk) and stackr-sync.conf (syncNames,
// the slots it waits for) are rewritten when they differ, and it reloads when
// one was or when the live synchronous_standby_names is not syncNames (a
// reload that failed earlier is retried). It returns an error unless the live
// value is syncNames afterwards: only then is it true.
func (f *Flow) SyncOwned(ctx context.Context, it store.Tile, syncNames string) error {
	_, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return err
	}
	i = f.withNodeMem(ctx, it, i)
	dir := pgDataDir(i.Major)
	changed := false
	for _, c := range [][2]string{{"stackr-owned.conf", pgOwnedConf(i)}, {"stackr-sync.conf", pgSyncConf(syncNames)}} {
		name, want := c[0], c[1]
		have, err := x.Exec(ctx, []string{"sh", "-c", `cat "$0" 2>/dev/null; true`, dir + "/" + name})
		if err != nil {
			return err
		}
		if have == want {
			continue
		}
		if _, err := x.Exec(ctx, pgFileWrite(i, name, want)); err != nil {
			return err
		}
		changed = true
	}
	psql := append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c")
	live := func() (string, error) {
		out, err := x.Exec(ctx, append(slices.Clone(psql), `SELECT current_setting('synchronous_standby_names')`))
		return strings.TrimSpace(out), err
	}
	cur, err := live()
	if err != nil {
		return err
	}
	if !changed && cur == syncNames {
		return nil
	}
	for try := 0; ; try++ {
		if _, err := x.Exec(ctx, append(slices.Clone(psql), `SELECT pg_reload_conf()`)); err != nil {
			return err
		}
		select { // the reload is a signal: give the postmaster a moment
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		if cur, err = live(); err != nil {
			return err
		}
		if cur == syncNames {
			return nil
		}
		if try == 4 {
			return fmt.Errorf("synchronous_standby_names did not take effect after a reload")
		}
	}
}

// PrimeSync writes names into the sync conf of the standby reached by exec and
// reloads, ahead of its promote: the setting is then in force from the first
// write the new primary accepts (it is ignored while in recovery).
func (f *Flow) PrimeSync(ctx context.Context, it store.Tile, exec func(ctx context.Context, cmd []string) (string, error), names string) error {
	_, i, _, err := f.postgres(ctx, it)
	if err != nil {
		return err
	}
	if _, err := exec(ctx, pgFileWrite(i, "stackr-sync.conf", pgSyncConf(names))); err != nil {
		return err
	}
	_, err = exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", `SELECT pg_reload_conf()`))
	return err
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

// A promoted directory (marker, no standby.signal) is never emptied: it is a
// primary now, and the failover recreates its container with the primary's
// command (PromotedSpec).
//
// standbyPrep runs before the shared start tail in a standby's container.
// The passfile was copied in before the start (never in the env); its owner
// and mode are set here, as root. A data directory without the completion
// marker or without standby.signal is emptied and filled by pg_basebackup
// from the primary as the replication role, over a slot made beforehand;
// never -R, which would write primary_conninfo (and a password) to
// postgresql.auto.conf, which beats stackr-owned.conf. The marker is written
// last, after the cleanup, standby.signal, ownership and a sync. A failed
// copy empties the directory again and stops the container: falling through
// to the image's entrypoint would initdb a new, independent cluster here,
// and a directory without standby.signal would start as a primary. The
// primary's conf files and auto.conf come along in the copy and are
// dropped, for the tail to write this member's.
const standbyPrep = `[ -s ` + ReplPassfile + ` ] && chown postgres:postgres ` + ReplPassfile + ` && chmod 600 ` + ReplPassfile + ` || exit 1
d="$PGDATA"
if [ -s "$d/PG_VERSION" ] && [ -e "$d/` + standbyDone + `" ] && [ ! -e "$d/standby.signal" ]; then
echo "stackr: this data directory was promoted; it does not start as a standby" >&2
exit 1
fi
if [ ! -s "$d/PG_VERSION" ] || [ ! -e "$d/standby.signal" ] || [ ! -e "$d/` + standbyDone + `" ]; then
mkdir -p "$d" && find "$d" -mindepth 1 -delete || exit 1
if ! PGPASSFILE=` + ReplPassfile + ` pg_basebackup -h "$STACKR_PRIMARY" -p "$STACKR_PORT" -U ` + ReplUser +
	` -D "$d" -X stream -S "$STACKR_SLOT" -c fast -w; then
find "$d" -mindepth 1 -delete
exit 1
fi
rm -f "$d/stackr.conf" "$d/stackr-owned.conf" "$d/stackr-sync.conf" "$d/.stackr.conf.host" "$d/postgresql.auto.conf"
touch "$d/standby.signal"
chown -R postgres:postgres "$d"
chmod 700 "$d"
sync
touch "$d/` + standbyDone + `" && chown postgres:postgres "$d/` + standbyDone + `" && sync || exit 1
fi
[ -e "$d/standby.signal" ] && [ -e "$d/` + standbyDone + `" ] || exit 1
`

// standbyCommand is a standby container's command: the prep, the shared
// start tail, then postgres with the same owned flags as the primary.
func standbyCommand(i Instance) []string {
	cmd := []string{"sh", "-c", standbyPrep + pgStartTail, "sh", "postgres"}
	for _, f := range pgFlags(i.Major, 0, 0) {
		cmd = append(cmd, "-c", f)
	}
	return cmd
}

// StandbyPassfile is the content of the standby's passfile: the replication
// role's password for the primary's alias and port. The orchestrator copies
// it into the created container (ReplPassfile) before it starts.
func (f *Flow) StandbyPassfile(ctx context.Context, it store.Tile) ([]byte, error) {
	m, i, _, err := f.postgres(ctx, it)
	if err != nil {
		return nil, err
	}
	if m.ReplPassword == "" {
		return nil, errs.Conflictf("%s has no replication role yet", it.Slug)
	}
	return []byte(fmt.Sprintf("%s:%d:*:%s:%s\n", i.Host, i.Port, ReplUser, m.ReplPassword)), nil
}

// ApplySettingsOn is ApplySettings for a running standby, reached by exec:
// its conf file is made the effective settings and reloaded (or, for a
// change that needs a restart, reloaded too, so pg_settings reports it as
// pending). The caller restarts the standby then.
func (f *Flow) ApplySettingsOn(
	ctx context.Context,
	it store.Tile,
	exec func(ctx context.Context, cmd []string) (string, error),
	overrides map[string]string,
	changed, unset []string,
) (restart bool, err error) {
	_, i, _, err := f.postgres(ctx, it)
	if err != nil {
		return false, err
	}
	i = f.withNodeMem(ctx, it, i)
	i.Settings = overrides
	x := Tools{Exec: exec}
	if restart, err = pgApply(ctx, i, x, changed, unset); err == nil && restart {
		_, err = exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", `SELECT pg_reload_conf()`))
	}
	return restart, err
}

// StandbyPending says whether the standby reached by exec has a setting that
// waits for a restart.
func (f *Flow) StandbyPending(ctx context.Context, it store.Tile, exec func(ctx context.Context, cmd []string) (string, error)) (bool, error) {
	_, i, _, err := f.postgres(ctx, it)
	if err != nil {
		return false, err
	}
	out, err := exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", `SELECT coalesce(bool_or(pending_restart), false)::int FROM pg_settings`))
	return strings.TrimSpace(out) == "1", err
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
	// no secret in the env, which docker inspect shows: not the admin
	// password (the data directory exists, so no first init needs it) and
	// not the replication one (a passfile, StandbyPassfile)
	env := slices.DeleteFunc(def.Config(i), func(e string) bool { return strings.HasPrefix(e, "POSTGRES_PASSWORD=") })
	env = append(env,
		"STACKR_PRIMARY="+i.PrimaryHost,
		"STACKR_PORT="+strconv.Itoa(i.Port),
		"STACKR_SLOT="+i.Slot,
		"STACKR_PG_REWRITE=1", // a standby's conf files are rewritten from the env at every start
	)
	sum := sha256.Sum256([]byte(pgConf(i)))
	return docker.ContainerSpec{
		Name:       mem.Container,
		Image:      img,
		Cmd:        standbyCommand(i),
		Env:        env,
		Labels:     map[string]string{LabelMember: mem.ID, LabelConf: hex.EncodeToString(sum[:8])},
		Volumes:    []string{mem.Volume + ":" + pgMount(i.Major)},
		Networks:   []docker.NetAttach{{Name: operated.Network(m.ID), Aliases: []string{ReadAlias(i.Host)}}},
		ExtraHosts: []string{hostLine},
		MemLimitMB: it.MemLimitMB,
		Restart:    "no",
	}, nil
}
