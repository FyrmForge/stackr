package operated

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A postgres instance's settings: stackr keeps the user's overrides in its
// own database and writes the effective set, computed defaults under the
// overrides, into stackr.conf in the data directory (pgStartScript includes
// it). The few settings stackr owns are -c start flags (pgFlags), which beat
// every file; they are refused here.

// PGMain and PGAdvanced are the settings the panel offers by name; any other
// name the instance knows is an "other" setting.
var (
	PGMain = []string{
		"max_connections", "work_mem", "statement_timeout", "idle_in_transaction_session_timeout",
		"log_min_duration_statement", "timezone",
	}
	PGAdvanced = []string{
		"shared_buffers", "effective_cache_size", "autovacuum_vacuum_scale_factor", "autovacuum_max_workers",
		"autovacuum_vacuum_cost_limit", "random_page_cost", "effective_io_concurrency", "max_wal_size",
		"checkpoint_timeout", "max_parallel_workers_per_gather", "lock_timeout", "log_lock_waits",
		"default_statistics_target",
	}
)

// pgOwned are the names stackr owns or reserves; the prefixes follow.
var pgOwned = []string{
	"wal_level", "restore_command", "listen_addresses", "port", "hba_file", "ident_file",
	"shared_preload_libraries", "allow_alter_system", "wal_log_hints", "primary_conninfo",
	"synchronous_standby_names", "primary_slot_name", "max_wal_senders", "max_replication_slots", "hot_standby",
	"max_slot_wal_keep_size", "data_directory", "config_file", "include", "include_dir",
	"include_if_exists", "reserved_connections", "superuser_reserved_connections",
}

var pgOwnedPrefix = []string{"archive_", "ssl", "recovery_", "unix_socket_"}

// PGOwned says whether stackr owns name: it is refused as a setting.
func PGOwned(name string) bool {
	return slices.Contains(pgOwned, name) ||
		slices.ContainsFunc(pgOwnedPrefix, func(p string) bool { return strings.HasPrefix(name, p) })
}

var pgName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)?$`)

// CheckSettings refuses what no instance could take: a name that is not a
// setting's shape, an owned one, a value that is empty or cannot sit in the
// file. Whether postgres knows the name is asked of the running instance.
func CheckSettings(s map[string]string) error {
	for _, k := range slices.Sorted(maps.Keys(s)) {
		if err := CheckSettingName(k); err != nil {
			return err
		}
		v := s[k]
		if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\n\r\x00") {
			return fmt.Errorf("%s: the value is empty or has a line break; unset it instead", k)
		}
	}
	return nil
}

// CheckSettingName is CheckSettings' check of one name, for an unset.
func CheckSettingName(k string) error {
	switch {
	case !pgName.MatchString(k) || len(k) > 63:
		return fmt.Errorf("%q is not a postgres setting name", k)
	case PGOwned(k):
		return fmt.Errorf("%s is managed by stackr and cannot be set", k)
	}
	return nil
}

// pgComputed are the defaults stackr sets before the user's overrides. With
// no memory limit on the tile postgres shares the node: a fixed shared_buffers
// and half the node's memory as the cache estimate. With one, they follow
// the limit. memMB is the tile's limit (0 = none), nodeMB the node's memory
// (0 = unknown).
// ponytail: an unknown node size assumes 2 GB; the node's own report is
// the source when it has one.
func pgComputed(memMB, nodeMB int) map[string]string {
	mb := func(n int) string { return strconv.Itoa(max(n, 1)) + "MB" }
	c := map[string]string{"random_page_cost": "1.1"}
	if memMB > 0 {
		c["shared_buffers"] = mb(memMB / 4)
		c["effective_cache_size"] = mb(memMB * 3 / 4)
		c["maintenance_work_mem"] = mb(memMB / 20)
		return c
	}
	if nodeMB <= 0 {
		nodeMB = 2048
	}
	c["shared_buffers"] = "256MB"
	c["effective_cache_size"] = mb(nodeMB / 2)
	c["maintenance_work_mem"] = "64MB"
	return c
}

// slotKeepStepMB is the floor of max_slot_wal_keep_size (and the cap when
// the node's disk is unknown); the cap moves in whole GB.
const slotKeepStepMB = 10 << 10

// slotKeepMB is max_slot_wal_keep_size: the larger of 10 GB and 10% of the
// data node's free disk, but never more than the free disk less a headroom
// (walBudgetMB), in whole GB and at least 1 GB. freeMB 0 = unknown, 10 GB.
// It is in stackr-owned.conf, a reload: the watch tick moves it as the free
// disk does.
func slotKeepMB(freeMB, totalMB int) int {
	if freeMB <= 0 {
		return slotKeepStepMB
	}
	want := min(max(slotKeepStepMB, freeMB/10), walBudgetMB(freeMB, totalMB))
	return max(want/1024*1024, minWALBudgetMB)
}

// pgOwnedConf is stackr-owned.conf, included after stackr.conf: the owned
// settings that are only a reload (nothing here is a postmaster setting). On
// a standby, primary_conninfo (its secret is in a passfile, not here) and its
// slot; every member gets the slot cap, so a promoted one has it too.
// On a primary, synchronous_standby_names is there when it waits for a standby
// (SyncStandbyNames); the watch tick keeps it.
func pgOwnedConf(i Instance) string {
	var b strings.Builder
	b.WriteString("# written by stackr; edits here are overwritten\n")
	kv := func(k, v string) { b.WriteString(k + " = '" + strings.ReplaceAll(v, `'`, `''`) + "'\n") }
	if i.SlotKeepMB > 0 {
		kv("max_slot_wal_keep_size", strconv.Itoa(i.SlotKeepMB)+"MB")
	}
	if i.Slot == "" && i.SyncNames != "" {
		kv("synchronous_standby_names", i.SyncNames)
	}
	if i.Slot != "" {
		kv("primary_conninfo", fmt.Sprintf("host=%s port=%d user=%s passfile=%s application_name=%s connect_timeout=5",
			i.PrimaryHost, i.Port, ReplUser, ReplPassfile, i.Slot))
		kv("primary_slot_name", i.Slot)
	}
	return b.String()
}

// PGEffective is the computed defaults with the user's overrides over them.
func PGEffective(i Instance) map[string]string {
	e := pgComputed(i.MemMB, i.NodeMemMB)
	maps.Copy(e, i.Settings)
	return e
}

// pgConf is stackr.conf: the effective settings, sorted, quoted.
func pgConf(i Instance) string {
	e := PGEffective(i)
	var b strings.Builder
	b.WriteString("# written by stackr from its own database; edits here are overwritten\n")
	for _, k := range slices.Sorted(maps.Keys(e)) {
		v := strings.ReplaceAll(strings.ReplaceAll(e[k], `\`, `\\`), `'`, `''`)
		b.WriteString(k + " = '" + v + "'\n")
	}
	return b.String()
}

// pgReloadContext are the pg_settings contexts a reload applies; every
// other one (postmaster, internal) waits for a restart.
var pgReloadContext = []string{"sighup", "user", "superuser", "superuser-backend", "backend"}

// pgJudge decides how changed settings are applied, from the contexts the
// running instance reports (pg_settings.context by name): any that only
// takes effect at start means a restart; one the instance does not report,
// or cannot change (internal), is refused. A name only unset may be unknown.
func pgJudge(contexts map[string]string, changed, unset []string) (restart bool, err error) {
	for _, n := range changed {
		c, ok := contexts[n]
		switch {
		case !ok:
			if slices.Contains(unset, n) {
				continue
			}
			return false, fmt.Errorf("%s is not a setting this postgres knows", n)
		case c == "internal":
			return false, fmt.Errorf("%s is fixed and cannot be changed", n)
		case !slices.Contains(pgReloadContext, c):
			restart = true
		}
	}
	return restart, nil
}

// pgInList is names as a SQL IN list, for the pg_settings query. Names are
// checked (CheckSettingName), so they are safe in SQL.
func pgInList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = "'" + n + "'"
	}
	return strings.Join(q, ",")
}

// PGWorkMemRisk says whether every connection using its work_mem would
// outgrow the node's memory. Values read as postgres does: a bare number is
// the setting's own unit (work_mem kB).
func PGWorkMemRisk(workMem, maxConn string, nodeMB int) bool {
	w, ok := pgKB(workMem, 1)
	c, err := strconv.Atoi(strings.TrimSpace(maxConn))
	if !ok || err != nil || nodeMB <= 0 {
		return false
	}
	return w*c/1024 > nodeMB
}

// pgKB reads a size setting in kB; bare is the multiplier of a bare number.
func pgKB(v string, bare int) (int, bool) {
	v = strings.TrimSpace(v)
	i := strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' })
	if i < 0 {
		i = len(v)
	}
	n, err := strconv.Atoi(v[:i])
	if err != nil {
		return 0, false
	}
	switch strings.TrimSpace(v[i:]) {
	case "":
		return n * bare, true
	case "kB":
		return n, true
	case "MB":
		return n * 1024, true
	case "GB":
		return n * 1024 * 1024, true
	}
	return 0, false
}
