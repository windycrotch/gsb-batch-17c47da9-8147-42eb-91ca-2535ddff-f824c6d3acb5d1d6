/*
Copyright 2022-2025 The nagare media authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package event

import (
	"context"
	"sync"
	"testing"
	"time"
)

type seqEvent struct {
	n int
}

func TestChannelStreamPreservesOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := NewStreamBuf(64)
	s.Start(ctx)

	ch := s.SubBuf(64)
	defer s.Desub(ch)

	const n = 500

	// Publish concurrently with reading: the stream has bounded buffers, so a publisher blocks while a subscriber is
	// not draining.
	publishDone := make(chan struct{})
	go func() {
		defer close(publishDone)
		for i := 0; i < n; i++ {
			s.Pub(&seqEvent{n: i})
		}
	}()

	for i := 0; i < n; i++ {
		select {
		case e := <-ch:
			ev, ok := e.(*seqEvent)
			if !ok {
				t.Fatalf("unexpected event type %T", e)
			}
			if ev.n != i {
				t.Fatalf("event out of order: got %d, want %d", ev.n, i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for event %d", i)
		}
	}

	select {
	case <-publishDone:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher did not finish")
	}
}

func TestChannelStreamConcurrentPublishersPreservePerPublisherOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := NewStreamBuf(1024)
	s.Start(ctx)

	ch := s.SubBuf(1024)
	defer s.Desub(ch)

	const publishers = 8
	const perPublisher = 100

	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perPublisher; i++ {
				s.Pub(&seqEvent{n: p*perPublisher + i})
			}
		}()
	}

	// Drain concurrently with publishing, since the stream's buffers are bounded.
	got := make(map[int]int, publishers*perPublisher)
	deadline := time.After(10 * time.Second)
	remaining := publishers * perPublisher
	var lastByPublisher [publishers]int
	for i := 0; i < publishers; i++ {
		lastByPublisher[i] = -1
	}
	for remaining > 0 {
		select {
		case e := <-ch:
			ev := e.(*seqEvent)
			got[ev.n]++
			p := ev.n / perPublisher
			idx := ev.n % perPublisher
			if idx <= lastByPublisher[p] {
				t.Fatalf("events from publisher %d arrived out of order: %d after %d", p, idx, lastByPublisher[p])
			}
			lastByPublisher[p] = idx
			remaining--
		case <-deadline:
			t.Fatalf("timed out; received %d of %d events", publishers*perPublisher-remaining, publishers*perPublisher)
		}
	}

	wg.Wait()

	if len(got) != publishers*perPublisher {
		t.Fatalf("expected %d distinct events, got %d", publishers*perPublisher, len(got))
	}
	for _, c := range got {
		if c != 1 {
			t.Fatalf("received a duplicated event %d times", c)
		}
	}
}

func TestChannelStreamPubAfterShutdownDoesNotBlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	s := NewStreamBuf(1)
	s.Start(ctx)

	// Fill the buffer so that a blocking publish would block if the stream were still consuming.
	s.Pub(&seqEvent{n: 0})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10; i++ {
			s.Pub(&seqEvent{n: i})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Pub blocked on a full buffer before shutdown")
	}

	cancel()

	// After shutdown, publishing must not block forever even without a subscriber draining the stream.
	done = make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			s.Pub(&seqEvent{n: i})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Pub blocked after stream shutdown")
	}
}
