package tile

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
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
				if k == "health_http_status" {
					*p = 200
				}
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

// Message 27 names image_ref; it lands under the form's image field.
func TestImageRefRefusalLandsUnderImage(t *testing.T) {
	err := rowRefusal(errs.Invalidf("image_ref", "Change the image on its own."), "image update_policy")
	bad, ok := errs.IsInvalid(err)
	if !ok || bad.Field != "image" || bad.Msg != "Change the image on its own." {
		t.Fatalf("refusal = %#v", err)
	}
}

func TestHealthStatusText(t *testing.T) {
	for in, want := range map[string]int{"": 0, "200": 200, "599": 599, "2xx": 2, "5XX": 5, "1xx": 1} {
		if got, err := parseStatus(in); in != "" && (err != nil || got != want) {
			t.Errorf("parseStatus(%q) = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"6xx", "0xx", "99", "600", "abc", "2x"} {
		if _, err := parseStatus(bad); err == nil {
			t.Errorf("parseStatus(%q) accepted", bad)
		}
	}
	tl := service.Tile{HealthHTTPStatus: 2}
	if shown("health_http_status", &tl.HealthHTTPStatus) != "2xx" {
		t.Error("class 2 not shown as 2xx")
	}
	tl.HealthHTTPStatus = 404
	if shown("health_http_status", &tl.HealthHTTPStatus) != "404" {
		t.Error("code not shown as is")
	}
	if err := write("health_http_status", &tl.HealthHTTPStatus, "oops"); err == nil {
		t.Error("bad status accepted")
	}
}

// An empty path or command turns its check off and clears the rest of it.
func TestHealthClearOnEmpty(t *testing.T) {
	row := service.Tile{Kind: "service", HealthHTTPStatus: 2, HealthHTTPIntervalS: 5, HealthHTTPTimeoutS: 3, HealthHTTPRetries: 2,
		HealthExecIntervalS: 5, HealthExecTimeoutS: 3, HealthExecRetries: 2, HealthExecStartPeriodS: 4}
	form := url.Values{"fields": {strings.Join(healthKeysForTest, " ")}, "health_http_interval_s": {"9"}, "health_exec_interval_s": {"9"}}
	if err := apply(&row, posted(form), form); err != nil {
		t.Fatal(err)
	}
	if row.HealthHTTPStatus != 0 || row.HealthHTTPIntervalS != 0 || row.HealthHTTPTimeoutS != 0 || row.HealthHTTPRetries != 0 ||
		row.HealthExecIntervalS != 0 || row.HealthExecTimeoutS != 0 || row.HealthExecRetries != 0 || row.HealthExecStartPeriodS != 0 {
		t.Errorf("not cleared: %+v", row)
	}
}

// With the path empty the check is going away: a bad status text must not
// refuse the Save. With a path it still does.
func TestHealthBadStatusWithEmptyPath(t *testing.T) {
	row := service.Tile{Kind: "service", HealthHTTPStatus: 200}
	form := url.Values{"fields": {strings.Join(healthKeysForTest, " ")}, "health_http_status": {"oops"}}
	if err := apply(&row, posted(form), form); err != nil || row.HealthHTTPStatus != 0 {
		t.Errorf("empty path: err = %v, status = %d", err, row.HealthHTTPStatus)
	}
	form["health_http_path"] = []string{"/up"}
	if err := apply(&service.Tile{Kind: "service"}, posted(form), form); err == nil {
		t.Error("bad status with a path accepted")
	}
}

var healthKeysForTest = []string{"health_http_path", "health_http_status", "health_http_interval_s", "health_http_timeout_s", "health_http_retries",
	"health_exec_cmd", "health_exec_interval_s", "health_exec_timeout_s", "health_exec_retries", "health_exec_start_period_s"}

func TestHealthFormKey(t *testing.T) {
	for in, want := range map[string]string{
		"health.http": "health_http_path", "health.http.path": "health_http_path", "health.http.status": "health_http_status",
		"health.http.interval": "health_http_interval_s", "health.http.timeout": "health_http_timeout_s", "health.http.retries": "health_http_retries",
		"health.exec": "health_exec_cmd", "health.exec.cmd": "health_exec_cmd", "health.exec.interval": "health_exec_interval_s",
		"health.exec.timeout": "health_exec_timeout_s", "health.exec.retries": "health_exec_retries", "health.exec.start_period": "health_exec_start_period_s",
	} {
		if got := formKey(in); got != want {
			t.Errorf("formKey(%q) = %q, want %q", in, got, want)
		}
	}
}
