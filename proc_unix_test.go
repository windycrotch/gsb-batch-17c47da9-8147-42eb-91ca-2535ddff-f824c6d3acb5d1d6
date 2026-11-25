//go:build !windows

package hls

import (
	"os/exec"
	"strings"
)

func countFFmpegProcesses() int {
	out, err := exec.Command("pgrep", "-x", "ffmpeg").Output()
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			count++
		}
	}
	return count
}
