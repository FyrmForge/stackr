package env

import (
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// The Extensions row lists the daily check's findings one line each.
func TestExtUpdateLines(t *testing.T) {
	if got := extUpdateLines(nil); got != nil {
		t.Errorf("nil = %v", got)
	}
	u := &service.PGExtUpdates{Changes: []service.PGExtChange{{Package: "pgvector", From: "0.8.6-1", To: "0.8.7-1"}}}
	if got := extUpdateLines(u); len(got) != 1 || got[0] != "pgvector 0.8.6-1 to 0.8.7-1" {
		t.Errorf("lines = %v", got)
	}
}

// A refusal that names the image, preload or extensions field is drawn
// under it, the service's image_ref under the image; anything else stays a
// banner.
func TestExtFieldErrors(t *testing.T) {
	by, rest := extFieldErrors(errors.Join(
		errs.Invalidf("image_ref", "no such image"),
		errs.Invalidf("extensions", "bad name"),
	))
	if rest != nil || by["image"] != "no such image" || by["extensions"] != "bad name" {
		t.Errorf("by = %v, rest = %v", by, rest)
	}
	other := errs.Conflictf("%s has a standby.", "pg")
	if by, rest := extFieldErrors(other); len(by) != 0 || rest == nil {
		t.Errorf("conflict: by = %v, rest = %v", by, rest)
	}
}
