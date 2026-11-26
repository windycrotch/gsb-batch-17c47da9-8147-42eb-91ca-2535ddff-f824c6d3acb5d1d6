//go:build !windows

package main

import "syscall"

// processAlive reports whether a process with the given pid exists.
// On Unix, sending signal 0 performs the existence check without
// delivering any signal.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
