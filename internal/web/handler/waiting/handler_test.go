package waiting

import (
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

func TestSummarize(t *testing.T) {
	outs := []service.PromoteOutcome{
		{Pair: service.PromotePair{ReleaseID: "a", EnvID: "e"}},
		{Pair: service.PromotePair{ReleaseID: "b", EnvID: "e"}, Skip: service.SkipReview},
		{Pair: service.PromotePair{ReleaseID: "c", EnvID: "e"}},
	}
	n, st := summarize(outs)
	if n != "1 of 3 skipped: needs full review" || st["b:e"] != "Skipped: needs full review" || len(st) != 1 {
		t.Errorf("notice %q, status %v", n, st)
	}
	n, st = summarize([]service.PromoteOutcome{{Pair: service.PromotePair{ReleaseID: "a", EnvID: "e"}, Err: errors.New("boom")}})
	if n != "1 failed" || st["a:e"] != "Not promoted: could not queue the promote" {
		t.Errorf("notice %q, status %v", n, st)
	}
	if n, _ := summarize(outs[:1]); n != "Promoting 1 release." {
		t.Errorf("notice %q", n)
	}
}
