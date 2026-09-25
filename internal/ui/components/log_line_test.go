package components

import (
	"context"
	"strings"
	"testing"
)

func TestNewLogLine(t *testing.T) {
	for text, want := range map[string]LogLineView{
		"2026/09/25 ERROR db gone":           {Level: "error", Tok: "ERROR"},
		"level=warning slow query":           {Level: "warn", Tok: "warning"},
		`{"level":"INFO","msg":"up"}`:        {Level: "info"},
		`app: {"severity":"trace","x":1}`:    {Level: "debug"},
		"listening on :80":                   {},
		"errors=0 is not a level word match": {},
	} {
		got := NewLogLine("", text)
		if got.Level != want.Level || got.Tok != want.Tok {
			t.Errorf("%q = %q/%q, want %q/%q", text, got.Level, got.Tok, want.Level, want.Tok)
		}
	}
}

func TestLogLineMarkup(t *testing.T) {
	var b strings.Builder
	_ = LogLine(NewLogLine("12:00:01", "a ERROR b")).Render(context.Background(), &b)
	want := `<div data-level="error"><span data-ts>12:00:01 </span>a <span data-tok>ERROR</span> b</div>`
	if b.String() != want {
		t.Errorf("got  %s\nwant %s", b.String(), want)
	}
}
