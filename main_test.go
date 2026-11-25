package hls

import (
	"os"
	"testing"
)

// TestMain makes sure a failing test can never leave an ffmpeg process behind
// on the machine: after the suite runs, any stray ffmpeg is force-terminated.
// The production code already guarantees process termination on
// cancellation/failure; this is purely a test-hygiene backstop.
func TestMain(m *testing.M) {
	code := m.Run()
	if code != 0 {
		killStrayFFmpeg()
	}
	os.Exit(code)
}
