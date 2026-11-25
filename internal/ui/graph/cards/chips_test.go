package cards

import "testing"

func TestChipsWaitingStaysAlone(t *testing.T) {
	shown, more := Chips("service", true, Access{Waiting: true}, false)
	if len(shown) != 1 || shown[0].Text != "Waiting for an admin" || more != 2 {
		t.Fatalf("got %v +%d", shown, more)
	}
	if shown, more = Chips("service", true, Access{}, false); len(shown) != 2 || more != 0 {
		t.Fatalf("got %v +%d", shown, more)
	}
}
