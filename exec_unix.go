//go:build !windows && !plan9 && !js && !wasip1

package hls

import (
	"os/exec"
	"syscall"
)

// configureProcAttr puts ffmpeg into its own process group so that
// cancellation can signal the whole group instead of only the parent.
func configureProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessTree sends SIGKILL to ffmpeg's entire process group. Because
// Setpgid was set at start, the negative PID addresses every descendant, so
// no orphaned child can keep writing segments. The subsequent cmd.Wait() in
// runFFmpeg reaps the process.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
