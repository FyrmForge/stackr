package components

import "testing"

// A type-the-name confirm without "what is kept" is a programmer error.
func TestConfirmNeedsKept(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Confirm with Word and no Kept did not panic")
		}
	}()
	Confirm(ConfirmView{Button: "Delete", Word: "uploads", Action: "/x"})
}
