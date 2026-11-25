package components

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseJSONLog(t *testing.T) {
	big := `{"msg":"x","a":"` + strings.Repeat("é", 2500) + `"}`
	for _, c := range []struct {
		name, in, msg string
		fields        []LogField
		ok            bool
	}{
		{"msg", `{"msg":"hi","b":2,"a":"x"}`, "hi", []LogField{{"a", "x"}, {"b", "2"}}, true},
		{"message", `{"message":"hi","level":"info"}`, "hi", nil, true},
		{"@m", `{"@m":"hi","@l":"Warning"}`, "hi", nil, true},
		{"nested", `{"msg":"r","req":{"method":"GET","h":{"ua":"x"}}}`, "r", []LogField{{"req.h.ua", "x"}, {"req.method", "GET"}}, true},
		{"array", `{"msg":"r","t":["a",1],"e":{},"n":null}`, "r", []LogField{{"e", "{}"}, {"n", "null"}, {"t", `["a",1]`}}, true},
		{"no message key", `{"b":"x  y","a":1}`, "a=1 b=x y", []LogField{{"a", "1"}, {"b", "x  y"}}, true},
		{"redundant dropped", `{"msg":"m","level":"info","time":"t","ts":1,"@t":2,"req.level":3,"a":1}`, "m", []LogField{{"a", "1"}, {"req.level", "3"}}, true},
		{"nothing left is raw", `{"level":"info","ts":1}`, `{"level":"info","ts":1}`, nil, true},
		{"non-string msg skipped", `{"msg":5,"message":"m"}`, "m", []LogField{{"msg", "5"}}, true},
		{"plain", "hello world", "", nil, false},
		{"invalid", `{"msg":`, "", nil, false},
		{"trailing junk", `{"a":1} x`, "", nil, false},
		{"array line", `[1,2]`, "", nil, false},
		{"ansi and stamp", "2026-10-21T12:00:01Z \x1b[2m{\"msg\":\"hi\",\"a\":1}\x1b[0m", "hi", []LogField{{"a", "1"}}, true},
	} {
		msg, fs, ok := ParseJSONLog(c.in)
		if ok != c.ok || msg != c.msg || !reflect.DeepEqual(fs, c.fields) {
			t.Errorf("%s: got %q %v %v, want %q %v %v", c.name, msg, fs, ok, c.msg, c.fields, c.ok)
		}
	}
	// no message: summary cut at 120 runes
	if m, _, _ := ParseJSONLog(`{"k":"` + strings.Repeat("v", 300) + `"}`); len([]rune(m)) != 121 || !strings.HasSuffix(m, "…") {
		t.Errorf("summary = %d runes: %q", len([]rune(m)), m)
	}
	// cap: 120 keys -> 100 fields
	var b strings.Builder
	b.WriteString(`{"msg":"x"`)
	for i := 0; i < 120; i++ {
		b.WriteString(`,"k` + string(rune('a'+i/26)) + string(rune('a'+i%26)) + `":1`)
	}
	b.WriteString("}")
	if _, fs, _ := ParseJSONLog(b.String()); len(fs) != 100 {
		t.Errorf("fields = %d, want 100", len(fs))
	}
	_, fs, _ := ParseJSONLog(big)
	if n := len([]rune(fs[0].Val)); n != 2001 {
		t.Errorf("value runes = %d, want 2001", n)
	}
}

func TestLogRowsJSON(t *testing.T) {
	render := func(l LogLineView) string {
		var b strings.Builder
		_ = LogLine(l).Render(context.Background(), &b)
		return b.String()
	}
	j := render(NewLogLine(time.Unix(1e9, 0), `{"level":"warn","msg":"slow","ms":9}`))
	for _, w := range []string{"<details ", "<summary", ">slow ", "+1 field<svg", "<dt", "ms", "grid-cols-", "pl-[9ch]"} {
		if !strings.Contains(j, w) {
			t.Errorf("live json row missing %q: %s", w, j)
		}
	}
	if !strings.Contains(j, `data-level="warn"`) {
		t.Errorf("level lost: %s", j)
	}
	if p := render(NewLogLine(time.Time{}, `{"level":"warn","msg":"only"}`)); strings.Contains(p, "<details") || !strings.Contains(p, "only") {
		t.Errorf("message-only json row not plain: %s", p)
	}
	if p := render(NewLogLine(time.Time{}, "plain line")); strings.Contains(p, "<details") {
		t.Errorf("plain row has details: %s", p)
	}
	hist := func(l LogHistoryLine) string {
		var b strings.Builder
		_ = LogHistoryMore(LogHistoryView{Lines: []LogHistoryLine{l}}).Render(context.Background(), &b)
		return b.String()
	}
	h := hist(LogHistoryLine{Text: "slow", Fields: []LogField{{"a", "1"}}})
	if !strings.Contains(h, "<details") || !strings.Contains(h, "+1 field<") || !strings.Contains(h, "<dt") || !strings.Contains(h, "<svg") || !strings.Contains(h, "sm:pl-[calc(16ch+0.5rem)]") {
		t.Errorf("history json row: %s", h)
	}
	if p := hist(LogHistoryLine{Text: "plain"}); strings.Contains(p, "<details") {
		t.Errorf("plain history row has details: %s", p)
	}
}
