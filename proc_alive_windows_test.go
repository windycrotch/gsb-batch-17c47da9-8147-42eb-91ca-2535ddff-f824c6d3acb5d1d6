//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

// processAlive reports whether a process with the given PID is still running.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH").Output()
	if err != nil {
		// If we cannot determine liveness, do not claim the process is dead.
		return true
	}
	return strings.Contains(string(out), strconv.Itoa(pid))
}
