package service

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/orgplan"
)

func TestConfigErrLineIsTheFirstLineOnce(t *testing.T) {
	err := "yaml: yaml: line 27: field bogus_key not found\n  26 | options:\n> 27 | bogus_key: [\n  28 |"
	if got, want := configErrLine(err), "line 27: field bogus_key not found"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	rows, _, _ := windowPlanRows([]planFacts{{ID: "p", Status: orgplan.Error, Err: err}})
	if len(rows) != 1 || rows[0].Err != "line 27: field bogus_key not found" {
		t.Fatalf("window row err: %+v", rows)
	}
}
