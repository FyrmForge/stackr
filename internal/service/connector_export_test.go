package service

import (
	"context"
	"io"
)

// ServerFile is serverFile for the external tests.
func (o *Orchestrator) ServerFile(ctx context.Context, commit string) ([]byte, string, error) {
	return o.serverFile(ctx, commit, io.Discard)
}
