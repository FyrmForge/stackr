package worker

import "syscall"

// freebind lets the listener take the node's tunnel address before wg0 has
// it: the worker brings wg0 up from its saved mesh spec after it listens.
func freebind(_, _ string, c syscall.RawConn) error {
	var err error
	if cerr := c.Control(func(fd uintptr) {
		err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_FREEBIND, 1)
	}); cerr != nil {
		return cerr
	}
	return err
}
