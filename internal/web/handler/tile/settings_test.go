package tile

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// A form drawn from a row and posted back unchanged writes the same row,
// for every kind, and a key the kind does not carry is never drawn.
func TestSettingsRoundTrip(t *testing.T) {
	for kind, keys := range carries {
		want := service.Tile{Kind: kind}
		for _, k := range keys {
			switch p := field(&want, k).(type) {
			case *string:
				*p = "x-" + k
			case *int:
				*p = 7
			case *float64:
				*p = 0.5
			case *bool:
				*p = true
			}
		}
		want.BuildArgs = ""
		if strings.Contains(strings.Join(keys, " "), "build_args") {
			want.BuildArgs = `{"A":"1","B":"x=y"}`
		}

		s := settingsView(want)
		form := url.Values{"fields": {s.Keys()}}
		for k, v := range s.Vals {
			if v != "" {
				form.Set(k, v)
			}
		}

		got := service.Tile{Kind: kind}
		if err := apply(&got, posted(form), form); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s round trip:\n got %+v\nwant %+v", kind, got, want)
		}
	}
}

// A drawn field left empty clears its column, even when nothing posts.
func TestSettingsClear(t *testing.T) {
	row := service.Tile{Kind: "service", Privileged: true, CPULimit: 2}
	form := url.Values{"fields": {settingsView(row).Keys()}}
	if err := apply(&row, posted(form), form); err != nil {
		t.Fatal(err)
	}
	if row.Privileged || row.CPULimit != 0 {
		t.Errorf("cleared row = %+v", row)
	}
}
