//go:build !windows

package main

// End-to-end interruption via a real OS signal. This complements the
// in-process context-cancellation test by exercising the signal
// handler, the kill of the in-flight ffmpeg child, partial-output
// cleanup and resume on the next run. It runs on Unix; the in-process
// test covers the same logic on every platform.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSignalInterruptAndResume(t *testing.T) {
	buildTools(t)
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	names := []string{"a.avi", "b.avi", "c.avi", "d.avi"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(src, n),
			[]byte(strings.Repeat("Z", 4096)), 0644); err != nil {
			t.Fatal(err)
		}
	}
	control := filepath.Join(root, "control.json")
	// b.avi's fake ffmpeg hangs until killed, simulating an in-flight
	// transcode at the moment the operator presses Ctrl-C.
	if err := os.WriteFile(control,
		[]byte(`{"output_size":2048,"hang_inputs":{"b.avi":1}}`), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(ffcvtBin, "-d", src, "-t", "", "-debug", "0",
		"-bt", "0", "-ffmpeg", fakeFF, "-ffprobe", fakeFP)
	cmd.Env = append(os.Environ(), "FFCVT_FAKE_CONTROL="+control)
	// Own process group so the signal reaches ffcvt and not the test.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Let a.avi finish and b.avi enter its hang.
	time.Sleep(700 * time.Millisecond)
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if exitErr, ok := err.(*exec.ExitError); !ok ||
		exitErr.ExitCode() != exitInterrupted {
		t.Fatalf("interrupted run err=%v, want exit code %d", err, exitInterrupted)
	}

	// a.avi completed; b.avi's partial output cleaned; c/d untouched.
	if _, err := verifyReadable(filepath.Join(src, "a_.mkv")); err != nil {
		t.Fatalf("completed output lost: %v", err)
	}
	for _, n := range []string{"b_.mkv", "c_.mkv", "d_.mkv"} {
		if _, err := os.Stat(filepath.Join(src, n)); !os.IsNotExist(err) {
			t.Errorf("interrupted/untouched output present: %s", n)
		}
	}
	// No transient artifacts remain.
	entries, _ := os.ReadDir(src)
	for _, en := range entries {
		nm := en.Name()
		if strings.Contains(nm, ".ffcvt-tmp-") ||
			strings.HasSuffix(nm, ".ffcvt-lock") {
			t.Errorf("leftover artifact after signal: %s", nm)
		}
	}

	// Resume with the hang removed: b/c/d complete, a is skipped.
	control2 := filepath.Join(root, "control2.json")
	if err := os.WriteFile(control2, []byte(`{"output_size":2048}`), 0644); err != nil {
		t.Fatal(err)
	}
	resume := exec.Command(ffcvtBin, "-d", src, "-t", "", "-debug", "0",
		"-bt", "0", "-nc", "-ffmpeg", fakeFF, "-ffprobe", fakeFP)
	resume.Env = append(os.Environ(), "FFCVT_FAKE_CONTROL="+control2)
	out, err := resume.CombinedOutput()
	if err != nil {
		t.Fatalf("resume run failed: %v\n%s", err, out)
	}
	txt := string(out)
	if !strings.Contains(txt, "success=3 failure=0 skip=1") &&
		!strings.Contains(txt, "3 succeeded, 0 failed, 1 skipped") {
		t.Fatalf("resume summary wrong:\n%s", txt)
	}
	for _, n := range []string{"b_.mkv", "c_.mkv", "d_.mkv"} {
		if _, err := verifyReadable(filepath.Join(src, n)); err != nil {
			t.Errorf("resumed output %s unreadable: %v", n, err)
		}
	}
}
