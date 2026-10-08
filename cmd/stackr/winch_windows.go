//go:build windows

package main

import "context"

// watchWinch: Windows has no SIGWINCH; the size is sent once at start.
func watchWinch(context.Context, func()) {}
