// Command stackr is the CLI, a plain HTTP client of the stackrd API: it
// decides nothing, it asks. The command tree is in cmds.go.
package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/signal"

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
		client:  http.DefaultClient,
	}
	root := a.root()
	root.SetArgs(args)
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(errw)
	if err := a.load(); err != nil {
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
		Use:           "stackr",
		Short:         "Drive a stackr server from the terminal",
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
	return root
}

// group is the RunE of every command that only holds others: bare, it
// prints help (exit 0); with an unknown word, a usage error (exit 2).
func group(c *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usage("unknown command %q for %q", args[0], c.CommandPath())
	}
	return c.Help()
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
