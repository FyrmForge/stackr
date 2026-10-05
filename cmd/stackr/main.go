// Command stackr is the CLI, a plain HTTP client of the stackrd API: it
// decides nothing, it asks. The command tree is in cmds.go.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// version is set at build time via ldflags.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	tty := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, tty)
	stop()
	os.Exit(code)
}

// run is the whole CLI against given streams; tests call it directly.
func run(ctx context.Context, args []string, in io.Reader, out, errw io.Writer, tty bool) int {
	a := &app{
		ctx:     ctx,
		in:      bufio.NewReader(in),
		out:     out,
		errw:    errw,
		tty:     tty,
		cfgPath: configPath(),
		client:  newClient(),
	}
	root := a.root()
	root.SetArgs(args)
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(errw)
	if err := a.load(); err != nil {
		return a.fail(err)
	}
	if err := helpOnUnknown(root, args); err != nil {
		return a.fail(err)
	}
	if err := root.ExecuteContext(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return 1 // follow already said what goes on
		}
		return a.fail(err)
	}
	return 0
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "stackr",
		Short: "Drive a stackr server from the terminal",
		Long: `Drive a stackr server from the terminal.

Environment (each beats the config file; nothing here is written to disk):
  STACKR_KEY     API key, so CI needs no login
  STACKR_SERVER  server URL
  STACKR_ORG     org slug
  STACKR_CONFIG  config file path`,
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE:          group,
	}
	root.PersistentFlags().BoolVar(&a.json, "json", false, "print JSON; errors go to stderr as {error, status, code}")
	root.PersistentFlags().BoolVarP(&a.yes, "yes", "y", false, "skip confirmation prompts (never overrides a server refusal)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageErr{err.Error()} })
	root.AddCommand(a.commands()...)
	root.SetHelpCommand(&cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		RunE: func(c *cobra.Command, args []string) error {
			t, rest, err := root.Find(args)
			if err != nil || (t == root && len(args) > 0) || len(rest) > 0 {
				return group(root, args)
			}
			return t.Help()
		},
	})
	return root
}

// group is the RunE of every command that only holds others: bare, it
// prints help (exit 0); with an unknown word, a usage error (exit 2).
func group(c *cobra.Command, args []string) error {
	if len(args) > 0 {
		msg := fmt.Sprintf("unknown command %q for %q", args[0], c.CommandPath())
		c.SuggestionsMinimumDistance = 2
		if s := c.SuggestionsFor(args[0]); len(s) > 0 {
			msg += "; did you mean " + strings.Join(s, ", ") + "?"
		}
		return usageErr{msg}
	}
	return c.Help()
}

// helpOnUnknown: cobra answers "stackr <unknown> --help" with the parent's
// help and exit 0; it gets group's usage error instead.
func helpOnUnknown(root *cobra.Command, args []string) error {
	if !slices.Contains(args, "--help") && !slices.Contains(args, "-h") {
		return nil
	}
	t, rest, err := root.Find(args)
	if err != nil || !t.HasSubCommands() {
		return nil
	}
	t.InitDefaultHelpFlag()
	if t.ParseFlags(rest) != nil || t.Flags().NArg() == 0 {
		return nil
	}
	return group(t, t.Flags().Args())
}

func noun(use, short string, subs ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ArbitraryArgs,
		RunE:  group,
	}
	c.AddCommand(subs...)
	return c
}

// leaf is one command. op names the API operations it calls (comma
// separated); the coverage test holds every route to one leaf.
func leaf(
	use, op, short string,
	nargs cobra.PositionalArgs,
	run func(c *cobra.Command, args []string) error,
) *cobra.Command {
	return &cobra.Command{
		Use:         use,
		Short:       short,
		Args:        nargs,
		RunE:        run,
		Annotations: map[string]string{"op": op},
	}
}

// exact and upTo are cobra's arg checks with exit code 2.
func exact(n int) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) != n {
			return usage("%s takes %d argument(s), got %d; see --help", c.CommandPath(), n, len(args))
		}
		return nil
	}
}

func upTo(n int) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) > n {
			return usage("%s takes at most %d argument(s), got %d; see --help", c.CommandPath(), n, len(args))
		}
		return nil
	}
}

func atLeast(n int) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) < n {
			return usage("%s takes at least %d argument(s); see --help", c.CommandPath(), n)
		}
		return nil
	}
}
