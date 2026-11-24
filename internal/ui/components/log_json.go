package components

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// LogField is one key of a JSON log line, nested keys dotted ("req.method").
type LogField struct{ Key, Val string }

const (
	maxLogFields   = 100
	maxLogFieldLen = 2000
)

// logMsgKeys are the keys a JSON line's message is read from, first present
// non-empty string wins.
var logMsgKeys = []string{"msg", "message", "@m", "@mt", "log", "text", "error"}

// logRedundant are the keys the row already shows: the level by its colour,
// the time as its own time cell.
var logRedundant = map[string]bool{
	"level": true, "lvl": true, "severity": true, "@l": true,
	"time": true, "ts": true, "timestamp": true, "@t": true,
}

// maxLogSummary is the longest key=value summary of a line with no message.
const maxLogSummary = 120

// ParseJSONLog reads a line that is a JSON object (after its stamp and
// terminal escapes). msg is the row text: the first message key's value, else
// the first fields as "key=value ..." (the raw line if none are left). fields
// is every other key flattened and sorted, minus the level and time keys the
// row already shows, at most 100, values cut at 2000 characters. ok is false
// for anything else.
func ParseJSONLog(line string) (msg string, fields []LogField, ok bool) {
	t := strings.TrimSpace(StripStamp(StripANSI(line)))
	if !strings.HasPrefix(t, "{") {
		return "", nil, false
	}
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	var obj map[string]any
	if dec.Decode(&obj) != nil || dec.More() {
		return "", nil, false
	}
	msg, used := t, ""
	for _, k := range logMsgKeys {
		if s, isStr := obj[k].(string); isStr && s != "" {
			msg, used = s, k
			break
		}
	}
	flat := map[string]string{}
	flatten("", obj, flat)
	delete(flat, used)
	for k := range flat {
		if logRedundant[strings.ToLower(k)] {
			delete(flat, k)
		}
	}
	keys := make([]string, 0, len(flat))
	for k := range flat {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxLogFields {
		keys = keys[:maxLogFields]
	}
	for _, k := range keys {
		v := flat[k]
		if r := []rune(v); len(r) > maxLogFieldLen {
			v = string(r[:maxLogFieldLen]) + "…"
		}
		fields = append(fields, LogField{k, v})
	}
	if used == "" && len(fields) > 0 {
		msg = fieldsSummary(fields)
	}
	return msg, fields, true
}

// fieldsSummary is "k=v k=v ..." over the leading fields, cut at 120 runes.
func fieldsSummary(fs []LogField) string {
	var b strings.Builder
	for _, f := range fs {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(f.Key + "=" + strings.Join(strings.Fields(f.Val), " "))
		if r := []rune(b.String()); len(r) > maxLogSummary {
			return string(r[:maxLogSummary]) + "…"
		}
	}
	return b.String()
}

// flatten puts v's leaves in out under dotted keys; arrays and empty objects
// are leaves shown as compact JSON, strings as they are.
func flatten(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		if len(x) > 0 || prefix == "" {
			for k, c := range x {
				if prefix != "" {
					k = prefix + "." + k
				}
				flatten(k, c, out)
			}
			return
		}
	case string:
		out[prefix] = x
		return
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	out[prefix] = strings.TrimSpace(b.String())
}

// fieldsLabel is the marker text: "+1 field", "+5 fields".
func fieldsLabel(n int) string {
	if n == 1 {
		return "+1 field"
	}
	return fmt.Sprintf("+%d fields", n)
}
