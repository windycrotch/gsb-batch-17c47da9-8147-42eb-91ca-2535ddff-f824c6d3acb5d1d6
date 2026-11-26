package main

// Hermetic, fully automated tests for batch transcoding reliability.
//
// Every test drives the real ffcvt code paths against a fake
// ffmpeg/ffprobe (a Go program built in TestMain) — no real ffmpeg, no
// shell pipeline, no network. The fake can be scripted to succeed,
// fail (leaving partial output), fail on probe, or hang until killed,
// which together exercise partial failure, interruption/resume,
// rerun-with--nc, -maxc and concurrent-process behavior.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	ffcvtBin string // built ffcvt executable (subprocess tests)
	fakeFF   string // fake ffmpeg executable
	fakeFP   string // fake ffprobe executable
)

func TestMain(m *testing.M) {
	// Shorten lock waiting for the whole suite; individual tests that
	// need contention set it explicitly too.
	lockWait = 400 * time.Millisecond
	// Build the fake commands and the ffcvt binary once into a stable
	// directory. They must NOT live in any per-test t.TempDir(), whose
	// cleanup would delete binaries still reused by later tests.
	dir, err := os.MkdirTemp("", "ffcvt-testbin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testbin dir:", err)
		os.Exit(1)
	}
	if err := buildTestTools(dir); err != nil {
		fmt.Fprintln(os.Stderr, "test tool build:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// buildTestTools compiles the fake ffmpeg/ffprobe and ffcvt into dir.
func buildTestTools(dir string) error {
	exe := func(name string) string {
		if runtime.GOOS == "windows" {
			return filepath.Join(dir, name+".exe")
		}
		return filepath.Join(dir, name)
	}
	fakeFF = exe("ffmpeg")
	if out, err := exec.Command("go", "build", "-o", fakeFF,
		"./internal/fakecmd").CombinedOutput(); err != nil {
		return fmt.Errorf("fake ffmpeg: %v: %s", err, out)
	}
	fakeFP = exe("ffprobe")
	if err := copyFileForTest(fakeFF, fakeFP); err != nil {
		return err
	}
	ffcvtBin = exe("ffcvt")
	if out, err := exec.Command("go", "build", "-o", ffcvtBin, ".").
		CombinedOutput(); err != nil {
		return fmt.Errorf("ffcvt: %v: %s", err, out)
	}
	return nil
}

// buildTools is retained as a no-op-safe entry point: binaries are built
// once in TestMain, so tests simply assert they exist.
func buildTools(t *testing.T) {
	t.Helper()
	if ffcvtBin == "" || !fileExist(ffcvtBin) {
		t.Fatal("test binaries were not built in TestMain")
	}
}

func copyFileForTest(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, in, 0755)
}

// ---- test environment helpers ----

// FakeControl mirrors internal/fakecmd.Control via its JSON contract.
type FakeControl struct {
	FailProbe     bool                `json:"fail_probe"`
	FailInputs    map[string]FailSpec `json:"fail_inputs"`
	HangInputs    map[string]int      `json:"hang_inputs"`
	OutputSize    int                 `json:"output_size"`
	CounterDir    string              `json:"counter_dir"`
	EncodeDelayMs int                 `json:"encode_delay_ms"`
	BarrierFile   string              `json:"barrier_file"`
	ReadyDir      string              `json:"ready_dir"`
}

// FailSpec mirrors internal/fakecmd.FailSpec.
type FailSpec struct {
	PartialBytes int  `json:"partial_bytes"`
	FailOnce     bool `json:"fail_once"`
}

type testEnv struct {
	src      string
	control  string
	counters string
	inputs   map[string]int64 // base name -> size
}

func newTestEnv(t *testing.T, c FakeControl) *testEnv {
	t.Helper()
	buildTools(t)
	root := t.TempDir()
	env := &testEnv{
		src:      filepath.Join(root, "src"),
		counters: filepath.Join(root, "counters"),
		inputs:   map[string]int64{},
	}
	if err := os.MkdirAll(env.src, 0755); err != nil {
		t.Fatal(err)
	}
	// control file
	env.control = filepath.Join(root, "control.json")
	data, _ := json.Marshal(c)
	if err := os.WriteFile(env.control, data, 0644); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *testEnv) mkInput(t *testing.T, name string, size int64) {
	t.Helper()
	p := filepath.Join(e.src, name)
	if err := os.WriteFile(p, []byte(strings.Repeat("I", int(size))), 0644); err != nil {
		t.Fatal(err)
	}
	e.inputs[name] = size
}

func (e *testEnv) outputFor(input string) string {
	base := strings.TrimSuffix(input, filepath.Ext(input))
	return filepath.Join(e.src, base+encodedExt)
}

// resetGlobals restores every piece of package-level state a batch run
// touches, so tests are independent of each other and of init order.
func resetGlobals(dir string) {
	vidCol = videoCol{}
	totalOrg, totalNew = 1, 1
	workDirs = nil
	cutOps = ""
	transpFrom, transpTo = "", ""
	encodedExt = _encodedExt
	rerunJournal = nil
	Opts = Options{
		Target:     "",
		Directory:  dir,
		Exts:       ".AVI.MKV.MP4.WEBM.MOV",
		FFMpeg:     fakeFF,
		FFProbe:    fakeFP,
		Debug:      0,
		BreathTime: 0,
		Lang:       "eng",
		Seg:        "",
		NoClobber:  false,
		MaxC:       0,
		NoExec:     false,
	}
	Opts.Ext = _encodedExt
}

func (e *testEnv) runInProcess(t *testing.T) (int, *Journal, BatchStats) {
	t.Helper()
	resetGlobals(e.src)
	// point the fake at its script
	t.Setenv("FFCVT_FAKE_CONTROL", e.control)
	// Load durable state before queueing, exactly as main() does, so
	// -nc skip classification works inside the walk.
	rerunJournal = loadJournal()
	if err := filepath.Walk(e.src, visit); err != nil {
		t.Fatal(err)
	}
	var stats BatchStats
	code := runBatchWithStats(time.Now(), &stats)
	j := loadJournal()
	return code, j, stats
}

// runBatchWithStats mirrors runBatch but also hands back the stats
// struct. runBatch itself keeps the production signature.
func runBatchWithStats(start time.Time, out *BatchStats) int {
	code := runBatch(start)
	*out = lastBatchStats
	return code
}

func (e *testEnv) countInvocations(t *testing.T, input string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.counters,
		filepath.Base(input)+".count"))
	if err != nil {
		return 0
	}
	var n int
	for _, ch := range string(data) {
		if ch >= '0' && ch <= '9' {
			n = n*10 + int(ch-'0')
		}
	}
	return n
}

func (e *testEnv) noLeftoverArtifacts(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(e.src)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		n := en.Name()
		if strings.Contains(n, ".ffcvt-tmp-") ||
			strings.HasSuffix(n, ".ffcvt-lock") {
			t.Errorf("leftover transient artifact: %s", n)
		}
	}
}

func readJournal(t *testing.T, dir string) *Journal {
	t.Helper()
	// loadJournal() derives its path from global Opts; save and restore
	// it so reading one test's journal cannot leak state into the next.
	savedDir, savedW := Opts.Directory, Opts.WDirectory
	Opts.Directory, Opts.WDirectory = dir, ""
	defer func() { Opts.Directory, Opts.WDirectory = savedDir, savedW }()
	return loadJournal()
}

// ---- tests ----

// All inputs succeed: every output exists, is fully readable, journal
// records success, and the totals reconcile.
func TestAllSuccess(t *testing.T) {
	env := newTestEnv(t, FakeControl{OutputSize: 4096, CounterDir: ""})
	env.counters = filepath.Join(t.TempDir(), "c")
	env.mkInput(t, "a.avi", 100*1024)
	env.mkInput(t, "b.avi", 200*1024)
	env.mkInput(t, "c.mkv", 50*1024)

	code, j, stats := env.runInProcess(t)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}
	if stats.Success != 3 || stats.Failure != 0 || stats.Skip != 0 {
		t.Fatalf("stats = %+v, want 3 success", stats)
	}
	for in := range env.inputs {
		out := env.outputFor(in)
		size, err := verifyReadable(out)
		if err != nil {
			t.Fatalf("output for %s not readable: %v", in, err)
		}
		if size != 4096 {
			t.Errorf("output size for %s = %d, want 4096", in, size)
		}
		key, _ := filepath.Abs(filepath.Join(env.src, in))
		if j.Files[key] == nil || j.Files[key].Result != ResultSuccess {
			t.Errorf("journal missing success for %s: %+v", in, j.Files[key])
		}
	}
	// savings denominator covers the successful inputs only
	var wantOrg int64 = 350 * 1024
	if stats.OrgBytes != wantOrg {
		t.Errorf("OrgBytes = %d, want %d", stats.OrgBytes, wantOrg)
	}
	if stats.NewBytes != 3*4096 {
		t.Errorf("NewBytes = %d, want %d", stats.NewBytes, 3*4096)
	}
	env.noLeftoverArtifacts(t)
}

// One input fails after writing a large partial file: the batch keeps
// going, the partial output is removed, the failure is journaled, and
// the failed input's bytes stay out of the space-savings math.
func TestPartialFailureIsContainedAndCleaned(t *testing.T) {
	env := newTestEnv(t, FakeControl{
		OutputSize: 1024,
		FailInputs: map[string]FailSpec{
			"b.avi": {PartialBytes: 200 * 1024}, // much larger than the old 500B threshold
		},
	})
	env.mkInput(t, "a.avi", 100*1024)
	env.mkInput(t, "b.avi", 200*1024)
	env.mkInput(t, "c.avi", 300*1024)

	code, j, stats := env.runInProcess(t)
	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if stats.Success != 2 || stats.Failure != 1 {
		t.Fatalf("stats = %+v, want 2 success / 1 failure", stats)
	}
	// failed output must not exist under any name at the destination
	if _, err := os.Stat(env.outputFor("b.avi")); !os.IsNotExist(err) {
		t.Fatalf("failed output left behind: %v", err)
	}
	keyB, _ := filepath.Abs(filepath.Join(env.src, "b.avi"))
	if got := j.Files[keyB]; got == nil || got.Result != ResultFailure || got.Error == "" {
		t.Fatalf("journal failure entry wrong: %+v", got)
	}
	// successful outputs still complete
	for _, in := range []string{"a.avi", "c.avi"} {
		if _, err := verifyReadable(env.outputFor(in)); err != nil {
			t.Fatalf("output for %s unreadable: %v", in, err)
		}
	}
	// failed bytes (200 KiB) excluded from both savings terms
	if stats.FailBytes != 200*1024 {
		t.Errorf("FailBytes = %d, want %d", stats.FailBytes, 200*1024)
	}
	if stats.OrgBytes != 400*1024 {
		t.Errorf("OrgBytes = %d, want %d (failed input excluded)",
			stats.OrgBytes, 400*1024)
	}
	env.noLeftoverArtifacts(t)
}

// A probe failure is a normal per-file failure, not a batch abort.
func TestProbeFailureContinuesBatch(t *testing.T) {
	env := newTestEnv(t, FakeControl{
		OutputSize: 1024,
		FailProbe:  true,
	})
	env.mkInput(t, "a.avi", 1024)
	env.mkInput(t, "b.avi", 1024)

	code, _, stats := env.runInProcess(t)
	if code != exitFailure {
		t.Fatalf("exit = %d, want failure", code)
	}
	if stats.Failure != 2 || stats.Success != 0 {
		t.Fatalf("stats = %+v, want 2 failures", stats)
	}
	for _, in := range []string{"a.avi", "b.avi"} {
		if _, err := os.Stat(env.outputFor(in)); !os.IsNotExist(err) {
			t.Errorf("probe failure left output for %s", in)
		}
	}
}

// -nc rerun: previously successful files are skipped (ffmpeg never
// re-invoked for them); a previously failed file is retried.
func TestNoClobberRerunSkipsSuccessRetriesFailure(t *testing.T) {
	counterDir := filepath.Join(t.TempDir(), "cnt")
	env := newTestEnv(t, FakeControl{
		OutputSize: 2048,
		CounterDir: counterDir,
		FailInputs: map[string]FailSpec{
			"b.avi": {PartialBytes: 64, FailOnce: true},
		},
	})
	env.mkInput(t, "a.avi", 100*1024)
	env.mkInput(t, "b.avi", 100*1024)
	env.mkInput(t, "c.avi", 100*1024)

	if code, _, stats := env.runInProcess(t); code != exitFailure ||
		stats.Success != 2 || stats.Failure != 1 {
		t.Fatalf("first run stats = %+v code = %d", stats, code)
	}

	// Second run with -nc: failure retried (now succeeds), successes skip.
	resetGlobals(env.src)
	Opts.NoClobber = true
	env.counters = counterDir
	t.Setenv("FFCVT_FAKE_CONTROL", env.control)
	rerunJournal = loadJournal()
	if err := filepath.Walk(env.src, visit); err != nil {
		t.Fatal(err)
	}
	var stats BatchStats
	code := runBatchWithStats(time.Now(), &stats)
	if code != exitOK {
		t.Fatalf("rerun exit = %d, want OK", code)
	}
	if stats.Success != 1 || stats.Skip != 2 || stats.Failure != 0 {
		t.Fatalf("rerun stats = %+v, want 1 success / 2 skip", stats)
	}
	if n := env.countInvocations(t, "a.avi"); n != 1 {
		t.Errorf("a.avi encoded %d times, want exactly 1 (skipped on rerun)", n)
	}
	if n := env.countInvocations(t, "c.avi"); n != 1 {
		t.Errorf("c.avi encoded %d times, want exactly 1 (skipped on rerun)", n)
	}
	if n := env.countInvocations(t, "b.avi"); n != 2 {
		t.Errorf("b.avi encoded %d times, want 2 (failed then retried)", n)
	}
	if _, err := verifyReadable(env.outputFor("b.avi")); err != nil {
		t.Fatalf("retried output unreadable: %v", err)
	}
}

// Interruption mid-batch: completed results are retained, the in-flight
// file is cleaned up and not marked successful, untouched files are
// untouched. A later run resumes from the remaining files.
func TestInterruptionResumesFromRemaining(t *testing.T) {
	env := newTestEnv(t, FakeControl{
		OutputSize: 2048,
		HangInputs: map[string]int{"b.avi": 1},
	})
	env.mkInput(t, "a.avi", 1024)
	env.mkInput(t, "b.avi", 1024)
	env.mkInput(t, "c.avi", 1024)
	env.mkInput(t, "d.avi", 1024)

	resetGlobals(env.src)
	t.Setenv("FFCVT_FAKE_CONTROL", env.control)
	if err := filepath.Walk(env.src, visit); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	batchCancel = &contextCanceller{c: ctx}
	go func() {
		time.Sleep(400 * time.Millisecond)
		cancel()
	}()
	code := runBatch(time.Now())
	if code != exitInterrupted {
		t.Fatalf("interrupted run exit = %d, want %d", code, exitInterrupted)
	}
	batchCancel = nil

	// a.avi finished; b.avi cleaned up; c.avi/d.avi never started.
	if _, err := verifyReadable(env.outputFor("a.avi")); err != nil {
		t.Fatalf("completed output lost after interrupt: %v", err)
	}
	if _, err := os.Stat(env.outputFor("b.avi")); !os.IsNotExist(err) {
		t.Fatalf("in-flight output not cleaned up")
	}
	for _, in := range []string{"c.avi", "d.avi"} {
		if _, err := os.Stat(env.outputFor(in)); !os.IsNotExist(err) {
			t.Errorf("untouched %s has an output", in)
		}
	}
	j := readJournal(t, env.src)
	keyB, _ := filepath.Abs(filepath.Join(env.src, "b.avi"))
	if st := j.Files[keyB]; st != nil && st.Result == ResultSuccess {
		t.Fatalf("interrupted file recorded as success: %+v", st)
	}
	keyC, _ := filepath.Abs(filepath.Join(env.src, "c.avi"))
	if st := j.Files[keyC]; st != nil {
		t.Fatalf("untouched file has a journal entry: %+v", st)
	}
	env.noLeftoverArtifacts(t)

	// Resume with -nc and the hang removed: b/c/d get done, the already
	// successful a.avi is skipped. Reuse the same directory and journal.
	resumeControl := writeControl(t, FakeControl{OutputSize: 2048})
	resetGlobals(env.src)
	Opts.NoClobber = true
	t.Setenv("FFCVT_FAKE_CONTROL", resumeControl)
	rerunJournal = loadJournal()
	if err := filepath.Walk(env.src, visit); err != nil {
		t.Fatal(err)
	}
	var stats2 BatchStats
	code2 := runBatchWithStats(time.Now(), &stats2)
	j2 := loadJournal()
	if code2 != exitOK {
		t.Fatalf("resume exit = %d", code2)
	}
	if stats2.Success != 3 || stats2.Skip != 1 {
		t.Fatalf("resume stats = %+v, want 3 success / 1 skip", stats2)
	}
	for _, in := range []string{"b.avi", "c.avi", "d.avi"} {
		if _, err := verifyReadable(env.outputFor(in)); err != nil {
			t.Errorf("resumed output for %s unreadable: %v", in, err)
		}
		key, _ := filepath.Abs(filepath.Join(env.src, in))
		if j2.Files[key] == nil || j2.Files[key].Result != ResultSuccess {
			t.Errorf("journal missing resumed success for %s", in)
		}
	}
}

// -maxc bounds conversions but unprocessed files are not successes and
// the summary still prints (no os.Exit short-circuit).
func TestMaxCLimitAndSummary(t *testing.T) {
	env := newTestEnv(t, FakeControl{OutputSize: 1024})
	for _, n := range []string{"a.avi", "b.avi", "c.avi", "d.avi"} {
		env.mkInput(t, n, 1024)
	}
	resetGlobals(env.src)
	Opts.MaxC = 2
	t.Setenv("FFCVT_FAKE_CONTROL", env.control)
	if err := filepath.Walk(env.src, visit); err != nil {
		t.Fatal(err)
	}
	var stats BatchStats
	code := runBatchWithStats(time.Now(), &stats)
	if code != exitOK {
		t.Fatalf("maxc exit = %d", code)
	}
	if stats.Success != 2 || stats.Failure != 0 || stats.Skip != 0 {
		t.Fatalf("stats = %+v, want exactly 2 successes", stats)
	}
	existing := 0
	for _, n := range []string{"a.avi", "b.avi", "c.avi", "d.avi"} {
		if fileExist(env.outputFor(n)) {
			existing++
		}
	}
	if existing != 2 {
		t.Errorf("outputs on disk = %d, want 2", existing)
	}
	j := readJournal(t, env.src)
	if len(j.Files) != 2 {
		t.Errorf("journal entries = %d, want 2 (unprocessed not recorded)", len(j.Files))
	}
}

// -maxc with a failure inside the budget: the failure counts, files past
// the budget are not successes, and the summary still reconciles.
func TestMaxCWithFailureDoesNotCountUnprocessed(t *testing.T) {
	env := newTestEnv(t, FakeControl{
		OutputSize: 1024,
		FailInputs: map[string]FailSpec{
			"b.avi": {PartialBytes: 100},
		},
	})
	env.mkInput(t, "a.avi", 10*1024)
	env.mkInput(t, "b.avi", 20*1024) // fails
	env.mkInput(t, "c.avi", 30*1024) // would be 3rd: beyond maxc=2
	env.mkInput(t, "d.avi", 40*1024) // unprocessed

	resetGlobals(env.src)
	Opts.MaxC = 2
	t.Setenv("FFCVT_FAKE_CONTROL", env.control)
	if err := filepath.Walk(env.src, visit); err != nil {
		t.Fatal(err)
	}
	var stats BatchStats
	code := runBatchWithStats(time.Now(), &stats)
	if code != exitFailure {
		t.Fatalf("exit = %d, want failure (one file failed)", code)
	}
	if stats.Success != 1 || stats.Failure != 1 || stats.Skip != 0 {
		t.Fatalf("stats = %+v, want 1 success / 1 failure / 0 skip", stats)
	}
	// c.avi and d.avi must not have outputs or success journal entries.
	for _, n := range []string{"c.avi", "d.avi"} {
		if fileExist(env.outputFor(n)) {
			t.Errorf("unprocessed %s has an output", n)
		}
		key, _ := filepath.Abs(filepath.Join(env.src, n))
		if st := loadJournal().Files[key]; st != nil {
			t.Errorf("unprocessed %s journaled: %+v", n, st)
		}
	}
}

// Mixed batch totals reconcile byte-for-byte: success bytes drive the
// savings denominator/numerator, failed bytes are tracked separately and
// skipped bytes are neither.
func TestSummaryByteReconciliation(t *testing.T) {
	env := newTestEnv(t, FakeControl{
		OutputSize: 4096,
		FailInputs: map[string]FailSpec{
			"bad.mkv": {PartialBytes: 5000},
		},
	})
	// two successes, one failure, then pre-seed one skip via -nc rerun
	env.mkInput(t, "ok1.avi", 50*1024)
	env.mkInput(t, "ok2.avi", 70*1024)
	env.mkInput(t, "bad.mkv", 90*1024)

	code, _, stats := env.runInProcess(t)
	if code != exitFailure {
		t.Fatalf("exit = %d, want failure", code)
	}
	if stats.Success != 2 || stats.Failure != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	// explicit reconciliation assertions
	var wantOrg int64 = 120 * 1024 // only the two successful inputs
	var wantNew int64 = 2 * 4096
	var wantFail int64 = 90 * 1024
	if stats.OrgBytes != wantOrg || stats.NewBytes != wantNew ||
		stats.FailBytes != wantFail {
		t.Fatalf("byte totals = org %d (want %d), new %d (want %d), "+
			"fail %d (want %d)",
			stats.OrgBytes, wantOrg, stats.NewBytes, wantNew,
			stats.FailBytes, wantFail)
	}
	// saved percentage uses success-only terms
	wantPct := (wantOrg - wantNew) * 100 / wantOrg
	if got := (stats.OrgBytes - stats.NewBytes) * 100 / stats.OrgBytes; got != wantPct {
		t.Errorf("saved pct = %d, want %d", got, wantPct)
	}
	// counts add up to every queued file exactly once
	if stats.Success+stats.Failure+stats.Skip != 3 {
		t.Errorf("outcomes do not partition the 3 inputs: %+v", stats)
	}
	// rerun with -nc: the two successes become skips, the failure retries
	resetGlobals(env.src)
	Opts.NoClobber = true
	t.Setenv("FFCVT_FAKE_CONTROL", writeControl(t, FakeControl{OutputSize: 4096}))
	rerunJournal = loadJournal()
	if err := filepath.Walk(env.src, visit); err != nil {
		t.Fatal(err)
	}
	var rerun BatchStats
	if rc := runBatchWithStats(time.Now(), &rerun); rc != exitOK {
		t.Fatalf("rerun exit = %d", rc)
	}
	if rerun.Success != 1 || rerun.Skip != 2 || rerun.Failure != 0 {
		t.Fatalf("rerun stats = %+v, want 1 success / 2 skip", rerun)
	}
	if rerun.SkipBytes != wantOrg {
		t.Errorf("skip bytes = %d, want %d", rerun.SkipBytes, wantOrg)
	}
}

// A live lock held by another process causes a safe skip, never
// corruption or an overwrite.
func TestLiveLockIsSafelySkipped(t *testing.T) {
	env := newTestEnv(t, FakeControl{OutputSize: 1024})
	env.mkInput(t, "a.avi", 1024)
	env.mkInput(t, "b.avi", 1024)

	resetGlobals(env.src)
	t.Setenv("FFCVT_FAKE_CONTROL", env.control)
	if err := filepath.Walk(env.src, visit); err != nil {
		t.Fatal(err)
	}
	// Simulate another live process holding b.avi's output lock.
	lockP := lockPathFor(env.outputFor("b.avi"))
	if err := os.MkdirAll(filepath.Dir(lockP), 0755); err != nil {
		t.Fatal(err)
	}
	data := lockData{pid: os.Getpid(), startedAt: time.Now()}.serialize()
	if err := os.WriteFile(lockP, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lockP)

	var stats BatchStats
	code := runBatchWithStats(time.Now(), &stats)
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if stats.Success != 1 || stats.Skip != 1 || stats.Locked != 1 {
		t.Fatalf("stats = %+v, want 1 success / 1 locked skip", stats)
	}
	if fileExist(env.outputFor("b.avi")) {
		t.Error("locked file was overwritten with an output")
	}
}

// A stale lock whose owner is dead is reclaimed and work proceeds.
func TestStaleLockIsReclaimed(t *testing.T) {
	env := newTestEnv(t, FakeControl{OutputSize: 1024})
	env.mkInput(t, "a.avi", 1024)

	// pick a pid that is not alive
	deadPID := os.Getpid() + 100000
	for processAlive(deadPID) {
		deadPID++
	}
	resetGlobals(env.src)
	t.Setenv("FFCVT_FAKE_CONTROL", env.control)
	if err := filepath.Walk(env.src, visit); err != nil {
		t.Fatal(err)
	}
	lockP := lockPathFor(env.outputFor("a.avi"))
	if err := os.MkdirAll(filepath.Dir(lockP), 0755); err != nil {
		t.Fatal(err)
	}
	stale := lockData{pid: deadPID, startedAt: time.Now()}.serialize()
	if err := os.WriteFile(lockP, []byte(stale), 0644); err != nil {
		t.Fatal(err)
	}

	var stats BatchStats
	code := runBatchWithStats(time.Now(), &stats)
	if code != exitOK || stats.Success != 1 {
		t.Fatalf("code=%d stats=%+v, want reclaimed success", code, stats)
	}
	if fileExist(lockP) {
		t.Error("reclaimed lock file left behind")
	}
}

// Dry run (-n) prints commands but creates no outputs and no journal.
func TestNoExecTouchesNothing(t *testing.T) {
	env := newTestEnv(t, FakeControl{OutputSize: 1024})
	env.mkInput(t, "a.avi", 1024)
	resetGlobals(env.src)
	Opts.NoExec = true
	t.Setenv("FFCVT_FAKE_CONTROL", env.control)
	if err := filepath.Walk(env.src, visit); err != nil {
		t.Fatal(err)
	}
	runBatch(time.Now())
	entries, _ := os.ReadDir(env.src)
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), "_.mkv") ||
			strings.HasSuffix(en.Name(), ".json") {
			t.Errorf("dry run created artifact: %s", en.Name())
		}
	}
}

// Segment mode (-Seg) writes multiple numbered parts atomically: on
// success every part is renamed to its final name and verified; a
// failure leaves neither temp parts nor final parts behind.
func TestSegmentModeSuccessAndFailure(t *testing.T) {
	// Success path
	env := newTestEnv(t, FakeControl{OutputSize: 1024})
	env.mkInput(t, "a.avi", 5000)
	resetGlobals(env.src)
	Opts.Seg = "00:10:00"
	t.Setenv("FFCVT_FAKE_CONTROL", env.control)
	if err := filepath.Walk(env.src, visit); err != nil {
		t.Fatal(err)
	}
	var stats BatchStats
	if code := runBatchWithStats(time.Now(), &stats); code != exitOK {
		t.Fatalf("segment run exit = %d", code)
	}
	if stats.Success != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	// final segment parts present and readable, no temp parts
	base := filepath.Join(env.src, "a")
	for _, idx := range []string{"00", "01"} {
		final := fmt.Sprintf("%s%s_.mkv", base, idx)
		if _, err := verifyReadable(final); err != nil {
			t.Errorf("final segment %s unreadable: %v", final, err)
		}
	}
	entries, _ := os.ReadDir(env.src)
	for _, en := range entries {
		if strings.Contains(en.Name(), ".ffcvt-tmp-") {
			t.Errorf("temp segment left behind: %s", en.Name())
		}
	}

	// Failure path: partial segments must be fully cleaned.
	env2 := newTestEnv(t, FakeControl{
		OutputSize: 1024,
		FailInputs: map[string]FailSpec{"b.avi": {PartialBytes: 100}},
	})
	env2.mkInput(t, "b.avi", 5000)
	resetGlobals(env2.src)
	Opts.Seg = "00:10:00"
	t.Setenv("FFCVT_FAKE_CONTROL", env2.control)
	if err := filepath.Walk(env2.src, visit); err != nil {
		t.Fatal(err)
	}
	var st2 BatchStats
	if code := runBatchWithStats(time.Now(), &st2); code != exitFailure {
		t.Fatalf("failing segment run exit = %d", code)
	}
	if st2.Failure != 1 {
		t.Fatalf("stats = %+v", st2)
	}
	left, _ := os.ReadDir(env2.src)
	for _, en := range left {
		nm := en.Name()
		if strings.HasSuffix(nm, "_.mkv") || strings.Contains(nm, ".ffcvt-tmp-") {
			t.Errorf("failed segment run left artifact: %s", nm)
		}
	}
}

// Work-directory (-w) mode places outputs and the journal under the
// re-rooted work tree and still supports -nc rerun.
func TestWorkDirectoryModeAndRerun(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	out := filepath.Join(root, "out")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"q.avi", filepath.Join("sub", "r.avi")} {
		if err := os.WriteFile(filepath.Join(src, p),
			[]byte(strings.Repeat("W", 3000)), 0644); err != nil {
			t.Fatal(err)
		}
	}
	control := filepath.Join(root, "control.json")
	if err := os.WriteFile(control,
		[]byte(`{"output_size":1000}`), 0644); err != nil {
		t.Fatal(err)
	}

	run := func(nc bool) BatchStats {
		resetGlobals(src)
		Opts.WDirectory = out
		Opts.NoClobber = nc
		// Mirror main()'s work-dir re-rooting and encodedExt adjustment.
		Opts.WDirectory = filepath.Join(Opts.WDirectory,
			filepath.Base(filepath.Clean(src)))
		encodedExt = ".mkv"
		t.Setenv("FFCVT_FAKE_CONTROL", control)
		rerunJournal = loadJournal()
		if err := filepath.Walk(src, visit); err != nil {
			t.Fatal(err)
		}
		var st BatchStats
		if code := runBatchWithStats(time.Now(), &st); code != exitOK {
			t.Fatalf("run (nc=%v) exit = %d", nc, code)
		}
		return st
	}

	first := run(false)
	if first.Success != 2 {
		t.Fatalf("first run stats = %+v, want 2 success", first)
	}
	// outputs live under out/src, preserving the sub/ structure
	for _, rel := range []string{"q.mkv", filepath.Join("sub", "r.mkv")} {
		if _, err := verifyReadable(filepath.Join(out, "src", rel)); err != nil {
			t.Errorf("work-dir output %s unreadable: %v", rel, err)
		}
	}
	// journal sits at the root of the output tree
	if _, err := os.Stat(filepath.Join(out, "src", ".ffcvt-state.json")); err != nil {
		t.Errorf("journal not at output root: %v", err)
	}
	second := run(true)
	if second.Skip != 2 || second.Success != 0 {
		t.Fatalf("nc rerun stats = %+v, want 2 skip", second)
	}
}

// Two real ffcvt processes on the same directory never corrupt an
// output: every final file is complete and exactly one process wins
// each output.
func TestTwoConcurrentProcesses(t *testing.T) {
	buildTools(t)
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	names := []string{"a.avi", "b.avi", "c.avi", "d.avi",
		"e.avi", "f.avi", "g.avi", "h.avi"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(src, n),
			[]byte(strings.Repeat("Z", 4096)), 0644); err != nil {
			t.Fatal(err)
		}
	}
	control := filepath.Join(root, "control.json")
	// A per-encode delay keeps both processes alive and overlapping; the
	// counter directory records every fake ffmpeg invocation so the test
	// can prove no input is encoded twice or left corrupted.
	counterDir := filepath.Join(root, "counters")
	ctrl := FakeControl{
		OutputSize:    2048,
		EncodeDelayMs: 200,
		CounterDir:    counterDir,
	}
	cdata, _ := json.Marshal(ctrl)
	if err := os.WriteFile(control, cdata, 0644); err != nil {
		t.Fatal(err)
	}

	runOne := func() *exec.Cmd {
		cmd := exec.Command(ffcvtBin,
			"-d", src, "-t", "", "-debug", "0", "-bt", "0",
			"-ffmpeg", fakeFF, "-ffprobe", fakeFP,
		)
		cmd.Env = append(os.Environ(), "FFCVT_FAKE_CONTROL="+control)
		return cmd
	}

	c1, c2 := runOne(), runOne()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	outputs := make([][]byte, 2)
	startOne := func(idx int, cmd *exec.Cmd) {
		defer wg.Done()
		out, err := cmd.CombinedOutput()
		outputs[idx], errs[idx] = out, err
	}
	wg.Add(2)
	go startOne(0, c1)
	go startOne(1, c2)
	wg.Wait()
	// A run that had to skip locked files still exits OK; a run with a
	// real failure would exit 1.
	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent process %d failed: %v", i, e)
		}
	}
	// Running concurrently, the later process must take a safe path for
	// files the earlier one owns — backing off an active lock ("locked by
	// another process") or skipping once the holder finishes — rather
	// than corrupting any output. Both are reported for visibility; the
	// hard correctness assertions below (single encode, intact bytes)
	// are what actually guarantee the requirement.
	combined := string(outputs[0]) + string(outputs[1])
	lockSkips := strings.Count(combined, "locked by another process")
	summarySkips := strings.Count(combined, "Skipping [")
	t.Logf("contention observed: %d active-lock backoffs, %d completion skips",
		lockSkips, summarySkips)

	// Every output exists, is fully readable, has the exact fake size,
	// and its bytes match the deterministic fake payload end to end
	// (interleaved/overwritten writes would corrupt this).
	wantPayload := strings.Repeat("FAKEDATA-", (2048/9)+1)[:2048]
	for _, n := range names {
		out := filepath.Join(src,
			strings.TrimSuffix(n, ".avi")+_encodedExt)
		size, err := verifyReadable(out)
		if err != nil {
			t.Fatalf("concurrent output %s unreadable: %v", n, err)
		}
		if size != 2048 {
			t.Errorf("concurrent output %s size = %d, want 2048", n, size)
		}
		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("read concurrent output %s: %v", n, err)
		}
		if string(got) != wantPayload {
			t.Errorf("concurrent output %s content corrupted/interleaved", n)
		}
	}
	// No locks or temp files survive.
	entries, _ := os.ReadDir(src)
	for _, en := range entries {
		nm := en.Name()
		if strings.HasSuffix(nm, ".ffcvt-lock") ||
			strings.Contains(nm, ".ffcvt-tmp-") {
			t.Errorf("concurrent run left artifact: %s", nm)
		}
	}
	// Every input was encoded at most once across both processes: the
	// locks must have prevented the two processes from both writing the
	// same output. (Skipped inputs were never handed to ffmpeg at all.)
	totalEncodes := 0
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(counterDir, n+".count"))
		if err != nil {
			continue // a skipped input has no counter
		}
		var cnt int
		fmt.Sscanf(string(data), "%d", &cnt)
		if cnt > 1 {
			t.Errorf("input %s was encoded %d times across the two processes, want <= 1",
				n, cnt)
		}
		totalEncodes += cnt
	}
	if totalEncodes != len(names) {
		t.Errorf("total encodes = %d, want %d (one per input)",
			totalEncodes, len(names))
	}
	// The journal must show success for every input exactly once.
	j := readJournal(t, src)
	if len(j.Files) != len(names) {
		t.Fatalf("journal has %d entries, want %d", len(j.Files), len(names))
	}
	for _, n := range names {
		key, _ := filepath.Abs(filepath.Join(src, n))
		if st := j.Files[key]; st == nil || st.Result != ResultSuccess {
			t.Errorf("journal entry for %s = %+v", n, st)
		}
	}
}

// ---- small helpers shared by tests ----

func writeControl(t *testing.T, c FakeControl) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "control.json")
	data, _ := json.Marshal(c)
	if err := os.WriteFile(p, data, 0644); err != nil {
		t.Fatal(err)
	}
	return p
}
