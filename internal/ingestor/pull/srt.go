package pull

// srt.go — SRT pull ingestor (caller mode).
//
// Dials a remote SRT endpoint and reads raw MPEG-TS. SRT is a transport
// protocol — no codec conversion is needed; bytes are forwarded directly to
// the TSDemuxPacketReader pipeline.
//
// URL format (all standard SRT query parameters are honoured):
//
//	srt://host:port
//	srt://host:port?streamid=live/stream&latency=200&passphrase=secret
//	srt://host:port?conntimeo=5000&rcvlatency=120&peerlatency=120
//
// Any parameters in domain.Input.Params are merged into the URL query string
// before parsing, so callers can pass SRT options without embedding them in
// the URL (useful when the URL is user-supplied and opaque).
//
// Concurrency model
//
// gosrt's SetReadDeadline is a no-op, so a blocked conn.Read() can only be
// released by closing the connection. Every Open() therefore starts a watcher
// goroutine that closes the underlying connection when the caller's context
// is cancelled.
//
// All mutable state (the active session, including the connection and the
// watcher goroutine) is owned by exactly one *srtSession at a time and
// guarded by SRTReader.mu. The session is never mutated in place after
// publication: Open() installs a new session and Close() detaches the whole
// session under the same lock. That makes the open → read → close → reopen
// lifecycle race-free even when stop/reconnect interleave:
//
//   - a session finished by context cancellation or an upstream disconnect
//     detaches itself, so the next Open() installs a fresh session whose
//     connection can never be closed by the previous generation's watcher;
//   - Read() snapshots the session pointer under the lock and then blocks on
//     that immutable session's connection, so Close() racing a Read never
//     touches a freed connection nor nils a pointer the Read still uses;
//   - Close() is idempotent and safe before Open(); the watcher goroutine is
//     joined before the connection's Close() returns, so no goroutine leaks.
//
// Read error classification:
//
//   - caller context cancelled (either via ctx or explicit Close) →
//     context.Canceled / context.DeadlineExceeded, so the worker treats it as
//     a local stop rather than an upstream event;
//   - upstream closed the connection → io.EOF, keeping the existing
//     failover/backoff contract in the ingestor worker.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sync"
	"time"

	srt "github.com/datarhei/gosrt"

	"github.com/datvietvac-techhub/open-streamer/internal/domain"
)

// srtReadChunk matches the SRT live-streaming convention: one datagram carries
// exactly 7 MPEG-TS packets (7 × 188 = 1316 bytes ≤ typical SRT payload size).
const srtReadChunk = 188 * 7

// errSRTClosed is recorded on a session when the reader is shut down locally
// (Close() without a cancelled caller context). gosrt returns the same io.EOF
// from a blocked Read for both a local close and an upstream shutdown, so the
// session needs its own marker to tell the two cases apart.
var errSRTClosed = errors.New("srt reader: closed")

// srtConn is the subset of srt.Conn the reader depends on. Declaring it as an
// interface lets tests substitute an in-process connection; production code
// always runs against the real srt.Conn returned by srt.Dial.
type srtConn interface {
	Read(p []byte) (int, error)
	Close() error
	StreamId() string
}

// srtDialFunc opens one SRT caller connection. It is a field (not a package
// variable) so tests can inject a fake dialer per reader without mutating
// global state.
type srtDialFunc func(network, address string, cfg srt.Config) (srtConn, error)

// srtSession is the immutable-at-publication state of one Open() generation.
// Nothing outside SRTReader mutates its fields after the session is installed;
// only the session's own mutex transitions closeErr/closed during teardown.
type srtSession struct {
	conn srtConn

	// stopWatch is closed once to tell the watcher goroutine that the
	// session is being torn down locally (Close path), as opposed to the
	// caller context firing.
	stopWatch     chan struct{}
	stopWatchOnce sync.Once

	// watchDone is closed by the watcher goroutine on exit. Close() joins on
	// it so no watcher outlives the connection it may close.
	watchDone chan struct{}

	closeMu  sync.Mutex
	closed   bool  // true once finish() has run
	closeErr error // why the session ended: nil = upstream ended it

	// closeOnce serialises conn.Close() across watcher/Close/Read.
	closeOnce sync.Once
}

// SRTReader dials an SRT server in caller mode and emits raw MPEG-TS chunks.
// It implements TSChunkReader and is intended to wrap with NewTSDemuxPacketReader.
type SRTReader struct {
	input domain.Input
	buf   []byte

	// dial is overridable in tests; defaults to the real gosrt dialer.
	dial srtDialFunc

	mu      sync.Mutex
	session *srtSession // nil when not open
}

// NewSRTReader constructs an SRTReader without opening a connection.
func NewSRTReader(input domain.Input) *SRTReader {
	r := &SRTReader{
		input: input,
		buf:   make([]byte, srtReadChunk),
	}
	r.dial = r.defaultDial
	return r
}

// defaultDial adapts the package-level srt.Dial to the srtDialFunc seam.
func (r *SRTReader) defaultDial(network, address string, cfg srt.Config) (srtConn, error) {
	return srt.Dial(network, address, cfg)
}

// Open dials the remote SRT endpoint in caller mode and starts the context
// watcher for the new session.
// All SRT options present in the URL query string or in input.Params are
// applied to the connection config via Config.UnmarshalURL.
//
// Open fails if the reader is already open — callers must Close() before
// re-opening. A failed dial leaves no session behind, so the reader can be
// retried freely.
func (r *SRTReader) Open(ctx context.Context) error {
	cfg, host, err := r.buildConfig()
	if err != nil {
		return fmt.Errorf("srt reader: config %q: %w", r.input.URL, err)
	}

	r.mu.Lock()
	if r.session != nil {
		r.mu.Unlock()
		return errors.New("srt reader: already open")
	}
	dial := r.dial
	r.mu.Unlock()

	conn, err := dial("srt", host, cfg)
	if err != nil {
		return fmt.Errorf("srt reader: dial %q: %w", host, err)
	}

	sess := &srtSession{
		conn:      conn,
		stopWatch: make(chan struct{}),
		watchDone: make(chan struct{}),
	}

	// Install the session before starting the watcher so a cancellation that
	// fires immediately still resolves against a published session.
	r.mu.Lock()
	if r.session != nil {
		// Defensive: a concurrent Open is rejected above, so this is only
		// reachable if the contract is broken externally. Tear down the
		// connection we just dialled rather than leaking it.
		r.mu.Unlock()
		_ = conn.Close()
		return errors.New("srt reader: already open")
	}
	r.session = sess
	r.mu.Unlock()

	go r.watchContext(ctx, sess)

	slog.Info("srt reader: connected",
		"url", r.input.URL,
		"host", host,
		"stream_id", conn.StreamId(),
		"latency_ms", cfg.ReceiverLatency.Milliseconds(),
	)

	return nil
}

// watchContext closes the session's connection when the caller's ctx is
// cancelled. It exits without touching the connection when stopWatch closes
// first (explicit Close path). Either way it closes watchDone exactly once so
// Close() can join it.
func (r *SRTReader) watchContext(callerCtx context.Context, sess *srtSession) {
	defer close(sess.watchDone)

	select {
	case <-callerCtx.Done():
		// Context-driven stop. Record the context's error before detaching so
		// a blocked Read returns ctx.Err() rather than io.EOF.
		r.finishSession(sess, callerCtx.Err())
	case <-sess.stopWatch:
		// Explicit Close() owns the teardown; nothing more to do here.
	}
}

// finishSession closes the underlying connection once and detaches the
// session from the reader when it is still the active generation. It is safe
// to call from the watcher goroutine, from Read (upstream EOF), and from
// Close — every path converges here and every operation is idempotent.
//
// reason is nil when the peer ended the connection (Read saw io.EOF on its
// own), and non-nil for every locally-initiated teardown (context error or
// explicit Close).
func (r *SRTReader) finishSession(sess *srtSession, reason error) {
	sess.closeMu.Lock()
	if sess.closed {
		sess.closeMu.Unlock()
		return
	}
	sess.closed = true
	sess.closeErr = reason
	sess.closeMu.Unlock()

	// Unblocks any Read blocked on this connection. gosrt's Close is itself
	// idempotent; the Once additionally guarantees a single call from this
	// type regardless of how many paths race here.
	sess.closeOnce.Do(func() {
		_ = sess.conn.Close()
	})

	// Detach only if we are still the installed session. A newer Open()
	// generation can never be affected: it would have replaced r.session, and
	// its connection is a different object.
	r.mu.Lock()
	if r.session == sess {
		r.session = nil
	}
	r.mu.Unlock()
}

// Read returns the next raw MPEG-TS chunk from the SRT stream.
//
// Error contract:
//   - context.Canceled / context.DeadlineExceeded when the caller's context
//     was cancelled or the reader was Closed locally;
//   - io.EOF when the remote endpoint ended the connection — the ingestor
//     worker relies on this for its failover/backoff path;
//   - any other error is the underlying transport failure.
func (r *SRTReader) Read(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	sess := r.session
	r.mu.Unlock()
	if sess == nil {
		return nil, errSRTClosed
	}

	n, err := sess.conn.Read(r.buf)
	if n > 0 {
		out := make([]byte, n)
		copy(out, r.buf[:n])
		return out, nil
	}

	if err == nil {
		return nil, nil // transient zero-byte read; caller will retry
	}

	// gosrt surfaces both local close and peer shutdown as io.EOF (and
	// occasionally ErrClientClosed on the dialer wrapper). The session's
	// recorded close reason is the only reliable discriminator.
	if errors.Is(err, io.EOF) || errors.Is(err, srt.ErrClientClosed) {
		// Snapshot the teardown reason under the session lock.
		sess.closeMu.Lock()
		closed := sess.closed
		reason := sess.closeErr
		sess.closeMu.Unlock()

		if closed {
			// Local teardown. Prefer the caller's live context error so a
			// deadline cancellation still reports DeadlineExceeded.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if reason != nil {
				return nil, reason
			}
			return nil, errSRTClosed
		}

		// The connection ended without a local close: the peer went away.
		// Detach the dead session so the next Open() starts clean, and keep
		// the historical io.EOF contract for the worker's reconnect path.
		r.finishSession(sess, nil)
		return nil, io.EOF
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return nil, fmt.Errorf("srt reader: read: %w", err)
}

// Close stops the context watcher, closes the SRT socket, and waits for the
// watcher goroutine to exit. Safe to call before Open, concurrently with
// Read or another Close, and repeatedly. It always returns nil: a double
// close and a close-before-open are normal lifecycle events, not errors.
func (r *SRTReader) Close() error {
	r.mu.Lock()
	sess := r.session
	r.mu.Unlock()
	if sess == nil {
		return nil
	}

	// Tell the watcher not to run its own teardown, then wait for it. The
	// watcher might already be finishing a context cancellation; in that case
	// signalling stopWatch again is harmless because its select has already
	// chosen the callerCtx branch and finishSession is idempotent. sync.Once
	// keeps concurrent Close() calls from panicking on a closed channel.
	sess.stopWatchOnce.Do(func() { close(sess.stopWatch) })
	<-sess.watchDone

	// Runs the idempotent conn.Close + detach. If the watcher already did it
	// (context cancellation raced this Close), this is a no-op.
	r.finishSession(sess, errSRTClosed)
	return nil
}

// ─── Config helpers ──────────────────────────────────────────────────────────

// buildConfig constructs the gosrt Config from the input URL and Params.
// Returns the resolved host address alongside the config.
func (r *SRTReader) buildConfig() (srt.Config, string, error) {
	rawURL := r.mergedURL()

	cfg := srt.DefaultConfig()
	host, err := cfg.UnmarshalURL(rawURL)
	if err != nil {
		return srt.Config{}, "", err
	}

	// Explicit connect timeout from InputNetConfig overrides any ?conntimeo= in the URL.
	if r.input.Net.TimeoutSec > 0 {
		cfg.ConnectionTimeout = time.Duration(r.input.Net.TimeoutSec) * time.Second
	}

	return cfg, host, nil
}

// mergedURL returns the input URL with any input.Params merged into the query
// string (Params values override URL-embedded values for the same key).
func (r *SRTReader) mergedURL() string {
	if len(r.input.Params) == 0 {
		return r.input.URL
	}
	u, err := url.Parse(r.input.URL)
	if err != nil {
		return r.input.URL
	}
	q := u.Query()
	for k, v := range r.input.Params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
