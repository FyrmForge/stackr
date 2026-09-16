package stackconf

// Exporting live state back to a stack file.
//
// A panel-first user had no way to move to config-as-code except by
// hand-writing the file and hoping it matched. The export is the read side of
// the same serializer the UI-staging path already uses (StateToResolved), so
// exporting a stack and then planning that file against the same stack is an
// empty diff. That round trip is the only definition of "correct" that matters
// here, and it is what TestExportRoundTrips checks.
//
// Secrets are declarations only: names and options, never values. The file is
// meant to go into git.

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ExportFile turns a Resolved into the file that would produce it.
func ExportFile(r *Resolved) *File {
	// moved: is deliberately absent: a marker is an instruction about a rename
	// that has already happened, so exporting live state has nothing to say
	// about one.
	f := &File{Version: 1, Stack: r.Stack, UIEdits: r.UIEdits, Domains: r.Domains,
		Vars: r.Vars, Defaults: r.Defaults, PREnvs: r.PREnvs, Proxy: ProxyConf{Middlewares: r.Middlewares}}
	// Shared instances live in the home environment, which is not a rung and
	// so is never written as one.
	if home, ok := r.Envs[repo.HomeSlug]; ok && len(home.Tiles) > 0 {
		f.Shared = map[string]RawMap{}
		for name, tc := range home.Tiles {
			f.Shared[name] = tileRaw(tc)
		}
	}
	f.Environments = EnvsNode{Envs: map[string]EnvConf{}}
	declared := SecretsNode{}
	for _, envName := range r.EnvOrder {
		re := r.Envs[envName]
		ec := EnvConf{Protected: re.Protected, Color: re.Color, ApplyPolicy: re.ApplyPolicy,
			Defaults: re.Defaults, Vars: re.Vars, Tiles: map[string]TileNode{}}
		for name, tc := range re.Tiles {
			ec.Tiles[name] = TileNode{Raw: tileRaw(tc)}
		}
		f.Environments.Order = append(f.Environments.Order, envName)
		f.Environments.Envs[envName] = ec
		// Secrets are declared once at stack level: they are the same
		// declaration in every env, and repeating them per env would read as
		// four different secrets.
		for name, sc := range re.Secrets {
			declared[name] = sc
		}
	}
	if len(declared) > 0 {
		f.Secrets = declared
	}
	return f
}

// tileRaw renders one tile as the map the file writes. Round-tripped through
// YAML rather than reflected over: TileConf's yaml tags are the file grammar,
// so going through them is what keeps export and parse from drifting.
func tileRaw(tc TileConf) RawMap {
	b, err := yaml.Marshal(tc)
	if err != nil {
		return RawMap{}
	}
	var out RawMap
	if err := yaml.Unmarshal(b, &out); err != nil {
		return RawMap{}
	}
	// TileConf's yaml tags carry no omitempty, so a straight marshal writes
	// every field, including `volumes: []` for a tile that has none. Dropping
	// the keys that match a zero tile leaves the file a person would write,
	// and re-parses identically: an absent key and a zero value are the same
	// thing to the decoder.
	for k, v := range out {
		if z, ok := zeroTile[k]; ok && sameYAML(v, z) {
			delete(out, k)
		}
	}
	// type: service is the default, so writing it everywhere is noise.
	if out["type"] == "service" {
		delete(out, "type")
	}
	return out
}

// zeroTile is the rendering of a tile that sets nothing, computed once.
var zeroTile = func() RawMap {
	b, err := yaml.Marshal(TileConf{})
	if err != nil {
		return RawMap{}
	}
	var out RawMap
	_ = yaml.Unmarshal(b, &out)
	return out
}()

// sameYAML compares two decoded values by their rendering, which is exact for
// the shapes a tile body holds and needs no reflection over them.
func sameYAML(a, b any) bool {
	ab, err1 := yaml.Marshal(a)
	bb, err2 := yaml.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

// ExportYAML renders the file. Two spaces, because that is what every example
// in the docs uses and a file a person will edit should look like the ones
// they learned from.
func ExportYAML(r *Resolved) ([]byte, error) {
	var buf yamlBuffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(ExportFile(r)); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.b, nil
}

// yamlBuffer is a bytes.Buffer by another name; the encoder wants an io.Writer
// and this keeps the import list to what the file actually uses.
type yamlBuffer struct{ b []byte }

func (w *yamlBuffer) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

// MarshalYAML writes environments back in the map form, in ladder order. The
// list form is lossy the moment an environment carries anything of its own,
// and the order is the ladder, so it is never sorted.
func (e EnvsNode) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.MappingNode}
	for _, name := range e.Order {
		key := &yaml.Node{Kind: yaml.ScalarNode, Value: name}
		val := &yaml.Node{}
		if err := val.Encode(e.Envs[name]); err != nil {
			return nil, err
		}
		n.Content = append(n.Content, key, val)
	}
	return n, nil
}

// MarshalYAML writes a tile body, or `false` for an exclusion.
func (t TileNode) MarshalYAML() (any, error) {
	if t.Excluded {
		return false, nil
	}
	return t.Raw, nil
}

// MarshalYAML keeps declared secrets in a stable order. A bare declaration
// writes as a null body, which is how the grammar spells "set out of band".
func (sn SecretsNode) MarshalYAML() (any, error) {
	names := make([]string, 0, len(sn))
	for name := range sn {
		names = append(names, name)
	}
	sort.Strings(names)
	n := &yaml.Node{Kind: yaml.MappingNode}
	for _, name := range names {
		key := &yaml.Node{Kind: yaml.ScalarNode, Value: name}
		val := &yaml.Node{}
		sc := sn[name]
		if sc == (SecretConf{}) {
			val.Kind, val.Tag, val.Value = yaml.ScalarNode, "!!null", ""
		} else if err := val.Encode(sc); err != nil {
			return nil, fmt.Errorf("secret %s: %w", name, err)
		}
		n.Content = append(n.Content, key, val)
	}
	return n, nil
}
