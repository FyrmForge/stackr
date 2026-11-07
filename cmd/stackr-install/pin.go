package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// restorePin is the digest of the image a restore of an archive from build ver
// pulls. This installer's own release uses its embedded digest (pinFor); an
// older or newer one takes the digest asset of that release, so a fresh
// install of the current release can still restore last month's backup. A
// build with no digest of its own pulls by tag, as everywhere.
func restorePin(ctx context.Context, ver, own, embedded, format string) (string, error) {
	if embedded == "" || strings.TrimPrefix(ver, "v") == strings.TrimPrefix(own, "v") {
		return pinFor(ver, own, embedded)
	}
	v := strings.TrimPrefix(ver, "v")
	dg, err := installspec.ReleaseDigest(ctx, format, "v"+v)
	if errors.Is(err, installspec.ErrNoDigest) {
		return "", fmt.Errorf("release v%s declares no image digest; pass --image to pull what you name", v)
	}
	if err != nil {
		return "", fmt.Errorf("read the image digest of v%s (pass --image to skip): %w", v, err)
	}
	return dg, nil
}
