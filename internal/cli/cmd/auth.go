package cmd

import (
	"errors"
	"fmt"
	"os"

	huh "charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/FyrmForge/stackr/internal/cli"
)

func newLoginCmd(rt *Runtime) *cobra.Command {
	var withKey bool
	cmd := &cobra.Command{
		Use:   "login <url>",
		Short: "Authenticate (browser flow, or paste a key)",
		Long: `Authenticate against a stackr server. The default is the browser flow; with
--with-key you paste an API key instead (masked input).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			if withKey {
				key, kerr := rt.PromptSecret("API key: ")
				if kerr != nil {
					return kerr
				}
				err = cli.LoginWithKey(cmd.Context(), args[0], key)
			} else {
				err = cli.LoginBrowser(cmd.Context(), args[0])
			}
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Logged in.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&withKey, "with-key", false, "paste an API key instead of the browser flow")
	return cmd
}

func newLogoutCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the saved credential",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cli.Clear(); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Logged out.")
			return nil
		},
	}
}

// choose resolves a selection: match idFlag if given, auto-pick a lone
// option, else a huh select on a TTY. Non-TTY with several options fails
// naming the flag.
func (rt *Runtime) choose(label, flagName, idFlag string, n int, opt func(i int) (name, id string)) (int, error) {
	if idFlag != "" {
		for i := 0; i < n; i++ {
			if _, id := opt(i); id == idFlag {
				return i, nil
			}
		}
		return 0, fmt.Errorf("no %s with id %q", label, idFlag)
	}
	if n == 1 {
		return 0, nil
	}
	if rt.JSON || !rt.StdinTTY {
		return 0, fmt.Errorf("several %ss; pass %s <id>", label, flagName)
	}
	options := make([]huh.Option[int], n)
	for i := 0; i < n; i++ {
		name, _ := opt(i)
		options[i] = huh.NewOption(name, i)
	}
	var pick int
	form := rt.form(huh.NewSelect[int]().Title("Select a " + label).Options(options...).Value(&pick))
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return 0, ErrCancelled
		}
		return 0, err
	}
	return pick, nil
}

func newLinkCmd(rt *Runtime) *cobra.Command {
	var stackFlag, envFlag, appFlag string
	cmd := &cobra.Command{
		Use:   "link",
		Short: "Bind this directory to a stack + env + tile",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			stacks, err := client.Stacks(cmd.Context())
			if err != nil {
				return err
			}
			if len(stacks) == 0 {
				return fmt.Errorf("no stacks visible to this key")
			}
			i, err := rt.choose("stack", "--stack", stackFlag, len(stacks), func(i int) (string, string) {
				return stacks[i].Name, stacks[i].ID
			})
			if err != nil {
				return err
			}
			stack := stacks[i]

			envs, err := client.Envs(cmd.Context(), stack.ID)
			if err != nil {
				return err
			}
			var env cli.Env
			if len(envs) == 0 && envFlag != "" {
				// don't silently drop an explicit selection
				return fmt.Errorf("no environment with id %q; the stack has none", envFlag)
			}
			if len(envs) > 0 {
				i, err := rt.choose("environment", "--env", envFlag, len(envs), func(i int) (string, string) {
					return envs[i].Name, envs[i].ID
				})
				if err != nil {
					return err
				}
				env = envs[i]
			}

			apps, err := client.Apps(cmd.Context(), stack.ID, env.ID)
			if err != nil {
				return err
			}
			var app cli.App
			if len(apps) == 0 && appFlag != "" {
				return fmt.Errorf("no tile with id %q; the stack/env has none", appFlag)
			}
			if len(apps) > 0 {
				i, err := rt.choose("tile", "--tile", appFlag, len(apps), func(i int) (string, string) {
					return apps[i].Name, apps[i].ID
				})
				if err != nil {
					return err
				}
				app = apps[i]
			}

			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			if err := cli.SaveLink(dir, cli.Link{
				Stack: stack.ID, StackName: stack.Name,
				Env: env.ID, EnvName: env.Name,
				App: app.ID, AppName: app.Name,
			}); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(struct {
					Dir   string `json:"dir"`
					Stack string `json:"stack"`
					Env   string `json:"env,omitempty"`
					App   string `json:"app,omitempty"`
				}{dir, stack.ID, env.ID, app.ID})
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Linked %s\n  stack: %s\n  env:   %s\n  tile:  %s\n",
				dir, stack.Name, orNone(env.Name), orNone(app.Name))
			return nil
		},
	}
	cmd.Flags().StringVar(&stackFlag, "stack", "", "stack id")
	cmd.Flags().StringVar(&envFlag, "env", "", "environment id")
	cmd.Flags().StringVar(&appFlag, "tile", "", "tile id")
	return cmd
}

func newUnlinkCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "unlink",
		Short: "Remove the link for this directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cli.RemoveLink(); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("this directory is not linked")
				}
				return err
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Unlinked.")
			return nil
		},
	}
}

type statusOut struct {
	URL    string `json:"url"`
	Linked bool   `json:"linked"`
	Dir    string `json:"dir,omitempty"`
	Stack  string `json:"stack,omitempty"`
	Env    string `json:"env,omitempty"`
	App    string `json:"app,omitempty"`
}

func newStatusCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show login + what this directory is linked to",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := cli.Load()
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if _, err := client.Stacks(cmd.Context()); err != nil {
				return fmt.Errorf("not authenticated: %w", err)
			}
			link, dir, lerr := rt.Link()
			if lerr != nil && !errors.Is(lerr, os.ErrNotExist) {
				// a corrupt link file is an error, not "unlinked", every
				// other command will fail on it, status must not say all-clear
				return lerr
			}
			out := statusOut{URL: cfg.URL}
			if lerr == nil {
				out = statusOut{cfg.URL, true, dir, link.StackName, link.EnvName, link.AppName}
			}
			if rt.JSON {
				return rt.EmitJSON(out)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Logged in to %s\n", cfg.URL)
			if !out.Linked {
				_, _ = fmt.Fprintln(rt.Stdout, "No stack linked here. Run `stackr link`")
				return nil
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Linked (%s)\n  stack: %s\n  env:   %s\n  tile:  %s\n",
				dir, link.StackName, orNone(link.EnvName), orNone(link.AppName))
			return nil
		},
	}
}
