package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// ensureRegistry creates the head's image registry, or replaces one made from
// another spec. Its images live on the named volume, so replacing loses none.
func (r runner) ensureRegistry(ctx context.Context, dataDir string) error {
	// A unix socket path holds at most 107 bytes.
	if sock := installspec.RegistrySocket(dataDir); len(sock) > 107 {
		return fmt.Errorf("the data dir path is too long: the registry socket %s needs 107 bytes or fewer", sock)
	}
	want := installspec.Registry(dataDir)
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
	if err := os.MkdirAll(installspec.RegistryDir(dataDir), 0o700); err != nil {
		return err
	}
	return r.docker(ctx, want.RunArgs()...) // docker run makes the named volume
}
