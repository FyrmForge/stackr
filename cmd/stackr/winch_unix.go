//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// watchWinch calls f on every window resize until ctx ends.
func watchWinch(ctx context.Context, f func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ch:
				f()
			case <-ctx.Done():
				return
			}
		}
	}()
}
