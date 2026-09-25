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
)

// Export is live written as the org file: the org's params (a secret by
// name and type only, the file goes in git), defaults, env colours, the
// stacks bound to a repo (a stack with no file has nothing to point at)
// and the org's domains. Diffing it against the same live is clean. Block
// style, v0's two-space indent.
func Export(live Live) ([]byte, error) {
	og := live.Org
	f := File{
		Version: 1,
		Org:     og.Name,
		Params:  map[string]map[string]Param{},
		Stacks:  map[string]StackRef{},
	}
	for key, v := range live.Params {
		c, n, _ := strings.Cut(key, ".")
		if f.Params[c] == nil {
			f.Params[c] = map[string]Param{}
		}
		decl := Param{Type: params.Secret}
		if !v.Secret {
			decl = Param{
				Type:  params.Param,
				Value: &v.V,
			}
		}
		f.Params[c][n] = decl
	}
	s, err := settings.Parse(og.Settings)
	if err != nil {
		return nil, err
	}
	if d := Defaults(s); d != (Defaults{}) {
		f.Defaults = &d
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
