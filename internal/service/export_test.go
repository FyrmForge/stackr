package service

import "github.com/FyrmForge/stackr/internal/worker"

// NoHostInterfaces makes the fake controller report no interfaces, so a
// test never sees the machine it runs on.
func NoHostInterfaces() (restore func()) {
	old := hostInterfaces
	hostInterfaces = func() []worker.Iface { return nil }
	return func() { hostInterfaces = old }
}
