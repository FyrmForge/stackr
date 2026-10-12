package worker

import (
	"context"
	"io"
	"log/slog"
	"net"
	"time"
)

// AlivePort is the worker's public liveness port: outside the tunnel, it
// answers AliveReply to anyone and reads nothing, so the controller can tell
// "tunnel down, host alive" from "down". No data, no commands.
const (
	AlivePort  = "7444"
	AliveReply = "stackr-worker\n"
)

// serveAlive answers every connection on addr with AliveReply and closes it.
func serveAlive(ctx context.Context, addr string) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.WriteString(c, AliveReply)
		c.Close()
	}
}

// Alive dials a worker's liveness port and reports whether it answered.
func Alive(ctx context.Context, host string) bool { return aliveAt(ctx, host, AlivePort) }

func aliveAt(ctx context.Context, host, port string) bool {
	d := net.Dialer{Timeout: 3 * time.Second}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	b := make([]byte, len(AliveReply))
	if _, err := io.ReadFull(c, b); err != nil {
		slog.Debug("worker: alive read", "host", host, "err", err)
		return false
	}
	return string(b) == AliveReply
}
