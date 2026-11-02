package main

import (
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

var recordCols = []string{"id", "host", "type", "value", "written", "not_live", "put_back_at"}

// records is the custom DNS record verbs of one owner (a tile domain's
// host, or a managed domain): use names the owner's args, n of them, and
// path resolves the owner's records path from them. ops are the API
// operations of ls, add and rm.
func (a *app) records(use string, n int, ops [3]string, path func(c *cobra.Command, args []string) (string, error)) *cobra.Command {
	ls := leaf("ls "+use, ops[0], "List the custom DNS records and where stackr writes them", exact(n),
		func(c *cobra.Command, args []string) error {
			p, err := path(c, args)
			if err != nil {
				return err
			}
			v, err := a.call(GET, p, nil)
			if err != nil || a.json {
				if err == nil {
					err = a.show(v)
				}
				return err
			}
			m := asMap(v)
			if s := cell(m["source"]); s != "(none)" {
				a.say("%s", s)
			} else {
				a.say("No managed domain covers this name; records cannot be added.")
			}
			return a.show(m["records"], recordCols...)
		})
	var name string
	add := leaf("add "+use+" <type> <value>", ops[1],
		"Add a custom DNS record: TXT, MX (\"10 mx.example.net\"), CNAME, A or AAAA", exact(n+2),
		func(c *cobra.Command, args []string) error {
			p, err := path(c, args)
			if err != nil {
				return err
			}
			v, err := a.call(POST, p, map[string]any{"name": name, "type": args[n], "value": args[n+1]})
			if err != nil {
				return err
			}
			if note := cell(asMap(v)["note"]); note != "(none)" && !a.json {
				a.say("%s", note)
			}
			return a.show(v, recordCols...)
		})
	add.Flags().StringVar(&name, "name", "", "the name below, e.g. _dmarc (default: the name itself)")
	rm := leaf("rm "+use+" <record>", ops[2], "Remove a custom DNS record by id or value; stackr deletes it from the zone", exact(n+1),
		func(c *cobra.Command, args []string) error {
			p, err := path(c, args)
			if err != nil {
				return err
			}
			v, err := a.call(GET, p, nil)
			if err != nil {
				return err
			}
			var hit []map[string]any
			for _, r := range asList(asMap(v)["records"]) {
				if m := asMap(r); cell(m["id"]) == args[n] || cell(m["value"]) == args[n] {
					hit = append(hit, m)
				}
			}
			if len(hit) != 1 {
				return usage("%d records match %q; pass the id (records ls)", len(hit), args[n])
			}
			if err := a.confirm("Remove " + cell(hit[0]["type"]) + " " + cell(hit[0]["host"]) + "? stackr deletes it from the zone."); err != nil {
				return err
			}
			if _, err = a.call(DELETE, p+"/"+url.PathEscape(cell(hit[0]["id"])), nil); err != nil {
				return err
			}
			a.say("%s %s: removed", cell(hit[0]["type"]), cell(hit[0]["host"]))
			return nil
		})
	return noun("records", "Custom DNS records (TXT, MX, CNAME, A, AAAA)", ls, add, rm)
}

// tileRecords is `stackr tile domain records`: a tile domain's, by host.
func (a *app) tileRecords() *cobra.Command {
	return a.records("<host>", 1,
		[3]string{"domain.list,domain.records", "domain.list,domain.record-add", "domain.list,domain.records,domain.record-rm"},
		func(c *cobra.Command, args []string) (string, error) {
			p, err := a.path(c, atTile, "")
			if err != nil {
				return "", err
			}
			cur, err := a.find(p+"/domains", "domain", args[0], "host", "id")
			if err != nil {
				return "", err
			}
			return p + "/domains/" + url.PathEscape(cell(cur["id"])) + "/records", nil
		})
}

// dnsRecords is `stackr server|org connectors records`: a managed
// domain's own.
func (a *app) dnsRecords(op func(string) string, base func(c *cobra.Command) (string, error)) *cobra.Command {
	return a.records("<connector> <domain>", 2,
		[3]string{op("records"), op("record-add"), op("records") + "," + op("record-rm")},
		func(c *cobra.Command, args []string) (string, error) {
			b, err := base(c)
			if err != nil {
				return "", err
			}
			return b + "/" + url.PathEscape(args[0]) + "/domains/" + url.PathEscape(args[1]) + "/records", nil
		})
}

var (
	managedDomainCols = []string{"name", "scope_text", "via", "orgs", "records"}
	managedRecordCols = []string{"name", "type", "value", "whose_text", "host", "drift"}
)

// managedDomains is `stackr server|org domains [domain]`: every managed
// domain (scope, connector, who may use it), or one and the records
// stackr wrote on it. base is the level's managed-domains path.
func (a *app) managedDomains(ops string, base func() (string, error)) *cobra.Command {
	return leaf("domains [domain]", ops, "List the managed domains, or one domain's records stackr wrote", upTo(1),
		func(_ *cobra.Command, args []string) error {
			p, err := base()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				p += "/" + url.PathEscape(args[0])
			}
			v, err := a.call(GET, p, nil)
			if err != nil || a.json {
				if err == nil {
					err = a.show(v)
				}
				return err
			}
			if len(args) == 0 {
				var rows []any
				for _, d := range asList(v) {
					m := asMap(d)
					m["orgs"] = whoMay(m)
					m["records"] = len(asList(m["records"]))
					rows = append(rows, m)
				}
				return a.show(rows, managedDomainCols...)
			}
			m := asMap(v)
			a.say("%s: %s, %s", cell(m["name"]), cell(m["scope_text"]), cell(m["via"]))
			if who := whoMay(m); who != "" {
				a.say("Available to %s", who)
			}
			a.say("Only records stackr wrote are listed; the rest are at the DNS host.")
			return a.show(m["records"], managedRecordCols...)
		})
}

// whoMay says who may use a managed domain; "" when the server does not
// say (an org's view).
func whoMay(m map[string]any) string {
	if m["all_orgs"] == true {
		return "every org"
	}
	var out []string
	for _, o := range asList(m["orgs"]) {
		out = append(out, cell(o))
	}
	return strings.Join(out, ", ")
}
