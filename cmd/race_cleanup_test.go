package cmd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alfg/ffmpegd/ffmpeg"
)

// gateRunner blocks each run until the test supplies and closes a gate.
type gateRunner struct {
	gates chan chan struct{}
}

func (g *gateRunner) Run(t *Task, report ProgressFunc) error {
	release := <-g.gates
	select {
	case <-release:
		report(Status{Percent: 99})
		return nil
	case <-t.Context().Done():
		return ffmpeg.ErrCancelled
	}
}

// A6: cancel racing natural completion, with duplicate cancels, converges to a
// single terminal state and a single wire terminal event.
func TestA6_CancelCompletionRaceUniqueTerminal(t *testing.T) {
	gr := &gateRunner{gates: make(chan chan struct{}, 128)}
	h := newHarness(t, 16, gr)
	c := h.dial("racer-a6")

	const iterations = 40
	for i := 0; i < iterations; i++ {
		release := make(chan struct{})
		gr.gates <- release

		c.send(encodeMsg("", "in.mp4", "out.mp4"))
		acc, err := c.waitFor(waitLong, func(s Status) bool { return s.Type == eventAccepted })
		if err != nil {
			t.Fatalf("iter %d accepted: %v", i, err)
		}
		id := findEvent(acc, eventAccepted).TaskID
		if _, err := c.waitFor(waitLong, func(s Status) bool {
			return s.Type == eventStarted && s.TaskID == id
		}); err != nil {
			t.Fatalf("iter %d started: %v", i, err)
		}

		// Hammer Cancel concurrently while natural completion is released.
		var wg sync.WaitGroup
		for k := 0; k < 4; k++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = h.s.Cancel(id, "racer-a6")
			}()
		}
		if i%3 == 0 {
			// Give the cancelers a head start sometimes.
			time.Sleep(time.Microsecond)
		}
		close(release)
		wg.Wait()

		termEvs, err := c.waitFor(waitLong, func(s Status) bool {
			return isTerminalEvent(s) && s.TaskID == id
		})
		if err != nil {
			t.Fatalf("iter %d terminal: %v", i, err)
		}
		n := 0
		var last Status
		for _, st := range termEvs {
			if isTerminalEvent(st) && st.TaskID == id {
				n++
				last = st
			}
		}
		if n != 1 {
			t.Fatalf("iter %d terminal count = %d, want 1", i, n)
		}
		if last.Type != eventCompleted && last.Type != eventCancelled {
			t.Fatalf("iter %d bad terminal %q", i, last.Type)
		}

		// No second terminal / late progress may follow.
		for _, st := range c.drain(40 * time.Millisecond) {
			if st.TaskID != id {
				continue
			}
			if isTerminalEvent(st) {
				t.Fatalf("iter %d second terminal after convergence: %+v", i, st)
			}
			if st.Type == eventProgress {
				t.Fatalf("iter %d progress after terminal: %+v", i, st)
			}
		}

		// Once terminal and retained, further cancel is a clear finished error.
		_, err = h.s.Cancel(id, "racer-a6")
		var fin *TaskFinishedError
		if !errors.As(err, &fin) {
			t.Fatalf("iter %d post-terminal cancel err = %v, want TaskFinishedError", i, err)
		}
	}
}

// The queued->cancelled transition must make beginRun fail atomically, which is
// the guarantee that a cancelled queued task never reaches the runner.
func TestA2_QueuedCancelBlocksBeginRun(t *testing.T) {
	called := false
	s := NewServer(Options{
		QueueSize: 4,
		Runner: funcRunner(func(*Task, ProgressFunc) error {
			called = true
			return nil
		}),
		AllowAllOrigins: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = s.Shutdown(ctx) })
	// Deliberately do not Start the dispatcher.

	queued, err := s.Submit(Message{Type: msgEncode, ClientID: "o"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(queued.Ref, "o"); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}
	if s.beginRun(queued) {
		t.Fatal("beginRun succeeded for a cancelled queued task")
	}
	if called {
		t.Fatal("runner called for cancelled queued task")
	}
	if queued.State() != stateCancelled {
		t.Fatalf("state = %s, want cancelled", queued.State())
	}

	// A normally dequeued task can start and then be signalled as running.
	other, err := s.Submit(Message{Type: msgEncode, ClientID: "o"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.beginRun(other) {
		t.Fatal("beginRun should win for an untouched queued task")
	}
	action, _, enc, _ := other.applyCancel()
	if action != cancelRunningSignalled {
		t.Fatalf("action = %v, want running-signalled", action)
	}
	_ = enc
}

// Stress the cancel-vs-dequeue window under the race detector.
func TestA2_DequeueCancelStress(t *testing.T) {
	g := newGatedRunner()
	h := newHarness(t, 4096, g)

	blocker, err := h.s.Submit(Message{Type: msgEncode, ClientID: "stress"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, g, waitLong)

	const n = 300
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		tk, err := h.s.Submit(Message{Type: msgEncode, ClientID: "stress", Input: "x"}, nil)
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		ids[i] = tk.Ref
	}

	var wg sync.WaitGroup
	workers := 8
	ch := make(chan string)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range ch {
				_, _ = h.s.Cancel(id, "stress")
			}
		}()
	}
	for _, id := range ids {
		ch <- id
	}
	close(ch)

	time.Sleep(20 * time.Millisecond)
	g.releaseAll()
	wg.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.s.tasks.activeForOwner("stress")) == 0 && len(h.s.queue) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if active := h.s.tasks.activeForOwner("stress"); len(active) != 0 {
		t.Fatalf("%d tasks still active", len(active))
	}

	ran := g.ranRefs()
	for _, tk := range h.s.tasks.all() {
		switch tk.State() {
		case stateCompleted:
			if !ran[tk.Ref] {
				t.Fatalf("task %s completed without the runner launching", tk.Ref)
			}
		case stateCancelled:
			// Valid whether it was cancelled in queue (never ran) or while
			// running (ran and was signalled).
		default:
			t.Fatalf("task %s unexpected state %s", tk.Ref, tk.State())
		}
	}
	_ = blocker
}

// A8: terminal records expire through the janitor/reaper and then disappear.
func TestA8_TerminalRecordExpiry(t *testing.T) {
	s := NewServer(Options{
		QueueSize:       4,
		Runner:          funcRunner(func(*Task, ProgressFunc) error { return nil }),
		AllowAllOrigins: true,
		TerminalTTL:     25 * time.Millisecond,
	})
	s.Start()
	hs := newTestServer(t, s)
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = s.Shutdown(ctx) })

	c := (&harness{t: t, s: s, hs: hs}).dial("expiry-a8")
	c.send(encodeMsg("", "in.mp4", "out.mp4"))
	done, err := c.waitFor(waitLong, func(st Status) bool { return st.Type == eventCompleted })
	if err != nil {
		t.Fatal(err)
	}
	id := findEvent(done, eventCompleted).TaskID

	if _, err := s.Cancel(id, "expiry-a8"); err == nil {
		t.Fatal("cancel immediately after completion should report finished")
	}
	// Force retention expiry deterministically, then the record is gone.
	if removed := s.tasks.reap(time.Now().Add(time.Hour)); removed != 1 {
		t.Fatalf("reap removed %d records, want 1", removed)
	}
	if _, err := s.Cancel(id, "expiry-a8"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("cancel after cleanup err = %v, want ErrTaskNotFound", err)
	}
}

// A8: shutdown cancels work and leaves no triggerable records or admission.
func TestA8_ShutdownClearsRecords(t *testing.T) {
	g := newGatedRunner()
	h := newHarness(t, 2, g)
	c := h.dial("shutdown-a8")

	c.send(encodeMsg("", "one.mp4", "one.mp4"))
	first := waitStarted(t, g, waitLong)

	c.send(encodeMsg("", "two.mp4", "two.mp4"))
	acc, err := c.waitFor(waitLong, func(st Status) bool {
		return st.Type == eventAccepted && st.TaskID != first.Ref
	})
	if err != nil {
		t.Fatal(err)
	}
	queuedID := findEvent(acc, eventAccepted).TaskID

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := h.s.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	cancel()

	if !h.s.isClosed() {
		t.Fatal("server not marked closed")
	}
	if n := len(h.s.tasks.all()); n != 0 {
		t.Fatalf("records after shutdown = %d, want 0", n)
	}
	if _, err := h.s.Submit(Message{Type: msgEncode}, nil); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("submit after shutdown err = %v, want ErrServerClosed", err)
	}
	if _, err := h.s.Cancel(queuedID, "shutdown-a8"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("cancel after shutdown err = %v, want ErrTaskNotFound", err)
	}

	launches := g.launchCount()
	time.Sleep(120 * time.Millisecond)
	if g.launchCount() != launches {
		t.Fatal("runner launched again after shutdown")
	}
}
