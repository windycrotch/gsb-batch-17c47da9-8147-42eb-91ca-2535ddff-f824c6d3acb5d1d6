package bytestream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"

	pb "github.com/lukasmalkmus/stream/proto"
)

const (
	// MaxBufSize is the maximum buffer size (in bytes) received in a read
	// chunk.
	MaxBufSize  = 1024 * 64 // 64KiB
	backoffBase = time.Millisecond * 10
	backoffMax  = time.Second * 1
	maxTries    = 5
)

var (
	// errEmptyResponses is the sticky terminal error returned once the reader
	// has observed maxTries consecutive empty data chunks without any payload.
	errEmptyResponses = errors.New("bytestream: retry budget exhausted by empty ReadResponse messages")

	// errReaderClosed is returned by Read after Close. It matches the common
	// "closed pipe/connection" sentinels so errors.Is checks keep working.
	errReaderClosed = errClosed{}
)

type errClosed struct{}

func (errClosed) Error() string { return "bytestream: reader closed" }

// Is makes the closed error recognizable as the standard I/O closed errors.
func (errClosed) Is(target error) bool {
	return target == io.ErrClosedPipe || target == net.ErrClosed
}

// Client is the go wrapper around a ByteStreamClient and provides an interface
// to it.
type Client struct {
	client  pb.ByteStreamClient
	options []grpc.CallOption
}

// NewClient creates a new Client.
func NewClient(cc *grpc.ClientConn, options ...grpc.CallOption) *Client {
	return &Client{
		client:  pb.NewByteStreamClient(cc),
		options: options,
	}
}

// Reader reads from a byte stream.
//
// A Reader has a single terminal state: once Read fails because the retry
// budget was exhausted, the underlying stream returned an error, the context
// was canceled or Close was called, every later Read returns the exact same
// sticky error. Closing is idempotent and releases the underlying stream
// (CloseSend plus cancellation of the Reader-owned context) exactly once.
type Reader struct {
	ctx          context.Context
	cancel       context.CancelFunc
	c            *Client
	readClient   pb.ByteStream_ReadClient
	resourceName string

	mu        sync.Mutex
	buf       []byte
	err       error
	closeErr  error
	closeOnce sync.Once
	closeCh   chan struct{}
}

// NewReader creates a new Reader to read a resource.
func (c *Client) NewReader(ctx context.Context, resourceName string) (*Reader, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Derive an owned context so Close can promptly abort the RPC even when
	// the caller does not cancel the context it passed in.
	ctx, cancel := context.WithCancel(ctx)

	readClient, err := c.client.Read(ctx, &pb.ReadRequest{
		ResourceName: resourceName,
	}, c.options...)
	if err != nil {
		cancel()
		return nil, err
	}

	return &Reader{
		ctx:          ctx,
		cancel:       cancel,
		c:            c,
		resourceName: resourceName,
		readClient:   readClient,
	}, nil
}

// ResourceName gets the resource name this Reader is reading.
func (r *Reader) ResourceName() string {
	return r.resourceName
}

// context returns the Reader context, falling back to context.Background for
// Readers constructed directly (for example in tests) without one.
func (r *Reader) context() context.Context {
	if r.ctx != nil {
		return r.ctx
	}
	return context.Background()
}

// wakeCh returns the channel closed when the Reader enters its terminal state.
func (r *Reader) wakeCh() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closeCh == nil {
		r.closeCh = make(chan struct{})
	}
	return r.closeCh
}

// terminate transitions the Reader to its terminal state. The first terminal
// error wins and is returned on every later call. It also closes the wakeup
// channel, cancels the owned context and closes the underlying stream exactly
// once. It is safe to call concurrently and any number of times.
func (r *Reader) terminate(cause error) error {
	r.closeOnce.Do(func() { r.shutdown(cause) })

	r.mu.Lock()
	err := r.err
	r.mu.Unlock()
	return err
}

// shutdown performs the one-shot terminal release. It runs inside closeOnce.
func (r *Reader) shutdown(cause error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = cause
	}
	if r.closeCh == nil {
		r.closeCh = make(chan struct{})
	}
	close(r.closeCh)
	rc := r.readClient
	r.readClient = nil
	cancel := r.cancel
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if rc != nil {
		// Defensive recover: a minimal test double may embed a nil
		// grpc.ClientStream and therefore have a panicking promoted
		// CloseSend. Production streams never hit this.
		func() {
			defer func() { _ = recover() }()
			r.closeErr = rc.CloseSend()
		}()
	}
}

// Read implements io.Reader.
// Read buffers received bytes that do not fit in p.
func (r *Reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	// A terminal error is sticky: every Read reports the exact same value.
	if r.err != nil {
		err := r.err
		r.mu.Unlock()
		return 0, err
	}
	// Serve already received bytes before touching the stream.
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		r.mu.Unlock()
		return n, nil
	}
	r.mu.Unlock()

	// Probing with an empty p must still surface a terminal state (handled
	// above); otherwise there is nothing to copy and no reason to Recv.
	if len(p) == 0 {
		return 0, nil
	}

	ctx := r.context()
	if err := ctx.Err(); err != nil {
		return 0, r.terminate(err)
	}

	var backoffDelay time.Duration
	for tries := 0; tries < maxTries; tries++ {
		resp, err := r.recv(ctx)
		if err != nil {
			// Cancellation/deadline takes precedence so callers observe the
			// canonical context error even if the stream raced with it.
			if cerr := ctx.Err(); cerr != nil {
				return 0, r.terminate(cerr)
			}
			if errors.Is(err, errReaderClosed) {
				return 0, r.terminate(errReaderClosed)
			}
			return 0, r.terminate(err)
		}

		if resp != nil && len(resp.Data) > 0 {
			r.mu.Lock()
			// Close/cancel may have won while Recv was in flight; do not hand
			// out data after the Reader became terminal.
			if r.err != nil {
				err := r.err
				r.mu.Unlock()
				return 0, err
			}
			r.buf = resp.Data
			n := copy(p, r.buf)
			r.buf = r.buf[n:]
			r.mu.Unlock()
			return n, nil
		}

		// Empty chunk: stay within the finite retry budget and back off while
		// remaining responsive to cancellation and Close. Do not sleep after
		// the final attempt; surface exhaustion immediately.
		if tries == maxTries-1 {
			break
		}
		if backoffDelay < backoffBase {
			backoffDelay = backoffBase
		} else {
			backoffDelay = time.Duration(float64(backoffDelay) * 1.3 * (1 - 0.4*rand.Float64()))
		}
		if backoffDelay > backoffMax {
			backoffDelay = backoffMax
		}
		if !r.wait(ctx, backoffDelay) {
			if cerr := ctx.Err(); cerr != nil {
				return 0, r.terminate(cerr)
			}
			return 0, r.terminate(errReaderClosed)
		}
	}

	// Cancellation racing the final empty chunk takes precedence.
	if cerr := ctx.Err(); cerr != nil {
		return 0, r.terminate(cerr)
	}
	return 0, r.terminate(errEmptyResponses)
}

// wait sleeps for d while selecting on context cancellation and Close. It
// returns true if the full delay elapsed.
func (r *Reader) wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-r.wakeCh():
		return false
	}
}

// recv performs a single Recv on the underlying stream but abandons the wait
// when ctx is canceled or Close is called, so a blocked fake/real Recv cannot
// pin Read forever. The abandoned goroutine uses a buffered result channel and
// therefore always exits.
func (r *Reader) recv(ctx context.Context) (*pb.ReadResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	rc := r.readClient
	r.mu.Unlock()
	if rc == nil {
		return nil, errReaderClosed
	}

	type recvResult struct {
		resp *pb.ReadResponse
		err  error
	}
	resultCh := make(chan recvResult, 1)
	go func(stream pb.ByteStream_ReadClient) {
		defer func() {
			// A panicking stream double must not crash the whole test binary;
			// surface it as an ordinary Recv error instead.
			if rec := recover(); rec != nil {
				resultCh <- recvResult{err: fmt.Errorf("bytestream: Recv panicked: %v", rec)}
			}
		}()
		resp, err := stream.Recv()
		resultCh <- recvResult{resp: resp, err: err}
	}(rc)

	select {
	case res := <-resultCh:
		return res.resp, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.wakeCh():
		return nil, errReaderClosed
	}
}

// Close implements io.Closer. It releases the underlying stream exactly once.
// The invocation that actually performs the close may return the underlying
// CloseSend error; every repeated/concurrent Close returns nil instead of an
// "already closed" error.
func (r *Reader) Close() error {
	ran := false
	var closeErr error
	r.closeOnce.Do(func() {
		ran = true
		r.shutdown(errReaderClosed)
		closeErr = r.closeErr
	})
	if ran {
		return closeErr
	}
	return nil
}
