package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The acceptance tests drive the real HTTP handler, the real SessionManager and
// the real yt-dlp/ffmpeg pipeline. To keep that hermetic they need local mock
// executables. Instead of shipping shell scripts (which are not portable to
// Windows), the test binary copies itself under the names below and dispatches
// on its own basename before any test runs.
const (
	mockYtDlpName  = "mock-ytdlp"
	mockFFmpegName = "mock-ffmpeg"
)

// TestMain doubles as the entry point for the mock executables.
func TestMain(m *testing.M) {
	switch mockKind(filepath.Base(os.Args[0])) {
	case mockYtDlpName:
		os.Exit(runMockYtDlp())
	case mockFFmpegName:
		os.Exit(runMockFFmpeg())
	default:
		os.Exit(m.Run())
	}
}

func mockKind(base string) string {
	base = strings.TrimSuffix(base, ".exe")
	// Accept both the "mock-*" names used by acceptance tests and the real tool
	// names, so a server built from the real config (which resolves ffmpeg from
	// PATH) can be driven by the same mock copies.
	switch {
	case base == mockYtDlpName || base == "yt-dlp":
		return mockYtDlpName
	case base == mockFFmpegName || base == "ffmpeg":
		return mockFFmpegName
	}
	return base
}

// mockEnvPath returns the log file used by the mock executables.
func mockEnvPath() string { return os.Getenv("MOCK_LOG") }

func mockLog(event string, args ...any) {
	path := mockEnvPath()
	if path == "" {
		return
	}
	line := event
	if len(args) > 0 {
		line = fmt.Sprintf("%s %v", event, args)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s\n", line)
	f.Close()
}

func runMockYtDlp() int {
	mockLog("ytdlp-start", os.Getpid())
	defer mockLog("ytdlp-exit", os.Getpid())

	rawURL := os.Args[len(os.Args)-1]
	parsed, err := url.Parse(rawURL)
	mode := ""
	if err == nil {
		mode = parsed.Query().Get("mockmode")
	}
	if mode == "" && strings.Contains(rawURL, "fail") {
		mode = "fail"
	}

	switch mode {
	case "fail":
		return 1
	case "short":
		_, _ = os.Stdout.WriteString("SHORT-AUDIO-PAYLOAD\n")
		return 0
	case "longshot":
		// Finite output large enough that ffmpeg cannot drain it before the
		// consumer reads a chunk, yet small enough to finish on its own.
		line := []byte("MOCKMP3FRAME-0123456789ABCDEF\n")
		for i := 0; i < 4000; i++ {
			if _, err := os.Stdout.Write(line); err != nil {
				return 0
			}
		}
		return 0
	}

	// Long-lived stream: keep producing audio until the consumer goes away
	// (write error) or the process is killed (SIGKILL via context/Close).
	chunk := []byte("MOCKMP3FRAME-0123456789ABCDEF\n")
	for {
		if _, err := os.Stdout.Write(chunk); err != nil {
			return 0
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runMockFFmpeg() int {
	mockLog("ffmpeg-start", os.Getpid())
	defer mockLog("ffmpeg-exit", os.Getpid())
	_, _ = io.Copy(os.Stdout, os.Stdin)
	return 0
}

// installMockBinaries copies the test binary into a temp dir under the mock
// names and returns the yt-dlp and ffmpeg paths.
func installMockBinaries(t *testing.T) (ytdlpPath, ffmpegPath string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating test binary: %v", err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("reading test binary: %v", err)
	}

	dir := t.TempDir()
	ext := ""
	if filepath.Ext(self) == ".exe" {
		ext = ".exe"
	}

	ytdlpPath = filepath.Join(dir, mockYtDlpName+ext)
	ffmpegPath = filepath.Join(dir, mockFFmpegName+ext)
	for _, p := range []string{ytdlpPath, ffmpegPath} {
		if err := os.WriteFile(p, data, 0755); err != nil {
			t.Fatalf("writing mock binary %s: %v", p, err)
		}
	}
	return ytdlpPath, ffmpegPath
}

// mockLogCollector reads events written by mock processes.
type mockLogCollector struct {
	path string
}

func newMockLog(t *testing.T) *mockLogCollector {
	t.Helper()
	l := &mockLogCollector{path: filepath.Join(t.TempDir(), "mock.log")}
	t.Setenv("MOCK_LOG", l.path)
	return l
}

func (l *mockLogCollector) read() string {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return ""
	}
	return string(data)
}

func (l *mockLogCollector) count(event string) int {
	n := 0
	for _, line := range strings.Split(l.read(), "\n") {
		if strings.HasPrefix(line, event+" ") {
			n++
		}
	}
	return n
}

// waitFor polls until cond is true or the deadline expires.
func (l *mockLogCollector) waitFor(t *testing.T, what string, cond func(*mockLogCollector) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond(l) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; mock log:\n%s", what, l.read())
}

// waitForCount waits until exactly n occurrences of event were logged.
func (l *mockLogCollector) waitForCount(t *testing.T, event string, n int) {
	t.Helper()
	l.waitFor(t, fmt.Sprintf("%d x %q", n, event), func(c *mockLogCollector) bool {
		return c.count(event) >= n
	})
}

// pids returns the PIDs recorded for the given start event.
func (l *mockLogCollector) pids(event string) []int {
	var pids []int
	for _, line := range strings.Split(l.read(), "\n") {
		if !strings.HasPrefix(line, event+" [") {
			continue
		}
		start := strings.Index(line, "[") + 1
		end := strings.Index(line, "]")
		if start <= 0 || end <= start {
			continue
		}
		if pid, err := strconv.Atoi(line[start:end]); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// waitForExit waits until none of the PIDs recorded for startEvent are alive.
// Killed processes cannot log an exit event, so liveness is checked by PID.
func (l *mockLogCollector) waitForExit(t *testing.T, startEvent string, want int) {
	t.Helper()
	l.waitFor(t, fmt.Sprintf("%d x %q started", want, startEvent), func(c *mockLogCollector) bool {
		return len(c.pids(startEvent)) >= want
	})
	l.waitFor(t, fmt.Sprintf("%d x %q to exit", want, startEvent), func(c *mockLogCollector) bool {
		alive := 0
		for _, pid := range c.pids(startEvent) {
			if processAlive(pid) {
				alive++
			}
		}
		return alive == 0
	})
}
