package worker

import (
	"bytes"
	"context"
	"os/exec"

	"github.com/FyrmForge/stackr/internal/wgapply"
)

// ExecRunner runs real commands. `wg set` is done in-process over netlink:
// hosts have no wireguard-tools (join never installs them), and the exit
// link plans still speak wg's argv.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	if name == "wg" && len(args) > 0 && args[0] == "set" {
		return nil, wgapply.Set(args[1:])
	}
	c := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	return c.CombinedOutput()
}
