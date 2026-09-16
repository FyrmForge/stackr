package orgconf

// The org file's storage: block, the org's network shares. An org never
// touches a host's filesystem, so only smb and nfs are allowed; local pools
// stay per server. Tiles in the org mount any sub-path of a share through
// ${{ org.storage.NAME }} (storagetiles.Resolve).

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/storagetiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// StorageConf is one declared share.
type StorageConf struct {
	Backend string `yaml:"backend"` // smb | nfs
	Address string `yaml:"address"`
	// Export is the smb share name or the nfs exported directory.
	Export string `yaml:"export"`
	// Username and Password are literals or ${{ org.vars|secrets.NAME }}.
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
	// Path is an optional root inside the export.
	Path string `yaml:"path,omitempty"`
	Opts string `yaml:"opts,omitempty"`
}

var refRe = regexp.MustCompile(`\$\{\{([^}]*)\}\}`)

func validateStorage(m map[string]StorageConf) error {
	for name, sc := range m {
		if name != repo.Slugify(name) {
			return fmt.Errorf("storage %s: name must be lowercase letters, digits and dashes", name)
		}
		switch sc.Backend {
		case "smb", "nfs":
		case "local":
			return fmt.Errorf("storage %s: an org share is smb or nfs; local pools belong to a server", name)
		default:
			return fmt.Errorf("storage %s: backend must be smb or nfs", name)
		}
		if sc.Address == "" || sc.Export == "" {
			return fmt.Errorf("storage %s: address and export are required", name)
		}
		for _, v := range []string{sc.Username, sc.Password} {
			for _, body := range varref.Refs(v) {
				r, err := varref.Parse(body)
				if err != nil {
					return fmt.Errorf("storage %s: %w", name, err)
				}
				if r.Scope != "org" || (r.Slug != varref.BucketVars && r.Slug != varref.BucketSecrets) {
					return fmt.Errorf("storage %s: credentials take ${{ org.vars.NAME }} or ${{ org.secrets.NAME }}, not %s", name, r.Source)
				}
			}
		}
	}
	return nil
}

// want is the row a declared share becomes. missing names the org values its
// credentials reference that are not set yet.
func (sc StorageConf) want(orgVars []repo.Variable) (st repo.Storage, missing []string) {
	vals := map[string]string{}
	for _, v := range orgVars {
		vals[v.Name] = v.Value
	}
	expand := func(s string) string {
		return refRe.ReplaceAllStringFunc(s, func(m string) string {
			r, err := varref.Parse(refRe.FindStringSubmatch(m)[1])
			if err != nil {
				return "" // validateStorage already refused it
			}
			v, ok := vals[r.Name]
			if !ok {
				missing = append(missing, r.Name)
			}
			return v
		})
	}
	export := sc.Export
	if sc.Path != "" {
		export = strings.TrimSuffix(export, "/") + path.Clean("/"+sc.Path)
	}
	return repo.Storage{Backend: sc.Backend, Address: sc.Address, Export: export,
		Username: expand(sc.Username), Password: expand(sc.Password), Opts: sc.Opts}, missing
}

// orgShares is the org's share rows by slug.
func (r Runner) orgShares(ctx context.Context, org *repo.Org) (map[string]repo.Storage, error) {
	all, err := r.Store.ListStorage(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]repo.Storage{}
	for _, s := range all {
		if s.OrgID == org.ID {
			out[s.Slug] = s
		}
	}
	return out, nil
}

// shareUsers maps a share name to the tiles in the org whose storage: lines
// mount it.
func (r Runner) shareUsers(ctx context.Context, org *repo.Org) (map[string][]string, error) {
	stacks, err := r.Store.ListStacksByOrg(ctx, org.ID)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, s := range stacks {
		ts, err := r.Store.ListTilesByStack(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			for _, l := range strings.Split(t.Storage, "\n") {
				if name := varref.OrgStorageRef(l); name != "" {
					out[name] = append(out[name], s.Slug+"/"+t.Slug)
				}
			}
		}
	}
	return out, nil
}

func sameShare(cur, want repo.Storage) bool {
	return cur.Backend == want.Backend && cur.Address == want.Address && cur.Export == want.Export &&
		cur.Username == want.Username && cur.Password == want.Password && cur.Opts == want.Opts
}

func (r Runner) diffStorage(ctx context.Context, org *repo.Org, f *File, p *stackconf.Plan) error {
	cur, err := r.orgShares(ctx, org)
	if err != nil {
		return err
	}
	vars, err := r.Store.ListVariables(ctx, repo.OwnerOrg, org.ID)
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(f.Storage) {
		want, missing := f.Storage[name].want(vars)
		if len(missing) > 0 {
			p.Warnings = append(p.Warnings, "storage "+name+": waiting for org value "+strings.Join(missing, ", "))
		}
		c, ok := cur[name]
		if !ok {
			p.Changes = append(p.Changes, stackconf.Change{Kind: "create", Env: "org", Field: "storage", New: name,
				Note: "tiles in the org mount it as ${{ org.storage." + name + " }}"})
			continue
		}
		if sameShare(c, want) {
			continue
		}
		old, new_ := c.Backend+"://"+c.Address+"/"+c.Export, want.Backend+"://"+want.Address+"/"+want.Export
		if c.Username != want.Username || c.Password != want.Password {
			new_ += " (new credentials)"
		}
		if c.Opts != want.Opts {
			old, new_ = old+" "+c.Opts, new_+" "+want.Opts
		}
		p.Changes = append(p.Changes, stackconf.Change{Kind: "update", Env: "org", Field: "storage " + name, Old: old, New: new_,
			Note: "recreates the share's volumes; stop the tiles that mount it first"})
	}
	users, err := r.shareUsers(ctx, org)
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(cur) {
		if _, ok := f.Storage[name]; ok {
			continue
		}
		if u := users[name]; len(u) > 0 {
			sort.Strings(u)
			p.Errors = append(p.Errors, "storage "+name+" is still mounted by "+strings.Join(u, ", ")+"; drop those lines first")
			continue
		}
		p.Changes = append(p.Changes, stackconf.Change{Kind: "delete", Env: "org", Field: "storage", Old: name})
	}
	return nil
}

// applyStorage runs before the stacks so their tiles find the shares.
func (r Runner) applyStorage(ctx context.Context, org *repo.Org, f *File, failed *[]string) error {
	cur, err := r.orgShares(ctx, org)
	if err != nil {
		return err
	}
	vars, err := r.Store.ListVariables(ctx, repo.OwnerOrg, org.ID)
	if err != nil {
		return err
	}
	clus := r.Applier.Ops.Cluster
	for _, name := range sortedKeys(f.Storage) {
		want, missing := f.Storage[name].want(vars)
		if len(missing) > 0 {
			*failed = append(*failed, fmt.Sprintf("storage %s: org value %s is not set", name, strings.Join(missing, ", ")))
			continue
		}
		c, ok := cur[name]
		if !ok {
			want.ID, want.OrgID, want.Name, want.Slug = uuid.New().String(), org.ID, name, name
			want.Status, want.CreatedAt = "unknown", time.Now().UTC()
			if err := r.Store.CreateStorage(ctx, &want); err != nil {
				return err
			}
			continue
		}
		if sameShare(c, want) {
			continue
		}
		if clus != nil {
			if err := storagetiles.DropOrgShareVolumes(ctx, clus, &c); err != nil {
				*failed = append(*failed, err.Error())
				continue
			}
		}
		c.Backend, c.Address, c.Export, c.Username, c.Password, c.Opts =
			want.Backend, want.Address, want.Export, want.Username, want.Password, want.Opts
		if err := r.Store.UpdateStorage(ctx, &c); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(cur) {
		if _, ok := f.Storage[name]; ok {
			continue
		}
		c := cur[name]
		if clus != nil {
			if err := storagetiles.DropOrgShareVolumes(ctx, clus, &c); err != nil {
				*failed = append(*failed, err.Error())
				continue
			}
		}
		if err := r.Store.DeleteStorage(ctx, c.ID); err != nil {
			return err
		}
	}
	return nil
}
