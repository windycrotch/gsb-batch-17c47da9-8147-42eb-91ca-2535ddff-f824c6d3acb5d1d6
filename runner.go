package hls

import (
	"bytes"
	"context"
	"io"
	"os/exec"
)

// maxStderrCapture bounds how much ffmpeg stderr is retained per variant so a
// chatty failure cannot grow the task's memory without limit.
const maxStderrCapture = 32 * 1024

// ringBuffer is a bounded buffer keeping only the most recent bytes written to
// it (ffmpeg prints the useful error context at the very end of its output).
type ringBuffer struct {
	buf  []byte
	size int
}

func newRingBuffer(size int) *ringBuffer { return &ringBuffer{size: size} }

func (r *ringBuffer) Write(p []byte) (int, error) {
	if len(p) >= r.size {
		r.buf = append(r.buf[:0], p[len(p)-r.size:]...)
		return len(p), nil
	}
	if len(r.buf)+len(p) > r.size {
		drop := len(r.buf) + len(p) - r.size
		r.buf = append(r.buf[:0], r.buf[drop:]...)
	}
	r.buf = append(r.buf, p...)
	return len(p), nil
}

func (r *ringBuffer) String() string { return string(r.buf) }

// runFFmpeg starts ffmpeg, waits for it to finish and guarantees the process
// is terminated and reaped when ctx is canceled. It never leaves an orphan
// ffmpeg process writing into the output directory:
//
//   - On normal completion the process is reaped by Wait().
//   - On context cancellation the whole process tree is force-killed
//     (see killProcessTree) and Wait() still runs, so the process is reaped
//     rather than turning into a zombie/orphan.
//
// The returned error is nil on a clean exit; the tail of ffmpeg's stderr is
// embedded in the *ffmpegExitError on failure.
func runFFmpeg(ctx context.Context, ffmpegPath string, args []string) error {
	cmd := exec.Command(ffmpegPath, args...)
	configureProcAttr(cmd)

	stderr := newRingBuffer(maxStderrCapture)
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard

	killCalled := false
	// kill is idempotent: a cancellation that races with a natural exit must
	// not turn into an error, and repeated cancels are harmless.
	kill := func() {
		if killCalled || cmd.Process == nil {
			return
		}
		killCalled = true
		killProcessTree(cmd)
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	// Terminate on cancellation even while blocked in Wait(). stopWatcher is
	// closed on return so the goroutine exits once ffmpeg finishes naturally.
	stopWatcher := make(chan struct{})
	defer close(stopWatcher)
	go func() {
		select {
		case <-ctx.Done():
			kill()
		case <-stopWatcher:
		}
	}()

	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		// Cancellation wins regardless of how ffmpeg exited.
		return ctx.Err()
	}
	if waitErr != nil {
		return &ffmpegExitError{cause: waitErr, stderr: stderr.String()}
	}
	return nil
}

// ffmpegExitError wraps a non-zero ffmpeg exit with the tail of its stderr so
// callers can tell why a variant failed.
type ffmpegExitError struct {
	cause  error
	stderr string
}

func (e *ffmpegExitError) Error() string {
	stderr := bytes.TrimSpace([]byte(e.stderr))
	if len(stderr) > 0 {
		return "hls: ffmpeg exited with error: " + e.cause.Error() + ": " + string(stderr)
	}
	return "hls: ffmpeg exited with error: " + e.cause.Error()
}

func (e *ffmpegExitError) Unwrap() error { return e.cause }
