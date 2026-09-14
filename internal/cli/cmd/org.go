package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newOrgCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "org",
		Short: "Organizations and org-level config-as-code plans",
		Long: `Organizations. ` + "`ls`" + ` shows the slug every path form starts with; the
config binding is set in the panel (org settings → Config as code), and the
rest of this covers the plan/approve loop.`,
	}
	cmd.AddCommand(orgLsCmd(rt), orgRegistryCmd(rt), orgDefaultsCmd(rt), orgExportCmd(rt), orgMembersCmd(rt), orgInvitesCmd(rt), orgPreviewCmd(rt), orgPlanCmd(rt), orgPlansCmd(rt), orgApproveCmd(rt), orgRejectCmd(rt))
	return cmd
}

func orgPreviewCmd(rt *Runtime) *cobra.Command {
	var file string
	var detailed bool
	cmd := &cobra.Command{
		Use:   "preview <org-id>",
		Short: "Plan a local stackr-org.yml against live state; stores nothing",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			p, err := client.OrgPlanPreview(cmd.Context(), args[0], string(data))
			if err != nil {
				return err
			}
			if err := rt.emitPlan(p, "org"); err != nil {
				return err
			}
			if detailed {
				return detailedExitErr(p)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "stackr-org.yml", "org config file to preview")
	cmd.Flags().BoolVar(&detailed, "detailed-exitcode", false, "terraform-style exit codes")
	return cmd
}

func orgPlanCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "plan <org-id>",
		Short: "Re-plan the org against its bound config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			p, err := client.OrgPlanNow(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(p)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "%s\t%s\t%s\n", p.ID, p.Status, p.Summary)
			return nil
		},
	}
}

func orgPlansCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "plans <org-id>",
		Short: "List the org's config plans",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			ps, err := client.OrgPlans(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(ps)
			}
			cells := make([][]string, len(ps))
			for i, p := range ps {
				cells[i] = []string{p.ID, p.Status, p.Summary}
			}
			rt.Table([]string{"ID", "STATUS", "SUMMARY"}, cells)
			return nil
		},
	}
}

func orgApproveCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "approve <plan-id>",
		Short: "Apply an org config plan",
		Long: `Apply an org plan. Always confirms: the API has no read-one-org-plan route,
so the CLI cannot check what the plan does before applying it. Skip with
--yes / -y.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.Confirm(fmt.Sprintf("Apply org plan %s? (its contents cannot be previewed here)", args[0])); err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			p, err := client.ApproveOrgPlan(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(p)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "%s applied: %s\n", p.ID, p.Summary)
			return nil
		},
	}
}

func orgRejectCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "reject <plan-id>",
		Short: "Close an org config plan unapplied",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			p, err := client.RejectOrgPlan(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(p)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "%s rejected\n", p.ID)
			return nil
		},
	}
}
