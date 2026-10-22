package upgrade

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// ObserveOff: no observe container on this box (dev, tests), so ensure and
// remove do nothing.
func (f *Flow) ObserveOff() bool {
	return f.Docker == nil || f.Panel == nil || f.DataDir == "" || f.Version == "" || f.Version == "dev"
}

// EnsureObserve runs stackr-observe at this build's version with the given
// knobs: created when missing, recreated when its
// spec stamp differs (a new version or a changed knob). The data lives on
// the named volume, so a recreate keeps it.
func (f *Flow) EnsureObserve(ctx context.Context, o installspec.ObserveOpts) error {
	if f.ObserveOff() {
		return nil
	}
	want := installspec.Observe(f.Version, f.DataDir, o)
	cs, err := f.Docker.List(ctx, map[string]string{installspec.LabelRole: "observe"})
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.Labels[installspec.LabelSpec] == want.Labels[installspec.LabelSpec] {
			if c.State == "running" {
				return nil
			}
			return f.Docker.Start(ctx, c.ID)
		}
	}
	// Pull before the old one goes: a failed pull keeps it running.
	if err := f.Panel.Pull(ctx, want.Image, io.Discard); err != nil {
		return fmt.Errorf("pull %s: %w", want.Image, err)
	}
	if err := f.RemoveObserve(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(installspec.ObserveDir(f.DataDir), 0o700); err != nil {
		return err
	}
	_, err = f.Docker.Run(ctx, docker.ContainerSpec{
		Name: want.Name, Image: want.Image, Cmd: want.Cmd, Env: want.Env, Labels: want.Labels,
		Volumes: want.Volumes, Networks: []docker.NetAttach{{Name: want.Network}}, Restart: want.Restart,
		MemLimitMB: want.MemoryMB,
	})
	return err
}

// RemoveObserve stops and removes stackr-observe; its volume stays, so
// turning it back on keeps the history.
func (f *Flow) RemoveObserve(ctx context.Context) error {
	if f.ObserveOff() {
		return nil
	}
	cs, err := f.Docker.List(ctx, map[string]string{installspec.LabelRole: "observe"})
	if err != nil {
		return err
	}
	for _, c := range cs {
		if err := f.Docker.StopRemove(ctx, c.ID); err != nil {
			return fmt.Errorf("remove stackr-observe: %w", err)
		}
	}
	return nil
}

// ObserveState is the observe container's docker state, "" when there is none.
func (f *Flow) ObserveState(ctx context.Context) (string, error) {
	if f.ObserveOff() {
		return "", nil
	}
	cs, err := f.Docker.List(ctx, map[string]string{installspec.LabelRole: "observe"})
	if err != nil || len(cs) == 0 {
		return "", err
	}
	return cs[0].State, nil
}
