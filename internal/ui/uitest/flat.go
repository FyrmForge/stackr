// Package uitest holds checks shared by the UI package tests.
package uitest

import (
	"regexp"
	"strings"
	"testing"
)

var panelClass = regexp.MustCompile(`class="(?:[^"]* )?panel[ "]`)

// NoPanelInCard fails when rendered markup has an element with class "panel"
// inside a card: a section, a details.panel accordion row or the setup
// shell. Rows sit flat in a card, never in a box of their own.
func NoPanelInCard(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "<section") && !strings.Contains(out, `class="panel group"`) && !strings.Contains(out, "panel p-8 shadow-rw") {
		t.Fatal("no card in the output, the check proves nothing")
	}
	for _, card := range strings.Split(out, "<section")[1:] {
		_, card, _ = strings.Cut(card, ">") // past the card's own tag
		card, _, _ = strings.Cut(card, "</section>")
		if panelClass.MatchString(card) {
			t.Errorf("a panel box inside a section card\n%s", card)
		}
	}
	for _, card := range strings.Split(out, "<details")[1:] {
		head, body, _ := strings.Cut(card, ">")
		if !strings.Contains(head, `class="panel group"`) {
			continue
		}
		body, _, _ = strings.Cut(body, "</details>")
		if panelClass.MatchString(body) {
			t.Errorf("a panel box inside an accordion row\n%s", body)
		}
	}
	for _, shell := range strings.Split(out, `class="panel p-8 shadow-rw"`)[1:] {
		if panelClass.MatchString(shell) {
			t.Errorf("a panel box inside the setup shell\n%s", shell)
		}
	}
}
