package service

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

func TestSpoolCapsTheUpload(t *testing.T) {
	w := newWorld(t)
	if _, err := w.orch.spool(strings.NewReader("12345"), 5); err != nil {
		t.Fatalf("a dump at the limit = %v, want it taken", err)
	}
	_, err := w.orch.spool(strings.NewReader("123456"), 5)
	if _, ok := errs.IsTooLarge(err); !ok {
		t.Fatalf("a dump past the limit = %v, want TooLarge", err)
	}
	if lim, err := w.orch.SeedLimit(t.Context()); err != nil || lim <= 0 || lim > 10<<30 {
		t.Errorf("SeedLimit = %d, %v", lim, err)
	}
}
