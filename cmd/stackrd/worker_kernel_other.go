//go:build !linux

package main

import "github.com/FyrmForge/stackr/internal/wgapply"

// No kernel WireGuard here: /v1/mesh answers 501.
func kernelWG() wgapply.Applier { return nil }
