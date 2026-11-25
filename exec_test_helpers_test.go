package hls

import (
	"fmt"
	"os/exec"
)

// execLookPath / execCommand are thin wrappers kept in one place so the test
// suite stays independent of os/exec wiring details.
func execLookPath(file string) (string, error) {
	return exec.LookPath(file)
}

func execCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// fmtSscanf wraps fmt.Sscanf for the platform-specific test helpers.
func fmtSscanf(s string, n *int) (int, error) {
	return fmt.Sscanf(s, "%d", n)
}
