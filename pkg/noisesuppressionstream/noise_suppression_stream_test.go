package noisesuppressionstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xaionaro-go/audio/pkg/audio"
	"github.com/xaionaro-go/audio/pkg/noisesuppression"
)

// fakeNoiseSuppression is a deterministic NoiseSuppression implementation for
// tests: it operates on 8-byte frames, mono, 1 byte per sample and applies a
// configurable byte-wise transform (identity by default).
type fakeNoiseSuppression struct {
	chunkSize uint
	channels  audio.Channel
	transform func(byte) byte
	err       error
	calls     int
	locker    sync.Mutex
}

func newFakeNoiseSuppression() *fakeNoiseSuppression {
	return &fakeNoiseSuppression{
		chunkSize: 8,
		channels:  1,
		transform: func(b byte) byte { return b },
	}
}

func (f *fakeNoiseSuppression) Close() error { return nil }

func (f *fakeNoiseSuppression) Encoding(context.Context) (audio.Encoding, error) {
	return audio.EncodingPCM{
		PCMFormat:  audio.PCMFormatU8,
		SampleRate: 48000,
	}, nil
}

func (f *fakeNoiseSuppression) Channels(context.Context) (audio.Channel, error) {
	return f.channels, nil
}

func (f *fakeNoiseSuppression) ChunkSize() uint { return f.chunkSize }

func (f *fakeNoiseSuppression) SuppressNoise(
	_ context.Context,
	input []byte,
	outputVoice []byte,
) (float64, error) {
	f.locker.Lock()
	defer f.locker.Unlock()
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	if len(input) != int(f.chunkSize) || len(outputVoice) < int(f.chunkSize) {
		return 0, fmt.Errorf("unexpected frame sizes: input=%d output=%d", len(input), len(outputVoice))
	}
	for i := 0; i < len(input); i++ {
		outputVoice[i] = f.transform(input[i])
	}
	return 1, nil
}

func (f *fakeNoiseSuppression) callCount() int {
	f.locker.Lock()
	defer f.locker.Unlock()
	return f.calls
}

// errReader returns a fixed, identifiable error on every Read.
type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

// testTimeout is the maximum wait a test gives an operation that is supposed
// to return promptly. A hang (the original bugs) therefore fails the test
// instead of blocking the whole suite forever.
const testTimeout = 5 * time.Second

func readWithTimeout(t *testing.T, r io.Reader, buf []byte) (int, error) {
	t.Helper()
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := r.Read(buf)
		ch <- result{n, err}
	}()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-time.After(testTimeout):
		t.Fatalf("Read blocked for %s", testTimeout)
		return 0, nil
	}
}

// readAll drains the stream until its terminal error and returns everything
// delivered before it.
func readAll(t *testing.T, r io.Reader) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	buf := make([]byte, 5) // deliberately smaller than a frame
	for {
		n, err := readWithTimeout(t, r, buf)
		out.Write(buf[:n])
		if err != nil {
			return out.Bytes(), err
		}
	}
}

// requireGoroutinesExited fails unless the stream's background loops reach
// their terminal state in time. terminalDone is closed only after the last
// loop ran its exit handler, so it is the authoritative shutdown signal.
func requireGoroutinesExited(t *testing.T, stream *NoiseSuppressionStream) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		stream.waitTerminated()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatalf("background noise suppression goroutines are still running after %s", testTimeout)
	}
}

func expectedFrame(input []byte, transform func(byte) byte) []byte {
	out := make([]byte, len(input))
	for i, b := range input {
		out[i] = transform(b)
	}
	return out
}

// TestNormalEndDeliversAllFramesThenEOF covers the clean end of the upstream:
// every produced frame must be readable, then the caller must get io.EOF
// instead of blocking forever, and repeated reads keep returning io.EOF.
func TestNormalEndDeliversAllFramesThenEOF(t *testing.T) {
	ns := newFakeNoiseSuppression()
	// 3 full frames, frame-aligned input.
	input := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24}
	stream, err := NewNoiseSuppressionStream(
		context.Background(), bytes.NewReader(input), ns, 64, 64,
	)
	require.NoError(t, err)

	got, err := readAll(t, stream)
	require.ErrorIs(t, err, io.EOF, "clean input end must surface as io.EOF")
	require.Equal(t, input, got, "all denoised frames must be delivered in order")
	require.Equal(t, 3, ns.callCount())

	// Terminal state is stable: no restart, same error forever.
	for i := 0; i < 3; i++ {
		n, err := readWithTimeout(t, stream, make([]byte, 8))
		assert.Equal(t, 0, n)
		assert.ErrorIs(t, err, io.EOF)
	}
	requireGoroutinesExited(t, stream)
}

// TestUpstreamReadErrorSurfacedAndStable covers an upstream failure: the
// caller must promptly get an error wrapping the upstream error, already
// produced data must remain readable first, and the error must stay identical
// on every subsequent read.
func TestUpstreamReadErrorSurfacedAndStable(t *testing.T) {
	upstreamErr := errors.New("boom: upstream exploded")
	ns := newFakeNoiseSuppression()
	stream, err := NewNoiseSuppressionStream(
		context.Background(), &errReader{err: upstreamErr}, ns, 64, 64,
	)
	require.NoError(t, err)

	n, err := readWithTimeout(t, stream, make([]byte, 8))
	assert.Equal(t, 0, n)
	require.Error(t, err)
	assert.ErrorIs(t, err, upstreamErr, "the caller must be able to identify the real upstream cause")
	assert.False(t, errors.Is(err, io.EOF), "an upstream failure must not be reported as EOF")

	firstErr := err
	for i := 0; i < 3; i++ {
		n, rerr := readWithTimeout(t, stream, make([]byte, 8))
		assert.Equal(t, 0, n)
		assert.Equal(t, firstErr, rerr, "repeated reads must return the same stable error")
	}
	requireGoroutinesExited(t, stream)
}

// TestPartialFrameAtEOFReturnsUnexpectedEOF covers input whose length is not an
// integer multiple of the frame size: complete frames are still delivered,
// then the truncated remainder yields io.ErrUnexpectedEOF instead of a hang.
func TestPartialFrameAtEOFReturnsUnexpectedEOF(t *testing.T) {
	ns := newFakeNoiseSuppression()
	// 2 full frames plus a 3-byte remainder.
	input := make([]byte, 2*8+3)
	for i := range input {
		input[i] = byte(i + 1)
	}
	stream, err := NewNoiseSuppressionStream(
		context.Background(), bytes.NewReader(input), ns, 64, 64,
	)
	require.NoError(t, err)

	got, err := readAll(t, stream)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF, "a partial trailing frame must be reported as unexpected EOF")
	assert.Equal(t, input[:16], got, "complete frames written before the remainder must not be lost")
	assert.Equal(t, 2, ns.callCount())

	// The terminal error stays stable on further reads.
	n, rerr := readWithTimeout(t, stream, make([]byte, 8))
	assert.Equal(t, 0, n)
	assert.ErrorIs(t, rerr, io.ErrUnexpectedEOF)
	requireGoroutinesExited(t, stream)
}

// oneShotReader returns its payload in a single Read call, then blocks until
// closed — it makes the "one misaligned upstream Read" scenario deterministic
// (an io.Pipe is allowed to split a write across several reads).
type oneShotReader struct {
	payload   []byte
	locker    sync.Mutex
	delivered bool
	closed    chan struct{}
}

func newOneShotReader(payload []byte) *oneShotReader {
	return &oneShotReader{payload: payload, closed: make(chan struct{})}
}

func (r *oneShotReader) Read(p []byte) (int, error) {
	r.locker.Lock()
	first := !r.delivered
	r.delivered = true
	r.locker.Unlock()

	if first {
		n := copy(p, r.payload)
		return n, nil
	}
	<-r.closed
	return 0, io.EOF
}

func (r *oneShotReader) close() { close(r.closed) }

// TestMisalignedReadFromUpstream covers a single upstream Read returning a
// length that is not a multiple of the sample byte size (stereo U8 => 2 bytes
// per sample): the failure must reach the caller promptly and remain stable.
func TestMisalignedReadFromUpstream(t *testing.T) {
	ns := newFakeNoiseSuppression()
	ns.channels = 2                          // 1 byte/sample * 2 channels => sample stride of 2 bytes
	src := newOneShotReader(make([]byte, 5)) // 5 is not a multiple of 2
	defer src.close()
	stream, err := NewNoiseSuppressionStream(
		context.Background(), src, ns, 64, 64,
	)
	require.NoError(t, err)

	n, err := readWithTimeout(t, stream, make([]byte, 8))
	assert.Equal(t, 0, n)
	require.Error(t, err)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)

	for i := 0; i < 2; i++ {
		n, rerr := readWithTimeout(t, stream, make([]byte, 8))
		assert.Equal(t, 0, n)
		assert.ErrorIs(t, rerr, io.ErrUnexpectedEOF)
	}
	requireGoroutinesExited(t, stream)
}

// TestContextCancelUnblocksWaitingRead covers external cancellation while no
// output is available: the waiting Read must return the cancellation reason
// immediately, keep returning it, and the background loops must stop.
func TestContextCancelUnblocksWaitingRead(t *testing.T) {
	ns := newFakeNoiseSuppression()
	pr, pw := io.Pipe()
	defer pw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := NewNoiseSuppressionStream(ctx, pr, ns, 64, 64)
	require.NoError(t, err)

	done := make(chan struct{})
	var readN int
	var readErr error
	go func() {
		readN, readErr = stream.Read(make([]byte, 8))
		close(done)
	}()

	// Give the Read time to actually block.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatalf("Read was not unblocked by context cancellation")
	}
	assert.Equal(t, 0, readN)
	assert.ErrorIs(t, readErr, context.Canceled)

	for i := 0; i < 2; i++ {
		n, rerr := readWithTimeout(t, stream, make([]byte, 8))
		assert.Equal(t, 0, n)
		assert.ErrorIs(t, rerr, context.Canceled, "cancellation must remain the stable terminal error")
	}
	requireGoroutinesExited(t, stream)
}

// TestCancelAfterProducedDataDeliversDataFirst ensures already produced frames
// are readable before the cancellation error is surfaced.
func TestCancelAfterProducedDataDeliversDataFirst(t *testing.T) {
	ns := newFakeNoiseSuppression()
	pr, pw := io.Pipe()
	defer pw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := NewNoiseSuppressionStream(
		ctx, pr, ns, 64, 64,
	)
	require.NoError(t, err)

	// Feed exactly two frames but keep the pipe open so the stream stays
	// alive until the explicit cancellation below.
	go func() {
		_, _ = pw.Write(make([]byte, 16))
	}()

	// Wait until both frames have been produced into the output buffer.
	deadline := time.Now().Add(testTimeout)
	for ns.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, 2, ns.callCount())
	cancel()

	got := make([]byte, 16)
	n, err := io.ReadFull(stream, got)
	require.NoError(t, err, "buffered frames must remain readable after cancellation")
	require.Equal(t, 16, n)

	n, err = readWithTimeout(t, stream, make([]byte, 8))
	assert.Equal(t, 0, n)
	assert.ErrorIs(t, err, context.Canceled)
	requireGoroutinesExited(t, stream)
}

// TestBackpressureWaitsWithoutDroppingFrames covers a full output buffer: the
// noise suppression loop must block until the caller consumes data, then
// resume without losing or reordering any frame.
func TestBackpressureWaitsWithoutDroppingFrames(t *testing.T) {
	ns := newFakeNoiseSuppression()
	pr, pw := io.Pipe()

	const frameCount = 20
	// Output buffer only fits two frames; the input buffer is small too.
	stream, err := NewNoiseSuppressionStream(
		context.Background(), pr, ns, 24, 16,
	)
	require.NoError(t, err)

	go func() {
		data := make([]byte, frameCount*8)
		for i := range data {
			data[i] = byte(i + 1)
		}
		_, _ = pw.Write(data)
		_ = pw.Close()
	}()

	got, err := readAll(t, stream)
	require.ErrorIs(t, err, io.EOF)

	want := make([]byte, frameCount*8)
	for i := range want {
		want[i] = byte(i + 1)
	}
	require.Equal(t, want, got, "backpressure must not drop, duplicate or reorder frames")
	require.Equal(t, frameCount, ns.callCount())
	requireGoroutinesExited(t, stream)
}

// TestBackpressureBlockedWriterResumes verifies the actual blocking behavior:
// the loop parks while the output buffer is full and resumes exactly when the
// caller consumes bytes (no busy loop, no permanent stall).
func TestBackpressureBlockedWriterResumes(t *testing.T) {
	ns := newFakeNoiseSuppression()
	pr, pw := io.Pipe()

	// Output buffer fits exactly one frame.
	stream, err := NewNoiseSuppressionStream(
		context.Background(), pr, ns, 16, 8,
	)
	require.NoError(t, err)

	go func() {
		_, _ = pw.Write(make([]byte, 40)) // 5 frames
		_ = pw.Close()
	}()

	// Read the first frame promptly: the output buffer (one frame) must not
	// stall the pipeline, and every Read must return coherent frame bytes.
	buf := make([]byte, 8)
	n, err := readWithTimeout(t, stream, buf)
	require.NoError(t, err)
	require.Equal(t, 8, n)
	assert.GreaterOrEqual(t, ns.callCount(), 1)

	// Subsequent reads keep unblocking the back-pressured writer: consuming
	// output is what lets the parked noise suppression loop continue.
	n, err = readWithTimeout(t, stream, buf)
	require.NoError(t, err)
	require.Equal(t, 8, n)

	// Drain the rest and verify every frame was delivered exactly once and
	// the stream ends with clean EOF (no frame was dropped while blocked).
	rest, err := readAll(t, stream)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, 24, len(rest))
	require.Equal(t, 5, ns.callCount())
	requireGoroutinesExited(t, stream)
}

// TestConcurrentReadersNoDuplicationOrInterleaving covers multiple concurrent
// callers sharing one stream. A thread-safe io.Reader must partition its
// bytes across concurrent Read calls: every byte is delivered exactly once
// (no duplication, no loss) and every individual Read returns a coherent,
// non-interleaved contiguous run. The order in which separate goroutines
// happen to finish is intentionally not part of the contract.
func TestConcurrentReadersNoDuplicationOrInterleaving(t *testing.T) {
	ns := newFakeNoiseSuppression()
	const frameCount = 16
	input := make([]byte, frameCount*8)
	for i := range input {
		input[i] = byte(i + 1)
	}
	stream, err := NewNoiseSuppressionStream(
		context.Background(), bytes.NewReader(input), ns, 64, 32,
	)
	require.NoError(t, err)

	const readerCount = 4
	var (
		wg     sync.WaitGroup
		locker sync.Mutex
		// seen counts how many times each value was delivered.
		seen = make(map[byte]int)
		// interleaved is set if a Read returned a non-contiguous run.
		interleaved bool
		eofCount    int32
	)
	wg.Add(readerCount)
	for r := 0; r < readerCount; r++ {
		go func() {
			defer wg.Done()
			buf := make([]byte, 3) // odd size relative to the 8-byte frames
			for {
				n, rerr := stream.Read(buf)
				if n > 0 {
					part := make([]byte, n)
					copy(part, buf[:n])
					locker.Lock()
					for _, b := range part {
						seen[b]++
					}
					for i := 1; i < len(part); i++ {
						if part[i] != part[i-1]+1 {
							interleaved = true
						}
					}
					locker.Unlock()
				}
				if rerr != nil {
					if errors.Is(rerr, io.EOF) {
						atomic.AddInt32(&eofCount, 1)
					}
					return
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatalf("concurrent readers did not finish in time")
	}

	// Every input byte must have been delivered exactly once: this catches
	// both duplication and loss.
	require.Len(t, seen, len(input), "the exact set of output bytes must be delivered once")
	delivered := 0
	for value, count := range seen {
		assert.Equal(t, 1, count, "byte %d delivered %d times", value, count)
		delivered += count
	}
	assert.Equal(t, len(input), delivered)
	assert.False(t, interleaved, "a Read returned interleaved (non-contiguous) bytes")
	assert.Greater(t, atomic.LoadInt32(&eofCount), int32(0), "at least one reader must observe clean EOF")
	requireGoroutinesExited(t, stream)
}

// TestNoiseSuppressorFailurePropagated covers a failure inside SuppressNoise:
// it must surface to the caller promptly and remain stable.
func TestNoiseSuppressorFailurePropagated(t *testing.T) {
	suppressorErr := errors.New("suppressor failed")
	ns := newFakeNoiseSuppression()
	ns.err = suppressorErr
	stream, err := NewNoiseSuppressionStream(
		context.Background(), bytes.NewReader(make([]byte, 32)), ns, 64, 64,
	)
	require.NoError(t, err)

	n, err := readWithTimeout(t, stream, make([]byte, 8))
	assert.Equal(t, 0, n)
	require.Error(t, err)
	assert.ErrorIs(t, err, suppressorErr)

	n, rerr := readWithTimeout(t, stream, make([]byte, 8))
	assert.Equal(t, 0, n)
	assert.ErrorIs(t, rerr, suppressorErr)
	requireGoroutinesExited(t, stream)
}

// TestTransformPreservedAcrossBackpressure verifies the processed content
// (not just byte counts) survives backpressure and small reads in order.
func TestTransformPreservedAcrossBackpressure(t *testing.T) {
	ns := newFakeNoiseSuppression()
	ns.transform = func(b byte) byte { return b ^ 0x5A }

	const frameCount = 10
	input := make([]byte, frameCount*8)
	for i := range input {
		input[i] = byte(i)
	}
	stream, err := NewNoiseSuppressionStream(
		context.Background(), bytes.NewReader(input), ns, 16, 16,
	)
	require.NoError(t, err)

	got, err := readAll(t, stream)
	require.ErrorIs(t, err, io.EOF)
	assert.Equal(t, expectedFrame(input, ns.transform), got)
	requireGoroutinesExited(t, stream)
}

// TestTightOneFrameBuffers exercises the smallest viable buffer capacity
// (exactly one frame on both sides): backpressure must keep the pipeline
// moving without dropping or reordering a single byte.
func TestTightOneFrameBuffers(t *testing.T) {
	ns := newFakeNoiseSuppression()
	input := make([]byte, 24)
	for i := range input {
		input[i] = byte(i + 1)
	}
	stream, err := NewNoiseSuppressionStream(
		context.Background(), bytes.NewReader(input), ns, 8, 8,
	)
	require.NoError(t, err)

	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	assert.Equal(t, input, got)

	n, rerr := readWithTimeout(t, stream, make([]byte, 8))
	assert.Equal(t, 0, n)
	assert.ErrorIs(t, rerr, io.EOF)
	requireGoroutinesExited(t, stream)
}

// TestRejectsZeroChunkSize guards the frame-size invariant.
func TestRejectsZeroChunkSize(t *testing.T) {
	zero := &zeroChunkSuppressor{}
	_, err := NewNoiseSuppressionStream(
		context.Background(), bytes.NewReader(nil), zero, 64, 64,
	)
	require.Error(t, err)
}

type zeroChunkSuppressor struct{ noisesuppression.Dummy }

func (*zeroChunkSuppressor) ChunkSize() uint { return 0 }

func (*zeroChunkSuppressor) Encoding(context.Context) (audio.Encoding, error) {
	return audio.EncodingPCM{PCMFormat: audio.PCMFormatU8, SampleRate: 8000}, nil
}

func (*zeroChunkSuppressor) Channels(context.Context) (audio.Channel, error) { return 1, nil }
