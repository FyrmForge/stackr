package operated

import (
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

func TestPGChangeCheck(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		from, to int
		want     string // "" = allowed
	}{
		{17, 18, ""}, {18, 16, ""}, {14, 18, ""},
		{17, 17, "already runs"}, {17, 99, "not one stackr runs"}, {17, 13, "not one stackr runs"}, {0, 18, "no recorded major"},
	} {
		err := PGChangeCheck(c.from, c.to, now)
		if (err == nil) != (c.want == "") || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%d -> %d: %v, want %q", c.from, c.to, err, c.want)
		}
	}
	// an end-of-life target is refused, either way
	late := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	if _, ok := errs.IsInvalid(PGChangeCheck(18, 14, late)); !ok {
		t.Error("a downgrade to an EOL major was taken")
	}
}

func TestParseFacts(t *testing.T) {
	t.Parallel()
	out := "noise\nF\tenc\tUTF8\nF\tcollate\ten_US.utf8\nF\tctype\ten_US.utf8\nF\tprov\tc\nF\tsums\ton\nF\tboot\tstackr\nC\tpostgres\tpublic.t\t5\n"
	f, err := ParseFacts(out)
	if err != nil || f != (ClusterFacts{Boot: "stackr", Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8", Checksums: true}) {
		t.Errorf("facts = %+v, %v", f, err)
	}
	for name, bad := range map[string]string{
		"empty":    "",
		"shell":    strings.Replace(out, "UTF8", "UTF8; rm -rf /", 1),
		"icu":      strings.Replace(out, "prov\tc", "prov\ti", 1),
		"no boot":  strings.Replace(out, "F\tboot\tstackr\n", "", 1),
		"no ctype": strings.Replace(out, "F\tctype\ten_US.utf8\n", "", 1),
	} {
		if _, err := ParseFacts(bad); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

func TestCompareCounts(t *testing.T) {
	t.Parallel()
	before := "C\tdb\tpublic.a\t5\nC\tdb\tpublic.b\t7\nE\tdb\tpublic.big\t100\n"
	if d, big := CompareCounts(before, before, true); len(d) != 0 || big != 1 {
		t.Errorf("same: %v %d", d, big)
	}
	after := "C\tdb\tpublic.a\t6\nE\tdb\tpublic.big\t99\nC\tdb\tpublic.new\t1\n"
	d, _ := CompareCounts(before, after, true)
	want := []string{"db public.a has 6 rows, had 5", "db public.b is missing", "db public.big is 99 bytes, was 100", "db public.new is new"}
	if strings.Join(d, "|") != strings.Join(want, "|") {
		t.Errorf("diff = %q", d)
	}
	// a load does not keep the size of a big table: only its presence counts
	if d, _ := CompareCounts("E\tdb\tt\t100\n", "E\tdb\tt\t5\n", false); len(d) != 0 {
		t.Errorf("presence only: %v", d)
	}
	if d, _ := CompareCounts("E\tdb\tt\t100\n", "", false); len(d) != 1 {
		t.Errorf("a missing big table: %v", d)
	}
}

// The helper runs pg_upgrade in copy mode (the old volume is never linked
// into the new one), the dry run changes nothing, and the cluster's own
// settings come from the old one rather than the image's defaults.
func TestUpgradeScriptModes(t *testing.T) {
	t.Parallel()
	if strings.Contains(pgUpgradeScript, "--link") || !strings.Contains(pgUpgradeScript, "--copy") {
		t.Error("pg_upgrade is not run in copy mode")
	}
	for _, want := range []string{`-D "$NEWDIR"`, `initdb`, `pg_hba.conf`, `listen_addresses = '*'`, "pg_dumpall"} {
		if !strings.Contains(pgUpgradeScript, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	spec, err := (UpgradeRun{Name: "x", Step: PGStepCheck, OldVol: "old", From: 17, To: 18, Admin: "stackr", Facts: ClusterFacts{Encoding: "UTF8", Collate: "C", Ctype: "C"}}).Spec()
	if err != nil || len(spec.Volumes) != 1 || spec.Volumes[0] != "old:/old" {
		t.Errorf("check spec: %+v %v", spec.Volumes, err)
	}
	if _, err := (UpgradeRun{Admin: "x; reboot"}).Spec(); err == nil {
		t.Error("an admin name that is not a word was taken")
	}
	for _, env := range spec.Env {
		if strings.Contains(strings.ToLower(env), "password") {
			t.Errorf("a secret rides the env: %s", env)
		}
	}
}
