package upgrade

import (
	"context"
	"fmt"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// EnsureRegistry creates the head's image registry when the box has none (an
// install from before it) and recreates it when its spec stamp differs.
// Images live on the named volume, so recreating loses none.
func (f *Flow) EnsureRegistry(ctx context.Context) error {
	if f.Docker == nil {
		return nil
	}
	want := installspec.Registry()
	cs, err := f.Docker.List(ctx, map[string]string{installspec.LabelRole: "registry"})
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.Labels[installspec.LabelSpec] == want.Labels[installspec.LabelSpec] {
			return nil
		}
		if err := f.Docker.StopRemove(ctx, c.ID); err != nil {
			return fmt.Errorf("remove old registry: %w", err)
		}
	}
	ports := map[string]string{}
	for _, p := range want.Ports { // "127.0.0.1:5000:5000" = host key "127.0.0.1:5000", container "5000"
		i := strings.LastIndex(p, ":")
		ports[p[:i]] = p[i+1:]
	}
	_, err = f.Docker.Run(ctx, docker.ContainerSpec{
		Name: want.Name, Image: want.Image, Cmd: want.Cmd, Env: want.Env, Labels: want.Labels,
		Volumes: want.Volumes, Ports: ports, Restart: want.Restart,
	})
	return err
}
