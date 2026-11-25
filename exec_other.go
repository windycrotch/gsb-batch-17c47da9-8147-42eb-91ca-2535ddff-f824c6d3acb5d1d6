//go:build plan9 || js || wasip1

package hls

import "os/exec"

func configureProcAttr(cmd *exec.Cmd) {}

// These platforms do not expose process-group signaling; fall back to killing
// the ffmpeg process itself.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
