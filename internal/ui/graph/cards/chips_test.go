package cards

import (
	"strings"
	"testing"
)

// A card's chips wrap inside it instead of running out of the card, and
// the access chip carries its tooltip.
func TestChipsWrapAndExplain(t *testing.T) {
	out := render(t, Top("service", "api", "nginx:1", false, true))
	if !strings.Contains(out, `flex flex-wrap items-center`) {
		t.Errorf("chips do not wrap\n%s", out)
	}
	out = render(t, top("service", "api", "nginx:1", false, false, Access{Chip: "source", Title: "why", Waiting: true}))
	if !strings.Contains(out, `title="why">source</span>`) {
		t.Errorf("no tooltip on the access chip\n%s", out)
	}
}
