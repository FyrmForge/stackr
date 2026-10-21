package components

import (
	"context"
	"strings"
	"testing"
	"time"
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
		got := NewLogLine(time.Time{}, text)
		if got.Level != want.Level || got.Tok != want.Tok {
			t.Errorf("%q = %q/%q, want %q/%q", text, got.Level, got.Tok, want.Level, want.Tok)
		}
	}
}

func TestLogLineMarkup(t *testing.T) {
	var b strings.Builder
	_ = LogLine(NewLogLine(time.Date(2026, 10, 21, 12, 0, 1, 0, time.UTC), "a ERROR b")).Render(context.Background(), &b)
	want := `<div data-level="error"><span data-ts><local-time fmt="15:04:05"><time datetime="2026-10-21T12:00:01Z" title="2026-10-21T12:00:01Z">12:00:01</time></local-time> </span>a <span data-tok>ERROR</span> b</div>`
	if b.String() != want {
		t.Errorf("got  %s\nwant %s", b.String(), want)
	}
}

func TestLogLineANSI(t *testing.T) {
	l := NewLogLine(time.Time{}, "\x1b[2m11:22PM\x1b[0m \x1b[91mERR\x1b[0m \x1b[1mboom\x1b[0m\x1b[K")
	if l.Text != "11:22PM ERR boom" || l.Level != "error" {
		t.Fatalf("text %q level %q", l.Text, l.Level)
	}
	var b strings.Builder
	_ = LogLine(l).Render(context.Background(), &b)
	want := `<div data-level="error"><span data-ts></span><span data-d>11:22PM</span><span> </span><span data-c="red">ERR</span><span> </span><span data-b>boom</span></div>`
	if b.String() != want {
		t.Errorf("got  %s\nwant %s", b.String(), want)
	}
	if NewLogLine(time.Time{}, "plain").Segs != nil {
		t.Error("a plain line got segments")
	}
}
