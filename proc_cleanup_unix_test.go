//go:build !windows

package hls

import "os/exec"

func killStrayFFmpeg() {
	_ = exec.Command("pkill", "-x", "ffmpeg").Run()
}
