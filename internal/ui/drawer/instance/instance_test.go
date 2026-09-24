package instance

import (
	"context"
	"strings"
	"testing"
)

func TestSlicesRenders(t *testing.T) {
	var b strings.Builder
	err := Slices(View{
		Name:   "pg",
		Engine: "postgres",
		Scope:  "stack",
		Slices: []SliceRow{
			{
				ID:     "p1",
				Name:   "shop",
				DB:     "shop",
				Drawer: "/o/s/e/-/slices/p1?tab=bindings",
				Public: true,
			},
			{
				ID:       "p2",
				Name:     "old",
				OnRemove: "drop",
				Orphan:   true,
			},
		},
	}).Render(context.Background(), &b)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"-/slices/p1", "public", "consumer gone, drop pending", "stack"} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("lacks %q", w)
		}
	}
}
