// Package panel is stackrd's own container: find it, pull a new image, hand
// the swap to a one-shot helper, and the swap itself (run by that helper,
// since a process cannot outlive stopping its own container).
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// Docker is the slice of the daemon this leaf uses.
type Docker interface {
	Run(ctx context.Context, spec docker.ContainerSpec) (string, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	StopRemove(ctx context.Context, id string) error
	List(ctx context.Context, labels map[string]string) ([]docker.Container, error)
	Inspect(ctx context.Context, id string) (docker.Detail, error)
	Pull(ctx context.Context, ref, auth string, log io.Writer) error
	LocalDigest(ctx context.Context, ref string) (string, error)
}

const (
	LabelRole = "stackr.role"
	// SpecEnv carries the new panel's spec into the helper, as JSON.
	SpecEnv = "STACKR_SWAP_SPEC"
)

type Leaf struct {
	docker Docker
	// Gate for the new panel: migrations may run before it answers.
	Poll, Grace, Deadline time.Duration
}

func New(d Docker) *Leaf {
	return &Leaf{
		docker:   d,
		Poll:     time.Second,
		Grace:    10 * time.Second,
		Deadline: 3 * time.Minute,
	}
}

// Pull fetches a panel image (public, anonymous). One already here is
// kept: a release tag never changes, and a box can be fed images by hand.
func (l *Leaf) Pull(ctx context.Context, image string, log io.Writer) error {
	if _, err := l.docker.LocalDigest(ctx, image); err == nil {
		return nil
	}
	return l.docker.Pull(ctx, image, "", log)
}

// Launch starts the helper that swaps the panel to spec: the new image
// running `stackrd upgrade-swap` with the socket. Refused while a helper
// still runs, which is the "already running" guard across panel restarts.
func (l *Leaf) Launch(ctx context.Context, spec docker.ContainerSpec) error {
	busy, err := l.docker.List(ctx, map[string]string{LabelRole: "upgrader"})
	if err != nil {
		return err
	}
	for _, c := range busy {
		if c.State == "running" {
			return errs.Conflictf("an upgrade is already running")
		}
		_ = l.docker.StopRemove(ctx, c.ID) // a finished helper from last time
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	_, err = l.docker.Run(ctx, docker.ContainerSpec{
		// The image's entrypoint is /stackrd; Cmd is only its argument.
		Name:    "stackr-upgrader",
		Image:   spec.Image,
		Cmd:     []string{"upgrade-swap"},
		Env:     []string{SpecEnv + "=" + string(body)},
		Labels:  map[string]string{LabelRole: "upgrader"},
		Volumes: []string{"/var/run/docker.sock:/var/run/docker.sock"},
	})
	return err
}

// SpecFromEnv reads the helper's spec back.
func SpecFromEnv() (docker.ContainerSpec, error) {
	var s docker.ContainerSpec
	raw := os.Getenv(SpecEnv)
	if raw == "" {
		return s, errors.New(SpecEnv + " is not set")
	}
	return s, json.Unmarshal([]byte(raw), &s)
}

// Swap replaces the running panel with spec: stop the old one (kept), run
// the new one, gate it like a tile; pass removes the old, fail removes the
// new and starts the old again. Plain containers have no Swarm to roll back
// a failed swap, so this is that rollback.
func (l *Leaf) Swap(ctx context.Context, spec docker.ContainerSpec) error {
	old, err := l.docker.List(ctx, map[string]string{LabelRole: "panel"})
	if err != nil {
		return err
	}
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	spec.Labels[LabelRole] = "panel"
	for _, c := range old {
		if err := l.docker.Stop(ctx, c.ID); err != nil {
			return fmt.Errorf("stop old panel: %w", err)
		}
	}
	back := func(cause error) error {
		for _, c := range old {
			if err := l.docker.Start(ctx, c.ID); err != nil {
				return errors.Join(cause, fmt.Errorf("start old panel: %w", err))
			}
		}
		return cause
	}
	id, err := l.docker.Run(ctx, spec)
	if err != nil {
		return back(fmt.Errorf("run new panel: %w", err))
	}
	if err := docker.Healthy(ctx, l.docker.Inspect, id, l.Poll, l.Grace, l.Deadline); err != nil {
		_ = l.docker.StopRemove(ctx, id)
		return back(fmt.Errorf("new panel: %w", err))
	}
	for _, c := range old {
		_ = l.docker.StopRemove(ctx, c.ID)
	}
	return nil
}
