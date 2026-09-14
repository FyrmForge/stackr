package cmd

// The org registry verbs: the credentials CI pushes with, and the images that
// came out. Everything is scoped to one org, because the registry namespace is.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/FyrmForge/stackr/internal/cli"
)

func newImageCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "image",
		Aliases: []string{"images"},
		Short:   "Browse and prune the images in an organization's registry namespace",
	}
	cmd.AddCommand(imageLsCmd(rt), imageTagsCmd(rt), imageRmCmd(rt))
	return cmd
}

// orgFlag is the organization every registry verb acts on. A slug is fine: the
// API takes an id or a slug anywhere it takes an org.
type orgFlag struct{ org string }

func (o *orgFlag) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&o.org, "org", "", "organization id or slug (required)")
}

func (o *orgFlag) value() (string, error) {
	if o.org == "" {
		return "", usagef("--org <slug> is required; `stackr org ls` lists them")
	}
	return o.org, nil
}

func imageLsCmd(rt *Runtime) *cobra.Command {
	var of orgFlag
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the images in the organization's namespace",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			org, err := of.value()
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			imgs, err := client.RegistryImages(cmd.Context(), org)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(imgs)
			}
			rows := make([][]string, len(imgs))
			for i, im := range imgs {
				rows[i] = []string{im.Short, im.Name}
			}
			rt.Table([]string{"IMAGE", "REPOSITORY"}, rows)
			return nil
		},
	}
	of.register(cmd)
	return cmd
}

func imageTagsCmd(rt *Runtime) *cobra.Command {
	var of orgFlag
	cmd := &cobra.Command{
		Use:   "tags <image>",
		Short: "List an image's tags with their sizes and digests",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			org, err := of.value()
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			tags, err := client.RegistryTags(cmd.Context(), org, args[0])
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(tags)
			}
			rows := make([][]string, len(tags))
			for i, t := range tags {
				rows[i] = []string{t.Tag, strconv.FormatInt(t.Size/(1024*1024), 10), t.Digest}
			}
			rt.Table([]string{"TAG", "SIZE MB", "DIGEST"}, rows)
			return nil
		},
	}
	of.register(cmd)
	return cmd
}

func imageRmCmd(rt *Runtime) *cobra.Command {
	var of orgFlag
	cmd := &cobra.Command{
		Use:     "rm <image>:<tag>",
		Aliases: []string{"delete"},
		Short:   "Delete one image tag",
		Long: `Delete one image tag.

Refused while a live deployment still references it: a restart would pull the
tag and get a manifest-unknown error with nothing left to explain it. The disk
space comes back with the next nightly cleanup, not immediately.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			org, err := of.value()
			if err != nil {
				return err
			}
			// The last colon: the image name holds slashes and may hold a
			// registry port, so splitting on the first one would cut it apart.
			i := strings.LastIndex(args[0], ":")
			if i < 0 {
				return usagef("give the image as <image>:<tag>")
			}
			name, tag := args[0][:i], args[0][i+1:]
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if err := rt.Confirm(fmt.Sprintf("Delete %s:%s?", name, tag)); err != nil {
				return err
			}
			if err := client.DeleteRegistryTag(cmd.Context(), org, name, tag); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{args[0], true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Deleted "+args[0])
			return nil
		},
	}
	of.register(cmd)
	return cmd
}

func orgRegistryCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage the organization's registry push credentials",
	}
	creds := &cobra.Command{
		Use:     "creds",
		Aliases: []string{"credentials"},
		Short:   "Credentials CI uses to push into this organization's namespace",
	}
	creds.AddCommand(orgCredsLsCmd(rt), orgCredsAddCmd(rt), orgCredsRmCmd(rt))
	cmd.AddCommand(creds)
	return cmd
}

func orgCredsLsCmd(rt *Runtime) *cobra.Command {
	var of orgFlag
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the organization's registry credentials",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			org, err := of.value()
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			creds, err := client.RegistryCredentials(cmd.Context(), org)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(creds)
			}
			rows := make([][]string, len(creds))
			for i, cr := range creds {
				used := "never"
				if cr.LastUsedAt != nil {
					used = cr.LastUsedAt.Format("2006-01-02 15:04")
				}
				kind := "user"
				if cr.System {
					kind = "stackr"
				}
				rows[i] = []string{cr.Name, kind, cr.Prefix + "…", used, cr.ID}
			}
			rt.Table([]string{"NAME", "OWNER", "PREFIX", "LAST USED", "ID"}, rows)
			return nil
		},
	}
	of.register(cmd)
	return cmd
}

func orgCredsAddCmd(rt *Runtime) *cobra.Command {
	var of orgFlag
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Mint a registry credential; the secret is shown once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			org, err := of.value()
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			cr, err := client.AddRegistryCredential(cmd.Context(), org, args[0])
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(cr)
			}
			// Stored hashed, so this is the only time it can be printed.
			_, _ = fmt.Fprintf(rt.Stdout,
				"Created %s.\n\n  docker login <registry> -u %s -p %s\n\nThe secret is not stored and will not be shown again.\n",
				cr.Name, org, cr.Secret)
			return nil
		},
	}
	of.register(cmd)
	return cmd
}

func orgCredsRmCmd(rt *Runtime) *cobra.Command {
	var of orgFlag
	cmd := &cobra.Command{
		Use:   "rm <credential-id>",
		Short: "Revoke a registry credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			org, err := of.value()
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if err := rt.Confirm("Revoke credential " + args[0] + "? Anything pushing with it stops working."); err != nil {
				return err
			}
			if err := client.DeleteRegistryCredential(cmd.Context(), org, args[0]); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{args[0], true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Revoked "+args[0])
			return nil
		},
	}
	of.register(cmd)
	return cmd
}

// newRegistryCmd is the admin view: the managed registry's TLS domain, and the
// external registries a tile can pull from. Per-org push credentials live under
// `stackr org registry`, which is a different thing on purpose.
func newRegistryCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage the managed registry and external ones (admin)",
	}
	cmd.AddCommand(registryLsCmd(rt), registryAddCmd(rt), registrySetCmd(rt), registryRmCmd(rt))
	return cmd
}

func registryLsCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List registries",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			regs, err := client.Registries(cmd.Context())
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(regs)
			}
			rows := make([][]string, len(regs))
			for i, r := range regs {
				kind := "external"
				if r.Managed {
					kind = "managed"
				}
				rows[i] = []string{r.Name, kind, r.URL, orNone(r.Domain), r.ID}
			}
			rt.Table([]string{"NAME", "KIND", "URL", "DOMAIN", "ID"}, rows)
			return nil
		},
	}
}

func registryAddCmd(rt *Runtime) *cobra.Command {
	var url, user, pass string
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Record an external registry a tile can pull from",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if url == "" {
				return usagef("--url <host[:port]> is required")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if pass == "" && user != "" {
				if pass, err = rt.PromptSecret("Registry password"); err != nil {
					return err
				}
			}
			r, err := client.AddRegistry(cmd.Context(),
				cli.Registry{Name: args[0], URL: url, Username: user}, pass)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(r)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Added %s (%s)\n", r.Name, r.ID)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&url, "url", "", "host[:port], no scheme (required)")
	f.StringVar(&user, "username", "", "pull username")
	f.StringVar(&pass, "password", "", "pull password (prompted when a username is given)")
	return cmd
}

func registrySetCmd(rt *Runtime) *cobra.Command {
	var domain string
	cmd := &cobra.Command{
		Use:   "set <registry-id>",
		Short: "Set the managed registry's TLS domain",
		Long: `Set the managed registry's TLS domain.

The domain is what lets a worker pull without an insecure-registries entry in
its daemon.json. Pass an empty value to clear it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("domain") {
				return usagef("--domain <host> is required (pass \"\" to clear it)")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			r, err := client.PatchRegistry(cmd.Context(), args[0], map[string]any{"domain": domain})
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(r)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "%s domain: %s\n", r.Name, orNone(r.Domain))
			return nil
		},
	}
	cmd.Flags().StringVar(&domain, "domain", "", "TLS hostname traefik routes to the registry")
	return cmd
}

func registryRmCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <registry-id>",
		Aliases: []string{"delete"},
		Short:   "Remove an external registry",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if err := rt.Confirm("Remove registry " + args[0] + "?"); err != nil {
				return err
			}
			if err := client.DeleteRegistry(cmd.Context(), args[0]); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{args[0], true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Removed "+args[0])
			return nil
		},
	}
}
