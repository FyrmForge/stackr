package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

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
	if a.cfg.Server == "" || (a.cfg.Key == "" && path != "/auth/exchange") {
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
	req, err := http.NewRequestWithContext(a.ctx, method, strings.TrimRight(a.cfg.Server, "/")+"/api/v1"+path, r)
	if err != nil {
		return nil, err
	}
	if a.cfg.Key != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.Key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		defer func() { _ = res.Body.Close() }()
		e := &apiErr{Status: res.StatusCode}
		if b, _ := io.ReadAll(res.Body); json.Unmarshal(b, e) != nil || e.Msg == "" {
			e.Msg = http.StatusText(res.StatusCode)
		}
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
				row[i] = cell(m[c])
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
