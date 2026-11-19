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
	out = render(t, top("service", "api", "nginx:1", false, false, Access{Chip: "source", Title: "why", Waiting: true}, false))
	if !strings.Contains(out, `title="why">source</span>`) {
		t.Errorf("no tooltip on the access chip\n%s", out)
	}
}

// A card with a line over its footer drops the kind chip, which that line
// would land on in the fixed card height.
func TestNotedCardDropsKindChip(t *testing.T) {
	if out := render(t, top("service", "api", "nginx:1", false, false, Access{}, true)); strings.Contains(out, ">service<") {
		t.Errorf("kind chip under the note\n%s", out)
	}
	card := render(t, Card(CardView{ID: "n", Kind: "service", Name: "api", Footer: FooterView{Kind: "service", StoppedAt: "img"}}))
	if strings.Contains(card, ">service<") || !strings.Contains(card, "Stopped at img.") {
		t.Errorf("card with a stop note:\n%s", card)
	}
}
