//go:build !windows

package main

import (
	"os"
	"syscall"
)

// terminateSignals is the set of signals that request a graceful stop
// on Unix-like systems.
func terminateSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}
