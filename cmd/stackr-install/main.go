// Command stackr-install installs stackr on a fresh Docker host. Everything
// lives in internal/installer; this is only the entry point.
package main

import (
	"os"

	"github.com/FyrmForge/stackr/internal/installer"
)

func main() { os.Exit(installer.Main(os.Args[1:])) }
