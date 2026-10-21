// Package logs is the log store: lines in VictoriaLogs, written as JSON lines
// and read back with LogsQL built from a Filter.
package logs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Sources of a line.
const (
	SourceApp    = "app"
	SourceAccess = "access"
	SourceRun    = "run"
	SourceBuild  = "build"
)

// Line is one log line. The ids are what a filter matches on; the names are
// for display. HTTP holds an access line's fields, keyed "http.method" and
// so on.
type Line struct {
	Time      time.Time
	Msg       string
	OrgID     string
	Org       string
	StackID   string
	Stack     string
	EnvID     string
	Env       string
	TileID    string
	Tile      string
	Kind      string // the tile's kind: service | cron | function | ...
	Node      string
	Container string
	Stream    string // stdout | stderr
	Source    string
	RunID     string
	DeployID  string
	HTTP      map[string]string
}

// fields are the Line's fields as VictoriaLogs stores them, empty ones left out.
func (l Line) fields() map[string]string {
	m := map[string]string{
		"org_id": l.OrgID, "org": l.Org, "stack_id": l.StackID, "stack": l.Stack,
		"env_id": l.EnvID, "env": l.Env, "tile_id": l.TileID, "tile": l.Tile,
		"kind": l.Kind, "node": l.Node, "container": l.Container, "stream": l.Stream,
		"source": l.Source, "run_id": l.RunID, "deploy_id": l.DeployID,
	}
	for k, v := range l.HTTP {
		m[k] = v
	}
	for k, v := range m {
		if v == "" {
			delete(m, k)
		}
	}
	return m
}

func lineOf(m map[string]any) Line {
	s := func(k string) string { v, _ := m[k].(string); return v }
	l := Line{
		Msg: s("_msg"), OrgID: s("org_id"), Org: s("org"), StackID: s("stack_id"), Stack: s("stack"),
		EnvID: s("env_id"), Env: s("env"), TileID: s("tile_id"), Tile: s("tile"), Kind: s("kind"),
		Node: s("node"), Container: s("container"), Stream: s("stream"), Source: s("source"),
		RunID: s("run_id"), DeployID: s("deploy_id"),
	}
	l.Time, _ = time.Parse(time.RFC3339Nano, s("_time"))
	for k, v := range m {
		if x, ok := v.(string); ok && strings.HasPrefix(k, "http.") {
			if l.HTTP == nil {
				l.HTTP = map[string]string{}
			}
			l.HTTP[k] = x
		}
	}
	return l
}

// Filter narrows a query. Org is required: the service sets it from the
// caller's scope, never from the request. The rest are optional; Text is a
// phrase searched in the message.
type Filter struct {
	Org, Stack, Env, Tile, Kind, Source, Stream, Run, Text string
}

// ErrNoOrg is a query without an org.
var ErrNoOrg = errors.New("logs: a query needs an org")

// quote is s as a LogsQL quoted string: nothing inside can end it.
func quote(s string) string { return strconv.Quote(s) }

// Query is the LogsQL of f, newest first, at most limit lines.
func (f Filter) Query(limit int) (string, error) {
	if f.Org == "" {
		return "", ErrNoOrg
	}
	var b strings.Builder
	for _, p := range [...]struct{ field, v string }{
		{"org_id", f.Org}, {"stack_id", f.Stack}, {"env_id", f.Env}, {"tile_id", f.Tile},
		{"kind", f.Kind}, {"source", f.Source}, {"stream", f.Stream}, {"run_id", f.Run},
	} {
		if p.v != "" {
			fmt.Fprintf(&b, "%s:=%s ", p.field, quote(p.v))
		}
	}
	if f.Text != "" {
		b.WriteString(quote(f.Text) + " ")
	}
	fmt.Fprintf(&b, "| sort by (_time desc) | limit %d", max(limit, 1))
	return b.String(), nil
}

// Store keeps and reads lines.
type Store interface {
	Write(ctx context.Context, ls []Line) error
	// Query is f's lines in [from, to), newest first.
	Query(ctx context.Context, f Filter, from, to time.Time, limit int) ([]Line, error)
	// Newest is the time of the newest line since from; zero when there is none.
	// Not scoped to an org: the shipper uses it to resume after an outage.
	Newest(ctx context.Context, from time.Time) (time.Time, error)
}

// Victoria is a VictoriaLogs single node.
type Victoria struct {
	URL    string       // base URL, e.g. http://observe
	Client *http.Client // nil = http.DefaultClient
}

var _ Store = (*Victoria)(nil)

func (v *Victoria) do(req *http.Request) ([]byte, error) {
	c := v.Client
	if c == nil {
		c = http.DefaultClient
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("victorialogs %s: %s: %s", req.URL.Path, res.Status, bytes.TrimSpace(b))
	}
	return b, nil
}

func (v *Victoria) Write(ctx context.Context, ls []Line) error {
	if len(ls) == 0 {
		return nil
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, l := range ls {
		m := l.fields()
		m["time"], m["msg"] = l.Time.UTC().Format(time.RFC3339Nano), l.Msg
		if err := enc.Encode(m); err != nil {
			return err
		}
	}
	q := url.Values{
		"_stream_fields": {"org_id,stack_id,env_id,tile_id,kind,source"},
		"_time_field":    {"time"}, "_msg_field": {"msg"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.URL+"/insert/jsonline?"+q.Encode(), &body)
	if err != nil {
		return err
	}
	_, err = v.do(req)
	return err
}

func (v *Victoria) Newest(ctx context.Context, from time.Time) (time.Time, error) {
	form := url.Values{
		"query": {"* | sort by (_time desc) | limit 1"},
		"start": {from.UTC().Format(time.RFC3339Nano)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.URL+"/select/logsql/query", strings.NewReader(form.Encode()))
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	b, err := v.do(req)
	if err != nil {
		return time.Time{}, err
	}
	var m map[string]any
	if len(bytes.TrimSpace(b)) == 0 || json.Unmarshal(bytes.SplitN(b, []byte("\n"), 2)[0], &m) != nil {
		return time.Time{}, nil
	}
	return lineOf(m).Time, nil
}

func (v *Victoria) Query(ctx context.Context, f Filter, from, to time.Time, limit int) ([]Line, error) {
	q, err := f.Query(limit)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"query": {q},
		"start": {from.UTC().Format(time.RFC3339Nano)},
		"end":   {to.UTC().Format(time.RFC3339Nano)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.URL+"/select/logsql/query", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	b, err := v.do(req)
	if err != nil {
		return nil, err
	}
	var out []Line
	for _, raw := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		out = append(out, lineOf(m))
	}
	return out, nil
}
