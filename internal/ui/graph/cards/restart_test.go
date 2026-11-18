package cards

import (
	"strings"
	"testing"
)

// A saved change that waits for a restart marks the card; a card with none
// carries no mark.
func TestCardRestartPending(t *testing.T) {
	with := render(t, Card(CardView{ID: "t1", Kind: "service", Name: "sonarr", RestartPending: true}))
	if !strings.Contains(with, "Restart pending") {
		t.Errorf("no mark:\n%s", with)
	}
	if without := render(t, Card(CardView{ID: "t1", Kind: "service", Name: "sonarr"})); strings.Contains(without, "Restart pending") {
		t.Error("an unmarked card says it waits")
	}
}
