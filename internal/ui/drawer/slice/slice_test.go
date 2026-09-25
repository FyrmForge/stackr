package slice

import (
	"context"
	"strings"
	"testing"
)

// Bindings show names, never values; detach only while a consumer holds it.
func TestBindingsRenders(t *testing.T) {
	var b strings.Builder
	err := Bindings(View{
		Name:     "shop",
		DB:       "shop",
		OnRemove: "keep",
		Consumer: true,
		Outputs:  []string{"DATABASE_URL"},
		Detach:   "/o/s/e/-/slices/p1/detach",
	}).Render(context.Background(), &b)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"DATABASE_URL", "/-/slices/p1/detach", "<confirm-dialog"} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("lacks %q", w)
		}
	}
}
