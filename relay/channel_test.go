package relay

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestChannel_PubSub(t *testing.T) {
	ch := NewChannel("test", uint(1316*50))
	pool := NewBufferPool(4)
	expected := []byte{1, 2, 3, 4}

	// sub
	out, unsub, err := ch.Sub()
	if err != nil {
		t.Fatal(err)
	}
	data := pool.Get()
	copy(data.Bytes(), expected)
	data.Resize(4)

	// pub
	ch.Pub(data)
	got, _ := out.Read()

	if !reflect.DeepEqual(got.Bytes(), expected) {
		t.Errorf("Sub ret = %x, want %x", got.Bytes(), expected)
	}
	got.Release()

	// unsub
	unsub()

	// pub2
	data = pool.Get()
	copy(data.Bytes(), expected)
	data.Resize(4)
	ch.Pub(data)
	got, _ = out.Read()

	if got != nil {
		t.Errorf("Read after unsub ret %v, want nil", got)
	}
}

func TestChannel_DropOnOverflow(t *testing.T) {
	ch := NewChannel("test", uint(1316*50))

	sub, _, err := ch.Sub()
	if err != nil {
		t.Fatal(err)
	}
	capacity := cap(sub.ch) + 1
	pool := NewBufferPool(1)

	// Overflow subscriber on purpose
	for range capacity {
		buf := pool.Get()
		buf.Resize(0)
		ch.Pub(buf)
		// Should drop and close subscriber on last iteration
	}

	// Check removal
	if remaining := len(ch.subs); remaining > 0 {
		t.Errorf("Got %d remaining channels, expected 0", remaining)
	}

	if _, err := sub.Read(); err == nil {
		t.Error("Expected overflowed subscriber to be closed")
	}
}

func TestChannel_Close(t *testing.T) {
	ch := NewChannel("test", 1)
	sub1, _, err := ch.Sub()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ch.Sub()
	if err != nil {
		t.Fatal(err)
	}

	if got := len(ch.subs); got != 2 {
		t.Fatalf("Expected 2 subscribers on channel, got %d", got)
	}

	ch.Close()

	if got := len(ch.subs); got != 0 {
		t.Errorf("Expected 0 subscribers after close, got %d", got)
	}
	if got := ch.Stats().clients; got != 0 {
		t.Errorf("Expected 0 clients after close, got %d", got)
	}

	if _, err := sub1.Read(); err == nil {
		t.Error("Subscriber channel should be closed after Close")
	}
}

func TestChannel_UnsubscribeAfterClose(t *testing.T) {
	ch := NewChannel("test", 0)
	_, unsubscribe, err := ch.Sub()
	if err != nil {
		t.Fatal(err)
	}

	ch.Close()
	unsubscribe()
	unsubscribe()

	if got := ch.Stats().clients; got != 0 {
		t.Errorf("Expected 0 clients after unsubscribe on closed channel, got %d", got)
	}
	if ch.subs != nil {
		t.Errorf("Expected nil subscribers slice after close, got len=%d", len(ch.subs))
	}
}

func TestChannel_CloseIsIdempotent(t *testing.T) {
	ch := NewChannel("test", 1)
	sub, unsubscribe, err := ch.Sub()
	if err != nil {
		t.Fatal(err)
	}

	// Repeated and concurrent closes must not panic on close(closed).
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch.Close()
		}()
	}
	wg.Wait()
	ch.Close()

	if _, err := sub.Read(); err == nil {
		t.Error("Subscriber channel should be closed after Close")
	}
	// Unsubscribing after a completed teardown must stay a no-op.
	unsubscribe()
}

func TestChannel_SubscribeAfterCloseFails(t *testing.T) {
	ch := NewChannel("test", 1)
	ch.Close()

	sub, unsub, err := ch.Sub()
	if err != ErrChannelClosed {
		t.Fatalf("Sub after close: got err=%v sub=%v, want ErrChannelClosed", err, sub)
	}
	if unsub != nil {
		t.Error("Sub after close should not return an unsubscribe function")
	}
	// No phantom subscribers or metrics may be created on the closed channel.
	if ch.subs != nil {
		t.Errorf("Expected nil subscribers slice, got len=%d", len(ch.subs))
	}
	if got := ch.Stats().clients; got != 0 {
		t.Errorf("Expected 0 clients after failed Sub, got %d", got)
	}
}

func TestChannel_OverflowDropThenUnsubscribeIdempotent(t *testing.T) {
	ch := NewChannel("test", 1)
	pool := NewBufferPool(1)

	sub, unsub, err := ch.Sub()
	if err != nil {
		t.Fatal(err)
	}

	// Overflow the subscriber queue so Pub drops it and decrements the gauge.
	for range cap(sub.ch) + 1 {
		buf := pool.Get()
		buf.Resize(0)
		ch.Pub(buf)
	}

	if got := len(ch.subs); got != 0 {
		t.Fatalf("expected no remaining subscribers after overflow, got %d", got)
	}
	if got := ch.Stats().clients; got != 0 {
		t.Errorf("expected 0 clients after overflow drop, got %d", got)
	}
	if got := testutil.ToFloat64(ch.activeClients); got != 0 {
		t.Errorf("gauge after overflow drop: got %v, want 0", got)
	}

	// The dropped client's later (and repeated) cancellation must not go
	// negative or touch the slice.
	unsub()
	unsub()
	if got := testutil.ToFloat64(ch.activeClients); got != 0 {
		t.Errorf("gauge after late unsubscribe: got %v, want 0", got)
	}
}

func TestChannel_CloseDrainsQueuedBuffers(t *testing.T) {
	ch := NewChannel("test", 4)
	pool := NewBufferPool(1)

	sub, _, err := ch.Sub()
	if err != nil {
		t.Fatal(err)
	}

	// Queue buffers without consuming them.
	for range 3 {
		buf := pool.Get()
		buf.Resize(0)
		ch.Pub(buf)
	}

	ch.Close()

	// Delivery channel is closed and every queued buffer was released back
	// to the pool; reads return an error instead of returning recycled data.
	if _, err := sub.Read(); err == nil {
		t.Error("expected subscriber read to fail after close")
	}
}

func TestChannel_Stats(t *testing.T) {
	ch := NewChannel("test", 0)
	if num := ch.Stats().clients; num != 0 {
		t.Errorf("Expected 0 clients after create, got %d", num)
	}

	_, unsubscribe, err := ch.Sub()
	if err != nil {
		t.Fatal(err)
	}
	if num := ch.Stats().clients; num != 1 {
		t.Errorf("Expected 1 clients after subscribe, got %d", num)
	}

	unsubscribe()
	if num := ch.Stats().clients; num != 0 {
		t.Errorf("Expected 0 clients after unsubscribe, got %d", num)
	}
}

// run with high -count to reproduce race reliably
// e.g. go test -run 'TestChannel_PubMultipleSubscribers' ./... -count 100
func TestChannel_PubMultipleSubscribers(t *testing.T) {
	ch := NewChannel("test", 1)
	pool := NewBufferPool(4)
	expected := []byte{1, 2, 3, 4}
	iterations := 50000
	numReaders := 10
	done := make(chan struct{}, numReaders)

	var wg sync.WaitGroup
	wg.Add(numReaders)

	readAndRelease := func(sub *Subscriber) {
		defer wg.Done()
		for range iterations {
			buf, err := sub.Read()
			if err != nil {
				t.Errorf("read failed: %v", err)
				return
			}
			buf.Release()
			done <- struct{}{}
		}
	}

	for range numReaders {
		sub, _, err := ch.Sub()
		if err != nil {
			t.Fatal(err)
		}
		go readAndRelease(sub)
	}

	i := 0
	defer func() {
		fmt.Printf("got %d\n", i)
	}()

	for range iterations {
		buf := pool.Get()
		copy(buf.Bytes(), expected)
		buf.Resize(len(expected))
		ch.Pub(buf)
		i++
		for range numReaders {
			select {
			case <-done:
			case <-time.After(time.Millisecond * 100):
				t.Fatal("timeout waiting for readers to finish")
			}
		}
	}

	wg.Wait()
	if got := ch.Stats().clients; got != numReaders {
		t.Errorf("Expected %d clients after publish, got %d", numReaders, got)
	}
}
