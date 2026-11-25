//go:build windows

package hls

import (
	"os/exec"
	"strconv"
)

// configureProcAttr is a no-op on Windows: process-tree termination is handled
// explicitly through taskkill in killProcessTree.
func configureProcAttr(cmd *exec.Cmd) {}

// killProcessTree force-terminates ffmpeg and every child it spawned.
// `taskkill /T /F /PID` walks the Windows process tree (/T) and force-kills
// (/F); a plain Process.Kill would leave ffmpeg's children (muxers/encoders
// spawned via the shell) running and still writing segments. If taskkill is
// unavailable for any reason we fall back to killing the process directly.
// The subsequent cmd.Wait() in runFFmpeg reaps the process either way.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	taskkill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	if err := taskkill.Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}
