// Copyright 2024 Jelly Terra
// This Source Code Form is subject to the terms of the Mozilla Public License, v. 2.0
// that can be found in the LICENSE file and https://mozilla.org/MPL/2.0/.

package main

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	rtmpmsg "github.com/yutopp/go-rtmp/message"
)

type fakeEndpoint struct {
	label     string
	audioCnt  int64
	videoCnt  int64
	failVideo bool
	mu        sync.Mutex
	closed    int
}

func (f *fakeEndpoint) Label() string { return f.label }

func (f *fakeEndpoint) WriteAudio(timestamp uint32, payload []byte) error {
	atomic.AddInt64(&f.audioCnt, 1)
	return nil
}

func (f *fakeEndpoint) WriteVideo(timestamp uint32, payload []byte) error {
	if f.failVideo {
		return errors.New("simulated remote write error")
	}
	atomic.AddInt64(&f.videoCnt, 1)
	return nil
}

func (f *fakeEndpoint) WriteSetFrame(timestamp uint32, data *rtmpmsg.DataMessage) error {
	return nil
}

func (f *fakeEndpoint) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
}

func (f *fakeEndpoint) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func newTestHandler() *Handler {
	return &Handler{}
}

// A single remote write failure isolates only that remote: it is closed and
// stops receiving media, while every other endpoint keeps receiving audio/video.
func TestBroadcastIsolatesSingleRemoteFailure(t *testing.T) {
	h := newTestHandler()

	bad := &fakeEndpoint{label: "remote:bad/live", failVideo: true}
	good1 := &fakeEndpoint{label: "remote:good1/live"}
	good2 := &fakeEndpoint{label: "remote:good2/live"}
	h.AddEndpoint(bad)
	h.AddEndpoint(good1)
	h.AddEndpoint(good2)

	for i := 0; i < 5; i++ {
		if err := h.broadcast(func(ep Endpoint) error {
			return ep.WriteVideo(10, nil)
		}); err != nil {
			t.Fatalf("video write must not return a session error, got: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := h.broadcast(func(ep Endpoint) error {
			return ep.WriteAudio(10, nil)
		}); err != nil {
			t.Fatalf("audio write must not return a session error, got: %v", err)
		}
	}

	if bad.closeCount() != 1 {
		t.Fatalf("failed remote must be closed exactly once, got %d", bad.closeCount())
	}
	if len(h.snapshot()) != 2 {
		t.Fatalf("only the failed remote should be removed, got %d live endpoints", len(h.snapshot()))
	}
	if got := atomic.LoadInt64(&good1.videoCnt); got != 5 {
		t.Fatalf("good1 should receive all 5 videos, got %d", got)
	}
	if got := atomic.LoadInt64(&good2.videoCnt); got != 5 {
		t.Fatalf("good2 should receive all 5 videos, got %d", got)
	}
	if got := atomic.LoadInt64(&good1.audioCnt); got != 3 {
		t.Fatalf("good1 should receive all 3 audios, got %d", got)
	}
	if got := atomic.LoadInt64(&good2.audioCnt); got != 3 {
		t.Fatalf("good2 should receive all 3 audios, got %d", got)
	}
}

// A recording write failure is a session-level fatal error.
func TestBroadcastRecordingFailureIsFatal(t *testing.T) {
	h := newTestHandler()

	rec := &failingFlv{label: "recording:x.flv"}
	remote := &fakeEndpoint{label: "remote:good/live"}
	h.AddEndpoint(rec)
	h.AddEndpoint(remote)

	err := h.broadcast(func(ep Endpoint) error {
		return ep.WriteVideo(1, nil)
	})
	if !errors.Is(err, ErrSessionFatal) {
		t.Fatalf("recording write failure must wrap ErrSessionFatal, got: %v", err)
	}
}

// Every endpoint is closed exactly once even if shutdown is triggered
// repeatedly and concurrently, and afterClose hooks run exactly once.
func TestCloseAllIdempotentAndConcurrent(t *testing.T) {
	h := newTestHandler()

	var eps []*fakeEndpoint
	for i := 0; i < 4; i++ {
		ep := &fakeEndpoint{label: fmt.Sprintf("remote:%d/live", i)}
		eps = append(eps, ep)
		h.AddEndpoint(ep)
	}

	var hooks int64
	h.HooksOnClose = append(h.HooksOnClose, func() { atomic.AddInt64(&hooks, 1) })

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.OnClose()
		}()
	}
	wg.Wait()

	for i, ep := range eps {
		if c := ep.closeCount(); c != 1 {
			t.Fatalf("endpoint %d must be closed once, got %d", i, c)
		}
	}
	if got := atomic.LoadInt64(&hooks); got != 1 {
		t.Fatalf("afterClose hooks must run once, got %d", got)
	}
	if len(h.snapshot()) != 0 {
		t.Fatalf("no endpoint should remain live after close")
	}
}

// A client disconnect (OnClose) racing with media writes must never panic,
// must close every still-live endpoint exactly once (regardless of ordering),
// and must leave no endpoint behind. The race detector additionally verifies
// that all registry access is properly synchronized.
func TestConcurrentDisconnectAndWrites(t *testing.T) {
	h := newTestHandler()

	var eps []*trackingEndpoint
	for i := 0; i < 4; i++ {
		ep := newTrackingEndpoint(fmt.Sprintf("remote:%d/live", i))
		eps = append(eps, ep)
		h.AddEndpoint(ep)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() { // publisher side: keep writing until disconnect
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			_ = h.broadcast(func(ep Endpoint) error {
				return ep.WriteVideo(uint32(i), nil)
			})
		}
	}()

	go func() { // client side: disconnect concurrently
		defer wg.Done()
		h.OnClose()
	}()

	wg.Wait()

	for i, ep := range eps {
		ep.mu.Lock()
		if ep.closed != 1 {
			t.Fatalf("endpoint %d must be closed exactly once, got %d", i, ep.closed)
		}
		ep.mu.Unlock()
	}
	if len(h.snapshot()) != 0 {
		t.Fatalf("no endpoint should remain live after close")
	}
}

// The single-remote isolation path removes an endpoint from the registry before
// closing it, so no later broadcast can write to a closed endpoint.
func TestFailEndpointRemovesBeforeClose(t *testing.T) {
	h := newTestHandler()
	ep := newTrackingEndpoint("remote:bad/live")
	good := newTrackingEndpoint("remote:good/live")
	h.AddEndpoint(ep)
	h.AddEndpoint(good)

	h.failEndpoint(ep, "write", errors.New("boom"))

	if len(h.snapshot()) != 1 || h.snapshot()[0] != good {
		t.Fatalf("failed endpoint must be removed from the live set before close")
	}
	if ep.closed != 1 {
		t.Fatalf("failed endpoint must be closed once")
	}
}

// trackingEndpoint records close counts and total videos under a mutex.
type trackingEndpoint struct {
	label  string
	mu     sync.Mutex
	closed int
	videos int64
}

func newTrackingEndpoint(label string) *trackingEndpoint {
	return &trackingEndpoint{label: label}
}

func (t *trackingEndpoint) Label() string { return t.label }

func (t *trackingEndpoint) WriteAudio(uint32, []byte) error { return nil }

func (t *trackingEndpoint) WriteVideo(uint32, []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.videos++
	return nil
}

func (t *trackingEndpoint) WriteSetFrame(uint32, *rtmpmsg.DataMessage) error { return nil }

func (t *trackingEndpoint) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed++
}

// failingFlv behaves like a recording endpoint whose writes always fail.
type failingFlv struct{ label string }

func (f *failingFlv) Label() string { return f.label }
func (f *failingFlv) FatalOnWriteFailure() bool {
	return true
}
func (f *failingFlv) WriteAudio(uint32, []byte) error {
	return errors.New("disk full")
}
func (f *failingFlv) WriteVideo(uint32, []byte) error {
	return errors.New("disk full")
}
func (f *failingFlv) WriteSetFrame(uint32, *rtmpmsg.DataMessage) error {
	return errors.New("disk full")
}
func (f *failingFlv) Close() {}
