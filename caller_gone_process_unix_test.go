//go:build !windows

package main

import "syscall"

func signalProcessZero(pid int) error {
	return syscall.Kill(pid, 0)
}
