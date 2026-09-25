package components

import (
	"encoding/json"
	"regexp"
	"strings"
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
	"info": "info",
	"debug": "debug", "trace": "debug",
}

// NewLogLine reads a line's level: a JSON object's level, lvl or severity
// key, else the first level word in the text, which is also drawn bold.
// ponytail: JSON lines keep their raw text; v0's key=value expand is not
// ported.
func NewLogLine(time, text string) LogLineView {
	l := LogLineView{Time: time, Text: text}
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
