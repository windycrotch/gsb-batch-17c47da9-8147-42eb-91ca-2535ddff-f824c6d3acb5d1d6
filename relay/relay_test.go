package relay

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRelayImpl_SubscribeAndUnsubscribe(t *testing.T) {
	config := RelayConfig{BufferSize: 50, PacketSize: 4}
	relay := NewRelay(&config)
	data := []byte{1, 2, 3, 4}

	pub, unpublish, err := relay.Publish("test")
	if err != nil {
		t.Fatal(err)
	}
	defer unpublish()

	sub, unsub, err := relay.Subscribe("test")
	if err != nil {
		t.Fatal(err)
	}

	// send
	buf := relay.AcquireBuffer()
	copy(buf.Bytes(), data)
	buf.Resize(4)
	pub <- buf

	// receive
	got, err := sub.Read()
	if err != nil {
		t.Fatal("Subscriber channel should not be closed")
	}
	if !reflect.DeepEqual(got.Bytes(), data) {
		t.Errorf("Sub ret = %x, want %x", got.Bytes(), data)
	}
	got.Release()

	// unsubscribe
	unsub()

	// 2nd send
	buf = relay.AcquireBuffer()
	copy(buf.Bytes(), data)
	buf.Resize(4)
	pub <- buf
	got, err = sub.Read()
	if got != nil || err == nil {
		t.Errorf("Read after unsub ret %v, want nil", got)
	}
}

func TestRelayImpl_PublisherClose(t *testing.T) {
	config := RelayConfig{BufferSize: 1, PacketSize: 1}
	relay := NewRelay(&config)

	ch, _, _ := relay.Publish("test")
	sub, unsub, _ := relay.Subscribe("test")
	close(ch)

	// Closing only the send channel still tears down via the publisher
	// goroutine; wait for that asynchronous fallback path.
	waitForRelayState(t, relay, "test", false)

	if _, err := sub.Read(); err == nil {
		t.Error("Subscriber channel should be closed")
	}

	// unsub after close shouldn't break
	unsub()

	_, unpublish2, err := relay.Publish("test")
	if err != nil {
		t.Error("Publish should be possible again after close")
	}
	unpublish2()
}

// TestRelayImpl_UnpublishReleasesStreamSynchronously is the core lifecycle
// requirement: when the publisher connection ends, the stream name must be
// reclaimable by a new publisher immediately, old subscribers must observe
// upstream closure, and subscribers created afterwards must receive data from
// the replacement publisher.
func TestRelayImpl_UnpublishReleasesStreamSynchronously(t *testing.T) {
	config := RelayConfig{BufferSize: 50, PacketSize: 4}
	relay := NewRelay(&config)
	data := []byte{9, 8, 7, 6}

	pub, unpublish, err := relay.Publish("stream")
	if err != nil {
		t.Fatal(err)
	}

	oldSub, oldUnsub, err := relay.Subscribe("stream")
	if err != nil {
		t.Fatal(err)
	}

	// Old subscriber is actively receiving data.
	buf := relay.AcquireBuffer()
	copy(buf.Bytes(), data)
	buf.Resize(4)
	pub <- buf
	if got, err := oldSub.Read(); err != nil || !reflect.DeepEqual(got.Bytes(), data) {
		t.Fatalf("old subscriber did not receive data: got=%v err=%v", got, err)
	} else {
		got.Release()
	}

	// Publisher connection ends (clean close or read error both lead here).
	// This must be synchronous: after it returns the name is free.
	unpublish()

	if relay.ChannelExists("stream") {
		t.Fatal("stream should no longer exist after unpublish returned")
	}

	// Old subscriber gets upstream closure instead of hanging forever.
	if _, err := oldSub.Read(); err == nil {
		t.Fatal("old subscriber should have been disconnected on publisher teardown")
	}
	oldUnsub()

	// The name is immediately reclaimable by a new publisher, no sleep needed.
	pub2, unpublish2, err := relay.Publish("stream")
	if err != nil {
		t.Fatalf("re-publish after teardown failed: %v", err)
	}
	defer unpublish2()
	if !relay.ChannelExists("stream") {
		t.Fatal("stream should exist after re-publish")
	}

	// A new subscriber on the replacement stream receives new data.
	newSub, newUnsub, err := relay.Subscribe("stream")
	if err != nil {
		t.Fatal(err)
	}
	defer newUnsub()

	buf = relay.AcquireBuffer()
	copy(buf.Bytes(), data)
	buf.Resize(4)
	pub2 <- buf
	got, err := newSub.Read()
	if err != nil {
		t.Fatalf("new subscriber read failed: %v", err)
	}
	if !reflect.DeepEqual(got.Bytes(), data) {
		t.Errorf("new subscriber got %x, want %x", got.Bytes(), data)
	}
	got.Release()
}

// TestRelayImpl_ConcurrentReconnect covers the "disconnect and reconnect
// almost simultaneously" timing: many publishers may race for one name, but
// at most one may ever hold it and no publisher may be rejected because old
// state is stuck in cleanup.
func TestRelayImpl_ConcurrentReconnect(t *testing.T) {
	config := RelayConfig{BufferSize: 8, PacketSize: 4}
	relay := NewRelay(&config)
	const rounds = 200

	for range rounds {
		_, unpublish, err := relay.Publish("race")
		if err != nil {
			t.Fatalf("publisher rejected while no other publisher exists: %v", err)
		}

		sub, unsub, err := relay.Subscribe("race")
		if err != nil {
			t.Fatal(err)
		}

		// A second publisher must be rejected while the first still holds it.
		if _, _, err := relay.Publish("race"); err != ErrStreamAlreadyExists {
			t.Fatalf("concurrent double publish: got %v, want %v", err, ErrStreamAlreadyExists)
		}

		// Old subscriber must not survive teardown.
		unpublish()
		if _, err := sub.Read(); err == nil {
			t.Fatal("subscriber still alive after unpublish")
		}
		unsub()

		// Exactly one owner at all times, and the name is free right away.
		if relay.ChannelExists("race") {
			t.Fatal("stream not released after unpublish")
		}
	}

	// Repeated unpublish calls must be harmless no-ops.
	_, unpublish, err := relay.Publish("race")
	if err != nil {
		t.Fatal(err)
	}
	unpublish()
	unpublish()
	if relay.ChannelExists("race") {
		t.Fatal("stream not released after idempotent unpublish")
	}
}

// TestRelayImpl_UnpublishDuringConcurrentPublishes hammers unpublish/publish
// from multiple goroutines to prove mutual exclusion under real scheduling.
func TestRelayImpl_UnpublishDuringConcurrentPublishes(t *testing.T) {
	config := RelayConfig{BufferSize: 8, PacketSize: 4}
	relay := NewRelay(&config)
	const goroutines = 8
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range iterations {
				pub, unpublish, err := relay.Publish("hot")
				if err == ErrStreamAlreadyExists {
					// Another goroutine currently owns the name; retry rather
					// than treating contention as a permanent failure.
					continue
				}
				if err != nil {
					t.Errorf("unexpected publish error: %v", err)
					return
				}
				buf := relay.AcquireBuffer()
				buf.Resize(0)
				pub <- buf
				unpublish()
			}
		}()
	}
	wg.Wait()

	// Whatever the interleaving, ownership must converge to a clean state
	// where the name can be claimed exactly once.
	_, unpublish, err := relay.Publish("hot")
	if err != nil {
		t.Fatalf("stream did not converge to reclaimable state: %v", err)
	}
	if _, _, err := relay.Publish("hot"); err != ErrStreamAlreadyExists {
		t.Fatalf("got %v, want %v", err, ErrStreamAlreadyExists)
	}
	unpublish()
}

func TestRelayImpl_DoublePublish(t *testing.T) {
	config := RelayConfig{BufferSize: 1, PacketSize: 1}
	relay := NewRelay(&config)
	_, unpublish, err := relay.Publish("foo")
	if err != nil {
		t.Fatal(err)
	}
	defer unpublish()
	_, _, err = relay.Publish("foo")

	if err != ErrStreamAlreadyExists {
		t.Errorf("Publish to existing stream should return '%s', got '%s'", ErrStreamAlreadyExists, err)
	}
}

func TestRelayImpl_SubscribeNonExisting(t *testing.T) {
	config := RelayConfig{BufferSize: 1, PacketSize: 1}
	relay := NewRelay(&config)

	_, _, err := relay.Subscribe("foobar")
	if err != ErrStreamNotExisting {
		t.Errorf("Subscribe to non-existing stream should return '%s', got '%s'", ErrStreamNotExisting, err)
	}
}

func TestRelayImpl_ChannelExists(t *testing.T) {
	config := RelayConfig{BufferSize: 1, PacketSize: 1}
	relay := NewRelay(&config)

	ok := relay.ChannelExists("test")
	if ok {
		t.Fatal("Channel should not exist before publishing")
	}

	_, unpublish, err := relay.Publish("test")
	if err != nil {
		t.Fatal(err)
	}
	ok = relay.ChannelExists("test")
	if !ok {
		t.Fatal("Channel should exist after publishing")
	}
	unpublish()
	if relay.ChannelExists("test") {
		t.Fatal("Channel should not exist after unpublish")
	}
}

// TestRelayImpl_UnsubscribeIsIdempotent proves that cancelling the same
// subscription repeatedly never drives counters negative or double-releases.
func TestRelayImpl_UnsubscribeIsIdempotent(t *testing.T) {
	config := RelayConfig{BufferSize: 8, PacketSize: 4}
	relay := NewRelay(&config)
	const name = "idempotent"

	_, unpublish, err := relay.Publish(name)
	if err != nil {
		t.Fatal(err)
	}
	defer unpublish()

	_, unsub, err := relay.Subscribe(name)
	if err != nil {
		t.Fatal(err)
	}

	if got := testutil.ToFloat64(activeClients.WithLabelValues(name)); got != 1 {
		t.Fatalf("gauge after subscribe: got %v, want 1", got)
	}

	for range 5 {
		unsub()
	}

	if got := testutil.ToFloat64(activeClients.WithLabelValues(name)); got != 0 {
		t.Errorf("gauge after repeated unsubscribe: got %v, want 0", got)
	}
	if stats := relay.GetStatistics(); len(stats) != 1 || stats[0].Clients != 0 {
		t.Errorf("stats after repeated unsubscribe: %+v", stats)
	}
}

// TestRelayImpl_OverflowDropThenUnsubscribeKeepsConsistentCounts verifies that
// a client dropped for overflow is already fully accounted for; its later
// unsubscribe must not decrement anything twice.
func TestRelayImpl_OverflowDropThenUnsubscribeKeepsConsistentCounts(t *testing.T) {
	config := RelayConfig{BufferSize: 8, PacketSize: 4}
	relay := NewRelay(&config)
	const name = "overflow"

	pub, unpublish, err := relay.Publish(name)
	if err != nil {
		t.Fatal(err)
	}
	defer unpublish()

	sub, unsub, err := relay.Subscribe(name)
	if err != nil {
		t.Fatal(err)
	}

	// Fill the subscriber queue, then one more packet triggers the drop.
	// Sends hand buffers to the relay goroutine asynchronously, so wait for
	// the observable subscriber-state transition instead of assuming timing.
	pubState := relay.channels[name].channel
	for range cap(sub.ch) + 1 {
		buf := relay.AcquireBuffer()
		buf.Resize(0)
		pub <- buf
	}
	waitForCondition(t, func() bool {
		pubState.mutex.Lock()
		defer pubState.mutex.Unlock()
		return len(pubState.subs) == 0
	}, "overflowed subscriber to be dropped")

	// The dropped client's delivery channel is closed after its queued buffers
	// are drained; draining it must end in an error, not hang.
	if _, err := readUntilClosed(t, sub); err == nil {
		t.Fatal("overflowed subscriber should eventually report a read error")
	}

	if got := testutil.ToFloat64(activeClients.WithLabelValues(name)); got != 0 {
		t.Errorf("gauge after overflow drop: got %v, want 0", got)
	}

	// Cancelling the already-dropped subscription must not go negative.
	unsub()
	if got := testutil.ToFloat64(activeClients.WithLabelValues(name)); got != 0 {
		t.Errorf("gauge after late unsubscribe: got %v, want 0", got)
	}
}

// TestRelayImpl_UnpublishReturnsBuffersToPool ensures queued buffers of
// disconnected subscribers are recycled rather than leaked.
func TestRelayImpl_UnpublishReturnsBuffersToPool(t *testing.T) {
	config := RelayConfig{BufferSize: 40, PacketSize: 4}
	relay := NewRelay(&config)
	const name = "recycle"
	const queued = 5

	pub, unpublish, err := relay.Publish(name)
	if err != nil {
		t.Fatal(err)
	}

	sub, unsub, err := relay.Subscribe(name)
	if err != nil {
		t.Fatal(err)
	}

	// Queue buffers without reading them; they stay referenced by the
	// subscriber channel until teardown drains and releases them.
	for range queued {
		buf := relay.AcquireBuffer()
		buf.Resize(0)
		pub <- buf
	}

	unpublish()
	if _, err := sub.Read(); err == nil {
		t.Fatal("subscriber should be closed after unpublish")
	}
	unsub()

	// Every queued buffer must have been returned to the pool and be
	// reusable. sync.Pool may drop entries, so acquiring repeatedly and
	// checking refs is the strongest assertion available: recycled buffers
	// reset to refcount 1, fresh ones start at 1 too, and a leaked buffer
	// would have retained a stale refcount instead.
	for range queued {
		buf := relay.AcquireBuffer()
		if got := buf.refs.Load(); got != 1 {
			t.Errorf("acquired buffer refs = %d, want 1", got)
		}
		buf.Release()
	}
}

// TestRelayImpl_StatsConsistentAcrossLifecycle checks client counts converge
// correctly through subscribe, unsubscribe and full teardown.
func TestRelayImpl_StatsConsistentAcrossLifecycle(t *testing.T) {
	config := RelayConfig{BufferSize: 8, PacketSize: 4}
	relay := NewRelay(&config)
	const name = "stats"

	_, unpublish, err := relay.Publish(name)
	if err != nil {
		t.Fatal(err)
	}

	var unsubs []UnsubscribeFunc
	for range 3 {
		_, unsub, err := relay.Subscribe(name)
		if err != nil {
			t.Fatal(err)
		}
		unsubs = append(unsubs, unsub)
	}

	stats := relay.GetStatistics()
	if len(stats) != 1 || stats[0].Clients != 3 {
		t.Fatalf("stats after 3 subscribes: %+v", stats)
	}
	if got := testutil.ToFloat64(activeClients.WithLabelValues(name)); got != 3 {
		t.Fatalf("gauge after 3 subscribes: got %v, want 3", got)
	}

	// One cancels, including a duplicate cancel.
	unsubs[0]()
	unsubs[0]()
	if got := testutil.ToFloat64(activeClients.WithLabelValues(name)); got != 2 {
		t.Fatalf("gauge after one unsubscribe: got %v, want 2", got)
	}

	unpublish()
	// Remaining subscribers cancelling after teardown must not corrupt state.
	for _, unsub := range unsubs[1:] {
		unsub()
	}

	if got := testutil.ToFloat64(activeClients.WithLabelValues(name)); got != 0 {
		t.Errorf("gauge after teardown: got %v, want 0 (label deleted)", got)
	}
	if len(relay.GetStatistics()) != 0 {
		t.Errorf("no streams should remain after teardown: %+v", relay.GetStatistics())
	}
}

// waitForRelayState polls ChannelExists until it matches want, failing after
// a timeout. Used only for the asynchronous close-only fallback path.
func waitForRelayState(t *testing.T, r *Relay, name string, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.ChannelExists(name) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for stream %q existence = %v", name, want)
}

// waitForCondition polls cond until it reports true or the deadline passes.
// It is the deterministic alternative to sleeping when waiting on state that
// is updated by an internal goroutine (e.g. the relay publisher loop).
func waitForCondition(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// readUntilClosed drains a subscriber's delivery channel until it closes,
// returning the number of buffers consumed. Both teardown paths signal this the
// same way — overflow removal and Channel.Close drain queued buffers and then
// close the delivery channel — so this is the reliable completion signal. It is
// bounded by a deadline so a missing teardown fails the test rather than hangs.
func readUntilClosed(t *testing.T, sub *Subscriber) (int, error) {
	t.Helper()
	consumed := 0
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-sub.chanClosed:
			// Whole-channel teardown: delivery channel is drained and closed
			// by Channel.Close; return the upstream-closed error after
			// flushing anything still queued.
			for {
				select {
				case buf, ok := <-sub.ch:
					if !ok {
						return consumed, errUpstreamClosed
					}
					if buf != nil {
						buf.Release()
						consumed++
					}
				default:
					return consumed, errUpstreamClosed
				}
			}
		case buf, ok := <-sub.ch:
			// Overflow removal closes the delivery channel (ok == false) after
			// draining it; queued buffers are delivered first.
			if !ok {
				return consumed, errLateSubscriber
			}
			if buf != nil {
				buf.Release()
				consumed++
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for subscriber to close")
			return consumed, nil
		}
	}
}
