package pull

// srt_internal_test.go — lifecycle and concurrency tests for SRTReader using
// an in-process fake connection. No real sockets, no external network: the
// reader's dialer seam (srtDialFunc) is replaced per test.
//
// The fake connection models the gosrt behaviour the reader relies on:
// Read blocks while no data is available and returns io.EOF once Close has
// released it; Close is idempotent. A separate loopback test file exercises
// the real gosrt stack end to end.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	srt "github.com/datarhei/gosrt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datvietvac-techhub/open-streamer/internal/domain"
)

// ── Fake connection ─────────────────────────────────────────────────────────

// fakeSRTConn implements srtConn for tests. Read blocks on the data channel
// until a chunk is queued or the connection is closed, mirroring gosrt's
// blocking Read / Close-unblocks-Read semantics.
type fakeSRTConn struct {
	streamID string

	mu       sync.Mutex
	closed   bool
	closeCnt int
	readErr  error // optional permanent error surfaced after closeSig fires

	data     chan []byte
	closeSig chan struct{}
}

func newFakeSRTConn(streamID string) *fakeSRTConn {
	return &fakeSRTConn{
		streamID: streamID,
		data:     make(chan []byte, 16),
		closeSig: make(chan struct{}),
	}
}

func (f *fakeSRTConn) Read(p []byte) (int, error) {
	select {
	case b := <-f.data:
		return copy(p, b), nil
	case <-f.closeSig:
		f.mu.Lock()
		err := f.readErr
		f.mu.Unlock()
		if err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
}

func (f *fakeSRTConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.closeSig)
	}
	f.closeCnt++
	return nil
}

func (f *fakeSRTConn) StreamId() string { return f.streamID }

func (f *fakeSRTConn) CloseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCnt
}

// feed queues a chunk for a blocked Read.
func (f *fakeSRTConn) feed(b []byte) {
	f.data <- b
}

// failOnceClosed makes Read return err instead of io.EOF after Close.
func (f *fakeSRTConn) failOnceClosed(err error) {
	f.mu.Lock()
	f.readErr = err
	f.mu.Unlock()
}

// ── Fake dialer ─────────────────────────────────────────────────────────────

type fakeDialer struct {
	mu    sync.Mutex
	conns []*fakeSRTConn
	idx   int
	calls int
	err   error
}

func (d *fakeDialer) dial(_, _ string, _ srt.Config) (srtConn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.err != nil {
		return nil, d.err
	}
	if d.idx >= len(d.conns) {
		return nil, fmt.Errorf("fake dialer: exhausted")
	}
	c := d.conns[d.idx]
	d.idx++
	return c, nil
}

func (d *fakeDialer) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func newTestSRTReader(t *testing.T, dial srtDialFunc) *SRTReader {
	t.Helper()
	r := NewSRTReader(domain.Input{URL: "srt://127.0.0.1:1234?streamid=live/test"})
	r.dial = dial
	return r
}

// currentSession snapshots the installed session so tests can wait on the
// watcher goroutine's watchDone after Close.
func currentSession(r *SRTReader) *srtSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session
}

// waitWatcherDone fails the test if the watcher goroutine has not exited.
func waitWatcherDone(t *testing.T, sess *srtSession) {
	t.Helper()
	if sess == nil {
		return
	}
	select {
	case <-sess.watchDone:
	case <-time.After(2 * time.Second):
		t.Fatal("srt watcher goroutine leaked: watchDone not closed")
	}
}

// readAsync runs Read in a goroutine and returns its result channel.
func readAsync(r *SRTReader, ctx context.Context) <-chan readResult {
	ch := make(chan readResult, 1)
	go func() {
		b, err := r.Read(ctx)
		ch <- readResult{b: b, err: err}
	}()
	return ch
}

type readResult struct {
	b   []byte
	err error
}

// assertReadResult waits for an async Read and asserts its outcome.
func assertReadResult(t *testing.T, ch <-chan readResult, timeout time.Duration) readResult {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(timeout):
		t.Fatal("srt Read did not return in time")
		return readResult{}
	}
}

// ── Tests ───────────────────────────────────────────────────────────────────

// The production srt.Conn must satisfy the reader's internal seam.
var _ srtConn = (srt.Conn)(nil)

func TestSRTReader_CloseBeforeOpenIsSafe(t *testing.T) {
	t.Parallel()
	d := &fakeDialer{conns: []*fakeSRTConn{newFakeSRTConn("a"), newFakeSRTConn("b")}}
	r := newTestSRTReader(t, d.dial)

	// Close before any Open, and repeatedly: no error, no panic.
	require.NoError(t, r.Close())
	require.NoError(t, r.Close())

	// The reader remains usable afterwards.
	ctx := context.Background()
	require.NoError(t, r.Open(ctx))
	payload := bytes.Repeat([]byte{0x47}, 188)
	d.conns[0].feed(payload)
	got, err := r.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, payload, got)
	require.NoError(t, r.Close())
}

func TestSRTReader_DoubleCloseClosesConnOnce(t *testing.T) {
	t.Parallel()
	conn := newFakeSRTConn("a")
	r := newTestSRTReader(t, (&fakeDialer{conns: []*fakeSRTConn{conn}}).dial)

	require.NoError(t, r.Open(context.Background()))
	sess := currentSession(r)
	require.NoError(t, r.Close())
	require.NoError(t, r.Close())
	require.NoError(t, r.Close())

	assert.Equal(t, 1, conn.CloseCount(), "underlying conn.Close must run exactly once")
	waitWatcherDone(t, sess)
}

func TestSRTReader_OpenCloseReopenLifecycle(t *testing.T) {
	t.Parallel()
	const cycles = 5
	conns := make([]*fakeSRTConn, cycles)
	for i := range conns {
		conns[i] = newFakeSRTConn(fmt.Sprintf("c%d", i))
	}
	d := &fakeDialer{conns: conns}
	r := newTestSRTReader(t, d.dial)

	payload := bytes.Repeat([]byte{0xAB}, 1316)
	for i := 0; i < cycles; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		require.NoError(t, r.Open(ctx), "cycle %d: open", i)
		require.NotNil(t, currentSession(r), "cycle %d: session installed", i)

		conns[i].feed(payload)
		got, err := r.Read(ctx)
		require.NoError(t, err, "cycle %d: read", i)
		require.Equal(t, payload, got, "cycle %d: payload", i)

		require.NoError(t, r.Close(), "cycle %d: close", i)
		cancel()

		assert.Nil(t, currentSession(r), "cycle %d: session detached", i)
		assert.Equal(t, 1, conns[i].CloseCount(), "cycle %d: conn closed once", i)
	}
	assert.Equal(t, cycles, d.callCount(), "a fresh connection per Open")
}

func TestSRTReader_OpenWhileOpenFails(t *testing.T) {
	t.Parallel()
	conn := newFakeSRTConn("a")
	d := &fakeDialer{conns: []*fakeSRTConn{conn, newFakeSRTConn("b")}}
	r := newTestSRTReader(t, d.dial)

	require.NoError(t, r.Open(context.Background()))
	err := r.Open(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already open")

	// The failed Open must not have consumed/replaced the active session.
	require.NoError(t, r.Close())
	assert.Equal(t, 1, d.callCount())
}

func TestSRTReader_DialFailureLeavesNoSession(t *testing.T) {
	t.Parallel()
	good := newFakeSRTConn("good")
	d := &fakeDialer{
		conns: []*fakeSRTConn{good},
		err:   errors.New("dial boom"),
	}
	r := newTestSRTReader(t, d.dial)

	err := r.Open(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dial boom")
	assert.Nil(t, currentSession(r), "failed dial must not install a session")

	// Recover once the dialer works again.
	d.err = nil
	require.NoError(t, r.Open(context.Background()))
	require.NoError(t, r.Close())
	assert.Equal(t, 1, good.CloseCount())
}

func TestSRTReader_ReadAfterCloseDoesNotPanic(t *testing.T) {
	t.Parallel()
	conn := newFakeSRTConn("a")
	r := newTestSRTReader(t, (&fakeDialer{conns: []*fakeSRTConn{conn}}).dial)

	require.NoError(t, r.Open(context.Background()))
	require.NoError(t, r.Close())

	_, err := r.Read(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, errSRTClosed)
}

func TestSRTReader_CloseUnblocksBlockingRead(t *testing.T) {
	t.Parallel()
	conn := newFakeSRTConn("a")
	r := newTestSRTReader(t, (&fakeDialer{conns: []*fakeSRTConn{conn}}).dial)

	ctx := context.Background()
	require.NoError(t, r.Open(ctx))
	sess := currentSession(r)

	ch := readAsync(r, ctx)
	// Give the Read time to park inside conn.Read.
	time.Sleep(20 * time.Millisecond)

	require.NoError(t, r.Close())
	got := assertReadResult(t, ch, 2*time.Second)
	require.Error(t, got.err)
	assert.ErrorIs(t, got.err, errSRTClosed, "local close must not look like upstream EOF")
	assert.Equal(t, 1, conn.CloseCount())
	waitWatcherDone(t, sess)
}

func TestSRTReader_ContextCancelUnblocksBlockingRead(t *testing.T) {
	t.Parallel()
	conn := newFakeSRTConn("a")
	r := newTestSRTReader(t, (&fakeDialer{conns: []*fakeSRTConn{conn}}).dial)

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, r.Open(ctx))
	sess := currentSession(r)

	ch := readAsync(r, ctx)
	time.Sleep(20 * time.Millisecond)
	cancel()

	got := assertReadResult(t, ch, 2*time.Second)
	assert.ErrorIs(t, got.err, context.Canceled, "caller cancellation must be distinguishable from EOF")
	assert.Equal(t, 1, conn.CloseCount(), "watcher closed the connection")

	// Close after a context-driven stop stays idempotent and never double-closes.
	require.NoError(t, r.Close())
	assert.Equal(t, 1, conn.CloseCount())
	waitWatcherDone(t, sess)
}

func TestSRTReader_ContextCancelThenReopenUsesFreshConnection(t *testing.T) {
	t.Parallel()
	conn1 := newFakeSRTConn("gen1")
	conn2 := newFakeSRTConn("gen2")
	r := newTestSRTReader(t, (&fakeDialer{conns: []*fakeSRTConn{conn1, conn2}}).dial)

	// Generation 1: cancel its context, let the watcher tear it down.
	ctx1, cancel1 := context.WithCancel(context.Background())
	require.NoError(t, r.Open(ctx1))
	ch1 := readAsync(r, ctx1)
	cancel1()
	got := assertReadResult(t, ch1, 2*time.Second)
	assert.ErrorIs(t, got.err, context.Canceled)
	require.NoError(t, r.Close())
	assert.Equal(t, 1, conn1.CloseCount())

	// Generation 2 with a brand-new context. The old watcher must not have
	// touched this connection.
	ctx2 := context.Background()
	require.NoError(t, r.Open(ctx2))
	sess2 := currentSession(r)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, conn2.CloseCount(), "previous generation's watcher must not close the new conn")

	payload := bytes.Repeat([]byte{0x55}, 1316)
	conn2.feed(payload)
	got2, err := r.Read(ctx2)
	require.NoError(t, err)
	assert.Equal(t, payload, got2)

	require.NoError(t, r.Close())
	assert.Equal(t, 1, conn2.CloseCount())
	waitWatcherDone(t, sess2)
}

func TestSRTReader_UpstreamDisconnectReturnsEOF(t *testing.T) {
	t.Parallel()
	conn := newFakeSRTConn("a")
	r := newTestSRTReader(t, (&fakeDialer{conns: []*fakeSRTConn{conn}}).dial)

	ctx := context.Background()
	require.NoError(t, r.Open(ctx))

	// Simulate the peer dropping the connection: gosrt's blocked Read returns
	// io.EOF even though nobody called Close locally.
	require.NoError(t, conn.Close())
	got := assertReadResult(t, readAsync(r, ctx), 2*time.Second)
	assert.ErrorIs(t, got.err, io.EOF, "upstream disconnect must stay io.EOF for the backoff path")
	assert.Nil(t, currentSession(r), "dead session detaches itself")

	// Close after an upstream EOF is a safe no-op at the reader level. The
	// fake counts raw invocations (2: one simulating the peer, one from the
	// reader's idempotent teardown); real gosrt de-duplicates internally.
	require.NoError(t, r.Close())
	assert.Equal(t, 2, conn.CloseCount())
}

func TestSRTReader_UpstreamErrClientClosedMapsToEOF(t *testing.T) {
	t.Parallel()
	conn := newFakeSRTConn("a")
	conn.failOnceClosed(srt.ErrClientClosed)
	r := newTestSRTReader(t, (&fakeDialer{conns: []*fakeSRTConn{conn}}).dial)

	require.NoError(t, r.Open(context.Background()))
	require.NoError(t, conn.Close())
	got := assertReadResult(t, readAsync(r, context.Background()), 2*time.Second)
	assert.ErrorIs(t, got.err, io.EOF)
}

func TestSRTReader_DeadlineExceededReportedAsContextError(t *testing.T) {
	t.Parallel()
	conn := newFakeSRTConn("a")
	r := newTestSRTReader(t, (&fakeDialer{conns: []*fakeSRTConn{conn}}).dial)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.NoError(t, r.Open(ctx))

	got := assertReadResult(t, readAsync(r, ctx), 2*time.Second)
	assert.ErrorIs(t, got.err, context.DeadlineExceeded)
	require.NoError(t, r.Close())
}

func TestSRTReader_ReadWithPreCanceledContext(t *testing.T) {
	t.Parallel()
	conn := newFakeSRTConn("a")
	r := newTestSRTReader(t, (&fakeDialer{conns: []*fakeSRTConn{conn}}).dial)
	require.NoError(t, r.Open(context.Background()))
	t.Cleanup(func() { _ = r.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Read(ctx)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, conn.CloseCount(), "pre-canceled Read must not touch the connection")
}

func TestSRTReader_TransientTransportErrorIsNotEOF(t *testing.T) {
	t.Parallel()
	conn := newFakeSRTConn("a")
	conn.failOnceClosed(errors.New("srt: nope"))
	r := newTestSRTReader(t, (&fakeDialer{conns: []*fakeSRTConn{conn, newFakeSRTConn("b")}}).dial)

	ctx := context.Background()
	require.NoError(t, r.Open(ctx))
	require.NoError(t, conn.Close())

	got := assertReadResult(t, readAsync(r, ctx), 2*time.Second)
	require.Error(t, got.err)
	assert.NotErrorIs(t, got.err, io.EOF)
	assert.Contains(t, got.err.Error(), "srt: nope")
}

func TestSRTReader_ConcurrentCloses(t *testing.T) {
	t.Parallel()
	conn := newFakeSRTConn("a")
	r := newTestSRTReader(t, (&fakeDialer{conns: []*fakeSRTConn{conn}}).dial)
	require.NoError(t, r.Open(context.Background()))
	sess := currentSession(r)

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- r.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
	assert.Equal(t, 1, conn.CloseCount())
	waitWatcherDone(t, sess)
}

// TestSRTReader_ReadCloseRace interleaves a blocked read, data delivery,
// context cancellation and Close across many short lifecycles. Its main job
// is staying clean under `go test -race`: no data races, no use of a
// released connection, no leaked watcher. One reader per generation matches
// the production single-read-loop contract (gosrt's Conn.Read is not safe
// for concurrent callers); the stress comes from teardown interleavings.
func TestSRTReader_ReadCloseRace(t *testing.T) {
	t.Parallel()
	const cycles = 25

	for cycle := 0; cycle < cycles; cycle++ {
		conn := newFakeSRTConn(fmt.Sprintf("race-%d", cycle))
		r := newTestSRTReader(t, (&fakeDialer{conns: []*fakeSRTConn{conn}}).dial)

		ctx, cancel := context.WithCancel(context.Background())
		require.NoError(t, r.Open(ctx))
		sess := currentSession(r)

		var wg sync.WaitGroup
		// One blocked reader per generation, as in the worker's read loop.
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Read(ctx)
		}()
		// A feeder racing the teardown (sends may never be consumed).
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case conn.data <- bytes.Repeat([]byte{0x47}, 188):
				case <-conn.closeSig:
					return
				}
			}
		}()

		// Alternate teardown styles: explicit Close vs context cancel + Close.
		if cycle%2 == 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = r.Close()
			}()
		} else {
			cancel()
		}

		// Wait for readers to unwind, then normalise both teardown styles.
		wg.Wait()
		cancel()
		require.NoError(t, r.Close())
		assert.Equal(t, 1, conn.CloseCount())
		waitWatcherDone(t, sess)
	}
}

// TestSRTReader_ReopenAfterContextCancelRace hammers the exact ops scenario
// from the incident report: stop (context cancel) immediately followed by
// reopen, while a previous read is still unwinding.
func TestSRTReader_ReopenAfterContextCancelRace(t *testing.T) {
	t.Parallel()
	const cycles = 20

	conns := make([]*fakeSRTConn, cycles)
	for i := range conns {
		conns[i] = newFakeSRTConn(fmt.Sprintf("gen-%d", i))
	}
	r := newTestSRTReader(t, (&fakeDialer{conns: conns}).dial)

	for i := 0; i < cycles; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		require.NoError(t, r.Open(ctx))
		sess := currentSession(r)

		ch := readAsync(r, ctx)
		cancel()
		got := assertReadResult(t, ch, 2*time.Second)
		assert.ErrorIs(t, got.err, context.Canceled)

		// Reopen before/around Close ordering the worker uses.
		require.NoError(t, r.Close())
		waitWatcherDone(t, sess)
		assert.Equal(t, 1, conns[i].CloseCount())
	}
}
