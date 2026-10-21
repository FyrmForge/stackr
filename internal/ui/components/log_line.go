package components

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// logLevels is v0 logs.js's plain-text detection, first match wins.
var logLevels = []struct {
	re    *regexp.Regexp
	level string
}{
	{regexp.MustCompile(`(?i)\b(FATAL|PANIC|EXCEPTION|ERROR|ERR)\b`), "error"},
	{regexp.MustCompile(`(?i)\b(WARN|WARNING)\b`), "warn"},
	{regexp.MustCompile(`(?i)\bINFO\b`), "info"},
	{regexp.MustCompile(`(?i)\b(DEBUG|TRACE)\b`), "debug"},
}

// jsonLevels folds a structured line's level word onto the pane's four.
var jsonLevels = map[string]string{
	"fatal": "error", "panic": "error", "error": "error", "err": "error",
	"warn": "warn", "warning": "warn",
	"info":  "info",
	"debug": "debug", "trace": "debug",
}

// NewLogLine reads a line's level: a JSON object's level, lvl or severity
// key, else the first level word in the text, which is also drawn bold.
// ponytail: JSON lines keep their raw text; v0's key=value expand is not
// ported.
func NewLogLine(at time.Time, text string) LogLineView {
	l := LogLineView{Time: at}
	l.Text, l.Segs = ansiSegs(text)
	text = l.Text
	if i := strings.IndexByte(text, '{'); i >= 0 && strings.HasSuffix(text, "}") {
		var obj map[string]any
		if json.Unmarshal([]byte(text[i:]), &obj) == nil {
			for _, k := range []string{"level", "lvl", "severity"} {
				if s, ok := obj[k].(string); ok {
					l.Level = jsonLevels[strings.ToLower(s)]
					return l
				}
			}
			return l
		}
	}
	for _, x := range logLevels {
		if m := x.re.FindString(text); m != "" {
			l.Level, l.Tok = x.level, m
			return l
		}
	}
	return l
}

// ansiSeq is a terminal escape (CSI): SGR ("m") sets the look, the rest
// (cursor moves, clears) means nothing in a log and goes.
var ansiSeq = regexp.MustCompile("\x1b\\[([0-9;?]*)([A-Za-z])")

// ansiColors names the 8 SGR foregrounds; the CSS maps names to the palette.
var ansiColors = [8]string{"black", "red", "green", "yellow", "blue", "magenta", "cyan", "white"}

// LogSeg is a run of a coloured line with one look.
type LogSeg struct {
	Text      string
	Bold, Dim bool
	Color     string // an ansiColors name, "" = the line's own
}

// ansiSegs strips a line's escapes: the plain text, for level detection and
// search, and the runs to draw (nil when the line had no SGR).
func ansiSegs(s string) (string, []LogSeg) {
	if !strings.Contains(s, "\x1b[") {
		return s, nil
	}
	var plain strings.Builder
	var segs []LogSeg
	cur := LogSeg{}
	emit := func(t string) {
		if t == "" {
			return
		}
		plain.WriteString(t)
		seg := cur
		seg.Text = t
		segs = append(segs, seg)
	}
	at := 0
	for _, m := range ansiSeq.FindAllStringSubmatchIndex(s, -1) {
		emit(s[at:m[0]])
		at = m[1]
		if s[m[4]:m[5]] != "m" {
			continue
		}
		for _, p := range strings.Split(s[m[2]:m[3]], ";") {
			switch n, _ := strconv.Atoi(p); {
			case n == 0:
				cur = LogSeg{}
			case n == 1:
				cur.Bold = true
			case n == 2:
				cur.Dim = true
			case n == 22:
				cur.Bold, cur.Dim = false, false
			case n == 39:
				cur.Color = ""
			case n >= 30 && n <= 37:
				cur.Color = ansiColors[n-30]
			case n >= 90 && n <= 97:
				cur.Color = ansiColors[n-90]
			}
		}
	}
	emit(s[at:])
	return plain.String(), segs
}
