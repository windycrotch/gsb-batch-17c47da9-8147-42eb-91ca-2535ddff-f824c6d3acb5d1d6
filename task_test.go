package hls

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ffmpegExe resolves a real ffmpeg executable for the acceptance tests.
func ffmpegExe(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("FFMPEG_BIN"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, candidate := range []string{"ffmpeg", "ffmpeg.exe"} {
		if p, err := execLookPath(candidate); err == nil {
			return p
		}
	}
	// Common location on the evaluation Windows machine.
	winCandidate := `C:\ffmpeg-dl\ffmpeg-9.1-essentials_build\bin\ffmpeg.exe`
	if _, err := os.Stat(winCandidate); err == nil {
		return winCandidate
	}
	t.Skip("ffmpeg not found; set FFMPEG_BIN to run the acceptance tests")
	return ""
}

// makeTestVideo renders a real, self-contained short video with ffmpeg's
// testsrc source. durationSeconds controls the length.
func makeTestVideo(t *testing.T, ffmpeg, dir, name string, durationSeconds int) string {
	t.Helper()
	return makeTestVideoSpec(t, ffmpeg, dir, name, durationSeconds, "1280x720", 24)
}

// makeTestVideoSpec renders a testsrc clip with an explicit size and frame
// rate. Long, dense sources are used to keep the (ultrafast) transcode busy
// long enough to be canceled mid-run.
func makeTestVideoSpec(t *testing.T, ffmpeg, dir, name string, durationSeconds int, size string, rate int) string {
	t.Helper()
	src := filepath.Join(dir, name)
	args := []string{
		"-hide_banner", "-y",
		"-f", "lavfi",
		"-i", fmt.Sprintf("testsrc=size=%s:rate=%d:duration=%d", size, rate, durationSeconds),
		"-f", "lavfi",
		"-i", fmt.Sprintf("sine=frequency=440:duration=%d", durationSeconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac",
		src,
	}
	if out, err := execCommand(ffmpeg, args...); err != nil {
		t.Fatalf("failed to create test video: %v\n%s", err, string(out))
	}
	return src
}

// makeLongTestVideo creates a dense 1080p30 source long enough that an
// ultrafast transcode stays running for the cancellation tests to act.
func makeLongTestVideo(t *testing.T, ffmpeg, dir, name string) string {
	t.Helper()
	return makeTestVideoSpec(t, ffmpeg, dir, name, 600, "1920x1080", 30)
}

func makeCorruptFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("this is definitely not a media file"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// waitFor polls until cond() is true or the deadline elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// listFiles returns the regular files inside dir (non-recursive), sorted.
func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func hasStaging(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".task-") && strings.HasSuffix(e.Name(), ".staging") {
			return true
		}
	}
	return false
}

// waitForFFmpegRunning waits until an ffmpeg process beyond the baseline count
// exists. The staging-directory check is added as a second signal: it proves
// the task started and ffmpeg opened its output files, which matters on hosts
// where process enumeration is briefly delayed.
func waitForFFmpegRunning(t *testing.T, baseline int, target string) {
	t.Helper()
	waitFor(t, 30*time.Second, func() bool {
		return countFFmpegProcesses() > baseline && hasStaging(t, target)
	}, "ffmpeg process to start and staging to appear")
}

// --- Validation (no ffmpeg needed) -----------------------------------------

func TestSubmitValidation(t *testing.T) {
	ffmpeg := ffmpegExe(t)
	work := t.TempDir()
	src := makeTestVideo(t, ffmpeg, work, "src.mp4", 1)
	target := filepath.Join(work, "out")

	cases := []struct {
		name string
		cfg  Config
		want error
	}{
		{"missing source", Config{FFmpegPath: ffmpeg, TargetPath: target, Resolutions: []string{"360p"}}, errors.New("source")},
		{"missing target", Config{FFmpegPath: ffmpeg, SrcPath: src, Resolutions: []string{"360p"}}, errors.New("target")},
		{"no resolutions", Config{FFmpegPath: ffmpeg, SrcPath: src, TargetPath: target}, ErrNoResolutions},
		{"unknown resolution", Config{FFmpegPath: ffmpeg, SrcPath: src, TargetPath: target, Resolutions: []string{"240p"}}, ErrUnknownResolution},
		{"unknown mixed with valid", Config{FFmpegPath: ffmpeg, SrcPath: src, TargetPath: target, Resolutions: []string{"360p", "999p"}}, ErrUnknownResolution},
		{"duplicate resolution", Config{FFmpegPath: ffmpeg, SrcPath: src, TargetPath: target, Resolutions: []string{"360p", "360p"}}, errors.New("duplicate")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Submit(context.Background(), tc.cfg)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want.Error())) && !errors.Is(err, tc.want) {
				t.Fatalf("error %q does not match expected %q", err.Error(), tc.want.Error())
			}
			// Nothing must have been created in the target on a rejected submit.
			if _, statErr := os.Stat(target); statErr == nil {
				if entries, _ := os.ReadDir(target); len(entries) != 0 {
					t.Fatalf("rejected submit left artifacts: %v", entries)
				}
			}
		})
	}
}

// --- Success path with real ffmpeg -----------------------------------------

func TestTranscodeSuccessAllVariants(t *testing.T) {
	ffmpeg := ffmpegExe(t)
	work := t.TempDir()
	src := makeTestVideo(t, ffmpeg, work, "src.mp4", 3)
	target := filepath.Join(work, "hls")

	resolutions := []string{"360p", "480p", "720p", "1080p"}
	task, err := Transcode(context.Background(), Config{
		FFmpegPath:     ffmpeg,
		SrcPath:        src,
		TargetPath:     target,
		Resolutions:    resolutions,
		MasterPlaylist: "master.m3u8",
	})
	if err != nil {
		t.Fatalf("Transcode failed: %v", err)
	}
	if task.Status() != StatusCompleted {
		t.Fatalf("expected completed, got %s", task.Status())
	}

	files := listFiles(t, target)

	// Every requested variant has its playlist and at least one segment.
	for _, res := range resolutions {
		if !contains(files, res+".m3u8") {
			t.Fatalf("missing variant playlist %s.m3u8; files=%v", res, files)
		}
		foundSegment := false
		for _, f := range files {
			if strings.HasPrefix(f, res+"_") && strings.HasSuffix(f, ".ts") {
				foundSegment = true
			}
		}
		if !foundSegment {
			t.Fatalf("no segments for %s; files=%v", res, files)
		}
	}

	if !contains(files, "master.m3u8") {
		t.Fatalf("master playlist missing; files=%v", files)
	}
	if hasStaging(t, target) {
		t.Fatal("staging directory was not removed after success")
	}

	// Master playlist must reference every requested variant and nothing else.
	master, err := os.ReadFile(filepath.Join(target, "master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	masterText := string(master)
	for _, res := range resolutions {
		if !strings.Contains(masterText, res+".m3u8") {
			t.Fatalf("master playlist does not reference %s:\n%s", res, masterText)
		}
	}

	// Each variant playlist must be a finished VOD playlist and point at
	// segments that actually exist on disk.
	for _, res := range resolutions {
		pl, err := os.ReadFile(filepath.Join(target, res+".m3u8"))
		if err != nil {
			t.Fatal(err)
		}
		text := string(pl)
		if !strings.Contains(text, "#EXT-X-ENDLIST") {
			t.Fatalf("%s playlist is not finalized (missing ENDLIST)", res)
		}
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasSuffix(line, ".ts") {
				if _, err := os.Stat(filepath.Join(target, line)); err != nil {
					t.Fatalf("playlist %s references missing segment %s", res, line)
				}
			}
		}
	}

	// Wait must be idempotent and keep returning nil.
	if err := task.Wait(); err != nil {
		t.Fatalf("second Wait returned error: %v", err)
	}
}

// --- Failure path: no task artifacts may remain ----------------------------

func TestTranscodeFailureCleansArtifacts(t *testing.T) {
	ffmpeg := ffmpegExe(t)
	work := t.TempDir()
	// Pre-existing unrelated content must survive the failure untouched.
	target := filepath.Join(work, "hls")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	keepFile := filepath.Join(target, "keep-me.txt")
	if err := os.WriteFile(keepFile, []byte("pre-existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	corrupt := makeCorruptFile(t, work, "broken.mp4")

	resolutions := []string{"360p", "480p", "720p"}
	task, err := Transcode(context.Background(), Config{
		FFmpegPath:  ffmpeg,
		SrcPath:     corrupt,
		TargetPath:  target,
		Resolutions: resolutions,
	})
	if err == nil {
		t.Fatal("expected transcode of corrupt input to fail")
	}
	if task.Status() != StatusFailed {
		t.Fatalf("expected failed status, got %s (err=%v)", task.Status(), err)
	}

	var variantErr *VariantError
	if !errors.As(err, &variantErr) {
		t.Fatalf("expected *VariantError, got %T: %v", err, err)
	}

	// No artifacts from this task may remain: no segments, variant playlists,
	// master playlist, or staging directory.
	files := listFiles(t, target)
	for _, f := range files {
		if strings.HasSuffix(f, ".ts") || strings.HasSuffix(f, ".m3u8") {
			t.Fatalf("failure left artifact %s in target dir", f)
		}
		if strings.HasPrefix(f, ".task-") {
			t.Fatalf("failure left staging artifact %s", f)
		}
	}
	if hasStaging(t, target) {
		t.Fatal("failure left a staging directory behind")
	}

	// The pre-existing file must be untouched.
	data, err := os.ReadFile(keepFile)
	if err != nil || string(data) != "pre-existing" {
		t.Fatalf("pre-existing content damaged: %q %v", data, err)
	}
}

// --- Cancellation: process killed/reaped, artifacts removed ----------------

func TestCancelTerminatesFFmpegAndCleansArtifacts(t *testing.T) {
	ffmpeg := ffmpegExe(t)
	work := t.TempDir()
	// A long, dense 1080p job keeps ffmpeg running long enough to cancel.
	src := makeLongTestVideo(t, ffmpeg, work, "long.mp4")
	target := filepath.Join(work, "hls")

	baseline := countFFmpegProcesses()

	task, err := Submit(context.Background(), Config{
		FFmpegPath:  ffmpeg,
		SrcPath:     src,
		TargetPath:  target,
		Resolutions: []string{"1080p"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Wait until ffmpeg is actually running (and has likely opened segment
	// files inside the staging directory).
	waitForFFmpegRunning(t, baseline, target)

	// The target directory must not show a published master playlist while the
	// task is still running.
	if _, err := os.Stat(filepath.Join(target, "playlist.m3u8")); !os.IsNotExist(err) {
		t.Fatalf("master playlist visible before task completion: %v", err)
	}

	if err := task.Cancel(); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}

	waitErr := task.Wait()
	if !errors.Is(waitErr, ErrTaskCanceled) {
		t.Fatalf("expected ErrTaskCanceled, got %v", waitErr)
	}
	if task.Status() != StatusCanceled {
		t.Fatalf("expected canceled status, got %s", task.Status())
	}

	// The ffmpeg process must be gone: no orphan keeps writing segments.
	waitFor(t, 10*time.Second, func() bool {
		return countFFmpegProcesses() <= baseline
	}, "ffmpeg processes to be terminated after cancel")

	// Everything the canceled task produced must be gone.
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		if hasStaging(t, target) {
			t.Fatal("cancel left a staging directory behind")
		}
		files := listFiles(t, target)
		if len(files) != 0 {
			t.Fatalf("cancel left artifacts in target dir: %v", files)
		}
	}

	// A second cancel and a subsequent Wait must remain safe.
	if err := task.Cancel(); err != nil {
		t.Fatalf("second Cancel failed: %v", err)
	}
	if err := task.Wait(); !errors.Is(err, ErrTaskCanceled) {
		t.Fatalf("Wait after second cancel: %v", err)
	}
}

// --- Context cancellation behaves like Task.Cancel -------------------------

func TestContextCancellation(t *testing.T) {
	ffmpeg := ffmpegExe(t)
	work := t.TempDir()
	src := makeLongTestVideo(t, ffmpeg, work, "long.mp4")
	target := filepath.Join(work, "hls")

	baseline := countFFmpegProcesses()
	ctx, cancel := context.WithCancel(context.Background())

	task, err := Submit(ctx, Config{
		FFmpegPath:  ffmpeg,
		SrcPath:     src,
		TargetPath:  target,
		Resolutions: []string{"1080p"},
	})
	if err != nil {
		t.Fatal(err)
	}

	waitForFFmpegRunning(t, baseline, target)

	cancel()

	if err := task.Wait(); !errors.Is(err, ErrTaskCanceled) {
		t.Fatalf("expected ErrTaskCanceled from context cancel, got %v", err)
	}
	if task.Status() != StatusCanceled {
		t.Fatalf("expected canceled, got %s", task.Status())
	}
	waitFor(t, 10*time.Second, func() bool {
		return countFFmpegProcesses() <= baseline
	}, "ffmpeg to be reaped after context cancel")
	if hasStaging(t, target) {
		t.Fatal("context cancel left staging directory")
	}
}

// --- Concurrent submissions to the same target directory -------------------

func TestConcurrentSubmissionsSameDirectory(t *testing.T) {
	ffmpeg := ffmpegExe(t)
	work := t.TempDir()
	src := makeTestVideo(t, ffmpeg, work, "src.mp4", 5)
	target := filepath.Join(work, "hls")

	const n = 4
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted []*Task
		rejected int
	)

	// All submissions fire at once. At most one may own the target directory;
	// the others must be rejected with ErrTargetBusy rather than racing on
	// identically named segment files.
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			task, err := Submit(context.Background(), Config{
				FFmpegPath:  ffmpeg,
				SrcPath:     src,
				TargetPath:  target,
				Resolutions: []string{"360p", "480p"},
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !errors.Is(err, ErrTargetBusy) {
					t.Errorf("unexpected submit error: %v", err)
				}
				rejected++
				return
			}
			accepted = append(accepted, task)
		}()
	}
	close(start)
	wg.Wait()

	if len(accepted) != 1 {
		t.Fatalf("expected exactly 1 accepted task, got %d (%d rejected)", len(accepted), rejected)
	}
	if rejected != n-1 {
		t.Fatalf("expected %d rejections, got %d", n-1, rejected)
	}

	owner := accepted[0]
	if err := owner.Wait(); err != nil {
		t.Fatalf("owning task failed: %v", err)
	}
	if owner.Status() != StatusCompleted {
		t.Fatalf("owning task status = %s", owner.Status())
	}

	// After the owner finishes, the directory becomes available again.
	task2, err := Submit(context.Background(), Config{
		FFmpegPath:  ffmpeg,
		SrcPath:     src,
		TargetPath:  target,
		Resolutions: []string{"720p"},
	})
	if err != nil {
		t.Fatalf("resubmission after completion should succeed, got %v", err)
	}
	if err := task2.Wait(); err != nil {
		t.Fatalf("second task failed: %v", err)
	}

	// Final artifacts must be one coherent set: every playlist references only
	// segments that exist, and the master playlist references only present
	// variant playlists.
	files := listFiles(t, target)
	if hasStaging(t, target) {
		t.Fatal("staging directory left behind")
	}
	masterPath := filepath.Join(target, "playlist.m3u8")
	master, err := os.ReadFile(masterPath)
	if err != nil {
		t.Fatalf("master playlist missing: %v (files=%v)", err, files)
	}
	for _, line := range strings.Split(string(master), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, ".m3u8") {
			if _, err := os.Stat(filepath.Join(target, line)); err != nil {
				t.Fatalf("master playlist references missing variant %s", line)
			}
		}
	}
	for _, f := range files {
		if !strings.HasSuffix(f, ".m3u8") {
			continue
		}
		pl, _ := os.ReadFile(filepath.Join(target, f))
		for _, line := range strings.Split(string(pl), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasSuffix(line, ".ts") {
				if _, err := os.Stat(filepath.Join(target, line)); err != nil {
					t.Fatalf("playlist %s references missing segment %s", f, line)
				}
			}
		}
	}
}

// --- Canceling one task must not affect a task in another directory --------

func TestCancelIsolationBetweenDirectories(t *testing.T) {
	ffmpeg := ffmpegExe(t)
	work := t.TempDir()
	src := makeLongTestVideo(t, ffmpeg, work, "long.mp4")

	targetA := filepath.Join(work, "dirA")
	targetB := filepath.Join(work, "dirB")

	baseline := countFFmpegProcesses()

	taskA, err := Submit(context.Background(), Config{
		FFmpegPath:  ffmpeg,
		SrcPath:     src,
		TargetPath:  targetA,
		Resolutions: []string{"1080p"},
	})
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := Submit(context.Background(), Config{
		FFmpegPath:  ffmpeg,
		SrcPath:     src,
		TargetPath:  targetB,
		Resolutions: []string{"1080p"},
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, func() bool {
		return countFFmpegProcesses() >= baseline+2 && hasStaging(t, targetA) && hasStaging(t, targetB)
	}, "both ffmpeg processes to start")

	// Cancel only A; B must keep running and complete successfully.
	if err := taskA.Cancel(); err != nil {
		t.Fatal(err)
	}
	if err := taskA.Wait(); !errors.Is(err, ErrTaskCanceled) {
		t.Fatalf("task A: %v", err)
	}
	if taskB.Status() != StatusRunning {
		t.Fatalf("task B should still be running while A is canceled, got %s", taskB.Status())
	}
	if err := taskB.Wait(); err != nil {
		t.Fatalf("task B was affected by canceling A: %v", err)
	}
	if taskB.Status() != StatusCompleted {
		t.Fatalf("task B status = %s", taskB.Status())
	}

	// A is fully cleaned, B published its complete artifacts.
	if entries, _ := os.ReadDir(targetA); len(entries) != 0 {
		t.Fatalf("canceled task A left artifacts: %v", entries)
	}
	if !contains(listFiles(t, targetB), "1080p.m3u8") {
		t.Fatalf("task B artifacts missing: %v", listFiles(t, targetB))
	}
	if !contains(listFiles(t, targetB), "playlist.m3u8") {
		t.Fatal("task B master playlist missing")
	}
}

// --- Backwards-compatible synchronous API ----------------------------------

func TestGenerateHLSSynchronous(t *testing.T) {
	ffmpeg := ffmpegExe(t)
	work := t.TempDir()
	src := makeTestVideo(t, ffmpeg, work, "src.mp4", 2)
	target := filepath.Join(work, "out")

	// Success path blocks until ffmpeg really exits.
	if err := GenerateHLS(ffmpeg, src, target, "480p"); err != nil {
		t.Fatalf("GenerateHLS failed: %v", err)
	}
	files := listFiles(t, target)
	if !contains(files, "480p.m3u8") {
		t.Fatalf("variant playlist missing: %v", files)
	}

	// Failure path reports the real ffmpeg exit error, synchronously.
	corrupt := makeCorruptFile(t, work, "broken.mp4")
	target2 := filepath.Join(work, "out2")
	if err := GenerateHLS(ffmpeg, corrupt, target2, "480p"); err == nil {
		t.Fatal("expected error for corrupt input")
	}
	if entries, _ := os.ReadDir(target2); len(entries) != 0 {
		t.Fatalf("failed GenerateHLS left artifacts: %v", entries)
	}

	// Unknown resolution keeps the existing error semantics.
	if err := GenerateHLS(ffmpeg, src, filepath.Join(work, "out3"), "240p"); !errors.Is(err, ErrUnknownResolution) {
		t.Fatalf("expected ErrUnknownResolution, got %v", err)
	}
}

// --- Pure unit tests --------------------------------------------------------

func TestDirLockRegistryRejectsConcurrentOwner(t *testing.T) {
	reg := newDirLockRegistry()
	dir := filepath.Join(t.TempDir(), "x")

	release, err := reg.tryAcquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.tryAcquire(filepath.Clean(dir + string(os.PathSeparator))); !errors.Is(err, ErrTargetBusy) {
		t.Fatalf("expected ErrTargetBusy, got %v", err)
	}
	release()

	release2, err := reg.tryAcquire(dir)
	if err != nil {
		t.Fatalf("lock should be available after release: %v", err)
	}
	release2()

	// Different directories never block each other.
	other := filepath.Join(t.TempDir(), "y")
	rel3, err := reg.tryAcquire(other)
	if err != nil {
		t.Fatal(err)
	}
	rel3()
}

func TestRingBufferKeepsTail(t *testing.T) {
	r := newRingBuffer(8)
	_, _ = r.Write([]byte("abcdefghij"))
	if got := r.String(); got != "cdefghij" {
		t.Fatalf("expected tail cdefghij, got %q", got)
	}
	_, _ = r.Write([]byte("XY"))
	if got := r.String(); got != "efghijXY" {
		t.Fatalf("expected efghijXY, got %q", got)
	}
}

func TestStatusStrings(t *testing.T) {
	cases := map[Status]string{
		StatusRunning:   "running",
		StatusCompleted: "completed",
		StatusFailed:    "failed",
		StatusCanceled:  "canceled",
	}
	for s, want := range cases {
		if s.String() != want {
			t.Fatalf("%d.String() = %q, want %q", s, s.String(), want)
		}
	}
}

// fakeFFmpeg writes a platform-specific wrapper script that pretends to be
// ffmpeg: it exits successfully after creating the variant playlist and one
// segment named via the -hls_segment_filename / final playlist arguments.
// It lets tests drive the publish phase deterministically without real
// transcoding.
func fakeFFmpeg(t *testing.T, dir string) string {
	t.Helper()
	var name, content string
	if runtimeIsWindows() {
		name = filepath.Join(dir, "fakeffmpeg.bat")
		content = `@echo off
setlocal enabledelayedexpansion
set SEG=
set M3U8=
:loop
if "%~1"=="" goto run
if /I "%~1"=="-hls_segment_filename" (set SEG=%~2& shift & shift & goto loop)
if /I "%~1"=="-i" (shift & shift & goto loop)
set M3U8=%~1
shift
goto loop
:run
echo #EXTM3U> "%M3U8%"
echo #EXT-X-ENDLIST>> "%M3U8%"
for %%F in ("%SEG%") do set SEGDIR=%%~dpF
echo fake-segment> "%SEGDIR%fake_000.ts"
exit /b 0
`
	} else {
		name = filepath.Join(dir, "fakeffmpeg.sh")
		content = `#!/bin/sh
seg=""
m3u8=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -hls_segment_filename) seg="$2"; shift 2 ;;
    -i) shift 2 ;;
    *) m3u8="$1"; shift ;;
  esac
done
printf '#EXTM3U\n#EXT-X-ENDLIST\n' > "$m3u8"
echo fake-segment > "$(dirname "$seg")/fake_000.ts"
exit 0
`
	}
	if err := os.WriteFile(name, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return name
}

func runtimeIsWindows() bool { return filepath.Separator == '\\' }

// TestPublishFailureRollsBackArtifacts forces publish() to fail after variant
// "transcoding" succeeded and asserts every artifact the task moved into the
// target directory is rolled back (the failed task must not leave its
// playlists/segments behind).
func TestPublishFailureRollsBackArtifacts(t *testing.T) {
	work := t.TempDir()
	fake := fakeFFmpeg(t, work)
	target := filepath.Join(work, "hls")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	// Deterministic publish failure: the 480p variant playlist destination
	// already exists as a directory, so renaming a file onto it fails on both
	// Windows and Unix (moveFile cannot remove a non-empty directory).
	blockerDir := filepath.Join(target, "480p.m3u8")
	if err := os.MkdirAll(filepath.Join(blockerDir, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}

	task, err := Submit(context.Background(), Config{
		FFmpegPath:     fake,
		SrcPath:        filepath.Join(work, "whatever.mp4"),
		TargetPath:     target,
		Resolutions:    []string{"360p", "480p"},
		MasterPlaylist: "playlist.m3u8",
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	err = task.Wait()
	if err == nil {
		t.Fatal("expected publish failure, got success")
	}
	if task.Status() != StatusFailed {
		t.Fatalf("expected failed status, got %s", task.Status())
	}

	// No task artifacts may remain besides the pre-existing blocker directory.
	files := listFiles(t, target)
	for _, f := range files {
		if strings.HasSuffix(f, ".ts") {
			t.Fatalf("publish failure left segment %s behind", f)
		}
		if f == "360p.m3u8" || f == "playlist.m3u8" {
			t.Fatalf("publish failure left artifact %s behind", f)
		}
	}
	if hasStaging(t, target) {
		t.Fatal("publish failure left staging directory behind")
	}
	// The blocker (external, pre-existing content) must not have been deleted.
	if _, err := os.Stat(filepath.Join(blockerDir, "inner")); err != nil {
		t.Fatalf("pre-existing directory was damaged: %v", err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
