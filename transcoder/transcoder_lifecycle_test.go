package transcoder

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/xfrr/goffmpeg"
)

// newTestTranscoder wires a Transcoder with an in-memory media file and the
// injected fake process factory, so no real ffmpeg/ffprobe binary is required.
func newTestTranscoder(t *testing.T, duration string) (*Transcoder, *fakeProcess) {
	t.Helper()
	factory, fp := newFakeFactory()

	tr := &Transcoder{}
	tr.SetConfiguration(goffmpeg.Configuration{})
	tr.newProcess = factory
	tr.stopGrace = 200 * time.Millisecond
	tr.SetMediaFile(newMediaWithDuration(duration))
	return tr, fp
}

// awaitDone waits for the terminal result of a run.
func awaitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("done channel never delivered its result")
		return nil
	}
}

// emitProgress writes one ffmpeg line through the fake process and blocks until
// the transcoder actually forwards a corresponding Progress on its output
// channel. This gives deterministic ordering: when it returns, the value is
// guaranteed visible, so a subsequent Stop/natural-exit cannot race it away.
func emitProgress(t *testing.T, fp *fakeProcess, out <-chan Progress) Progress {
	t.Helper()
	ready := fp.emitProgress(sampleProgressLine + "\r")
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("fake process never buffered the progress line")
	}
	select {
	case p := <-out:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("transcoder never forwarded the progress update")
		return Progress{}
	}
}

// A full healthy run: progress values flow, natural completion closes both
// channels exactly once and releases the stderr pipe.
func TestRun_NaturalCompletion(t *testing.T) {
	tr, fp := newTestTranscoder(t, "10")

	done := tr.Run(true)
	out := tr.Output()

	// Each call blocks until the value is actually forwarded on the channel.
	first := emitProgress(t, fp, out)
	second := emitProgress(t, fp, out)
	if first.FramesProcessed != "120" || first.CurrentBitrate == "" {
		t.Fatalf("progress fields not populated: %+v", first)
	}

	fp.closeStderr()
	fp.exit(nil) // natural, successful exit releases proc.Wait()
	if err := awaitDone(t, done); err != nil {
		t.Fatalf("expected nil terminal error, got %v", err)
	}

	// After completion the channel must be closed (no further values, no hang).
	assertChannelClosed(t, out)
	if second.FramesProcessed != "120" {
		t.Fatalf("second update not delivered: %+v", second)
	}
	if fp.stderrHandle.closeCount() != 1 {
		t.Errorf("stderr pipe close count = %d, want exactly 1", fp.stderrHandle.closeCount())
	}
}

// assertChannelClosed fails if a value can still be received (channel open).
func assertChannelClosed(t *testing.T, out <-chan Progress) {
	t.Helper()
	select {
	case p, ok := <-out:
		if ok {
			t.Fatalf("expected closed progress channel, got extra value %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("progress channel did not close after run ended")
	}
}

// Stopping must close the progress channel promptly, even though the fake
// process's stderr writer is never closed; the watchdog must then hard-kill the
// process because it ignores the graceful "q".
func TestRun_StopClosesProgressAndKills(t *testing.T) {
	tr, fp := newTestTranscoder(t, "10")

	done := tr.Run(true)
	out := tr.Output()

	// Wait until the update is genuinely delivered before stopping, so the
	// assertion below cannot be lost to a scheduling race.
	p := emitProgress(t, fp, out)
	if p.FramesProcessed != "120" {
		t.Fatalf("unexpected progress value: %+v", p)
	}

	if err := tr.Stop(); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	// The channel must close promptly even though the fake's stderr writer is
	// never closed by the process.
	assertChannelClosed(t, out)

	// Watchdog should force-kill since the fake never exits on its own.
	select {
	case <-fp.killed:
	case <-time.After(2 * time.Second):
		t.Fatal("process was not force-killed after grace period")
	}

	awaitDone(t, done)

	if !fp.receivedGracefulQ() {
		t.Error("Stop did not send the graceful q request on stdin")
	}
	if fp.stderrHandle.closeCount() != 1 {
		t.Errorf("stderr pipe close count = %d, want exactly 1", fp.stderrHandle.closeCount())
	}

	// Stop must be idempotent: a second call is a harmless no-op, no panic.
	if err := tr.Stop(); err != nil {
		t.Errorf("second Stop returned error: %v", err)
	}
}

// When Stop and natural completion happen together, both channels must close
// exactly once with no panic or block, regardless of ordering.
func TestRun_StopRacingWithNaturalExit(t *testing.T) {
	for i := 0; i < 25; i++ {
		tr, fp := newTestTranscoder(t, "10")
		done := tr.Run(true)
		out := tr.Output()

		// Interleave stop and natural exit aggressively.
		go func() { tr.Stop() }()
		go func() {
			fp.closeStderr()
			fp.exit(nil)
		}()

		assertChannelClosed(t, out)
		awaitDone(t, done)

		if n := fp.stderrHandle.closeCount(); n != 1 {
			t.Fatalf("iteration %d: stderr close count = %d, want 1", i, n)
		}
	}
}

// A process that fails to start must produce a recognizable failure on both the
// done channel and the progress channel, and everything must close (no hang, no
// leaked pipe).
func TestRun_StartFailureReportsErrorAndCloses(t *testing.T) {
	tr, fp := newTestTranscoder(t, "10")
	fp.startErr = errors.New("exec: no such file or directory")

	done := tr.Run(true)
	out := tr.Output()

	terminal := awaitDone(t, done)
	if terminal == nil {
		t.Fatal("expected terminal error on start failure")
	}

	// The failure must be the recognizable terminal progress value, after which
	// the channel is closed.
	p, ok := <-out
	if !ok {
		t.Fatal("expected a failure progress value, channel closed with none")
	}
	if p.Err == nil {
		t.Fatalf("expected progress.Err to carry the failure, got %+v", p)
	}
	assertChannelClosed(t, out)

	if fp.stderrHandle.closeCount() != 1 {
		t.Errorf("stderr pipe close count = %d, want 1", fp.stderrHandle.closeCount())
	}
}

// Output before any run returns an already-closed channel; it must not hang.
func TestOutput_WithoutRunDoesNotHang(t *testing.T) {
	tr := &Transcoder{}
	out := tr.Output()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected closed channel from Output with no active run")
		}
	case <-time.After(time.Second):
		t.Fatal("Output with no run blocked instead of returning a closed channel")
	}

	// Stop with no active run must be a safe no-op.
	if err := tr.Stop(); err != nil {
		t.Errorf("Stop with no run returned error: %v", err)
	}
}

// Multiple Output calls during one run return the same channel (single owner),
// avoiding duplicate/garbled end signals and double closes.
func TestOutput_SingleSharedChannelPerRun(t *testing.T) {
	tr, fp := newTestTranscoder(t, "10")
	done := tr.Run(true)

	a := tr.Output()
	b := tr.Output()
	if a != b {
		t.Fatal("Output must return the same channel for the active run")
	}

	fp.closeStderr()
	fp.exit(nil)

	// Wait for the full run finalization (which owns the pipe close) before
	// asserting the close count, rather than just the progress channel closing.
	if err := awaitDone(t, done); err != nil {
		t.Fatalf("unexpected terminal error: %v", err)
	}
	assertChannelClosed(t, a)
	if fp.stderrHandle.closeCount() != 1 {
		t.Errorf("stderr pipe close count = %d, want 1", fp.stderrHandle.closeCount())
	}
}

// After a run finishes, starting a new task on the same Transcoder must only
// expose the new task's output; the previous run's reader cannot see or close
// the new run's pipe.
func TestRun_RestartIsolatesProgressAndPipes(t *testing.T) {
	tr, fp1 := newTestTranscoder(t, "10")

	// First run: wait for a real delivered update, then stop it completely.
	done1 := tr.Run(true)
	firstProgressCh := tr.Output()
	p1 := emitProgress(t, fp1, firstProgressCh)
	if p1.FramesProcessed != "120" {
		t.Fatalf("first run unexpected update: %+v", p1)
	}
	tr.Stop()
	assertChannelClosed(t, firstProgressCh)
	awaitDone(t, done1)

	if fp1.stderrHandle.closeCount() != 1 {
		t.Fatalf("first run stderr close count = %d, want 1", fp1.stderrHandle.closeCount())
	}

	// The old run's channel must stay closed and never receive the new run's data.
	if _, ok := <-firstProgressCh; ok {
		t.Fatal("first run channel received data after restart")
	}

	// Second run via a fresh fake process on the same transcoder.
	fp2 := newFakeProcess("", nil)
	tr.newProcess = func(name string, args []string) process { return fp2 }

	done2 := tr.Run(true)
	secondProgressCh := tr.Output()
	if secondProgressCh == firstProgressCh {
		t.Fatal("new run must use a fresh progress channel")
	}
	p2 := emitProgress(t, fp2, secondProgressCh)
	if p2.FramesProcessed != "120" {
		t.Fatalf("second run unexpected update: %+v", p2)
	}

	fp2.closeStderr()
	fp2.exit(nil)
	if err := awaitDone(t, done2); err != nil {
		t.Fatalf("second run terminal error: %v", err)
	}
	assertChannelClosed(t, secondProgressCh)

	if fp2.stderrHandle.closeCount() != 1 {
		t.Errorf("second run stderr close count = %d, want 1", fp2.stderrHandle.closeCount())
	}
}

// A second Run while one is active must be rejected instead of clobbering state.
func TestRun_RejectsConcurrentRun(t *testing.T) {
	tr, fp := newTestTranscoder(t, "10")
	tr.Run(true)

	secondDone := tr.Run(true)
	select {
	case err := <-secondDone:
		if err == nil {
			t.Fatal("expected error when starting a concurrent run")
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent Run attempt hung")
	}

	fp.closeStderr()
	fp.exit(nil)
}

// Live stream (unknown duration) end to end: updates flow with zero percentage
// and the run still closes cleanly.
func TestRun_LiveStreamUnknownDuration(t *testing.T) {
	tr, fp := newTestTranscoder(t, "0") // zero/unknown duration
	done := tr.Run(true)
	out := tr.Output()

	got := emitProgress(t, fp, out)
	fp.closeStderr()
	fp.exit(nil)

	if err := awaitDone(t, done); err != nil {
		t.Fatalf("live run terminal error: %v", err)
	}
	assertChannelClosed(t, out)

	if got.Progress != 0 {
		t.Errorf("live percentage must be zero, got %v", got.Progress)
	}
	if got.FramesProcessed != "120" || got.CurrentBitrate == "" {
		t.Errorf("live update must still carry time/bitrate/frames: %+v", got)
	}
}

// User-created media input/output pipes must be closed exactly once on the end
// path. The counting wrappers detect leaks and duplicate closes deterministically.
func TestRun_MediaPipesReleasedOnce(t *testing.T) {
	tr, fp := newTestTranscoder(t, "10")

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	inReader := newCountingReadCloser(inR)
	outWriter := newCountingWriteCloser(outW)

	mf := tr.MediaFile()
	mf.SetInputPipe(true)
	mf.SetInputPipeReader(inReader)
	mf.SetInputPipeWriter(inW)
	mf.SetOutputPipe(true)
	mf.SetOutputPipeReader(outR)
	mf.SetOutputPipeWriter(outWriter)

	done := tr.Run(false)
	fp.exit(nil)
	awaitDone(t, done)

	if inReader.closeCount() != 1 {
		t.Errorf("input reader close count = %d, want exactly 1", inReader.closeCount())
	}
	if outWriter.closeCount() != 1 {
		t.Errorf("output writer close count = %d, want exactly 1", outWriter.closeCount())
	}

	_ = inW
	_ = outR
}

// A progress-disabled run still produces an Output channel that closes when the
// run ends, so a caller ranging over it can never hang.
func TestRun_NoProgressStillClosesOutput(t *testing.T) {
	tr, fp := newTestTranscoder(t, "10")

	done := tr.Run(false)
	out := tr.Output()

	fp.exit(nil)
	if err := awaitDone(t, done); err != nil {
		t.Fatalf("unexpected terminal error: %v", err)
	}
	assertChannelClosed(t, out)
}
