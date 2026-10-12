package main

import (
	"context"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// ensureRegistry creates the head's image registry, or replaces one made from
// another spec. Its images live on the named volume, so replacing loses none.
func (r runner) ensureRegistry(ctx context.Context) error {
	want := installspec.Registry()
	if r.exists(ctx, "container", installspec.RegistryName) {
		have, _ := read(ctx, "docker", "inspect", "-f", `{{index .Config.Labels "`+installspec.LabelSpec+`"}}`, installspec.RegistryName)
		if strings.TrimSpace(have) == want.Labels[installspec.LabelSpec] {
			r.say("  have", installspec.RegistryName)
			return nil
		}
		r.say("  replacing", installspec.RegistryName, "(its spec changed)")
		if err := r.docker(ctx, "rm", "-f", installspec.RegistryName); err != nil {
			return err
		}
	}
	return r.docker(ctx, want.RunArgs()...) // docker run makes the named volume r.docker(ctx, want.RunArgs()...)
}
