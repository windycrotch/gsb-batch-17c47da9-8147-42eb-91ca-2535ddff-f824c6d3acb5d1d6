package bytestream

// Compatibility with minimal stream doubles that embed a nil
// grpc.ClientStream and only implement Recv (CloseSend then panics through
// the nil promoted interface). The Reader must defend its close path.

import (
	"errors"
	"io"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/lukasmalkmus/stream/proto"
)

type agentVMinimalRecvOnly struct {
	grpc.ClientStream
	responses []*pb.ReadResponse
	errs      []error
	idx       int
}

func (m *agentVMinimalRecvOnly) Recv() (*pb.ReadResponse, error) {
	i := m.idx
	m.idx++
	if i < len(m.errs) && m.errs[i] != nil {
		return nil, m.errs[i]
	}
	if i < len(m.responses) {
		return m.responses[i], nil
	}
	return nil, io.EOF
}

func TestAgentVMinimalDouble_NoCloseSendImpl(t *testing.T) {
	// Normal EOF path must not panic even though CloseSend is a nil promoted
	// method.
	m := &agentVMinimalRecvOnly{
		responses: []*pb.ReadResponse{{Data: []byte("abc")}},
	}
	r := &Reader{readClient: m}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "abc" {
		t.Fatalf("got %q", got)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close must stay safe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}

	// Stream error path likewise.
	m2 := &agentVMinimalRecvOnly{errs: []error{errors.New("boom")}}
	r2 := &Reader{readClient: m2}
	if _, err := r2.Read(make([]byte, 4)); err == nil {
		t.Fatal("expected boom")
	}
	if err := r2.Close(); err != nil {
		t.Fatalf("Close after error: %v", err)
	}

	// Exhaustion path likewise.
	m3 := &agentVMinimalRecvOnly{
		responses: make([]*pb.ReadResponse, maxTries+2), // all non-nil but Data nil
	}
	for i := range m3.responses {
		m3.responses[i] = &pb.ReadResponse{}
	}
	r3 := &Reader{readClient: m3}
	if _, err := r3.Read(make([]byte, 4)); err == nil {
		t.Fatal("expected exhaustion error")
	}
	if err := r3.Close(); err != nil {
		t.Fatalf("Close after exhaustion: %v", err)
	}
}
