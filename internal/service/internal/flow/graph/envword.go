package graph

import "context"

// EnvWord is an env's worst-of tile word (the stack canvas's env card
// status): "" when the env has no tiles.
func (f *Flow) EnvWord(ctx context.Context, envID string) (string, error) {
	ts, err := f.Tiles.List(ctx, envID)
	if err != nil {
		return "", err
	}
	return f.worstOf(ctx, "", ts)
}
