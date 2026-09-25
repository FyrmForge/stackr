//go:build integration

package registry

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// Against Docker Hub: one public image.
func TestDockerHub(t *testing.T) {
	ctx := context.Background()
	dg, err := Digest(ctx, "alpine:3", "", "")
	if err != nil || !strings.HasPrefix(dg, "sha256:") {
		t.Fatalf("digest: %q %v", dg, err)
	}
	tags, err := Tags(ctx, "alpine", "", "")
	if err != nil || !slices.Contains(tags, "3") {
		t.Fatalf("tags: %d %v", len(tags), err)
	}
}
