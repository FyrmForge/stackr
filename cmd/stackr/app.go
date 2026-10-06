package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"
)

// app is the runtime every command reads: where to talk, how to print,
// whether it may ask.
type app struct {
	json, yes bool
	tty       bool // stdin and stdout are a terminal: prompts and borders
	in        *bufio.Reader
	out, errw io.Writer
	cfgPath   string
	cfg       config
	client    *http.Client
	ctx       context.Context
	noEnv     bool // login: its own arguments, not STACKR_*, say where and as whom
	planShown bool // a plan was printed as a table: a job's own "plan:" log lines repeat it
}

// config is the one file the CLI keeps, 0600.
type config struct {
	Server string          `json:"server"`
	Key    string          `json:"key"`
	Org    string          `json:"org"`
	Links  map[string]link `json:"links,omitempty"` // directory -> link
}

type link struct {
	Stack string `json:"stack,omitempty"`
	Env   string `json:"env,omitempty"`
	Tile  string `json:"tile,omitempty"`
}

func configPath() string {
	if p := os.Getenv("STACKR_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "stackr", "config.json")
}

// env is STACKR_<name> when set (and not logging in): CI authenticates with
// it and nothing is written to disk.
func (a *app) env(name, cfg string) string {
	if v := os.Getenv("STACKR_" + name); v != "" && !a.noEnv {
		return v
	}
	return cfg
}

func (a *app) server() string { return a.env("SERVER", a.cfg.Server) }

func (a *app) load() error {
	b, err := os.ReadFile(a.cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &a.cfg); err != nil {
		return fmt.Errorf("%s is corrupt: %w", a.cfgPath, err)
	}
	return nil
}

func (a *app) save() error {
	b, err := json.MarshalIndent(a.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.cfgPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(a.cfgPath, b, 0o600)
}

// here is the link of this directory or the nearest parent that has one.
func (a *app) here() (string, link) {
	dir, err := os.Getwd()
	if err != nil {
		return "", link{}
	}
	for d := dir; ; d = filepath.Dir(d) {
		if l, ok := a.cfg.Links[d]; ok {
			return d, l
		}
		if filepath.Dir(d) == d {
			return dir, link{}
		}
	}
}

// ---- errors and exit codes ----

// usageErr exits 2: the command line was wrong, not the request.
type usageErr struct{ msg string }

func (e usageErr) Error() string {
	return e.msg
}

func usage(format string, a ...any) error {
	return usageErr{fmt.Sprintf(format, a...)}
}

// apiErr is the API's typed refusal, printed as the server wrote it.
type apiErr struct {
	Msg    string `json:"error"`
	Status int    `json:"status"`
}

func (e *apiErr) Error() string { return e.Msg }

// newClient dials in 10s and waits 30s for headers; no overall timeout, so
// log and job streams run as long as the server keeps them open.
func newClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: t}
}

// flagNames turns API field names in a server message into the flags that
// set them: tile fields by the flag map in stack.go, limits.* by hand. It
// also drops the "name: " prefix of a positional argument's field error.
var flagNames = sync.OnceValue(func() *strings.Replacer {
	pairs := []string{"limits.memory_mb", "--memory", "limits.cpu", "--cpus", "name: ", "", "slug: ", ""}
	for flag, field := range tileFlags(&cobra.Command{}) {
		if strings.Contains(field, "_") { // plain words (user, image) would mangle prose
			pairs = append(pairs, field, "--"+flag)
		}
	}
	return strings.NewReplacer(pairs...)
})

// notFound words a 404 by what was asked for: the last name/kind pair of
// the request path, inside the names above it ("no tile "x" in shop/dev").
func notFound(path string) string {
	segs := strings.Split(strings.Trim(strings.SplitN(path, "?", 2)[0], "/"), "/")
	if len(segs) >= 2 && segs[0] == "orgs" { // the org is the caller's own
		segs = segs[2:]
	}
	if len(segs)%2 == 1 {
		segs = segs[:len(segs)-1]
	}
	if len(segs) < 2 {
		return "not found: " + path
	}
	un := func(s string) string {
		v, err := url.PathUnescape(s)
		if err != nil {
			return s
		}
		return v
	}
	var in []string
	for i := 1; i < len(segs)-2; i += 2 {
		in = append(in, un(segs[i]))
	}
	kind := strings.TrimSuffix(segs[len(segs)-2], "s")
	if kind == "sync-plan" { // the name is the env synced from, inside the stack
		kind, in = "env", in[:len(in)-1]
	}
	msg := fmt.Sprintf("no %s %q", kind, un(segs[len(segs)-1]))
	if len(in) > 0 {
		msg += " in " + strings.Join(in, "/")
	}
	return msg
}

// exitErr is an exit code with nothing to print: the output already said
// it (a plan's --detailed-exitcode).
type exitErr int

func (e exitErr) Error() string {
	return fmt.Sprintf("exit %d", int(e))
}

// exitCode: 0 fine, 2 usage, 1 everything else.
func exitCode(err error) int {
	var u usageErr
	var x exitErr
	switch {
	case err == nil:
		return 0
	case errors.As(err, &x):
		return int(x)
	case errors.As(err, &u), strings.HasPrefix(err.Error(), "unknown command"),
		strings.HasPrefix(err.Error(), "unknown flag"), strings.HasPrefix(err.Error(), "unknown shorthand"):
		return 2
	}
	return 1
}

func (a *app) fail(err error) int {
	code := exitCode(err)
	if _, quiet := err.(exitErr); quiet {
		return code
	}
	if a.json {
		body := map[string]any{"error": err.Error(), "code": code, "status": 0}
		var e *apiErr
		if errors.As(err, &e) {
			body["status"] = e.Status
		}
		b, _ := json.Marshal(body)
		_, _ = fmt.Fprintln(a.errw, string(b))
	} else {
		_, _ = fmt.Fprintln(a.errw, "error:", err)
	}
	return code
}

// ---- HTTP ----

// request sends body as JSON (nil = none) and returns the raw response;
// a non-2xx is an apiErr.
func (a *app) request(method, path string, body any) (*http.Response, error) {
	key := a.env("KEY", a.cfg.Key)
	if a.server() == "" || (key == "" && path != "/auth/exchange") {
		return nil, errors.New("not logged in; run stackr login <url>")
	}
	var r io.Reader
	if body != nil {
		if rd, ok := body.(io.Reader); ok {
			r = rd
		} else {
			b, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			r = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequestWithContext(a.ctx, method, strings.TrimRight(a.server(), "/")+"/api/v1"+path, r)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := a.client.Do(req)
	if err != nil {
		if a.ctx.Err() != nil {
			return nil, err
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if os.IsTimeout(err) {
			err = errors.New("timed out")
		}
		return nil, fmt.Errorf("can't reach %s: %w", a.server(), err)
	}
	if res.StatusCode >= 300 {
		defer func() { _ = res.Body.Close() }()
		e := &apiErr{Status: res.StatusCode}
		if b, _ := io.ReadAll(res.Body); json.Unmarshal(b, e) != nil || e.Msg == "" {
			e.Msg = http.StatusText(res.StatusCode)
		}
		if res.StatusCode == http.StatusNotFound && strings.EqualFold(e.Msg, http.StatusText(404)) {
			e.Msg = notFound(path)
		}
		e.Msg = flagNames().Replace(e.Msg)
		return nil, e
	}
	return res, nil
}

// call is request plus a decoded JSON answer (nil on 204).
func (a *app) call(method, path string, body any) (any, error) {
	res, err := a.request(method, path, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	d := json.NewDecoder(res.Body)
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return v, nil
}

// ---- output ----

// show prints v: JSON under --json, else a table of cols for a list, or
// the cols of one object as KEY VALUE rows (every scalar when none named).
func (a *app) show(v any, cols ...string) error {
	if a.json {
		if v == nil {
			return nil
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.out, string(b))
		return err
	}
	switch v := v.(type) {
	case []any:
		rows := make([][]string, 0, len(v))
		for _, it := range v {
			m, _ := it.(map[string]any)
			row := make([]string, len(cols))
			for i, c := range cols {
				if x, ok := m[c]; ok {
					row[i] = cell(x)
				} else {
					row[i] = "n/a" // this kind has no such field
				}
			}
			rows = append(rows, row)
		}
		a.table(cols, rows)
	case map[string]any:
		if len(cols) == 0 {
			for k, x := range v {
				switch x.(type) {
				case map[string]any, []any:
				default:
					cols = append(cols, k)
				}
			}
			sort.Strings(cols)
		}
		rows := make([][]string, 0, len(cols))
		for _, c := range cols {
			rows = append(rows, []string{c, cell(v[c])})
		}
		a.table(nil, rows)
	case nil:
	default:
		_, _ = fmt.Fprintln(a.out, cell(v))
	}
	return nil
}

func cell(v any) string {
	switch v := v.(type) {
	case nil:
		return "(none)"
	case string:
		if v == "" {
			return "(none)"
		}
		return v
	case map[string]any, []any:
		b, _ := json.Marshal(v)
		return string(b)
	}
	return fmt.Sprint(v)
}

// fit cuts each cell to its share of the terminal width, with an ellipsis.
// A column keeps at least 8 columns; piped output is never cut.
func (a *app) fit(header []string, rows [][]string) {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 || len(rows) == 0 {
		return
	}
	n := len(rows[0])
	widths := make([]int, n)
	for _, r := range append([][]string{header}, rows...) {
		for i, c := range r {
			if l := utf8.RuneCountInString(c); i < n && l > widths[i] {
				widths[i] = l
			}
		}
	}
	for { // shave the widest column until the row fits (2 spaces between)
		sum, widest := 2*(n-1), 0
		for i, l := range widths {
			sum += l
			if l > widths[widest] {
				widest = i
			}
		}
		if sum <= w || widths[widest] <= 8 {
			break
		}
		widths[widest]--
	}
	for _, r := range rows {
		for i, c := range r {
			if i < n && utf8.RuneCountInString(c) > widths[i] {
				r[i] = string([]rune(c)[:widths[i]-3]) + "..."
			}
		}
	}
}

// table: a header on a terminal, bare tab-separated rows when piped (so
// cut -f2 works), "(none)" on stderr when empty so stdout stays empty.
// ponytail: tabwriter columns, no borders; lipgloss if anyone misses them.
func (a *app) table(header []string, rows [][]string) {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(a.errw, "(none)")
		return
	}
	if !a.tty {
		for _, r := range rows {
			_, _ = fmt.Fprintln(a.out, strings.Join(r, "\t"))
		}
		return
	}
	a.fit(header, rows)
	w := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
	if header != nil {
		h := make([]string, len(header))
		for i, c := range header {
			h[i] = strings.ToUpper(c)
		}
		_, _ = fmt.Fprintln(w, strings.Join(h, "\t"))
	}
	for _, r := range rows {
		_, _ = fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	_ = w.Flush()
}

// say is a human line: stderr under --json, so stdout stays the document.
func (a *app) say(format string, args ...any) {
	w := a.out
	if a.json {
		w = a.errw
	}
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// ---- prompts ----

// confirm asks q. -y skips it; with --json or no terminal and no -y it
// refuses, naming the question. -y never means force.
func (a *app) confirm(q string) error {
	if a.yes {
		return nil
	}
	if a.json || !a.tty {
		return fmt.Errorf("refusing without --yes (non-interactive): %s", q)
	}
	_, _ = fmt.Fprintf(a.errw, "%s [y/N] ", q)
	line, _ := a.in.ReadString('\n')
	if s := strings.ToLower(strings.TrimSpace(line)); s != "y" && s != "yes" {
		return errors.New("stopped")
	}
	return nil
}

// secret: the flag (documented as "prefer the prompt"), then env, then a
// masked prompt; piped input gives one whole line.
func (a *app) secret(flagVal, env, prompt string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if v := os.Getenv(env); v != "" {
		return v, nil
	}
	if a.tty {
		_, _ = fmt.Fprint(a.errw, prompt+": ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		_, _ = fmt.Fprintln(a.errw)
		return string(b), err
	}
	if a.json {
		return "", fmt.Errorf("%s: pass it in %s", prompt, env)
	}
	line, err := a.in.ReadString('\n')
	if line = strings.TrimRight(line, "\r\n"); line == "" {
		return "", fmt.Errorf("%s: none given (flag, %s or stdin)", prompt, env)
	}
	return line, err
}

// changed builds a body from the flags the user actually gave: key per
// flag, typed by the flag. Nothing given is nil (B1, B21, B22).
func changed(c *cobra.Command, keys map[string]string) (map[string]any, error) {
	body := map[string]any{}
	var err error
	c.Flags().Visit(func(f *pflag.Flag) {
		key, ok := keys[f.Name]
		if !ok || err != nil {
			return
		}
		s := f.Value.String()
		switch f.Value.Type() {
		case "int":
			body[key], err = strconv.Atoi(s)
		case "float64":
			body[key], err = strconv.ParseFloat(s, 64)
		case "bool":
			body[key], err = strconv.ParseBool(s)
		default:
			body[key] = s
		}
	})
	if len(body) == 0 {
		return nil, err
	}
	return body, err
}

// closest is the entry of have within two edits of want, or "".
func closest(want string, have []string) string {
	best, bd := "", 3
	for _, h := range have {
		if d := editDistance(want, h); d < bd {
			best, bd = h, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}
