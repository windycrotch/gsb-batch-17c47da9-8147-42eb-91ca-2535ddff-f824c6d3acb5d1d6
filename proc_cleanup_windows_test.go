//go:build windows

package hls

import "os/exec"

func killStrayFFmpeg() {
	_ = exec.Command("taskkill", "/F", "/IM", "ffmpeg.exe").Run()
}
