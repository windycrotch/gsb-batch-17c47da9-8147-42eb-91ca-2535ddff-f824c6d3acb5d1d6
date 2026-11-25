//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setProcAttr puts ffmpeg in its own process group so stop can signal the
// whole group: the remux must not outlive the job.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateJob asks ffmpeg's process group to shut down. ffmpeg honours
// SIGTERM within a fraction of a second.
func terminateJob(cmd *exec.Cmd) {
	syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// killJob force-kills the process group once the grace period is up.
func killJob(cmd *exec.Cmd) {
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// terminatePid asks the previous helper to shut down. Its pidfile is already
// gone by the time this lands (TakeOver removes it first), so its teardown
// knows the directory is no longer its to sweep.
func terminatePid(pid int) {
	syscall.Kill(pid, syscall.SIGTERM)
}

// forceKillPid kills a previous helper that ignored terminatePid.
func forceKillPid(pid int) {
	syscall.Kill(pid, syscall.SIGKILL)
}
