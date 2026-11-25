package main

import (
	"context"
	"sync"
	"testing"
)

func TestSessionManager_ReleaseIsIdempotent(t *testing.T) {
	m := NewSessionManager(1)

	release, ok := m.Acquire(func() {})
	if !ok {
		t.Fatal("first Acquire failed")
	}
	if m.Active() != 1 {
		t.Fatalf("Active = %d, want 1", m.Active())
	}

	// Releasing repeatedly must free exactly one slot.
	release()
	release()
	release()
	if m.Active() != 0 {
		t.Fatalf("Active = %d, want 0", m.Active())
	}

	// Capacity must be exactly 1 again: one acquire succeeds, the next fails.
	r1, ok := m.Acquire(func() {})
	if !ok {
		t.Fatal("re-acquire after release failed")
	}
	if _, ok := m.Acquire(func() {}); ok {
		t.Fatal("acquired two slots with capacity 1; release over-freed")
	}
	r1()
}

func TestSessionManager_DoubleReleaseDoesNotStealOtherSlot(t *testing.T) {
	// Capacity 2. Two sessions hold the slots. If session A is released twice,
	// the second release must NOT consume session B's slot.
	m := NewSessionManager(2)

	relA, ok := m.Acquire(func() {})
	if !ok {
		t.Fatal("Acquire A failed")
	}
	if _, ok := m.Acquire(func() {}); !ok {
		t.Fatal("Acquire B failed")
	}

	relA()
	relA() // must be a no-op

	if got := m.Active(); got != 1 {
		t.Fatalf("Active = %d, want 1 (only A should have been released)", got)
	}

	// One slot must now be free for a new session; B's slot must be intact.
	relC, ok := m.Acquire(func() {})
	if !ok {
		t.Fatal("Acquire after double-release failed; the second release stole B's slot")
	}
	// And after C, the server is full again: A's slot + B's slot are consumed.
	if _, ok := m.Acquire(func() {}); ok {
		t.Fatal("more capacity than configured; a release over-freed")
	}
	relC()
}

func TestSessionManager_ReleaseConcurrent(t *testing.T) {
	m := NewSessionManager(1)
	release, ok := m.Acquire(func() {})
	if !ok {
		t.Fatal("Acquire failed")
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release()
		}()
	}
	wg.Wait()

	if m.Active() != 0 {
		t.Fatalf("Active = %d, want 0", m.Active())
	}
	// Exactly one slot must be free.
	a, ok := m.Acquire(func() {})
	if !ok {
		t.Fatal("no capacity after concurrent releases")
	}
	if _, ok := m.Acquire(func() {}); ok {
		t.Fatal("more than one slot freed by concurrent releases")
	}
	a()
}

func TestSessionManager_ShutdownCancelsAndRejects(t *testing.T) {
	m := NewSessionManager(4)

	cancelled := make(chan struct{}, 2)
	var mu sync.Mutex
	var count int
	mk := func() context.CancelFunc {
		return func() {
			mu.Lock()
			count++
			mu.Unlock()
			cancelled <- struct{}{}
		}
	}
	r1, ok := m.Acquire(mk())
	if !ok {
		t.Fatal("Acquire 1 failed")
	}
	r2, ok := m.Acquire(mk())
	if !ok {
		t.Fatal("Acquire 2 failed")
	}

	m.Shutdown()
	m.Shutdown() // idempotent

	<-cancelled
	<-cancelled
	mu.Lock()
	got := count
	mu.Unlock()
	if got != 2 {
		t.Errorf("cancel calls = %d, want 2", got)
	}

	// New sessions are refused after shutdown.
	if _, ok := m.Acquire(func() {}); ok {
		t.Fatal("Acquire succeeded after Shutdown")
	}

	// Releasing the pre-shutdown sessions still works and frees capacity.
	r1()
	r2()
	if m.Active() != 0 {
		t.Errorf("Active = %d, want 0", m.Active())
	}
}

func TestSessionManager_NilIsUnlimited(t *testing.T) {
	var m *SessionManager
	release, ok := m.Acquire(func() {})
	if !ok {
		t.Fatal("nil manager should admit")
	}
	release()
	release() // must not panic
}
