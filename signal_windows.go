//go:build windows

package main

import "os"

// terminateSignals is the set of signals that request a graceful stop
// on Windows. Go does not support SIGTERM delivery to Windows processes.
func terminateSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
