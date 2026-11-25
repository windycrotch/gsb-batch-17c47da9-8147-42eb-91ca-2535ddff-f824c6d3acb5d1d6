//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"syscall"
)

// The helper ships for macOS; the Windows port exists so the process
// lifecycle — and with it the acceptance suite that launches real helper
// processes — runs on a Windows dev box. Windows has no SIGTERM: "graceful"
// stop is the ownership poll in main.go noticing the pidfile is gone, and
// termination is a forced tree kill (taskkill /t also gets ffmpeg's
// children).

func setProcAttr(cmd *exec.Cmd) {}

func terminateJob(cmd *exec.Cmd) { killTree(cmd.Process.Pid) }

func killJob(cmd *exec.Cmd) { killTree(cmd.Process.Pid) }

func killTree(pid int) {
	exec.Command("taskkill", "/pid", strconv.Itoa(pid), "/t", "/f").Run()
}

func processAlive(pid int) bool {
	const (
		processQueryLimitedInformation = 0x1000
		stillActive                    = 259
	)
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	// The process object outlives the process itself while anyone holds a
	// handle to it, so an open handle alone does not mean "alive".
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// terminatePid is a no-op on Windows: the previous helper notices its lost
// ownership through the pidfile poll and exits on its own; forceKillPid is
// the escalation when it doesn't.
func terminatePid(pid int) {}

func forceKillPid(pid int) { killTree(pid) }
