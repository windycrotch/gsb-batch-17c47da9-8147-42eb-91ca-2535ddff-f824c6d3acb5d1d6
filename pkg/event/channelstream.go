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
)

const (
	// TODO: make this configurable in the application
	DefaultBufferLen = 32
)

type Stream interface {
	Start(ctx context.Context)
	Pub(e Event)
	Sub() <-chan Event
	SubBuf(n int) <-chan Event
	Desub(ch <-chan Event)
}

type channelStream struct {
	mtx sync.RWMutex

	recvCh chan Event
	done   chan struct{}
	subs   []*subscription
}

type subscription struct {
	ch   chan Event
	done chan struct{}
}

func NewStream() Stream {
	return NewStreamBuf(DefaultBufferLen)
}

func NewStreamBuf(n int) Stream {
	return &channelStream{
		recvCh: make(chan Event, n),
		done:   make(chan struct{}),
		subs:   make([]*subscription, 0),
	}
}

func (cs *channelStream) Pub(e Event) {
	// Do not block forever when the stream has been stopped. Otherwise, publishers (e.g. ingest HTTP handlers) could
	// get stuck once the context that drives the stream has been cancelled.
	select {
	case cs.recvCh <- e:
	case <-cs.done:
	}
}

func (cs *channelStream) Sub() <-chan Event {
	return cs.SubBuf(DefaultBufferLen)
}

func (cs *channelStream) SubBuf(n int) <-chan Event {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	sub := &subscription{
		ch:   make(chan Event, n),
		done: make(chan struct{}),
	}
	cs.subs = append(cs.subs, sub)
	return sub.ch
}

func (cs *channelStream) Desub(ch <-chan Event) {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()

	for i, sub := range cs.subs {
		if sub.ch == ch { // TODO: does this work?
			cs.removeSub(i)
			close(sub.done)
			return
		}
	}
}

func (cs *channelStream) removeSub(i int) {
	last := len(cs.subs) - 1
	cs.subs[i] = cs.subs[last]
	cs.subs[last] = nil
	cs.subs = cs.subs[:last]
}

func (cs *channelStream) Start(ctx context.Context) {
	go cs.start(ctx)
}

func (cs *channelStream) start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			cs.clean()
			return
		case e := <-cs.recvCh:
			cs.emit(e)
		}
	}
}

// emit forwards an event to all subscribers.
//
// Events are emitted synchronously and in the order they were published. Emitting each event from its own goroutine
// would allow events to be observed out of order, which manifest generation relies on: a fragment must never be listed
// before the event that initialized its track, and a state produced from an older event must never overwrite a newer
// one.
func (cs *channelStream) emit(e Event) {
	cs.mtx.RLock()
	subs := make([]*subscription, len(cs.subs))
	copy(subs, cs.subs)
	cs.mtx.RUnlock()

	for _, sub := range subs {
		select {
		case sub.ch <- e:
		case <-sub.done:
			// subscriber went away while we were emitting
		case <-cs.done:
			return
		}
	}
}

func (cs *channelStream) clean() {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()

	// Signal publishers and emitters that this stream no longer accepts events. Subscriber channels are intentionally
	// not closed: an emit that already copied the subscriber list may still select on them.
	select {
	case <-cs.done:
		// already cleaned
	default:
		close(cs.done)
	}

	cs.subs = cs.subs[:0]
}
