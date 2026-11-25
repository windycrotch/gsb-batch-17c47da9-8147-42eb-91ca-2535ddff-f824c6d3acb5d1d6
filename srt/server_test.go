package srt

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	gosrt "github.com/datarhei/gosrt"
	"github.com/voc/srtrelay/relay"
	"github.com/voc/srtrelay/stream"
)

func compareStats(got, expected []*relay.StreamStatistics) error {
	if len(got) != len(expected) {
		return fmt.Errorf("Wrong number of streams: got %d, expected %d", len(got), len(expected))
	}
	for i, v := range expected {
		if !reflect.DeepEqual(*v, *got[i]) {
			return fmt.Errorf("Invalid stream statistics: got %v, expected %v", *got[i], *v)
		}
	}
	return nil
}

func TestServerImpl_GetStatistics(t *testing.T) {
	r := relay.NewRelay(&relay.RelayConfig{
		BufferSize: 1,
		PacketSize: 1,
	})
	s := &Server{
		relay: r,
		config: &ServerConfig{
			Addresses: []netip.AddrPort{
				netip.MustParseAddrPort("127.0.0.1:1337"),
				netip.MustParseAddrPort("[::1]:1337"),
			}, PublicAddress: "testserver.de:1337",
		},
	}
	if _, _, err := r.Publish("s1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Subscribe("s1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Subscribe("s1"); err != nil {
		t.Fatal(err)
	}
	streams := s.GetStatistics()

	expected := []*relay.StreamStatistics{
		{Name: "s1", URL: "srt://testserver.de:1337?streamid=#!::m=request,r=s1", Clients: 2, Created: streams[0].Created}, // New Format
	}
	if err := compareStats(streams, expected); err != nil {
		t.Error(err)
	}
}

type testSocket struct {
	N  int
	ch chan []byte

	numWritten int
}

func (s *testSocket) Read(b []byte) (int, error) {
	buf, ok := <-s.ch
	if !ok {
		return 0, io.EOF
	}
	length := copy(b, buf)
	return length, nil
}

func (s *testSocket) Write(b []byte) (int, error) {
	s.numWritten++
	return len(b), nil
}

func (s *testSocket) Close() error { return nil }

func (s *testSocket) Stats(*gosrt.Statistics) {
}

func (s *testSocket) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
}

func TestPublish(t *testing.T) {
	s := NewServer(&Config{
		Server: ServerConfig{PacketSize: 1316},
		Relay:  relay.RelayConfig{BufferSize: 50, PacketSize: 1316},
	})

	rd := testSocket{ch: make(chan []byte)}
	wr := testSocket{}
	id, err := stream.NewStreamID("test", "", stream.ModePublish)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// pub routine
	go func() {
		defer wg.Done()
		err = s.publish(&srtConn{
			log:      slog.Default(),
			socket:   &rd,
			streamid: id,
		})
		if err != io.EOF {
			t.Error("publisher error", err)
		}
	}()

	// sub routine
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		err := s.play(&srtConn{
			log:      slog.Default(),
			socket:   &wr,
			streamid: id,
		})
		if err != nil {
			t.Error("player error", err)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 100; i++ {
		rd.ch <- []byte{1, 2, 3, 4}
		time.Sleep(1 * time.Millisecond)
	}
	close(rd.ch)
	wg.Wait()
	if wr.numWritten != 100 {
		t.Errorf("Wrong number of packets written: got %d, expected %d", wr.numWritten, 100)
	}
}

// TestPublishReconnect exercises the full observable lifecycle through the
// server's own handlers: publisher disconnect releases the stream, the old
// player observes upstream closure and exits, a replacement publisher reclaims
// the same name, and a new player receives its data.
func TestPublishReconnect(t *testing.T) {
	s := NewServer(&Config{
		Server: ServerConfig{PacketSize: 1316},
		Relay:  relay.RelayConfig{BufferSize: 50, PacketSize: 1316},
	})

	id, err := stream.NewStreamID("test", "", stream.ModePublish)
	if err != nil {
		t.Fatal(err)
	}

	// First publisher plus a player that joins mid-stream.
	pub1 := testSocket{ch: make(chan []byte)}
	player1 := testSocket{}

	var pubWg sync.WaitGroup
	pubWg.Add(1)
	go func() {
		defer pubWg.Done()
		if err := s.publish(&srtConn{
			log:      slog.Default(),
			socket:   &pub1,
			streamid: id,
		}); err != io.EOF {
			t.Error("first publisher error", err)
		}
	}()

	var playerWg sync.WaitGroup
	go func() {
		// Wait for the first publisher to register before subscribing,
		// mirroring a real client joining an existing stream.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && !s.relay.ChannelExists("test") {
			time.Sleep(time.Millisecond)
		}
		playerWg.Add(1)
		defer playerWg.Done()
		if err := s.play(&srtConn{
			log:      slog.Default(),
			socket:   &player1,
			streamid: id,
		}); err != nil {
			t.Error("first player error", err)
		}
	}()

	// Send a first batch, then the publisher connection drops (read EOF).
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 50; i++ {
		pub1.ch <- []byte{1, 2, 3, 4}
		time.Sleep(1 * time.Millisecond)
	}
	close(pub1.ch)
	pubWg.Wait()

	// Teardown is synchronous inside publish(): the name is free immediately.
	if s.relay.ChannelExists("test") {
		t.Fatal("stream should be released after publisher disconnect")
	}

	// The old player must terminate on upstream closure rather than hang.
	done := make(chan struct{})
	go func() {
		playerWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("old player did not disconnect after publisher teardown")
	}
	if player1.numWritten != 50 {
		t.Errorf("old player packets: got %d, expected 50", player1.numWritten)
	}

	// A replacement publisher claims the same name without being rejected.
	pub2 := testSocket{ch: make(chan []byte)}
	player2 := testSocket{}

	pubWg.Add(1)
	go func() {
		defer pubWg.Done()
		if err := s.publish(&srtConn{
			log:      slog.Default(),
			socket:   &pub2,
			streamid: id,
		}); err != io.EOF {
			t.Error("second publisher error", err)
		}
	}()

	// Give the replacement publisher time to register before playing.
	time.Sleep(50 * time.Millisecond)
	if !s.relay.ChannelExists("test") {
		t.Fatal("stream should exist after replacement publisher connected")
	}

	playerWg.Add(1)
	go func() {
		defer playerWg.Done()
		if err := s.play(&srtConn{
			log:      slog.Default(),
			socket:   &player2,
			streamid: id,
		}); err != nil {
			t.Error("second player error", err)
		}
	}()

	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 75; i++ {
		pub2.ch <- []byte{5, 6, 7, 8}
		time.Sleep(1 * time.Millisecond)
	}
	close(pub2.ch)
	pubWg.Wait()

	playerWg.Wait()
	if player2.numWritten != 75 {
		t.Errorf("new player packets: got %d, expected 75", player2.numWritten)
	}
}
