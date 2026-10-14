package upgrade

import (
	"context"
	"fmt"

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
	_, err = f.Docker.Run(ctx, docker.ContainerSpec{
		Name: want.Name, Image: want.Image, Cmd: want.Cmd, Env: want.Env, Labels: want.Labels,
		Volumes: want.Volumes, HostNetwork: want.HostNetwork, Restart: want.Restart,
	})
	return err
}
