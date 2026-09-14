package stackconf

import (
	"fmt"
	"sort"

	yaml "go.yaml.in/yaml/v3"
)

// FileList is the files: key, repo files shipped into the container. YAML
// accepts the map form (repo/path: /container/path[:template]) or a plain
// list of "repo/path:/container/path[:template]" lines; internally it is
// always the line form, sorted for the map case so rows and diffs stay
// canonical.
type FileList []string

func (f *FileList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.MappingNode:
		var m map[string]string
		if err := n.Decode(&m); err != nil {
			return err
		}
		lines := make([]string, 0, len(m))
		for k, v := range m {
			lines = append(lines, k+":"+v)
		}
		sort.Strings(lines)
		*f = lines
	case yaml.SequenceNode:
		var s []string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*f = s
	default:
		return fmt.Errorf("files: must be a map (repo/path: /container/path) or a list of lines")
	}
	return nil
}
