package access

import (
	"strings"
	"testing"
)

// The source chip's tooltip says what the grant means; the others keep
// their label.
func TestHint(t *testing.T) {
	if h := Parse("web src:abc").Hint(); !strings.Contains(h, "image and command") {
		t.Errorf("source hint = %q", h)
	}
	if h := Parse("web privileged").Hint(); h != "Privileged" {
		t.Errorf("privileged hint = %q", h)
	}
}
