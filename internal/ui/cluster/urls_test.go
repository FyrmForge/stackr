package cluster_test

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/cluster"
)

// An id or tab with URL syntax in it stays one path segment or one value.
func TestNodeURLsEscape(t *testing.T) {
	if got := cluster.NodeDrawer("a/b?c", "x&y"); got != "/-/admin/nodes/a%2Fb%3Fc?tab=x%26y" {
		t.Errorf("NodeDrawer = %q", got)
	}
	if got := cluster.NodePush("a&b", "x&y"); got != "?drawer=node:a%26b&tab=x%26y" {
		t.Errorf("NodePush = %q", got)
	}
}
