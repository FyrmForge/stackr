package operated

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestUpgradeRunPreloadAndImage(t *testing.T) {
	run := UpgradeRun{Name: "x", Step: PGStepCheck, OldVol: "old", From: 17, To: 18, Admin: "stackr", Facts: ClusterFacts{Encoding: "UTF8", Collate: "C", Ctype: "C"}}
	plain, err := run.Spec()
	if err != nil || plain.Image != PGUpgradeImage {
		t.Fatalf("default: %v %q", err, plain.Image)
	}
	for _, e := range plain.Env {
		if strings.HasPrefix(e, "PRELOAD=") {
			t.Fatalf("empty preload sets %s", e)
		}
	}
	run.Image, run.Preload = "stackr-helper:abc", []string{"vchord"}
	sp, err := run.Spec()
	if err != nil || sp.Image != "stackr-helper:abc" || !slices.Contains(sp.Env, "PRELOAD=pg_stat_statements,vchord") {
		t.Fatalf("recipe: %v %q %v", err, sp.Image, sp.Env)
	}
	for _, want := range []string{`-o "$SPL" -O "$SPL"`, `-c archive_mode=off $SPL"`, `-c port=5433 $SPL"`, `pg_upgrade_output.d`} {
		if !strings.Contains(pgUpgradeScript, want) {
			t.Errorf("script lacks %s", want)
		}
	}
	for _, bad := range []string{"vchord,evil", "a b", "Vchord", "x;reboot", "$(id)", "-c foo=bar", ""} {
		run.Preload = []string{bad}
		if _, err := run.Spec(); err == nil {
			t.Errorf("preload %q accepted", bad)
		}
	}
	run.Preload = nil
	for _, bad := range []string{"-v /:/x", "a b", "a\nb", "x;y"} {
		run.Image = bad
		if _, err := run.Spec(); err == nil {
			t.Errorf("image %q accepted", bad)
		}
	}
}

// with a preload the script hands the module list to pg_upgrade as -o and -O
func TestUpgradeScriptPreloadShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	line := strings.SplitN(strings.SplitN(pgUpgradeScript, "SPL=; PGU_PRE=()\n", 2)[1], "\n", 2)[0]
	out, err := exec.Command("bash", "-c", "PRELOAD=pg_stat_statements,vchord; SPL=; PGU_PRE=()\n"+line+"\nprintf '%s|' \"${PGU_PRE[@]}\"").CombinedOutput()
	if err != nil || string(out) != "-o|-c shared_preload_libraries=pg_stat_statements,vchord|-O|-c shared_preload_libraries=pg_stat_statements,vchord|" {
		t.Fatalf("%v %q", err, out)
	}
}
