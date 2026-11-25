package pull_test

// srt_loopback_test.go — end-to-end lifecycle tests for pull.SRTReader over a
// real gosrt connection. Both endpoints run inside the test process on the
// loopback interface (UDP), so no external network or SRT binary is required.
//
// These tests complement the fake-connection unit tests: they prove the
// reader's error classification against gosrt's actual Read/Close semantics.

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	srt "github.com/datarhei/gosrt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datvietvac-techhub/open-streamer/internal/domain"
	"github.com/datvietvac-techhub/open-streamer/internal/ingestor/pull"
)

// srtTestServer wraps an in-process gosrt listener. Each accepted connection
// is handed to the test through the conns channel so it can drive the peer
// side (feed data, close to simulate an upstream disconnect).
type srtTestServer struct {
	ln    srt.Listener
	mu    sync.Mutex
	conns chan srt.Conn
	stop  chan struct{}
}

func newSRTTestServer(t *testing.T) *srtTestServer {
	t.Helper()
	cfg := srt.DefaultConfig()
	// Keep the handshake/teardown snappy on loopback.
	cfg.ConnectionTimeout = 2 * time.Second

	ln, err := srt.Listen("srt", "127.0.0.1:0", cfg)
	require.NoError(t, err)

	s := &srtTestServer{
		ln:    ln,
		conns: make(chan srt.Conn, 4),
		stop:  make(chan struct{}),
	}

	go func() {
		for {
			req, err := ln.Accept2()
			if err != nil {
				return // listener closed
			}
			conn, err := req.Accept()
			if err != nil {
				continue
			}
			select {
			case s.conns <- conn:
			case <-s.stop:
				_ = conn.Close()
				return
			}
		}
	}()

	t.Cleanup(s.close)
	return s
}

func (s *srtTestServer) addr() string {
	return s.ln.Addr().String()
}

func (s *srtTestServer) accept(t *testing.T) srt.Conn {
	t.Helper()
	select {
	case conn := <-s.conns:
		return conn
	case <-time.After(3 * time.Second):
		t.Fatal("srt test server: no incoming connection")
		return nil
	}
}

func (s *srtTestServer) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.ln.Close()
}

func (s *srtTestServer) newReader(t *testing.T) *pull.SRTReader {
	t.Helper()
	return pull.NewSRTReader(domain.Input{
		URL: "srt://" + s.addr() + "?streamid=live/test",
		Net: domain.InputNetConfig{TimeoutSec: 2},
	})
}

// openWithRetry tolerates the brief window where a UDP socket torn down by a
// previous cycle is still releasing, mirroring an operator's stop/start click.
func openWithRetry(t *testing.T, r *pull.SRTReader, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := r.Open(ctx); err == nil {
			return
		} else {
			lastErr = err
			time.Sleep(25 * time.Millisecond)
		}
	}
	t.Fatalf("srt reader Open failed after retries: %v", lastErr)
}

func readWithTimeout(t *testing.T, r *pull.SRTReader, ctx context.Context, d time.Duration) ([]byte, error) {
	t.Helper()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := r.Read(ctx)
		ch <- res{b, err}
	}()
	select {
	case got := <-ch:
		return got.b, got.err
	case <-time.After(d):
		t.Fatal("srt Read blocked past timeout")
		return nil, nil
	}
}

// TestSRTReader_LoopbackFullLifecycle drives open → read → close → reopen
// against a real gosrt peer for several cycles.
func TestSRTReader_LoopbackFullLifecycle(t *testing.T) {
	srv := newSRTTestServer(t)

	const cycles = 3
	payload := bytes.Repeat([]byte{0x47}, 1316) // 7 × 188 MPEG-TS packets

	for cycle := 0; cycle < cycles; cycle++ {
		r := srv.newReader(t)
		ctx := context.Background()
		openWithRetry(t, r, ctx)

		peer := srv.accept(t)
		_, err := peer.Write(payload)
		require.NoError(t, err, "cycle %d: peer write", cycle)

		got, err := readWithTimeout(t, r, ctx, 3*time.Second)
		require.NoError(t, err, "cycle %d: read", cycle)
		require.Equal(t, payload, got, "cycle %d: payload round-trip", cycle)

		require.NoError(t, r.Close(), "cycle %d: close", cycle)
		// Both sides closing must not error either.
		require.NoError(t, peer.Close())
	}
}

// TestSRTReader_LoopbackContextCancel distinguishes a local stop from a peer
// disconnect: the blocked Read must surface context.Canceled, never io.EOF.
func TestSRTReader_LoopbackContextCancel(t *testing.T) {
	srv := newSRTTestServer(t)
	r := srv.newReader(t)

	ctx, cancel := context.WithCancel(context.Background())
	openWithRetry(t, r, ctx)
	peer := srv.accept(t)
	t.Cleanup(func() { _ = peer.Close() })

	readErr := make(chan error, 1)
	go func() {
		_, err := r.Read(ctx)
		readErr <- err
	}()

	// Let the Read park inside gosrt before cancelling.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-readErr:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled, "local cancellation must not be reported as EOF")
		assert.NotErrorIs(t, err, io.EOF)
	case <-time.After(3 * time.Second):
		t.Fatal("blocked Read did not return after context cancellation")
	}

	require.NoError(t, r.Close())
}

// TestSRTReader_LoopbackExplicitCloseUnblocksRead verifies Close alone
// (without touching the caller context) releases a blocked Read promptly.
func TestSRTReader_LoopbackExplicitCloseUnblocksRead(t *testing.T) {
	srv := newSRTTestServer(t)
	r := srv.newReader(t)

	ctx := context.Background()
	openWithRetry(t, r, ctx)
	peer := srv.accept(t)
	t.Cleanup(func() { _ = peer.Close() })

	readErr := make(chan error, 1)
	go func() {
		_, err := r.Read(ctx)
		readErr <- err
	}()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	require.NoError(t, r.Close())

	select {
	case err := <-readErr:
		require.Error(t, err)
		assert.NotErrorIs(t, err, io.EOF, "local Close must not masquerade as upstream EOF")
	case <-time.After(3 * time.Second):
		t.Fatal("blocked Read did not return after Close")
	}
	assert.Less(t, time.Since(start), 2*time.Second, "Close must unblock the Read promptly")
}

// TestSRTReader_LoopbackUpstreamDisconnectIsEOF proves the peer going away
// still yields io.EOF so the ingestor worker keeps its backoff reconnect path.
func TestSRTReader_LoopbackUpstreamDisconnectIsEOF(t *testing.T) {
	srv := newSRTTestServer(t)
	r := srv.newReader(t)

	ctx := context.Background()
	openWithRetry(t, r, ctx)
	peer := srv.accept(t)

	readErr := make(chan error, 1)
	go func() {
		_, err := r.Read(ctx)
		readErr <- err
	}()
	time.Sleep(100 * time.Millisecond)

	// Upstream tears its side down.
	require.NoError(t, peer.Close())

	select {
	case err := <-readErr:
		assert.ErrorIs(t, err, io.EOF, "upstream disconnect must remain io.EOF")
	case <-time.After(5 * time.Second):
		t.Fatal("blocked Read did not return after peer disconnect")
	}

	// Reader-level Close after EOF: safe, idempotent, never an error.
	require.NoError(t, r.Close())
	require.NoError(t, r.Close())
}

// TestSRTReader_LoopbackReopenAfterStop mirrors the ops pattern: stop the
// stream, immediately start it again against the same listener.
func TestSRTReader_LoopbackReopenAfterStop(t *testing.T) {
	srv := newSRTTestServer(t)
	r := srv.newReader(t)
	payload := bytes.Repeat([]byte{0x47}, 1316)

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		openWithRetry(t, r, ctx)
		peer := srv.accept(t)

		// Stop via context (the worker's shutdown path), then explicit Close.
		cancel()
		require.NoError(t, r.Close())
		require.NoError(t, peer.Close())

		// Reopen on the same instance with a fresh context and read again.
		ctx2 := context.Background()
		openWithRetry(t, r, ctx2)
		peer2 := srv.accept(t)
		_, err := peer2.Write(payload)
		require.NoError(t, err)
		got, err := readWithTimeout(t, r, ctx2, 3*time.Second)
		require.NoError(t, err)
		require.Equal(t, payload, got)
		require.NoError(t, r.Close())
		require.NoError(t, peer2.Close())
	}
}

// TestSRTReader_LoopbackConcurrentClose hammers Close concurrently while one
// Read is parked (the production contract: a single read loop per reader) to
// validate the locking against the real gosrt connection under -race.
func TestSRTReader_LoopbackConcurrentClose(t *testing.T) {
	srv := newSRTTestServer(t)

	for range 5 {
		r := srv.newReader(t)
		ctx := context.Background()
		openWithRetry(t, r, ctx)
		peer := srv.accept(t)

		var wg sync.WaitGroup
		// Exactly one blocked Read — gosrt's Conn.Read is not safe for
		// concurrent callers (it owns an internal read buffer).
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Read(ctx)
		}()
		time.Sleep(50 * time.Millisecond)
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = r.Close()
			}()
		}
		wg.Wait()
		_ = peer.Close()
	}
}
