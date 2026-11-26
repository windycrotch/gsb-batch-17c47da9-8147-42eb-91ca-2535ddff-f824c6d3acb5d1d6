package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lukasmalkmus/stream/bytestream"
	pb "github.com/lukasmalkmus/stream/proto"
)

// streamContextError preserves the canonical context error for errors.Is /
// errors.As while also exposing the matching gRPC status code (Canceled or
// DeadlineExceeded) through GRPCStatus.
type streamContextError struct {
	ctxErr error
	msg    string
}

func (e *streamContextError) Error() string { return e.msg }
func (e *streamContextError) Unwrap() error { return e.ctxErr }

func (e *streamContextError) GRPCStatus() *status.Status {
	code := codes.Unknown
	switch {
	case errors.Is(e.ctxErr, context.Canceled):
		code = codes.Canceled
	case errors.Is(e.ctxErr, context.DeadlineExceeded):
		code = codes.DeadlineExceeded
	}
	return status.New(code, e.msg)
}

// contextStatusError builds a streamContextError when cause is non-nil.
func contextStatusError(cause error, format string, args ...any) error {
	if cause == nil {
		return nil
	}
	return &streamContextError{ctxErr: cause, msg: fmt.Sprintf(format, args...)}
}

type bytestreamSrv struct{}

// Read is used to retrieve the contents of a resource as a sequence of bytes.
// The bytes are returned in a sequence of responses, and the responses are
// delivered as the results of a server-side streaming RPC.
func (b *bytestreamSrv) Read(req *pb.ReadRequest, stream pb.ByteStream_ReadServer) error {
	if req.ResourceName == "" {
		return status.Errorf(codes.InvalidArgument, "ReadRequest: empty or missing resource_name")
	}

	f, err := os.Open(req.ResourceName)
	if err != nil {
		return status.Errorf(codes.NotFound, "ReadRequest: resource %q doesn't exist", req.ResourceName)
	}
	// Returning from Read on every path (send failure, cancellation, read
	// error, EOF) releases the descriptor so clients can reopen the resource
	// and the file can be removed/renamed.
	defer f.Close()

	ctx := safeServerStreamContext(stream)

	buf := make([]byte, bytestream.MaxBufSize)
	for {
		// Client cancellation/timeout must stop further file reads and sends.
		if err := ctx.Err(); err != nil {
			return contextStatusError(err, "Read(resourceName=%q) canceled: %v", req.ResourceName, err)
		}

		n, readErr := f.Read(buf)
		if n > 0 {
			// Copy the chunk: handing buf[:n] to an asynchronous send would
			// otherwise race with the next f.Read into the same buffer.
			chunk := append([]byte(nil), buf[:n]...)
			if err := waitForSendOrCancel(ctx, stream, &pb.ReadResponse{Data: chunk}); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return contextStatusError(ctxErr, "Send(resourceName=%q): %v", req.ResourceName, ctxErr)
				}
				return status.Errorf(status.Code(err), "Send(resourceName=%q): %v", req.ResourceName, status.Convert(err).Message())
			}
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return status.Errorf(codes.Unknown, "Read(resourceName=%q): %v", req.ResourceName, readErr)
		}
		if n == 0 {
			return status.Errorf(codes.Internal, "nil error on empty read: io.Reader contract violated")
		}
	}

	log.Printf("Done streaming %q", req.ResourceName)

	return nil
}

// waitForSendOrCancel sends one response while also selecting on context
// cancellation. A blocked Send (possible with test doubles) therefore cannot
// keep Read alive after the client went away. The send goroutine uses a
// buffered result channel and finishes on its own once Send unblocks.
func waitForSendOrCancel(ctx context.Context, stream pb.ByteStream_ReadServer, resp *pb.ReadResponse) error {
	sendErr := make(chan error, 1)
	go func() {
		defer func() {
			// A panicking stream double must not crash the whole test binary.
			if rec := recover(); rec != nil {
				sendErr <- fmt.Errorf("Send panicked: %v", rec)
			}
		}()
		sendErr <- stream.Send(resp)
	}()

	select {
	case err := <-sendErr:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// safeServerStreamContext returns the stream context. Production gRPC streams
// and complete test doubles implement Context; defensive recovery keeps
// minimal doubles that only implement Send (embedding a nil
// grpc.ServerStream) usable, in which case send errors remain the cancellation
// signal.
func safeServerStreamContext(stream grpc.ServerStream) (ctx context.Context) {
	defer func() {
		if recover() != nil {
			ctx = context.Background()
		}
	}()
	if c := stream.Context(); c != nil {
		return c
	}
	return context.Background()
}

func main() {
	lis, err := net.Listen("tcp", ":10000")
	if err != nil {
		log.Printf("Listen(): %v", err)
		return
	}

	g := grpc.NewServer()
	pb.RegisterByteStreamServer(g, &bytestreamSrv{})

	log.Println("Serving on", lis.Addr().String())
	log.Fatalln(g.Serve(lis))
}
