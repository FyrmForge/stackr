package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/FyrmForge/stackr/internal/cli"
)

// varsScopeFlags are shared by set/get: which variable set to act on.
type varsScopeFlags struct {
	scope, org, env, stack, app string
}

func (v *varsScopeFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&v.scope, "scope", "", "level: org, stack, env or tile")
	f.StringVar(&v.org, "org", "", "org id (implies org scope)")
	f.StringVar(&v.env, "env", "", "env slug or id (implies env scope)")
	f.StringVar(&v.stack, "stack", "", "stack id (default: the linked stack)")
	f.StringVar(&v.app, "tile", "", "tile id (default: the linked tile)")
}

// resolve maps the selectors to a VarScope. --org/--env imply their level;
// --scope names one explicitly, and when it does, the matching selector must
// be present (a level asked for by name must never silently become another).
func (v *varsScopeFlags) resolve(cmd *cobra.Command, rt *Runtime, client *cli.Client) (cli.VarScope, error) {
	envScope := func() (cli.VarScope, error) {
		stack, err := rt.LinkedStack(v.stack)
		if err != nil {
			return cli.VarScope{}, err
		}
		envs, err := client.Envs(cmd.Context(), stack)
		if err != nil {
			return cli.VarScope{}, err
		}
		for _, e := range envs {
			if e.Slug == v.env || e.ID == v.env {
				return cli.VarScope{Env: e.ID}, nil
			}
		}
		return cli.VarScope{}, fmt.Errorf("no environment %s in this stack", v.env)
	}
	appScope := func() (cli.VarScope, error) {
		app, err := rt.resolveAppID(v.app)
		if err != nil {
			return cli.VarScope{}, err
		}
		return cli.VarScope{App: app}, nil
	}
	switch v.scope {
	case "":
	case "stack":
		stack, err := rt.LinkedStack(v.stack)
		if err != nil {
			return cli.VarScope{}, err
		}
		return cli.VarScope{Stack: stack}, nil
	case "org":
		if v.org == "" {
			return cli.VarScope{}, usagef("--scope org needs --org <id>")
		}
		return cli.VarScope{Org: v.org}, nil
	case "env":
		if v.env == "" {
			return cli.VarScope{}, usagef("--scope env needs --env <slug>")
		}
		return envScope()
	case "app", "tile": // tile is the user-facing noun; app is the wire name
		return appScope()
	default:
		return cli.VarScope{}, usagef("--scope must be org, stack, env or tile (got %s)", v.scope)
	}
	if v.org != "" {
		return cli.VarScope{Org: v.org}, nil
	}
	if v.env != "" {
		return envScope()
	}
	return appScope()
}

func newVarsCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vars",
		Short: "Manage variables at any scope",
	}
	cmd.AddCommand(varsSetCmd(rt), varsGetCmd(rt), varsPullCmd(rt))
	return cmd
}

func varsSetCmd(rt *Runtime) *cobra.Command {
	var sf varsScopeFlags
	var secret, generate bool
	var length int
	cmd := &cobra.Command{
		Use:   "set KEY=VAL ...",
		Short: "Set variables on a tile, env, stack or org",
		Long: `Set variables. --secret marks every pair on the line secret; KEY:secret=VAL
marks one, for mixing both in a single command. Stack and org variables
cascade down to every tile under them.

--generate takes bare names instead of KEY=VAL and mints each one on the
server, with the same generator the config file's ` + "`default: generated`" + ` uses.
A name that already has a value is left alone: generating over a live secret
would break everything reading it.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var vars []cli.Var
			for _, a := range args {
				if generate {
					name, suffix, found := strings.Cut(a, ":")
					if strings.Contains(a, "=") {
						return usagef("--generate takes a bare NAME, not KEY=VAL")
					}
					vars = append(vars, cli.Var{Name: name,
						Secret:   secret || (found && suffix == "secret"),
						Generate: true, Length: length})
					continue
				}
				k, v, ok := strings.Cut(a, "=")
				if !ok {
					return usagef("%q is not KEY=VAL", a)
				}
				sec := secret
				if name, suffix, found := strings.Cut(k, ":"); found && suffix == "secret" {
					k, sec = name, true
				}
				vars = append(vars, cli.Var{Name: k, Value: v, Secret: sec})
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			scope, err := sf.resolve(cmd, rt, client)
			if err != nil {
				return err
			}
			out, err := client.SetVarsAt(cmd.Context(), scope, vars)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(struct {
					Scope string `json:"scope"`
					Set   int    `json:"set"`
					Total int    `json:"total"`
				}{scope.Label(), len(vars), len(out)})
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Set %d variable(s) on %s; %d total\n", len(vars), scope.Label(), len(out))
			return nil
		},
	}
	sf.register(cmd)
	f := cmd.Flags()
	f.BoolVar(&secret, "secret", false, "mark every pair secret")
	f.BoolVar(&generate, "generate", false, "mint the value on the server; takes bare names")
	f.IntVar(&length, "length", 0, "generated length (default 32)")
	return cmd
}

type varRow struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Secret bool   `json:"secret,omitempty"`
}

// varRefExpr is how a tile reads this row. Env-scoped rows shadow the stack's,
// so they read through the same stack reference. A tile's own variables are not
// referenced by name from elsewhere, so they get no column value.
func varRefExpr(scope cli.VarScope, r varRow) string {
	level := ""
	switch {
	case scope.Org != "":
		level = "org"
	case scope.Stack != "", scope.Env != "":
		level = "stack"
	default:
		return ""
	}
	bucket := "vars"
	if r.Secret {
		bucket = "secrets"
	}
	return "${{ " + level + "." + bucket + "." + r.Name + " }}"
}

func varsGetCmd(rt *Runtime) *cobra.Command {
	var sf varsScopeFlags
	cmd := &cobra.Command{
		Use:     "get",
		Aliases: []string{"ls", "list"},
		Short:   "List a scope's variables (secret values masked)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			scope, err := sf.resolve(cmd, rt, client)
			if err != nil {
				return err
			}
			vars, err := client.VarsAt(cmd.Context(), scope)
			if err != nil {
				return err
			}
			// Plain values first, then secrets: the two are separate
			// namespaces in a reference (${{ stack.vars.X }} vs
			// ${{ stack.secrets.X }}), so a mixed list hides which one a name
			// is reachable under.
			rows := make([]varRow, 0, len(vars))
			for _, want := range []bool{false, true} {
				for _, v := range vars {
					if v.Secret != want {
						continue
					}
					value := v.Value
					if v.Secret {
						// Masked in JSON too: `vars pull` is the one deliberate
						// secret exporter.
						value = "(secret)"
					}
					rows = append(rows, varRow{v.Name, value, v.Secret})
				}
			}
			if rt.JSON {
				return rt.EmitJSON(rows)
			}
			cells := make([][]string, len(rows))
			for i, r := range rows {
				cells[i] = []string{r.Name, r.Value, varRefExpr(scope, r)}
			}
			rt.Table([]string{"NAME", "VALUE", "REFERENCE"}, cells)
			return nil
		},
	}
	sf.register(cmd)
	return cmd
}

func varsPullCmd(rt *Runtime) *cobra.Command {
	var output, app string
	var force bool
	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Write the app's resolved variables as dotenv",
		Long: `Write the resolved environment (references expanded, real secret values) as
dotenv. Files are 0600 and never overwritten without --force: this is the one
command that puts secrets on disk.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			appID, err := rt.resolveAppID(app)
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			vars, err := client.ResolvedVars(cmd.Context(), appID)
			if err != nil {
				return err
			}
			var b strings.Builder
			for _, v := range vars {
				b.WriteString(v.Name + "=" + cli.DotenvQuote(v.Value) + "\n")
			}
			if output == "" {
				_, _ = fmt.Fprint(rt.Stdout, b.String())
				return nil
			}
			if _, err := os.Stat(output); err == nil && !force {
				return fmt.Errorf("%s exists; pass --force to overwrite", output)
			}
			if err := os.WriteFile(output, []byte(b.String()), 0o600); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Wrote %d variable(s) to %s\n", len(vars), output)
			return nil
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "write to a file instead of stdout")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing file")
	cmd.Flags().StringVar(&app, "tile", "", "tile id (default: the linked tile)")
	return cmd
}
