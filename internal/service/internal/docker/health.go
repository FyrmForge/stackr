package docker

import (
	"context"
	"errors"
	"time"
)

// Healthy is the one health gate, for tiles and the panel alike. It reads
// two fields off one inspect: healthy (or running past grace when the
// container has no HEALTHCHECK, the image's own included) AND restart count
// still zero. A crash loop is "running" between crashes.
func Healthy(
	ctx context.Context,
	inspect func(context.Context, string) (Detail, error),
	id string,
	poll, grace, deadline time.Duration,
) error {
	start := time.Now()
	end := start.Add(deadline)
	for {
		d, err := inspect(ctx, id)
		switch {
		case err != nil:
			return err
		case d.RestartCount > 0:
			return errors.New("the container restarted during the health check")
		case !d.Running:
			return errors.New("the container exited during the health check")
		case d.Health == "unhealthy":
			return errors.New("the container reported unhealthy")
		case d.Health == "healthy":
			return nil
		case d.Health == "" && time.Since(start) >= grace:
			return nil
		case time.Now().After(end):
			return errors.New("the container was not healthy before the deadline")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
