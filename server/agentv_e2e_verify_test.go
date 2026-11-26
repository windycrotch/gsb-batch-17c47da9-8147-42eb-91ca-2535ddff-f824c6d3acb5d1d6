package main

// End-to-end checks through a real in-process gRPC transport (bufconn):
// production bytestreamSrv.Read + bytestream.Client/Reader. No real network
// or sound card is involved.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/lukasmalkmus/stream/bytestream"
	pb "github.com/lukasmalkmus/stream/proto"
)

const agentVBufnetAddr = "passthrough:///agentv-bufnet"

// agentVTrackedService signals when the production Read handler returns.
type agentVTrackedService struct {
	pb.ByteStreamServer
	finished chan error
}

func (s *agentVTrackedService) Read(req *pb.ReadRequest, stream pb.ByteStream_ReadServer) error {
	err := s.ByteStreamServer.Read(req, stream)
	select {
	case s.finished <- err:
	default:
	}
	return err
}

type agentVE2E struct {
	client   *bytestream.Client
	tracked  *agentVTrackedService
	listener *bufconn.Listener
	server   *grpc.Server
	conn     *grpc.ClientConn
}

func (e *agentVE2E) close() {
	if e.conn != nil {
		_ = e.conn.Close()
	}
	if e.server != nil {
		e.server.Stop()
	}
	if e.listener != nil {
		_ = e.listener.Close()
	}
}

func agentVStartE2E(t *testing.T) *agentVE2E {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	tracked := &agentVTrackedService{
		ByteStreamServer: &bytestreamSrv{},
		finished:         make(chan error, 64),
	}
	srv := grpc.NewServer()
	pb.RegisterByteStreamServer(srv, tracked)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(
		agentVBufnetAddr,
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		srv.Stop()
		_ = lis.Close()
		t.Fatalf("grpc.NewClient: %v", err)
	}

	return &agentVE2E{
		client:   bytestream.NewClient(conn),
		tracked:  tracked,
		listener: lis,
		server:   srv,
		conn:     conn,
	}
}

// Full production-path read returns exactly the file bytes and Close is
// idempotent.
func TestAgentVE2E_FullReadMatchesFile(t *testing.T) {
	env := agentVStartE2E(t)
	defer env.close()

	path, data := agentVMakeFile(t, 250_001)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	r, err := env.client.NewReader(ctx, path)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("bytes mismatch: got %d, want %d", len(got), len(data))
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
}

// Many Readers opened concurrently on the same resource must each receive the
// exact bytes and close independently without cross-stream interference.
func TestAgentVE2E_ConcurrentOpenSameResource(t *testing.T) {
	env := agentVStartE2E(t)
	defer env.close()

	path, data := agentVMakeFile(t, 512_003)
	const n = 8

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			r, err := env.client.NewReader(ctx, path)
			if err != nil {
				errs <- err
				return
			}
			got, err := io.ReadAll(r)
			if err != nil {
				_ = r.Close()
				errs <- err
				return
			}
			if !bytes.Equal(got, data) {
				errs <- errors.New("concurrent reader payload mismatch")
				_ = r.Close()
				return
			}
			if err := r.Close(); err != nil {
				errs <- err
				return
			}
			if err := r.Close(); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent reader failure: %v", err)
		}
	}
}

// Mid-stream cancel + Close must make the server handler return promptly and
// release the file, so reopening does not stack up live streams.
func TestAgentVE2E_CancelAndCloseReleasesServer(t *testing.T) {
	env := agentVStartE2E(t)
	defer env.close()

	path, _ := agentVMakeFile(t, 16<<20)
	ctx, cancel := context.WithCancel(context.Background())

	r, err := env.client.NewReader(ctx, path)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	// Read a small amount, then stop playback.
	first := make([]byte, 4096)
	if _, err := io.ReadFull(r, first); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}

	cancel()
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}

	select {
	case <-env.tracked.finished:
	case <-time.After(10 * time.Second):
		t.Fatal("server Read handler did not finish after client cancel/Close")
	}

	// The descriptor must be gone: rename/remove works immediately.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := os.Rename(path, path+".gone"); err == nil {
			_ = os.Remove(path + ".gone")
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not release the resource file after cancellation")
}

// Reopening the same resource after a canceled Reader must work and return
// complete bytes (no stacked/dead streams poisoning the resource).
func TestAgentVE2E_ReopenAfterCancel(t *testing.T) {
	env := agentVStartE2E(t)
	defer env.close()

	path, data := agentVMakeFile(t, 180_000)

	ctx1, cancel1 := context.WithCancel(context.Background())
	r1, err := env.client.NewReader(ctx1, path)
	if err != nil {
		t.Fatalf("NewReader #1: %v", err)
	}
	buf := make([]byte, 100)
	if _, err := io.ReadFull(r1, buf); err != nil {
		t.Fatalf("ReadFull #1: %v", err)
	}
	cancel1()
	_ = r1.Close()
	select {
	case <-env.tracked.finished:
	case <-time.After(10 * time.Second):
		t.Fatal("first server stream did not finish")
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	r2, err := env.client.NewReader(ctx2, path)
	if err != nil {
		t.Fatalf("NewReader #2: %v", err)
	}
	got, err := io.ReadAll(r2)
	if err != nil {
		t.Fatalf("ReadAll #2: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("reopened stream bytes mismatch: %d vs %d", len(got), len(data))
	}
	_ = r2.Close()
}
