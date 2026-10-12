package worker

import (
	"context"
	"net"
	"testing"
)

// The worker binds its tunnel address before stackr0 carries it.
func TestListenAddressNotYetAssigned(t *testing.T) {
	ln, err := (&net.ListenConfig{Control: freebind}).Listen(context.Background(), "tcp", "10.207.255.250:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
}
