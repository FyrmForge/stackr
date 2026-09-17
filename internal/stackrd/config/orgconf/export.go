package orgconf

// Exporting an organization back to its config file, the org-level half of
// stackconf's export. Stacks are listed as declarations only: each one's body
// lives in its own file, and inlining them here would produce a file that
// claims to own stacks it cannot round-trip.

import (
	"context"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/envcolor"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ExportFile builds the org file from live state.
func ExportFile(ctx context.Context, store repo.Store, org *repo.Org) (*File, error) {
	f := &File{Version: 1, Org: org.Name}
	f.Defaults.UIEdits = org.UIEditsDefault
	f.Defaults.EnvColors = envcolor.OrgDefaults(org)
	f.Defaults.DefaultsConf = defaultsFromSettings(settings.Parse(org.Settings))

	vars, err := store.ListVariables(ctx, repo.OwnerOrg, org.ID)
	if err != nil {
		return nil, err
	}
	for _, v := range vars {
		if v.Secret {
			// Declarations only: the file is meant to go into git, and a
			// secret's value is the one thing that must never be in it.
			if f.Secrets == nil {
				f.Secrets = stackconf.SecretsNode{}
			}
			f.Secrets[v.Name] = stackconf.SecretConf{}
			continue
		}
		if f.Vars == nil {
			f.Vars = map[string]string{}
		}
		f.Vars[v.Name] = v.Value
	}

	shares, err := store.ListStorage(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range shares {
		if s.OrgID != org.ID {
			continue
		}
		if f.Storage == nil {
			f.Storage = map[string]StorageConf{}
		}
		// The password never goes in the file: reference the org secret that
		// holds it, or declare one for the operator to set when none does.
		pw := ""
		if s.Password != "" {
			name := secretHolding(vars, s.Password)
			if name == "" {
				name = strings.ToUpper(strings.ReplaceAll(s.Slug, "-", "_")) + "_PASSWORD"
				if f.Secrets == nil {
					f.Secrets = stackconf.SecretsNode{}
				}
				f.Secrets[name] = stackconf.SecretConf{}
			}
			pw = "${{ org.secrets." + name + " }}"
		}
		f.Storage[s.Slug] = StorageConf{Backend: s.Backend, Address: s.Address, Export: s.Export,
			Username: s.Username, Password: pw, Opts: s.Opts}
	}

	all, err := store.ListDomainResources(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		if d.Level == "org" && d.OwnerID == org.ID {
			f.Domains = append(f.Domains, stackconf.DomainResConf{
				Host: d.Host, ACMEEmail: d.ACMEEmail, IncludeEnvOnDefault: d.IncludeEnvOnDefault})
		}
	}
	sort.Slice(f.Domains, func(i, j int) bool { return f.Domains[i].Host < f.Domains[j].Host })

	stacks, err := store.ListStacksByOrg(ctx, org.ID)
	if err != nil {
		return nil, err
	}
	for i := range stacks {
		s := &stacks[i]
		if !s.ConfigManaged() {
			// A panel-managed stack has no file for the org file to point at.
			// `stackr stack export` writes one; declaring it here first would
			// make the next org apply try to clone a repo nobody has bound.
			continue
		}
		if f.Stacks == nil {
			f.Stacks = map[string]StackRef{}
		}
		f.Stacks[s.Slug] = StackRef{Repo: s.ConfigRepo, Branch: s.ConfigBranch,
			Path: s.ConfigPath, Connector: s.ConfigConnectorID}
	}
	return f, nil
}

// defaultsFromSettings maps a stored settings blob onto the file's defaults:.
// build_node is deliberately absent: it is instance-wide and read only at the
// server level, so writing it into an org file would be a key that does
// nothing.
func defaultsFromSettings(s settings.Settings) stackconf.DefaultsConf {
	return stackconf.DefaultsConf{
		CronTimeoutMin:       s.CronTimeoutMin,
		CPULimit:             s.CPULimit,
		MemLimitMB:           s.MemLimitMB,
		RunRetentionDays:     s.RunRetentionDays,
		MetricRetentionHours: s.MetricRetentionHours,
		Protect:              s.Protect,
		ProtectUser:          s.ProtectUser,
		ProtectPassword:      s.ProtectPassword,
		NodeGroup:            s.NodeGroup,
	}
}

// ExportYAML renders it.
func ExportYAML(ctx context.Context, store repo.Store, org *repo.Org) ([]byte, error) {
	f, err := ExportFile(ctx, store, org)
	if err != nil {
		return nil, err
	}
	var buf yamlBuffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.b, nil
}

type yamlBuffer struct{ b []byte }

func (w *yamlBuffer) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

// secretHolding names the org secret whose value is v, "" when none does.
func secretHolding(vars []repo.Variable, v string) string {
	for _, x := range vars {
		if x.Secret && x.Value == v {
			return x.Name
		}
	}
	return ""
}
