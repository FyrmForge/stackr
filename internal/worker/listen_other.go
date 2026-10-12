//go:build !linux

package worker

import "syscall"

func freebind(_, _ string, _ syscall.RawConn) error { return nil }
