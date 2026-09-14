// Command stackr is the command-line client for a stackr server. The whole
// command surface lives in internal/cli/cmd; this is only the entry point.
package main

import (
	"os"

	"github.com/FyrmForge/stackr/internal/cli/cmd"
)

func main() { os.Exit(cmd.Execute()) }
