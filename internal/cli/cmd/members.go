package cmd

// Members, roles and invites. Onboarding a person was panel-only, so it was the
// one part of setting up an organization nobody could script.

import (
	"fmt"

	"github.com/spf13/cobra"
)

func orgMembersCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "members",
		Aliases: []string{"member"},
		Short:   "People in an organization and what they can do",
	}
	cmd.AddCommand(membersLsCmd(rt), membersAddCmd(rt), membersSetCmd(rt), membersRmCmd(rt))
	return cmd
}

func membersLsCmd(rt *Runtime) *cobra.Command {
	var of orgFlag
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List members and their roles",
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
			ms, err := client.Members(cmd.Context(), org)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(ms)
			}
			rows := make([][]string, len(ms))
			for i, m := range ms {
				rows[i] = []string{m.Email, orNone(m.Name), m.Role, m.UserID}
			}
			rt.Table([]string{"EMAIL", "NAME", "ROLE", "USER ID"}, rows)
			return nil
		},
	}
	of.register(cmd)
	return cmd
}

func membersAddCmd(rt *Runtime) *cobra.Command {
	var of orgFlag
	var role string
	var days int
	cmd := &cobra.Command{
		Use:   "add <email>",
		Short: "Invite somebody to the organization",
		Long: `Invite somebody to the organization.

Always an invite, never a direct membership: a user row is created by accepting
one, so granting access to an address that has never signed in would be access
to an account that does not exist. The printed link is a credential.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			org, err := of.value()
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			inv, err := client.AddMember(cmd.Context(), org, args[0], role, days)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(inv)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Invited %s as %s. The link is a credential:\n  %s\n",
				inv.Email, inv.Role, inv.URL)
			return nil
		},
	}
	of.register(cmd)
	cmd.Flags().StringVar(&role, "role", "member", "owner | member | viewer")
	cmd.Flags().IntVar(&days, "expires-days", 0, "link lifetime (default 7)")
	return cmd
}

func membersSetCmd(rt *Runtime) *cobra.Command {
	var of orgFlag
	var role string
	cmd := &cobra.Command{
		Use:   "set <user-id>",
		Short: "Change somebody's role",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if role == "" {
				return usagef("--role is required (owner, member or viewer)")
			}
			org, err := of.value()
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			m, err := client.SetMemberRole(cmd.Context(), org, args[0], role)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(m)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "%s is now %s\n", m.Email, m.Role)
			return nil
		},
	}
	of.register(cmd)
	cmd.Flags().StringVar(&role, "role", "", "owner | member | viewer (required)")
	return cmd
}

func membersRmCmd(rt *Runtime) *cobra.Command {
	var of orgFlag
	cmd := &cobra.Command{
		Use:     "rm <user-id>",
		Aliases: []string{"remove"},
		Short:   "Remove somebody from the organization",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			org, err := of.value()
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if err := rt.Confirm("Remove " + args[0] + " from " + org + "?"); err != nil {
				return err
			}
			if err := client.RemoveMember(cmd.Context(), org, args[0]); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{args[0], true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Removed "+args[0])
			return nil
		},
	}
	of.register(cmd)
	return cmd
}

func orgInvitesCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "invites",
		Aliases: []string{"invite"},
		Short:   "Open invitations to an organization",
	}
	cmd.AddCommand(invitesLsCmd(rt), invitesAddCmd(rt), invitesRmCmd(rt))
	return cmd
}

func invitesLsCmd(rt *Runtime) *cobra.Command {
	var of orgFlag
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List open invitations",
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
			invs, err := client.Invites(cmd.Context(), org)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(invs)
			}
			rows := make([][]string, len(invs))
			for i, v := range invs {
				state := "open"
				if v.Used {
					state = "used"
				}
				rows[i] = []string{orNone(v.Email), v.Role, state,
					v.ExpiresAt.Local().Format("2006-01-02"), v.ID}
			}
			rt.Table([]string{"EMAIL", "ROLE", "STATE", "EXPIRES", "ID"}, rows)
			return nil
		},
	}
	of.register(cmd)
	return cmd
}

func invitesAddCmd(rt *Runtime) *cobra.Command {
	var of orgFlag
	var role, email string
	var days int
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Mint an invitation link",
		Long: `Mint an invitation link.

Without --email the link works for anyone who has it, which is exactly as
open as it sounds.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			org, err := of.value()
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			inv, err := client.AddInvite(cmd.Context(), org, email, role, days)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(inv)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Invite for %s as %s. The link is a credential:\n  %s\n",
				orNone(inv.Email), inv.Role, inv.URL)
			return nil
		},
	}
	of.register(cmd)
	f := cmd.Flags()
	f.StringVar(&email, "email", "", "restrict the link to one address")
	f.StringVar(&role, "role", "member", "owner | member | viewer")
	f.IntVar(&days, "expires-days", 0, "link lifetime (default 7)")
	return cmd
}

func invitesRmCmd(rt *Runtime) *cobra.Command {
	var of orgFlag
	cmd := &cobra.Command{
		Use:     "rm <invite-id>",
		Aliases: []string{"revoke"},
		Short:   "Revoke an invitation",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			org, err := of.value()
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if err := client.DeleteInvite(cmd.Context(), org, args[0]); err != nil {
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
