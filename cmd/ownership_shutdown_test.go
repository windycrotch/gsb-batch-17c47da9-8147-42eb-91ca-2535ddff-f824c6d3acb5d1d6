package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A5 ownership boundary: another submitter may not read or cancel a task by
// its enumerable id.
func TestA5_CrossOwnerQueryAndCancelDenied(t *testing.T) {
	h := newHarness(t, 4, tickRunner{interval: 5 * time.Millisecond})
	alice := h.dial("alice-boundary")
	bob := h.dial("bob-boundary")

	alice.send(encodeMsg("", "in.mp4", "out.mp4"))
	prog, err := alice.waitFor(waitLong, func(s Status) bool { return s.Type == eventProgress })
	if err != nil {
		t.Fatal(err)
	}
	id := prog[len(prog)-1].TaskID

	// Bob cannot snapshot Alice's running task.
	bob.send(map[string]any{"type": msgQuery, "taskId": id})
	qErr, err := bob.waitFor(waitLong, func(s Status) bool { return s.Type == eventError })
	if err != nil {
		t.Fatalf("bob query produced no error: %v", err)
	}
	assertNotFound(t, findEvent(qErr, eventError))

	// Bob cannot cancel it.
	bob.send(map[string]any{"type": msgCancel, "taskId": id})
	cErr, err := bob.waitFor(waitLong, func(s Status) bool { return s.Type == eventError })
	if err != nil {
		t.Fatalf("bob cancel produced no error: %v", err)
	}
	assertNotFound(t, findEvent(cErr, eventError))

	task, ok := h.s.tasks.get(id, time.Now())
	if !ok {
		t.Fatal("alice task vanished")
	}
	if task.State() != stateRunning && task.State() != stateCancelling {
		t.Fatalf("bob cancel changed state to %s", task.State())
	}

	// Alice can still cancel her own task and gets the single terminal.
	alice.send(map[string]any{"type": msgCancel, "taskId": id})
	if _, err := alice.waitFor(waitLong, func(s Status) bool {
		return s.Type == eventCancelled && s.TaskID == id
	}); err != nil {
		t.Fatalf("alice own cancel failed: %v", err)
	}
}

func assertNotFound(t *testing.T, st *Status) {
	t.Helper()
	if st == nil || st.Err == "" {
		t.Fatalf("expected not-found error, got %+v", st)
	}
	if !strings.Contains(st.Err, "not found") {
		t.Fatalf("error %q is not a not-found error", st.Err)
	}
}

// A8 production cleanup: Shutdown closes live websocket connections and stops
// new upgrades.
func TestA8_ShutdownClosesWebsocket(t *testing.T) {
	h := newHarness(t, 4, funcRunner(func(*Task, ProgressFunc) error { return nil }))
	c := h.dial("close-me")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := h.s.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	cancel()

	_, _, err := c.read(3 * time.Second)
	if !errors.Is(err, errConnClosed) {
		t.Fatalf("read after shutdown err = %v, want errConnClosed", err)
	}

	// New websocket upgrades are refused once closed.
	_, _, dialErr := websocket.DefaultDialer.Dial(h.wsURL("late"), nil)
	if dialErr == nil {
		t.Fatal("expected dial after shutdown to fail")
	}
}
