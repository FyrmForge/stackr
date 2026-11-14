package env

import (
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

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
