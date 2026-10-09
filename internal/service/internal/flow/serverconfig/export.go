package serverconfig

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// DestParamCollection is the server params collection that holds a
// destination's keys: backup_<name>, the name lower-cased with anything but
// letters and digits turned into "_". Export writes the refs, so the
// orchestrator seeds these params from the destination rows before it
// gathers Live (and a fresh box sets them by hand before the first apply).
func DestParamCollection(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return "backup_" + b.String()
}

// DestKeys are the param names under DestParamCollection, one per key.
var DestKeys = []string{"access_key", "secret_key", "archive_key"}

func destRef(name, key string) string {
	return fmt.Sprintf("${{ server.params.%s.%s }}", DestParamCollection(name), key)
}

// Export is live written as the server file: every setting that is not at
// its default (the panel's destination by name), the cascade rung, the
// server params (a secret by name and type only, the file goes in git),
// routes, the S3 destinations with key refs, the instance domains, the
// server connectors by name with their shares, and the orgs that are bound
// to an org file. Diffing it against the same live is clean, provided the
// destination key params it declares are set (see DestParamCollection).
func Export(live Live) ([]byte, error) {
	f := File{Version: 1}
	f.Settings = exportSettings(live)
	if d := Defaults(live.Defaults); d != (Defaults{}) {
		if pw := d.ProtectPassword; pw != nil && !planfile.IsRef(*pw) {
			d.ProtectUser, d.ProtectPassword = nil, nil
		}
		if d != (Defaults{}) {
			f.Defaults = &d
		}
	}
	f.Params = map[string]map[string]Param{}
	for key, v := range live.Params {
		c, n, _ := strings.Cut(key, ".")
		if f.Params[c] == nil {
			f.Params[c] = map[string]Param{}
		}
		decl := Param{Type: params.Secret}
		if !v.Secret {
			decl = Param{Type: params.Param, Value: &v.V}
		}
		f.Params[c][n] = decl
	}
	for _, r := range live.Routes {
		f.Routes = append(f.Routes, Route{Host: r.Host, Mode: r.Mode, Target: r.Target, Insecure: r.Insecure})
	}
	if err := f.exportDests(live); err != nil {
		return nil, err
	}
	for _, r := range live.Domains {
		if r.Level == domainres.Instance {
			f.Domains = append(f.Domains, Domain{Host: r.Host, IncludeEnvOnDefault: r.IncludeEnvOnDefault, ACMEEmail: r.ACMEEmail})
		}
	}
	f.exportConnectors(live)
	f.exportOrgs(live)
	if len(f.Params) == 0 {
		f.Params = nil
	}
	slices.SortFunc(f.Routes, func(a, b Route) int { return strings.Compare(a.Host, b.Host) })
	slices.SortFunc(f.BackupDests, func(a, b Dest) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(f.Domains, func(a, b Domain) int { return strings.Compare(a.Host, b.Host) })
	slices.SortFunc(f.Connectors, func(a, b Connector) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(f.Orgs, func(a, b Org) int { return strings.Compare(a.Slug, b.Slug) })

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	err := enc.Close()
	return buf.Bytes(), err
}

// exportSettings writes each Flat knob whose effective value is not the
// catalogue default, as the type the knob has. The panel's backup dest is
// stored as an id and written as the destination's name.
func exportSettings(live Live) map[string]any {
	out := map[string]any{}
	for _, k := range settings.Catalogue {
		if k.Scopes != settings.Flat || k.ConfigOnly || k.ReadOnly {
			continue
		}
		v := strings.TrimSpace(live.eff(k.Key))
		if k.Key == "panel_backup_dest" {
			i := slices.IndexFunc(live.Dests, func(d store.BackupDest) bool { return d.ID == v })
			v = ""
			if i >= 0 && live.Dests[i].Kind != backup.Local {
				v = live.Dests[i].Name
			}
		}
		if norm(k.Key, v) == norm(k.Key, k.Default) {
			continue
		}
		// A value the panel took but Parse refuses (an acme_email with a
		// display name, a provider not in this build) stays out: the file
		// must parse, and a key the file leaves out is not touched.
		if _, err := CheckSetting(k.Key, v); err != nil {
			continue
		}
		switch n, err := strconv.Atoi(v); {
		case k.Type == settings.TBool && v != "":
			b, _ := strconv.ParseBool(v)
			out[k.Key] = b
		case k.Type == settings.TInt && err == nil:
			out[k.Key] = n
		default:
			out[k.Key] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// exportDests writes the global S3 destinations with refs for their keys
// and declares the params they name, so the file parses on its own.
func (f *File) exportDests(live Live) error {
	seen := map[string]string{}
	for _, d := range live.Dests {
		if d.Kind != backup.S3 || d.OrgID != nil {
			continue
		}
		coll := DestParamCollection(d.Name)
		if other, dup := seen[coll]; dup {
			return fmt.Errorf("export: destinations %s and %s both map to server params %s; rename one", other, d.Name, coll)
		}
		seen[coll] = d.Name
		if f.Params[coll] == nil {
			f.Params[coll] = map[string]Param{}
		}
		for _, k := range DestKeys {
			f.Params[coll][k] = Param{Type: params.Secret}
		}
		f.BackupDests = append(f.BackupDests, Dest{
			Name:       d.Name,
			Kind:       backup.S3,
			Endpoint:   d.Endpoint,
			Region:     d.Region,
			Bucket:     d.Bucket,
			AccessKey:  destRef(d.Name, "access_key"),
			SecretKey:  destRef(d.Name, "secret_key"),
			ArchiveKey: destRef(d.Name, "archive_key"),
			Shared:     d.Shared,
		})
	}
	return nil
}

func (f *File) exportConnectors(live Live) {
	for _, c := range live.Connectors {
		fc := Connector{Name: c.Connector.Name}
		switch {
		case c.Connector.ShareAll:
			fc.Share = &Share{All: true}
		case len(c.OrgIDs) > 0:
			fc.Share = &Share{Orgs: []string{}}
			for _, id := range c.OrgIDs {
				fc.Share.Orgs = append(fc.Share.Orgs, orgSlug(live.Orgs, id))
			}
			slices.Sort(fc.Share.Orgs)
		}
		f.Connectors = append(f.Connectors, fc)
	}
}

// exportOrgs lists the orgs bound to an org file; an unbound org has
// nothing to say and the file creates orgs only to bind them. A connector
// that is the org's own (not a server one) reads as no connector.
func (f *File) exportOrgs(live Live) {
	d := &differ{live: live}
	for _, o := range live.Orgs {
		if o.ConfigRepo == "" {
			continue
		}
		fo := Org{
			Slug:      o.Slug,
			Repo:      o.ConfigRepo,
			Branch:    o.ConfigBranch,
			Connector: d.connectorName(o.ConfigConnectorID),
			Auto:      o.ConfigAuto,
		}
		if o.ConfigPath != "" && o.ConfigPath != OrgFilePath {
			fo.Path = o.ConfigPath
		}
		if o.Name != o.Slug && slug.Make(o.Name) == o.Slug {
			fo.Name = o.Name
		}
		f.Orgs = append(f.Orgs, fo)
	}
}
