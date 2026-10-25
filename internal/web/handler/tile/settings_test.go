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
		form := url.Values{"fields": {s.Keys(keys...)}}
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
	form := url.Values{"fields": {settingsView(row).Keys(carries[row.Kind]...)}}
	if err := apply(&row, posted(form), form); err != nil {
		t.Fatal(err)
	}
	if row.Privileged || row.CPULimit != 0 {
		t.Errorf("cleared row = %+v", row)
	}
}

// A row's save writes only that row's fields: the Network row leaves the
// Host access checkboxes and the limits as stored, though they post
// nothing.
func TestSettingsRowSave(t *testing.T) {
	row := service.Tile{Kind: "service", Privileged: true, HostNetwork: true, CPULimit: 2, Command: "run", ContainerPort: 80}
	form := url.Values{"fields": {"endpoint_protocol port"}, "port": {"8080"}, "endpoint_protocol": {"https"}, "open": {"network"}}
	if err := apply(&row, posted(form), form); err != nil {
		t.Fatal(err)
	}
	want := service.Tile{Kind: "service", Privileged: true, HostNetwork: true, CPULimit: 2, Command: "run", ContainerPort: 8080, EndpointProtocol: "https"}
	if !reflect.DeepEqual(row, want) {
		t.Errorf("row after a Network save:\n got %+v\nwant %+v", row, want)
	}
}

// An empty limit inherits from the nearest rung that sets it; a set one
// inherits nothing.
func TestInherit(t *testing.T) {
	sv := settingsView(service.Tile{Kind: "service", MemLimitMB: 256})
	err := inherit(&sv, []rung{{"env", ""}, {"stack", `{"mem_limit_mb":512}`}, {"org", `{"cpu_limit":1.5}`}, {"server", `{"cpu_limit":4}`}})
	if err != nil {
		t.Fatal(err)
	}
	if sv.From["cpu_limit"] != "org" || sv.Inherit["cpu_limit"] != "1.5" || sv.From["mem_limit_mb"] != "" {
		t.Errorf("from %v inherit %v", sv.From, sv.Inherit)
	}
}

func TestIngressWord(t *testing.T) {
	for _, tc := range []struct{ name, own, from, want string }{
		{"h4", "h4", "", "h4"},
		{"h4", "", "From env: h4.", "h4 (from env)"},
		{"cluster default", "", "From default.", "cluster default"},
		{"eu", "", "From the placement.", "eu (from the placement)"},
	} {
		if got := ingressWord(tc.name, tc.own, tc.from); got != tc.want {
			t.Errorf("ingressWord(%q, %q, %q) = %q, want %q", tc.name, tc.own, tc.from, got, tc.want)
		}
	}
}
