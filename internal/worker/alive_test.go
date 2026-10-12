package worker

import (
	"context"
	"net"
	"testing"
	"time"
)

// The liveness listener answers its fixed line; a port that is not it does
// not count as alive.
func TestAlive(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = serveAlive(ctx, addr) }()
	_, port, _ := net.SplitHostPort(addr)
	deadline := time.Now().Add(2 * time.Second)
	for !aliveAt(ctx, "127.0.0.1", port) {
		if time.Now().After(deadline) {
			t.Fatal("listener never answered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	other, _ := net.Listen("tcp", "127.0.0.1:0")
	defer other.Close()
	go func() {
		c, err := other.Accept()
		if err == nil {
			_, _ = c.Write([]byte("HTTP/1.1 400 Bad\r\n"))
			c.Close()
		}
	}()
	_, oport, _ := net.SplitHostPort(other.Addr().String())
	if aliveAt(ctx, "127.0.0.1", oport) {
		t.Fatal("a foreign service counted as alive")
	}
}
