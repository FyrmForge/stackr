package orgconfig

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

// Export is live written as the org file: the org's params (a secret by
// name and type only, the file goes in git), defaults, env colours, the
// stacks bound to a repo (a stack with no file has nothing to point at)
// the org's domains and its network shares. Diffing it against the same live is clean. Block
// style, v0's two-space indent.
func Export(live Live) ([]byte, error) {
	og := live.Org
	f := File{
		Version: 1,
		Org:     og.Name,
		Stacks:  map[string]StackRef{},
	}
	exportParams(&f, live)
	s, err := settings.Parse(og.Settings)
	if err != nil {
		return nil, err
	}
	if d := Defaults(s); d != (Defaults{}) {
		if pw := d.ProtectPassword; pw != nil && !planfile.IsRef(*pw) {
			d.ProtectUser, d.ProtectPassword = nil, nil
		}
		if d != (Defaults{}) {
			f.Defaults = &d
		}
	}
	_ = json.Unmarshal([]byte(og.EnvColors), &f.EnvColors)
	for _, sl := range live.Stacks {
		st := sl.Stack
		if st.ConfigRepo == "" {
			continue
		}
		f.Stacks[st.Slug] = StackRef{
			Repo:      st.ConfigRepo,
			Branch:    st.ConfigBranch,
			Path:      st.ConfigPath,
			Connector: st.ConfigConnectorID,
		}
	}
	for _, d := range live.Domains {
		if d.Level != domainres.Org || d.OrgID == nil || *d.OrgID != og.ID {
			continue
		}
		f.Domains = append(f.Domains, Reservation{
			Host:                d.Host,
			ACMEEmail:           d.ACMEEmail,
			IncludeEnvOnDefault: d.IncludeEnvOnDefault,
		})
	}
	for _, sh := range live.Shares {
		if f.Shares == nil {
			f.Shares = map[string]Share{}
		}
		f.Shares[sh.Slug] = Share{
			Kind:     sh.Kind,
			Source:   sh.Source,
			Options:  sh.Options,
			User:     sh.User,
			Password: sh.PasswordRef,
		}
	}
	slices.SortFunc(f.Domains, func(a, b Reservation) int { return strings.Compare(a.Host, b.Host) })
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	err = enc.Close()
	return buf.Bytes(), err
}

// exportParams writes the ladder and the param blocks: tier slugs and pr, or
// all and pr without tiers. Envs with the same block share an a|b key and a
// secret is name and type only.
func exportParams(f *File, live Live) {
	blocks := map[string]map[string]planfile.Entry{}
	read := func(env string, vals map[string]params.Value) {
		if len(vals) == 0 {
			return
		}
		blocks[env] = map[string]planfile.Entry{}
		for key, v := range vals {
			e := planfile.Entry{Secret: v.Secret}
			if !v.Secret {
				e.Value = v.V
			}
			blocks[env][key] = e
		}
	}
	if len(live.Tiers) == 0 {
		read(All, live.Params)
	}
	for _, t := range live.Tiers {
		f.Tiers = append(f.Tiers, Tier{Slug: t.Slug, Locked: t.Locked})
		read(t.Slug, live.TierParams[t.Slug])
	}
	read(planfile.PR, live.PRParams)
	order := append(f.Tiers.Slugs(), planfile.PR)
	if len(f.Tiers) == 0 {
		order = []string{All, planfile.PR}
	}
	if g := planfile.Collapse(blocks, order); len(g) > 0 {
		f.Params.Tiered = g
	}
}
