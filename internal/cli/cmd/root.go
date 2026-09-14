package cmd

import (
	"context"
	"io"
	"os"
	"syscall"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// NewRoot builds the command tree over rt. IO goes through rt so tests can
// capture it.
func NewRoot(rt *Runtime) *cobra.Command {
	root := &cobra.Command{
		Use:   "stackr",
		Short: "the stackr command-line client",
		// Root with no args: help, exit 0 (the old CLI exited 2).
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
		// Rendering errors is the fang handler's job (or the test's).
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetIn(readerOrStdin(rt.Stdin))
	root.SetOut(rt.Stdout)
	root.SetErr(rt.Stderr)

	// "tile" is the user-facing noun; --app stays a silent alias for old
	// scripts and muscle memory (the API still says app on the wire).
	root.SetGlobalNormalizationFunc(func(f *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == "app" {
			name = "tile"
		}
		return pflag.NormalizedName(name)
	})

	pf := root.PersistentFlags()
	pf.BoolVar(&rt.JSON, "json", false, "machine-readable output: data as JSON on stdout, errors as JSON on stderr")
	pf.BoolVarP(&rt.Yes, "yes", "y", false, "skip confirmation prompts")

	// Flag-parse and arg errors are usage errors: exit 2.
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return WithExitCode(2, err)
	})

	root.AddCommand(
		newLoginCmd(rt), newLogoutCmd(rt), newLinkCmd(rt), newUnlinkCmd(rt), newStatusCmd(rt),
		newStackCmd(rt), newEnvCmd(rt), newTileCmd(rt),
		newLogsCmd(rt), newDeployCmd(rt), newForwardCmd(rt),
		newReleasesCmd(rt), newPromoteCmd(rt),
		newVarsCmd(rt), newInfraCmd(rt), newDomainCmd(rt),
		newProxyCmd(rt), newStorageCmd(rt),
		newBackupCmd(rt), newOrgCmd(rt), newPlanCmd(rt), deploymentCmd(rt), newImageCmd(rt), newRegistryCmd(rt), newDefaultsCmd(rt),
		legacyStacksCmd(rt), legacyEnvsCmd(rt), legacyDomainsCmd(rt),
	)
	return root
}

func readerOrStdin(r io.Reader) io.Reader {
	if r == nil {
		return os.Stdin
	}
	return r
}

// Execute runs the CLI and returns the process exit code. Fang owns the
// signal context, --version, completions, and error rendering.
func Execute() int {
	rt := NewRuntime()
	root := NewRoot(rt)
	root.SetArgs(RewriteArgs(os.Args[1:]))
	err := fang.Execute(context.Background(), root,
		fang.WithVersion(version),
		fang.WithNotifySignal(os.Interrupt, syscall.SIGTERM),
		fang.WithErrorHandler(func(w io.Writer, styles fang.Styles, err error) {
			rt.EmitError(err)
		}),
	)
	return ExitCode(err)
}
