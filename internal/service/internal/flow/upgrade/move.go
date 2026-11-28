package upgrade

import (
	"context"
	"io"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
)

// Move starts the helper that moves the panel database onto a node disk or
// back: the panel's own spec on its own image, a new name (the running panel
// keeps its name until the new one passes), and the move in the helper's
// environment. The pre-move archive is taken first, like an Update's. Returns
// the archive path.
func (f *Flow) Move(ctx context.Context, image string, mv panel.Move, log io.Writer) (string, error) {
	ctx = context.WithoutCancel(ctx) // the request that started this dies with the old container
	f.step(StepArchive)
	archive, err := f.Archive(ctx, log)
	if err != nil {
		return "", err
	}
	spec := f.Spec(image)
	spec.Image = image
	spec.Name = "stackr-" + strings.TrimPrefix(f.Version, "v") + "-m" + mv.MoveID[:min(6, len(mv.MoveID))]
	spec.Env = append(spec.Env, "STACKR_IMAGE="+image)
	f.step(StepSwap)
	if err := f.Panel.LaunchMove(ctx, spec, mv); err != nil {
		return "", err
	}
	return archive, nil
}
