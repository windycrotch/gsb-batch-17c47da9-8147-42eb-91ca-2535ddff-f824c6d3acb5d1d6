//go:build windows

package hls

import (
	"os/exec"
	"strings"
)

// countFFmpegProcesses returns how many ffmpeg.exe processes currently exist.
// On this host `tasklist /IM` is rejected by the Windows argument parser when
// launched through the MSYS/Git-Bash toolchain, so PowerShell's CIM-based
// Get-Process is used instead. The @(...) wrapping guarantees a countable
// result even when there is no match (an empty pipeline would print nothing).
func countFFmpegProcesses() int {
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		"@(Get-Process -Name ffmpeg -ErrorAction SilentlyContinue).Count").Output()
	if err != nil {
		return 0
	}
	var n int
	if _, scanErr := fmtSscanf(strings.TrimSpace(string(out)), &n); scanErr != nil {
		return 0
	}
	return n
}
