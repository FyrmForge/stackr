package main

import (
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The cluster trust commands: the root, planned rollover, break glass and the
// controller certificate. Per-node rejoin commands are `stackr node rejoin`.

func day(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("Jan 2, 2006")
	}
	return s
}

func names(v any) string {
	var out []string
	for _, n := range asList(v) {
		out = append(out, cell(asMap(n)["name"]))
	}
	return strings.Join(out, ", ")
}

func (a *app) clusterTrust() *cobra.Command {
	return leaf("trust", "admin.cluster-trust", "The cluster root, any rollover and the nodes not rejoined (server admin)", exact(0),
		func(*cobra.Command, []string) error {
			v, err := a.call(GET, "/admin/cluster-trust", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			m := asMap(v)
			a.say("Root     %s", cell(m["fingerprint"]))
			a.say("Created  %s", day(cell(m["created_at"])))
			a.say("Expires  %s", day(cell(m["expires_at"])))
			if b, _ := m["rolling_over"].(bool); b {
				a.say("Rollover running since %s; on the old root: %s", day(cell(m["rollover_started_at"])), orNone(names(m["on_old_root"])))
			} else {
				a.say("Rollover none running")
			}
			if nr := names(m["not_rejoined"]); nr != "" {
				a.say("Not rejoined: %s. Run stackr node rejoin <name> for each.", nr)
			}
			return nil
		})
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func (a *app) clusterRotateRoot() *cobra.Command {
	var finish bool
	c := leaf("rotate-root", "admin.root-rotate,admin.root-finish",
		"Replace the cluster root; every node gets a new certificate, nothing stops. --finish ends a stuck rollover (server admin)", exact(0),
		func(*cobra.Command, []string) error {
			if finish {
				if err := a.confirm("End the rollover now? The old root stops working; nodes still on it need a rejoin."); err != nil {
					return err
				}
				if _, err := a.call(POST, "/admin/cluster-trust/finish-rollover", nil); err != nil {
					return err
				}
				a.say("Rollover finished. Nodes still on the old root need stackr node rejoin <name>.")
				return nil
			}
			if err := a.confirm("Rotate the cluster root? Every node gets a new certificate; nothing stops while it runs."); err != nil {
				return err
			}
			if _, err := a.call(POST, "/admin/cluster-trust/rotate-root", nil); err != nil {
				return err
			}
			a.say("Rotating the root. The old root is dropped once every node has a new certificate; stackr cluster trust shows where it stands.")
			return nil
		})
	c.Flags().BoolVar(&finish, "finish", false, "end a running rollover now instead of waiting for offline nodes")
	return c
}

func (a *app) clusterBreakGlass() *cobra.Command {
	return leaf("break-glass", "admin.break-glass",
		"Wipe the root and every WireGuard key; each node must rejoin (server admin)", exact(0),
		func(*cobra.Command, []string) error {
			if err := a.confirm("Break glass? Tunnels between nodes stop until each node rejoins. Apps keep running where they are. Deploys and traffic between nodes stop."); err != nil {
				return err
			}
			if _, err := a.call(POST, "/admin/cluster-trust/break-glass", nil); err != nil {
				return err
			}
			v, err := a.call(GET, "/admin/cluster-trust", nil)
			if err != nil {
				return err
			}
			a.say("Done. New root, new keys. Nodes not rejoined: %s", orNone(names(asMap(v)["not_rejoined"])))
			a.say("Run stackr node rejoin <name> for each, then the command it prints on that machine.")
			return nil
		})
}

func (a *app) clusterRevokeControllerCert() *cobra.Command {
	return leaf("revoke-controller-cert", "admin.controller-cert-revoke",
		"Give the controller a new certificate; workers stop accepting the old one (server admin)", exact(0),
		func(*cobra.Command, []string) error {
			if err := a.confirm("Revoke the controller certificate? The controller gets a new one and workers stop accepting the old."); err != nil {
				return err
			}
			if _, err := a.call(POST, "/admin/cluster-trust/revoke-controller-cert", nil); err != nil {
				return err
			}
			a.say("Replacing the controller certificate. Workers stop accepting the old one within minutes.")
			return nil
		})
}
