package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bundledHelperEnv names the environment variable that points the suite at an
// already-built helper — the universal binary inside a packed .iinaplgz —
// instead of compiling one from source. packaging/test-package.sh sets it.
const bundledHelperEnv = "IINA_AIRPLAY_HELPER"

// locateHelper is the pure decision behind buildHelper: given the value of
// IINA_AIRPLAY_HELPER and a stat function, it returns either the binary to use,
// a request to build one, or a fatal message. It takes no *testing.T so the
// decision can be table-tested with fake paths, mirroring locateSystemFFmpeg in
// e2e_test.go.
//
// A set-but-unusable path is FATAL, never a fall back to `go build`: the reason
// to set this variable at all is to exercise the shipped binary, so a silent
// fallback would let a typo be mistaken for a passing bundled-binary run.
func locateHelper(env string, stat func(string) error) (path string, build bool, fatal string) {
	if env == "" {
		return "", true, ""
	}
	if err := stat(env); err != nil {
		return "", false, fmt.Sprintf("%s=%q is not usable: %v", bundledHelperEnv, env, err)
	}
	return env, false, ""
}

// Builds the helper binary once for integration tests — or returns the bundled
// one when IINA_AIRPLAY_HELPER points at it. Every test that calls this then
// drives the shipped universal binary, not just the e2e ones, so the watchdog,
// the pidfile takeover and the stdout protocol get exercised on whichever
// architecture is running the suite.
func buildHelper(t *testing.T) string {
	t.Helper()
	path, build, fatal := locateHelper(os.Getenv(bundledHelperEnv), func(p string) error {
		_, err := os.Stat(p)
		return err
	})
	if fatal != "" {
		t.Fatal(fatal)
	}
	if !build {
		return path
	}
	bin := filepath.Join(t.TempDir(), "airplay-helper")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func TestLocateHelper(t *testing.T) {
	statOK := func(string) error { return nil }
	statMissing := func(string) error { return errors.New("no such file or directory") }

	t.Run("unset builds from source", func(t *testing.T) {
		path, build, fatal := locateHelper("", statMissing)
		if !build || path != "" || fatal != "" {
			t.Fatalf("got (%q, %v, %q), want (\"\", true, \"\")", path, build, fatal)
		}
	})

	t.Run("set and present is used as-is", func(t *testing.T) {
		path, build, fatal := locateHelper("/pkg/bin/airplay-helper", statOK)
		if path != "/pkg/bin/airplay-helper" || build || fatal != "" {
			t.Fatalf("got (%q, %v, %q), want the bundled path with no build and no fatal", path, build, fatal)
		}
	})

	// The whole point of setting this variable is to exercise the SHIPPED
	// binary. A silent fallback to `go build` would let a typo in the CI
	// workflow read as a passing bundled-binary run — which is precisely the
	// failure the Intel job exists to rule out. Same contract as findFFmpeg.
	t.Run("set but unusable is fatal, never a fallback", func(t *testing.T) {
		_, build, fatal := locateHelper("/nope/airplay-helper", statMissing)
		if build {
			t.Fatal("a bad IINA_AIRPLAY_HELPER fell back to building from source; a typo must fail loudly")
		}
		if fatal == "" {
			t.Fatal("want a fatal message, got none")
		}
		for _, want := range []string{"IINA_AIRPLAY_HELPER", "/nope/airplay-helper"} {
			if !strings.Contains(fatal, want) {
				t.Errorf("fatal message %q does not mention %q", fatal, want)
			}
		}
	})
}

// startServe launches the real helper binary against the stub ffmpeg (the
// test binary in the given stub role) and returns the process and its stdout
// event stream. The stub role travels in the child's environment because the
// helper execs a bare ffmpeg path.
func startServe(t *testing.T, bin, out, stubMode string, parent int, extraArgs ...string) (*exec.Cmd, io.Reader) {
	t.Helper()
	args := []string{"serve",
		"-source", os.DevNull, "-out", out, "-ffmpeg", stubPath(t),
		"-parent", fmt.Sprint(parent), "-duration", "60",
		"-vcodec", "h264", "-acodec", "aac", "-achannels", "2", "-vmap", "0", "-amap", "1"}
	args = append(args, extraArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "AIRPLAY_STUB_MODE="+stubMode, "AIRPLAY_STUB_OUT="+out)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd, stdout
}

// waitReady scans helper stdout until a ready event and returns its url.
func waitReady(t *testing.T, stdout io.Reader, timeout time.Duration) string {
	t.Helper()
	sc := bufio.NewScanner(stdout)
	deadline := time.After(timeout)
	for {
		lineCh := make(chan string, 1)
		go func() {
			if sc.Scan() {
				lineCh <- sc.Text()
			} else {
				lineCh <- ""
			}
		}()
		select {
		case line := <-lineCh:
			if line == "" {
				t.Fatal("helper stdout closed before ready")
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("bad line %q", line)
			}
			if ev["event"] == "error" {
				t.Fatalf("helper errored: %v", ev)
			}
			if ev["event"] == "ready" {
				url, _ := ev["url"].(string)
				return url
			}
		case <-deadline:
			t.Fatal("no ready event in time")
		}
	}
}

// readEvents streams a helper's stdout events onto a channel for tests that
// watch more than one event (ready, then stopped, say).
func readEvents(stdout io.Reader) <-chan map[string]any {
	ch := make(chan map[string]any, 16)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var ev map[string]any
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				ch <- ev
			}
		}
	}()
	return ch
}

// waitEvent blocks until the named event arrives on ch. An error event from
// the helper fails the test immediately.
func waitEvent(t *testing.T, ch <-chan map[string]any, event string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("helper stdout closed before %q", event)
			}
			if ev["event"] == "error" {
				t.Fatalf("helper errored: %v", ev)
			}
			if ev["event"] == event {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %q event within %v", event, timeout)
		}
	}
}

// waitExit waits for the helper to exit and returns its exit error (nil on a
// clean exit).
func waitExit(t *testing.T, cmd *exec.Cmd, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatalf("helper still running after %v", timeout)
		return nil
	}
}

// castFiles is what a real cast leaves in the output directory: the
// playlist, the fMP4 init segment and at least one media segment. Every
// teardown path must remove all of them (issue #12: a stopped cast used to
// leave the whole remux in temp until the next cast).
var castFiles = []string{"index.m3u8", "init.mp4", "seg_0000.m4s"}

func assertSwept(t *testing.T, out string) {
	t.Helper()
	for _, f := range append(append([]string{}, castFiles...), "helper.pid") {
		if _, err := os.Stat(filepath.Join(out, f)); err == nil {
			t.Errorf("%s left behind in the output dir", f)
		}
	}
}

func assertCastFiles(t *testing.T, out string) {
	t.Helper()
	for _, f := range castFiles {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("%s missing from the output dir: %v", f, err)
		}
	}
}

func TestServeEndToEndWithStub(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	cmd, stdout := startServe(t, bin, out, "idle-cast", os.Getpid())

	readyURL := waitReady(t, stdout, 15*time.Second)
	if !strings.HasSuffix(readyURL, "/index.m3u8") || strings.Contains(readyURL, "127.0.0.1") {
		t.Fatalf("bad ready url %q", readyURL)
	}
	resp, err := http.Get(readyURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("playlist fetch: %d", resp.StatusCode)
	}
	// pidfile exists while serving
	if _, err := os.Stat(filepath.Join(out, "helper.pid")); err != nil {
		t.Fatal("pidfile missing while serving")
	}

	// `stop` kills the serving instance
	if err := exec.Command(bin, "stop", "-out", out).Run(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitExit(t, cmd, 10*time.Second)
}

func TestServeWithSubsAdvertisesRewrittenMaster(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	t.Setenv("AIRPLAY_STUB_MASTER", "#EXTM3U\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"default\",NAME=\"subtitle_0\",DEFAULT=NO,AUTOSELECT=YES,URI=\"index_vtt.m3u8\"\nindex.m3u8\n")
	_, stdout := startServe(t, bin, out, "sub-cast", os.Getpid(),
		"-smap", "2", "-sublang", "en", "-subname", "English")

	readyURL := waitReady(t, stdout, 15*time.Second)
	if !strings.HasSuffix(readyURL, "/master.m3u8") {
		t.Fatalf("with subs the helper must advertise the master playlist, got %q", readyURL)
	}
	resp, err := http.Get(readyURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	s := string(body)
	if !strings.Contains(s, "DEFAULT=YES") || !strings.Contains(s, `NAME="English"`) || !strings.Contains(s, `LANGUAGE="en"`) {
		t.Fatalf("served master not rewritten:\n%s", s)
	}
}

func TestServeCleansStaleSubtitleFiles(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	// Stale leftovers from a previous subtitled cast; this cast has no subs,
	// so nothing recreates them — they must be gone once serving is up.
	for _, f := range []string{"master.m3u8", "index_vtt.m3u8", "seg_0000.vtt"} {
		os.WriteFile(filepath.Join(out, f), []byte("stale"), 0o644)
	}
	_, stdout := startServe(t, bin, out, "idle-cast", os.Getpid())

	waitReady(t, stdout, 15*time.Second)
	for _, f := range []string{"master.m3u8", "index_vtt.m3u8", "seg_0000.vtt"} {
		if _, err := os.Stat(filepath.Join(out, f)); err == nil {
			t.Fatalf("stale %s survived cleanup", f)
		}
	}
}

func TestStopSweepsOutDir(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	cmd, stdout := startServe(t, bin, out, "idle-cast", os.Getpid())
	waitReady(t, stdout, 15*time.Second)
	assertCastFiles(t, out)

	if err := exec.Command(bin, "stop", "-out", out).Run(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitExit(t, cmd, 10*time.Second)
	assertSwept(t, out)
}

// After ffmpeg has finished (VOD complete) the helper keeps serving until
// stopped — a distinct branch of the serve loop that must sweep too.
func TestStopAfterPackagedSweepsOutDir(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	cmd, stdout := startServe(t, bin, out, "finish-cast", os.Getpid(), "-idle", "0")
	waitReady(t, stdout, 15*time.Second)
	// Give the serve loop time to consume ffmpeg's exit before stopping.
	time.Sleep(500 * time.Millisecond)

	if err := exec.Command(bin, "stop", "-out", out).Run(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitExit(t, cmd, 10*time.Second)
	assertSwept(t, out)
}

// IINA quitting (or crashing) is the other way a cast ends; the watchdog
// path must sweep just like the stop path.
func TestParentDeathSweepsOutDir(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	parent := startStubProcess(t, "sleep")
	cmd, stdout := startServe(t, bin, out, "idle-cast", parent.Process.Pid)
	waitReady(t, stdout, 15*time.Second)

	parent.Process.Kill()
	parent.Wait()
	waitExit(t, cmd, 10*time.Second) // watchdog polls every 2s
	assertSwept(t, out)
}

// A cast that fails part-way leaves partial segments; the error exit must
// sweep them as well.
func TestFailedCastSweepsOutDir(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	cmd, stdout := startServe(t, bin, out, "fail-cast", os.Getpid())
	b, _ := io.ReadAll(stdout)
	waitExit(t, cmd, 10*time.Second)
	sawError := false
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil && ev["event"] == "error" {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("expected an error event, got:\n%s", b)
	}
	assertSwept(t, out)
}

// ffmpeg dying before the playlist exists is the same clear result: an
// error event, a non-zero exit, and nothing left in the output directory.
func TestEarlyFailureGivesErrorAndLeavesNothing(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	cmd, stdout := startServe(t, bin, out, "fail-early", os.Getpid())
	b, _ := io.ReadAll(stdout)
	err := waitExit(t, cmd, 10*time.Second)
	if err == nil {
		t.Fatal("a failed cast must exit non-zero")
	}
	sawError := false
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil && ev["event"] == "error" {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("expected an error event, got:\n%s", b)
	}
	assertSwept(t, out)
}

// The core ownership invariant: once the directory's pidfile names someone
// else, the superseded instance's exit must not delete a single file — the
// segments there belong to the new cast.
func TestSupersededInstanceLeavesFilesAlone(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	cmd, stdout := startServe(t, bin, out, "idle-cast", os.Getpid())
	waitReady(t, stdout, 15*time.Second)
	assertCastFiles(t, out)

	// A newer cast takes over: TakeOver removes the old pidfile, the new
	// instance plants its own. From here on the files are the new cast's.
	foreign := startStubProcess(t, "sleep")
	if err := os.WriteFile(filepath.Join(out, "helper.pid"),
		[]byte(strconv.Itoa(foreign.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	// The ownership poll notices within a second; the exit must be clean
	// and the directory untouched.
	if err := waitExit(t, cmd, 10*time.Second); err != nil {
		t.Fatalf("superseded instance should exit cleanly, got %v", err)
	}
	assertCastFiles(t, out)
	if pid, err := readPidfile(filepath.Join(out, "helper.pid")); err != nil || pid != foreign.Process.Pid {
		t.Fatalf("foreign pidfile disturbed: pid=%d err=%v", pid, err)
	}
}

// A new cast on the same output directory takes it over: the old instance
// exits, and the files the new instance writes afterwards are still there
// once the old one is gone.
func TestNewCastTakesOverOldInstance(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	cmdA, stdoutA := startServe(t, bin, out, "idle-cast", os.Getpid())
	waitReady(t, stdoutA, 15*time.Second)

	cmdB, stdoutB := startServe(t, bin, out, "idle-cast", os.Getpid())
	waitReady(t, stdoutB, 15*time.Second)

	waitExit(t, cmdA, 10*time.Second)
	assertCastFiles(t, out)
	if pid, err := readPidfile(filepath.Join(out, "helper.pid")); err != nil || pid != cmdB.Process.Pid {
		t.Fatalf("pidfile should name the new instance: pid=%d err=%v", pid, err)
	}

	// And the new instance's own stop still sweeps.
	if err := exec.Command(bin, "stop", "-out", out).Run(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitExit(t, cmdB, 10*time.Second)
	assertSwept(t, out)
}

// The watchdog only polls every 2s. If IINA dies and a new cast takes the
// output directory over inside that window, the old instance's exit —
// whichever path fires first — must not destroy the new cast's products.
func TestParentDeathDuringTakeoverKeepsNewFiles(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	parent := startStubProcess(t, "sleep")
	cmdA, stdoutA := startServe(t, bin, out, "idle-cast", parent.Process.Pid)
	waitReady(t, stdoutA, 15*time.Second)

	// IINA vanishes; within the watchdog's poll window a new cast starts on
	// the same directory.
	parent.Process.Kill()
	parent.Wait()
	cmdB, stdoutB := startServe(t, bin, out, "idle-cast", os.Getpid())
	waitReady(t, stdoutB, 15*time.Second)
	waitExit(t, cmdA, 10*time.Second)

	// Past the watchdog window: the new cast's files must be untouched.
	time.Sleep(3 * time.Second)
	assertCastFiles(t, out)
	if pid, err := readPidfile(filepath.Join(out, "helper.pid")); err != nil || pid != cmdB.Process.Pid {
		t.Fatalf("pidfile should name the new instance: pid=%d err=%v", pid, err)
	}
}

// `stop` waits for the old instance to die before it sweeps. If a new cast
// has taken the directory over by then, stop must leave its files alone.
func TestStopLeavesNewCastsFilesAlone(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	// The old instance's ffmpeg ignores SIGTERM, so `stop` spends a while
	// inside TakeOver waiting for it — long enough for a new cast to plant
	// its pidfile before stop's sweep decision.
	cmdA, stdoutA := startServe(t, bin, out, "ignore-term-cast", os.Getpid())
	waitReady(t, stdoutA, 15*time.Second)

	stopDone := make(chan error, 1)
	go func() { stopDone <- exec.Command(bin, "stop", "-out", out).Run() }()
	// Let stop remove the pidfile and start waiting, then take over.
	time.Sleep(500 * time.Millisecond)
	cmdB, stdoutB := startServe(t, bin, out, "idle-cast", os.Getpid())
	waitReady(t, stdoutB, 15*time.Second)

	if err := <-stopDone; err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitExit(t, cmdA, 10*time.Second)
	assertCastFiles(t, out)
	if pid, err := readPidfile(filepath.Join(out, "helper.pid")); err != nil || pid != cmdB.Process.Pid {
		t.Fatalf("pidfile should name the new instance: pid=%d err=%v", pid, err)
	}
}

// Packaging done and nobody watching: the helper must end the cast itself —
// a clear stopped event, a swept directory, process gone.
func TestIdleTimeoutStopsAndSweeps(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	cmd, stdout := startServe(t, bin, out, "finish-cast", os.Getpid(), "-idle", "2")
	events := readEvents(stdout)
	waitEvent(t, events, "ready", 15*time.Second)

	ev := waitEvent(t, events, "stopped", 15*time.Second)
	if ev["reason"] != "idle" {
		t.Fatalf("stopped event should carry reason=idle, got %v", ev)
	}
	waitExit(t, cmd, 10*time.Second)
	assertSwept(t, out)
}

// A request restarts the idle clock: the TV polling the playlist is what
// "someone is watching" means.
func TestIdleTimerResetByRequests(t *testing.T) {
	bin := buildHelper(t)
	out := t.TempDir()
	cmd, stdout := startServe(t, bin, out, "finish-cast", os.Getpid(), "-idle", "4")
	events := readEvents(stdout)
	ready := waitEvent(t, events, "ready", 15*time.Second)
	url, _ := ready["url"].(string)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// t≈2s: someone pulls the playlist. Without this the idle timeout would
	// fire at t≈4s.
	time.Sleep(2 * time.Second)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// t≈5s: past the un-reset deadline (4s + check interval), the helper
	// must still be serving.
	time.Sleep(3 * time.Second)
	select {
	case err := <-exited:
		t.Fatalf("helper exited at the un-reset idle deadline (err=%v); the request should have reset the clock", err)
	default:
	}

	// The clock restarted at the request (t≈2s), so the cast ends at t≈6s.
	waitEvent(t, events, "stopped", 5*time.Second)
	<-exited
	assertSwept(t, out)
}
