package main

// Minimal server double: only Send is implemented; Context comes from a nil
// embedded grpc.ServerStream and would panic if called directly. The server
// must defend that lookup and still stream correctly / stop on Send error.

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/lukasmalkmus/stream/proto"
)

var errAgentVMinimalSend = errors.New("agentV minimal send failure")

type agentVMinimalSendOnly struct {
	grpc.ServerStream
	sent      [][]byte
	sendCalls int
	failAt    int // 0 = never fail; >0 = 1-based send call at which Send errors
}

func (m *agentVMinimalSendOnly) Send(resp *pb.ReadResponse) error {
	m.sendCalls++
	if m.failAt > 0 && m.sendCalls == m.failAt {
		return errAgentVMinimalSend
	}
	m.sent = append(m.sent, append([]byte(nil), resp.Data...))
	return nil
}

func TestAgentVMinimalServerDouble_StreamsWithoutContextImpl(t *testing.T) {
	path, data := agentVMakeFile(t, 200_000)
	srv := &bytestreamSrv{}
	m := &agentVMinimalSendOnly{}

	if err := srv.Read(&pb.ReadRequest{ResourceName: path}, m); err != nil {
		t.Fatalf("Read with Context-less double: %v", err)
	}
	var got bytes.Buffer
	for _, c := range m.sent {
		got.Write(c)
	}
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("bytes mismatch: %d vs %d", got.Len(), len(data))
	}
}

func TestAgentVMinimalServerDouble_SendErrorStops(t *testing.T) {
	path, _ := agentVMakeFile(t, 4<<20)
	srv := &bytestreamSrv{}
	m := &agentVMinimalSendOnly{failAt: 2}

	done := make(chan error, 1)
	go func() { done <- srv.Read(&pb.ReadRequest{ResourceName: path}, m) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected send failure to terminate Read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read did not stop after Send error on minimal double")
	}
	if m.sendCalls != 2 {
		t.Fatalf("send calls = %d, want 2", m.sendCalls)
	}
}
