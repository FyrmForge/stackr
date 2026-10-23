package main

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// kindField is a field a connector kind declares, as GET .../connector-kinds
// lists it.
type kindField struct {
	Name, Label string
	Secret      bool
}

type connectorKind struct {
	Name, Label string
	Redirects   bool
	Steps       []struct{ Fields []kindField }
}

// addConnector is `connectors add <kind>` at one level: the kind's
// declared fields, asked of the API first. A secret is never taken on argv:
// --stdin names the one read from stdin, else STACKR_<KIND>_<FIELD> or a
// prompt. base is the level's DNS connectors path; its parent holds the
// kinds.
func (a *app) addConnector(server bool, base func(c *cobra.Command) (string, error)) []*cobra.Command {
	kindsOp, op := "connector.kinds", "connector.add"
	if server {
		kindsOp, op = "admin.connector-kinds", "admin.connector-add"
	}
	var sets []string
	var stdin string
	add := leaf("add <kind>", op,
		"Add a connector of a kind (see kinds): checks it and, for DNS, lists its domains", exact(1),
		func(c *cobra.Command, args []string) error {
			b, err := base(c)
			if err != nil {
				return err
			}
			level := strings.TrimSuffix(b, "/dns-connectors")
			raw, err := a.call(GET, level+"/connector-kinds", nil)
			if err != nil {
				return err
			}
			var kinds []connectorKind
			j, _ := json.Marshal(raw)
			_ = json.Unmarshal(j, &kinds)
			var k *connectorKind
			for i := range kinds {
				if kinds[i].Name == args[0] {
					k = &kinds[i]
				}
			}
			if k == nil {
				return usage("no connector kind %q", args[0])
			}
			if k.Redirects {
				return usage("%s signs in on its own site: add it in the panel", k.Label)
			}
			given := map[string]string{}
			for _, s := range sets {
				name, val, ok := strings.Cut(s, "=")
				if !ok {
					return usage("--set takes field=value, not %q", s)
				}
				given[name] = val
			}
			body := map[string]string{}
			for _, s := range k.Steps {
				for _, f := range s.Fields {
					v, set := given[f.Name]
					delete(given, f.Name)
					switch {
					case f.Secret && set:
						return usage("%s is secret: pipe it with --stdin %s, or set %s", f.Name, f.Name, secretEnv(k.Name, f.Name))
					case f.Secret && stdin == f.Name:
						in, err := io.ReadAll(a.in)
						if err != nil {
							return err
						}
						v = strings.TrimSpace(string(in))
					case f.Secret:
						if v, err = a.secret("", secretEnv(k.Name, f.Name), k.Label+" "+f.Label); err != nil {
							return err
						}
					}
					if f.Secret && v == "" {
						return usage("no %s: pipe it with --stdin %s, or set %s", f.Label, f.Name, secretEnv(k.Name, f.Name))
					}
					body[f.Name] = v
				}
			}
			for name := range given {
				return usage("%s has no field %q", k.Label, name)
			}
			v, err := a.call(POST, level+"/connector-kinds/"+k.Name, body)
			if err != nil || a.json {
				if err == nil {
					err = a.show(v)
				}
				return err
			}
			m, _ := v.(map[string]any)
			conn, _ := m["connector"].(map[string]any)
			a.say("connector %s (%s)", cell(conn["id"]), cell(conn["name"]))
			if m["domains"] == nil {
				return nil
			}
			return a.show(m["domains"], dnsDomainCols...)
		})
	add.Example = "  pass show dns-key | stackr server connectors add <kind> --set user=jo --stdin key"
	add.Flags().StringArrayVar(&sets, "set", nil, "a field's value, field=value (repeatable; never a secret)")
	add.Flags().StringVar(&stdin, "stdin", "", "the secret field read from stdin (else STACKR_<KIND>_<FIELD> or a prompt)")
	kinds := leaf("kinds", kindsOp, "List the connector kinds and the fields each asks for", exact(0),
		func(c *cobra.Command, _ []string) error {
			b, err := base(c)
			if err != nil {
				return err
			}
			v, err := a.call(GET, strings.TrimSuffix(b, "/dns-connectors")+"/connector-kinds", nil)
			if err != nil || a.json {
				if err == nil {
					err = a.show(v)
				}
				return err
			}
			var ks []connectorKind
			j, _ := json.Marshal(v)
			_ = json.Unmarshal(j, &ks)
			for _, k := range ks {
				var fs []string
				for _, s := range k.Steps {
					for _, f := range s.Fields {
						if f.Secret {
							f.Name += " (secret)"
						}
						fs = append(fs, f.Name)
					}
				}
				a.say("%s\t%s\t%s", k.Name, k.Label, strings.Join(fs, ", "))
			}
			return nil
		})
	return []*cobra.Command{kinds, add}
}

// secretEnv is where a kind's secret field may come from besides stdin
// or a prompt: STACKR_<KIND>_<FIELD>, upper case.
func secretEnv(kind, field string) string {
	return "STACKR_" + strings.ToUpper(kind+"_"+field)
}
