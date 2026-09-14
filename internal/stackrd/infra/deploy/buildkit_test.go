package deploy

import "testing"

// buildkitd authenticates nothing: it builds whatever it is handed. Publishing
// its port put arbitrary code execution on every swarm node
// (docs/plans/39-codex-review-fixes.md, point 1). It has to stay on the
// overlay, reachable only by the alias the panel dials.
func TestBuildkitIsNotPublished(t *testing.T) {
	spec := buildkitSpec("node-1")
	if len(spec.Ports) != 0 || spec.HostPorts {
		t.Fatalf("buildkit must publish no port, got ports=%v hostPorts=%v", spec.Ports, spec.HostPorts)
	}
	if len(spec.Networks) != 1 || len(spec.Networks[0].Aliases) == 0 ||
		spec.Networks[0].Aliases[0] != buildkitAlias {
		t.Fatalf("buildkit needs the %q alias on the overlay, got %+v", buildkitAlias, spec.Networks)
	}
}
