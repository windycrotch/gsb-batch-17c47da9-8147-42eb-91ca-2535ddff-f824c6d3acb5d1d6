package bytestream

// Verification tests for the Reader lifecycle.
//
// All top-level identifiers carry the "agentV" prefix so these files can
// coexist with externally supplied grader test files in package bytestream
// without redeclaration conflicts.

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/lukasmalkmus/stream/proto"
)

// agentVFakeReadClient is a controllable pb.ByteStream_ReadClient double.
// Only Recv and CloseSend are operational; embedding the interface satisfies
// the rest of grpc.ClientStream.
type agentVFakeReadClient struct {
	grpc.ClientStream

	mu         sync.Mutex
	recvCalls  int
	closeCalls int
	recvFn     func(call int) (*pb.ReadResponse, error)
	closeFn    func(call int) error
}

func (f *agentVFakeReadClient) Recv() (*pb.ReadResponse, error) {
	f.mu.Lock()
	call := f.recvCalls
	f.recvCalls++
	fn := f.recvFn
	f.mu.Unlock()
	if fn == nil {
		return nil, io.EOF
	}
	return fn(call)
}

func (f *agentVFakeReadClient) CloseSend() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := f.closeCalls
	f.closeCalls++
	if f.closeFn != nil {
		return f.closeFn(call)
	}
	return nil
}

func (f *agentVFakeReadClient) counts() (recvCalls, closeCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recvCalls, f.closeCalls
}

// agentVWaitFor waits until cond succeeds or the timeout elapses.
func agentVWaitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond * 2)
	}
	if !cond() {
		t.Fatalf("timed out waiting for: %s", msg)
	}
}

// Empty chunks below the retry limit must be tolerated and subsequent data
// must still be delivered.
func TestAgentVReader_EmptyChunksThenRecover(t *testing.T) {
	fc := &agentVFakeReadClient{}
	payload := []string{"hello ", "world", "!"}
	fc.recvFn = func(call int) (*pb.ReadResponse, error) {
		switch call {
		case 0, 1:
			return &pb.ReadResponse{}, nil // server keep-alive empties
		case 2:
			return &pb.ReadResponse{Data: []byte(payload[0])}, nil
		case 3:
			return &pb.ReadResponse{Data: []byte(payload[1])}, nil
		case 4:
			return &pb.ReadResponse{Data: []byte(payload[2])}, nil
		default:
			return nil, io.EOF
		}
	}

	r := &Reader{ctx: context.Background(), readClient: fc}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll returned unexpected error after empty-chunk recovery: %v", err)
	}
	if string(got) != "hello world!" {
		t.Fatalf("payload mismatch: got %q", string(got))
	}

	// Normal completion closes the underlying stream exactly once.
	if _, closes := fc.counts(); closes != 1 {
		t.Fatalf("CloseSend calls after EOF = %d, want 1", closes)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("first Close after EOF: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("repeated Close must not error, got: %v", err)
	}
	if _, closes := fc.counts(); closes != 1 {
		t.Fatalf("CloseSend calls after repeated Close = %d, want exactly 1", closes)
	}
}

// Persistent empty chunks exhaust the finite retry budget: the first and all
// later Reads return the same sticky error with n == 0, and the stream closes
// once.
func TestAgentVReader_EmptyChunksExhaustedSticky(t *testing.T) {
	fc := &agentVFakeReadClient{
		recvFn: func(call int) (*pb.ReadResponse, error) {
			return &pb.ReadResponse{}, nil // forever empty
		},
	}
	r := &Reader{ctx: context.Background(), readClient: fc}
	buf := make([]byte, 32)

	n, errFirst := r.Read(buf)
	if errFirst == nil {
		t.Fatal("expected error after retry exhaustion, got nil")
	}
	if n != 0 {
		t.Fatalf("first exhausted Read returned n=%d, want 0", n)
	}

	recvCalls, closes := fc.counts()
	if recvCalls != maxTries {
		t.Fatalf("Recv calls = %d, want maxTries=%d", recvCalls, maxTries)
	}
	if closes != 1 {
		t.Fatalf("CloseSend calls after exhaustion = %d, want 1", closes)
	}

	for i := 0; i < 3; i++ {
		n, err := r.Read(buf)
		if n != 0 {
			t.Fatalf("sticky Read #%d returned n=%d, want 0", i, n)
		}
		if !errors.Is(err, errFirst) || err != errFirst {
			t.Fatalf("sticky Read #%d error = %v (%p), want identical %v (%p)", i, err, err, errFirst, errFirst)
		}
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close after exhaustion: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
	if _, closes := fc.counts(); closes != 1 {
		t.Fatalf("CloseSend calls = %d, want exactly 1", closes)
	}
}

// An underlying stream error becomes the stable terminal error.
func TestAgentVReader_RecvErrorSticky(t *testing.T) {
	errBoom := errors.New("agentV: stream broken")
	fc := &agentVFakeReadClient{
		recvFn: func(call int) (*pb.ReadResponse, error) {
			return nil, errBoom
		},
	}
	r := &Reader{ctx: context.Background(), readClient: fc}
	buf := make([]byte, 8)

	n, err := r.Read(buf)
	if n != 0 || !errors.Is(err, errBoom) {
		t.Fatalf("first Read = (%d, %v), want (0, %v)", n, err, errBoom)
	}
	n, err = r.Read(buf)
	if n != 0 || err != errBoom {
		t.Fatalf("second Read = (%d, %v), want (0, identical %v)", n, err, errBoom)
	}
	if _, closes := fc.counts(); closes != 1 {
		t.Fatalf("CloseSend calls = %d, want 1", closes)
	}
}

// Cancellation while Read is blocked inside Recv must make Read return
// context.Canceled promptly, sticky, and close the stream once.
func TestAgentVReader_CancelWhileBlockedInRecv(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	fc := &agentVFakeReadClient{
		recvFn: func(call int) (*pb.ReadResponse, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return nil, io.EOF
		},
	}
	defer close(release) // let the abandoned Recv goroutine finish

	r := &Reader{ctx: ctx, readClient: fc}
	type readResult struct {
		n   int
		err error
	}
	done := make(chan readResult, 1)
	go func() {
		n, err := r.Read(make([]byte, 16))
		done <- readResult{n, err}
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Recv was not entered in time")
	}

	cancel()
	select {
	case res := <-done:
		if res.n != 0 || !errors.Is(res.err, context.Canceled) {
			t.Fatalf("Read after cancel = (%d, %v), want (0, context.Canceled)", res.n, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return promptly after context cancellation")
	}

	// Stable identical cancellation error afterwards.
	n, err := r.Read(make([]byte, 8))
	if n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("Read after canceled Read = (%d, %v), want context.Canceled", n, err)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close after cancel: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("repeated Close after cancel: %v", err)
	}
	agentVWaitFor(t, time.Second, func() bool {
		_, c := fc.counts()
		return c == 1
	}, "exactly one CloseSend after cancel")
}

// Cancellation during the empty-chunk backoff wait must also be observed.
func TestAgentVReader_CancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fc := &agentVFakeReadClient{
		recvFn: func(call int) (*pb.ReadResponse, error) {
			return &pb.ReadResponse{}, nil
		},
	}
	r := &Reader{ctx: ctx, readClient: fc}

	type readResult struct {
		n   int
		err error
	}
	done := make(chan readResult, 1)
	go func() {
		n, err := r.Read(make([]byte, 16))
		done <- readResult{n, err}
	}()

	agentVWaitFor(t, time.Second, func() bool {
		recv, _ := fc.counts()
		return recv >= 1
	}, "first empty Recv")

	cancel()
	select {
	case res := <-done:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("Read = %v, want context.Canceled", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not respond to cancellation during backoff")
	}

	n, err := r.Read(make([]byte, 8))
	if n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("sticky Read = (%d, %v), want context.Canceled", n, err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

// Read after Close must fail immediately; repeated Close is a no-error
// no-op and CloseSend happens exactly once.
func TestAgentVReader_ReadAfterCloseAndIdempotentClose(t *testing.T) {
	recved := make(chan struct{}, 1)
	fc := &agentVFakeReadClient{
		recvFn: func(call int) (*pb.ReadResponse, error) {
			select {
			case recved <- struct{}{}:
			default:
			}
			return nil, errors.New("Recv must not be called after Close")
		},
	}
	r := &Reader{ctx: context.Background(), readClient: fc}

	if err := r.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	start := time.Now()
	n, err := r.Read(make([]byte, 16))
	if err == nil {
		t.Fatal("Read after Close returned nil error")
	}
	if n != 0 {
		t.Fatalf("Read after Close returned n=%d, want 0", n)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Read after Close took %v, want immediate failure", d)
	}
	// Idiomatic closed-pipe recognition.
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("closed error %v should match io.ErrClosedPipe", err)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("third Close: %v", err)
	}
	if recv, closes := fc.counts(); recv != 0 || closes != 1 {
		t.Fatalf("calls = (recv=%d, close=%d), want (0, 1)", recv, closes)
	}
}

// Close must also wake a Read currently waiting in Recv.
func TestAgentVReader_CloseUnblocksBlockedRead(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	fc := &agentVFakeReadClient{
		recvFn: func(call int) (*pb.ReadResponse, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return nil, nil
		},
	}
	defer close(release)

	r := &Reader{ctx: context.Background(), readClient: fc}
	done := make(chan error, 1)
	go func() {
		_, err := r.Read(make([]byte, 8))
		done <- err
	}()
	<-entered

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked Read returned nil after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock in-flight Read")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
	if _, closes := fc.counts(); closes != 1 {
		t.Fatalf("CloseSend calls = %d, want 1", closes)
	}
}

// Small destination buffers must reassemble chunk bytes exactly across the
// internal remainder buffer without additional Recv calls while buffered.
func TestAgentVReader_PartialReadsPreserveBytes(t *testing.T) {
	fc := &agentVFakeReadClient{
		recvFn: func(call int) (*pb.ReadResponse, error) {
			if call == 0 {
				return &pb.ReadResponse{Data: []byte("abcdefghij")}, nil
			}
			return nil, io.EOF
		},
	}
	r := &Reader{ctx: context.Background(), readClient: fc}

	var got []byte
	tmp := make([]byte, 3)
	for {
		n, err := r.Read(tmp)
		got = append(got, tmp[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if string(got) != "abcdefghij" {
		t.Fatalf("reassembled = %q, want abcdefghij", got)
	}
	// First ten bytes are served from one Recv plus EOF probe.
	if recv, _ := fc.counts(); recv != 2 {
		t.Fatalf("Recv calls = %d, want 2 (one data + EOF)", recv)
	}
}

// Concurrent Read/Close exercise the mutex and close-once paths under -race.
func TestAgentVReader_ConcurrentReadAndClose(t *testing.T) {
	fc := &agentVFakeReadClient{
		recvFn: func(call int) (*pb.ReadResponse, error) {
			time.Sleep(time.Millisecond)
			return &pb.ReadResponse{Data: []byte{byte(call)}}, nil
		},
	}
	r := &Reader{ctx: context.Background(), readClient: fc}

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 7)
			for i := 0; i < 50; i++ {
				if _, err := r.Read(buf); err != nil {
					return
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()

	if err := r.Close(); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
	if _, closes := fc.counts(); closes != 1 {
		t.Fatalf("CloseSend calls = %d, want 1", closes)
	}
}
