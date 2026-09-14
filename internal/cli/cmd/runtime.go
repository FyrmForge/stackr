// Package cmd is the cobra command tree for the stackr CLI. It replaces the
// hand-rolled dispatch in internal/cli/run.go (see docs/plans/07-cli-ux-rewrite.md
// and docs/plans/08-cli-ux-matrices.md).
package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	huh "charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/colorprofile"
	"golang.org/x/term"

	"github.com/FyrmForge/stackr/internal/cli"
)

// version is stamped at build time:
//
//	-ldflags "-X github.com/FyrmForge/stackr/internal/cli/cmd.version=..."
var version string

// Runtime carries everything a command touches from its environment, so tests
// can substitute all of it.
type Runtime struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// TTY-ness is checked independently: prompts key off stdin, tables/color
	// off stdout, spinners off stderr.
	StdinTTY, StdoutTTY, StderrTTY bool

	JSON bool // global --json
	Yes  bool // global --yes / -y

	Client func() (*cli.Client, error)
	Link   func() (cli.Link, string, error)
	Getenv func(string) string
}

// NewRuntime builds the real-environment runtime.
func NewRuntime() *Runtime {
	return &Runtime{
		Stdin:     os.Stdin,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		StdinTTY:  term.IsTerminal(int(os.Stdin.Fd())),
		StdoutTTY: term.IsTerminal(int(os.Stdout.Fd())),
		StderrTTY: term.IsTerminal(int(os.Stderr.Fd())),
		Client: func() (*cli.Client, error) {
			cfg, err := cli.Load()
			if err != nil {
				return nil, err
			}
			return cli.NewClient(cfg), nil
		},
		Link:   cli.FindLink,
		Getenv: os.Getenv,
	}
}

// LinkedStack resolves the target stack: --stack flag, else the link.
func (rt *Runtime) LinkedStack(stackFlag string) (string, error) {
	if stackFlag != "" {
		return stackFlag, nil
	}
	if link, _, err := rt.Link(); err == nil && link.Stack != "" {
		return link.Stack, nil
	}
	return "", errors.New("no stack; link this directory or pass --stack <id>")
}

// exitError carries a non-1 exit code up to main. Only main calls os.Exit.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// WithExitCode wraps err so ExitCode reports code.
func WithExitCode(code int, err error) error { return &exitError{code, err} }

func usagef(format string, a ...any) error {
	return &exitError{2, fmt.Errorf(format, a...)}
}

// ExitCode maps the error returned by Execute to a process exit code.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	// cobra has no typed error for these; they are usage errors (exit 2) like
	// flag-parse failures.
	if msg := err.Error(); strings.HasPrefix(msg, "unknown command") ||
		strings.HasPrefix(msg, "unknown flag") || strings.HasPrefix(msg, "accepts ") ||
		strings.HasPrefix(msg, "requires at least") {
		return 2
	}
	return 1
}

// jsonError is the stable machine-readable failure shape emitted to stderr
// under --json: {"error": "...", "status": 502, "code": "..."}.
type jsonError struct {
	Error  string `json:"error"`
	Status int    `json:"status,omitempty"`
	Code   string `json:"code,omitempty"`
}

// EmitError renders err for humans or machines. Used as the fang error
// handler; the styles are ignored under --json.
func (rt *Runtime) EmitError(err error) {
	if err.Error() == "" {
		// quiet exit-code carriers (--detailed-exitcode): the output already
		// said everything, the code is the message
		return
	}
	if !rt.JSON {
		_, _ = fmt.Fprintln(rt.Stderr, "stackr: "+err.Error())
		return
	}
	je := jsonError{Error: err.Error()}
	var se interface{ HTTPStatus() int }
	if errors.As(err, &se) {
		je.Status = se.HTTPStatus()
	}
	var ce interface{ APICode() string }
	if errors.As(err, &ce) {
		je.Code = ce.APICode()
	}
	b, _ := json.Marshal(je)
	_, _ = fmt.Fprintln(rt.Stderr, string(b))
}

// EmitJSON writes v as indented JSON to stdout. nil slices render as [].
func (rt *Runtime) EmitJSON(v any) error {
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice && rv.IsNil() {
		v = []any{}
	}
	enc := json.NewEncoder(rt.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Table renders rows: a headed lipgloss table on a TTY, headerless TSV when
// piped (matching the old CLI's script-friendly shape).
func (rt *Runtime) Table(headers []string, rows [][]string) {
	if !rt.StdoutTTY {
		for _, r := range rows {
			_, _ = fmt.Fprintln(rt.Stdout, strings.Join(r, "\t"))
		}
		return
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(rt.Stderr, "(none)")
		return
	}
	borderSt := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	headerSt := lipgloss.NewStyle().Bold(true).Padding(0, 1)
	cellSt := lipgloss.NewStyle().Padding(0, 1)
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(borderSt).
		StyleFunc(func(row, _ int) lipgloss.Style {
			if row == table.HeaderRow {
				return headerSt
			}
			return cellSt
		}).
		Headers(headers...).
		Rows(rows...)
	// colorprofile downgrades per NO_COLOR/TERM, same as fang's own output.
	_, _ = fmt.Fprintln(colorprofile.NewWriter(rt.Stdout, os.Environ()), t.Render())
}

// ErrCancelled is returned when the user declines a confirm.
var ErrCancelled = errors.New("cancelled")

// Confirm gates a destructive action: --yes skips it, a TTY prompts, and a
// non-TTY stdin refuses rather than proceeding implicitly.
func (rt *Runtime) Confirm(title string) error {
	if rt.Yes {
		return nil
	}
	if rt.JSON || !rt.StdinTTY {
		return fmt.Errorf("refusing without --yes (non-interactive): %s", title)
	}
	var ok bool
	form := rt.form(huh.NewConfirm().Title(title).Value(&ok))
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return ErrCancelled
		}
		return err
	}
	if !ok {
		return ErrCancelled
	}
	return nil
}

// form builds a one-group huh form over the runtime's IO. STACKR_ACCESSIBLE
// forces huh's screen-reader-friendly mode (TERM=dumb also triggers it).
func (rt *Runtime) form(fields ...huh.Field) *huh.Form {
	f := huh.NewForm(huh.NewGroup(fields...)).WithInput(rt.Stdin).WithOutput(rt.Stderr)
	if rt.Getenv != nil && rt.Getenv("STACKR_ACCESSIBLE") != "" {
		f = f.WithAccessible(true)
	}
	return f
}

// PromptSecret reads a secret: masked huh input on a TTY, one plain line from
// stdin otherwise (so scripts can pipe it in). Accessible mode also reads a
// plain line, huh's masked accessible input demands a real terminal fd.
func (rt *Runtime) PromptSecret(title string) (string, error) {
	if !rt.StdinTTY || (rt.Getenv != nil && rt.Getenv("STACKR_ACCESSIBLE") != "") {
		// Whole line, not Fscanln: a secret may contain spaces. EOF without a
		// trailing newline still counts as input.
		s, err := bufio.NewReader(rt.Stdin).ReadString('\n')
		s = strings.TrimRight(s, "\r\n")
		if err != nil && s == "" {
			return "", fmt.Errorf("%s: no input (stdin closed); pipe the value or run interactively", strings.TrimSuffix(title, ": "))
		}
		return s, nil
	}
	var s string
	form := rt.form(huh.NewInput().Title(title).EchoMode(huh.EchoModePassword).Value(&s))
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return "", ErrCancelled
		}
		return "", err
	}
	return s, nil
}
