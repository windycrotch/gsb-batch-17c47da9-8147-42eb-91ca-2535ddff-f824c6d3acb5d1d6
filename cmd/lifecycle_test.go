package cmd

import (
	"strings"
	"testing"
	"time"
)

const waitLong = 8 * time.Second

func encodeMsg(token, input, output string) map[string]any {
	m := map[string]any{"type": "encode", "input": input, "output": output}
	if token != "" {
		m["clientTaskId"] = token
	}
	return m
}

// A1: acceptance/start/progress/completion all carry the same server id and
// submitter identity, echo the client token, and go only to the owner.
func TestA1_LifecycleIdentityOnlyToOwner(t *testing.T) {
	r := funcRunner(func(t *Task, report ProgressFunc) error {
		report(Status{Percent: 42, Speed: "2.0x", FPS: 24})
		return nil
	})
	h := newHarness(t, 8, r)
	alice := h.dial("alice-a1")
	bob := h.dial("bob-a1")

	alice.send(encodeMsg("corr-1", "in.mp4", "out.mp4"))
	evs, err := alice.waitFor(waitLong, func(s Status) bool { return s.Type == eventCompleted })
	if err != nil {
		t.Fatalf("did not complete: %v\nevents: %+v", err, evs)
	}

	accepted := findEvent(evs, eventAccepted)
	if accepted == nil {
		t.Fatalf("missing accepted event: %+v", evs)
	}
	id := accepted.TaskID
	if id == "" || accepted.ID == "" || accepted.TaskIDSnake == "" || accepted.JobID == "" {
		t.Fatalf("accepted missing id aliases: %+v", accepted)
	}
	if accepted.Seq == 0 {
		t.Fatalf("accepted missing numeric seq: %+v", accepted)
	}
	if accepted.ClientTaskID != "corr-1" {
		t.Fatalf("client token echo = %q", accepted.ClientTaskID)
	}

	for _, want := range []string{eventAccepted, eventStarted, eventProgress, eventCompleted} {
		if findEvent(evs, want) == nil {
			t.Fatalf("missing phase %q in %+v", want, evs)
		}
	}
	prog := findEvent(evs, eventProgress)
	if prog.Percent != 42 {
		t.Fatalf("progress percent = %v, want 42", prog.Percent)
	}
	done := findEvent(evs, eventCompleted)
	if done.Percent != 100 || !done.Completed {
		t.Fatalf("completion should be 100/completed: %+v", done)
	}

	for i, ev := range evs {
		if ev.TaskID != id || ev.ID != id || ev.TaskIDSnake != id {
			t.Fatalf("event %d has inconsistent id: %+v", i, ev)
		}
		if ev.OwnerID != "alice-a1" || ev.ClientID != "alice-a1" || ev.SubmitterID != "alice-a1" || ev.SubmitterIDSnake != "alice-a1" {
			t.Fatalf("event %d missing owner identity: %+v", i, ev)
		}
	}

	// Raw wire payload must contain the snake_case identity keys too.
	alice.send(encodeMsg("corr-2", "in2.mp4", "out2.mp4"))
	deadline := time.Now().Add(waitLong)
	var rawAccepted map[string]any
	for time.Now().Before(deadline) {
		st, raw, err := alice.read(time.Until(deadline))
		if err != nil {
			t.Fatalf("reading second job: %v", err)
		}
		if st.Type == eventAccepted {
			rawAccepted = raw
			break
		}
	}
	if rawAccepted == nil {
		t.Fatal("no second accepted event")
	}
	for _, key := range []string{"task_id", "submitter_id", "clientId", "seq"} {
		if _, ok := rawAccepted[key]; !ok {
			t.Errorf("accepted JSON missing key %q: %v", key, rawAccepted)
		}
	}

	// A second task gets a distinct id.
	evs2, err := alice.waitFor(waitLong, func(s Status) bool { return s.Type == eventCompleted && s.TaskID != id })
	if err != nil {
		t.Fatalf("second job did not complete: %v", err)
	}
	if findEvent(evs2, eventCompleted).TaskID == id {
		t.Fatal("second task reused the first id")
	}

	// Another connected client must not receive Alice's task traffic.
	bob.expectSilence(350*time.Millisecond, "other owner connection")
}

// A2: cancelling a queued task terminates it exactly once and the runner never
// launches for it.
func TestA2_CancelQueuedNeverStartsFFmpeg(t *testing.T) {
	g := newGatedRunner()
	h := newHarness(t, 4, g)
	a := h.dial("alice-a2")

	a.send(encodeMsg("", "one.mp4", "one-out.mp4"))
	acc1, err := a.waitFor(waitLong, func(s Status) bool { return s.Type == eventAccepted })
	if err != nil {
		t.Fatal(err)
	}
	id1 := findEvent(acc1, eventAccepted).TaskID
	running := waitStarted(t, g, waitLong)
	if running.Ref != id1 {
		t.Fatalf("runner started %s, want accepted %s", running.Ref, id1)
	}

	a.send(encodeMsg("token-two", "two.mp4", "two-out.mp4"))
	acc2, err := a.waitFor(waitLong, func(s Status) bool {
		return s.Type == eventAccepted && s.TaskID != id1
	})
	if err != nil {
		t.Fatal(err)
	}
	id2 := findEvent(acc2, eventAccepted).TaskID
	if id2 == "" || id2 == id1 {
		t.Fatalf("bad second id %q vs %q", id2, id1)
	}

	// Cancel the queued task by its server id.
	a.send(map[string]any{"type": msgCancel, "taskId": id2})
	cancelEvs, err := a.waitFor(waitLong, func(s Status) bool {
		return s.Type == eventCancelled && s.TaskID == id2
	})
	if err != nil {
		t.Fatalf("did not observe cancelled for queued task: %v", err)
	}
	if c := findEvent(cancelEvs, eventCancelled); !c.Cancelled {
		t.Fatal("cancelled event should set cancelled=true")
	}

	// Give a racy dispatcher every chance to wrongly launch it.
	time.Sleep(150 * time.Millisecond)
	if got := g.launchCount(); got != 1 {
		t.Fatalf("runner launches = %d, want exactly 1 (only first task)", got)
	}
	if g.ranRefs()[id2] {
		t.Fatalf("queued cancelled task %s was launched", id2)
	}

	// Drain ack/strays: no completion/failure and no second cancel terminal.
	rest := a.drain(300 * time.Millisecond)
	all := append(append([]Status{}, cancelEvs...), rest...)
	completed, failed, cancelled := countTerminal(all, id2)
	if completed != 0 || failed != 0 {
		t.Fatalf("queued cancel produced terminal completed=%d failed=%d", completed, failed)
	}
	if cancelled != 1 {
		t.Fatalf("cancelled terminal count = %d, want 1", cancelled)
	}

	state := h.s.tasks.all()
	_ = state
	if task, ok := h.s.tasks.get(id2, time.Now()); !ok || task.State() != stateCancelled {
		t.Fatalf("task %s state = %v ok=%v, want cancelled", id2, func() TaskState {
			if task != nil {
				return task.State()
			}
			return ""
		}(), ok)
	}

	// First task still runs and completes normally once released.
	g.releaseAll()
	done1, err := a.waitFor(waitLong, func(s Status) bool {
		return s.Type == eventCompleted && s.TaskID == id1
	})
	if err != nil {
		t.Fatalf("first task did not finish: %v", err)
	}
	if findEvent(done1, eventCompleted).TaskID != id1 {
		t.Fatal("completed wrong task")
	}
}

// A3: cancelling a running task stops further progress and yields one terminal.
func TestA3_CancelRunningStopsProgress(t *testing.T) {
	h := newHarness(t, 4, tickRunner{interval: 5 * time.Millisecond})
	a := h.dial("alice-a3")

	a.send(encodeMsg("", "in.mp4", "out.mp4"))
	progEvs, err := a.waitFor(waitLong, func(s Status) bool { return s.Type == eventProgress && s.TaskID != "" })
	if err != nil {
		t.Fatalf("no progress before cancel: %v", err)
	}
	id := progEvs[len(progEvs)-1].TaskID

	a.send(map[string]any{"type": msgCancel, "taskId": id})
	termEvs, err := a.waitFor(waitLong, func(s Status) bool { return isTerminalEvent(s) && s.TaskID == id })
	if err != nil {
		t.Fatalf("no terminal after cancel: %v", err)
	}
	var terminals []Status
	for _, st := range termEvs {
		if isTerminalEvent(st) {
			terminals = append(terminals, st)
		}
	}
	if len(terminals) != 1 || terminals[0].Type != eventCancelled {
		t.Fatalf("terminals = %+v, want exactly one cancelled", terminals)
	}

	// Nothing terminal and no progress for this id may follow.
	after := a.drain(500 * time.Millisecond)
	for _, st := range after {
		if st.TaskID != id {
			continue
		}
		if st.Type == eventProgress {
			t.Fatalf("progress after cancel terminal: %+v", st)
		}
		if isTerminalEvent(st) {
			t.Fatalf("second terminal after cancel: %+v", st)
		}
	}

	task, ok := h.s.tasks.get(id, time.Now())
	if !ok || task.State() != stateCancelled {
		t.Fatalf("state ok=%v state=%v, want cancelled", ok, task.State())
	}
}

// A4: cancelling an unknown or already-ended task is an explicit error.
func TestA4_CancelUnknownAndFinishedErrors(t *testing.T) {
	h := newHarness(t, 4, funcRunner(func(*Task, ProgressFunc) error { return nil }))
	a := h.dial("alice-a4")

	a.send(map[string]any{"type": msgCancel, "taskId": "does-not-exist"})
	errEvs, err := a.waitFor(waitLong, func(s Status) bool { return s.Type == eventError })
	if err != nil {
		t.Fatalf("missing error for unknown cancel: %v", err)
	}
	ev := findEvent(errEvs, eventError)
	if ev.Err == "" || !strings.Contains(ev.Err, "not found") {
		t.Fatalf("unknown cancel error = %q", ev.Err)
	}
	if ev.OK == nil || *ev.OK {
		t.Fatalf("error event ok should be false: %+v", ev.OK)
	}

	a.send(encodeMsg("", "in.mp4", "out.mp4"))
	done, err := a.waitFor(waitLong, func(s Status) bool { return s.Type == eventCompleted })
	if err != nil {
		t.Fatal(err)
	}
	id := findEvent(done, eventCompleted).TaskID

	for i := 0; i < 2; i++ {
		a.send(map[string]any{"type": msgCancel, "taskId": id})
		errEvs, err = a.waitFor(waitLong, func(s Status) bool { return s.Type == eventError && s.TaskID == id })
		if err != nil {
			t.Fatalf("cancel finished task #%d: %v", i+1, err)
		}
		ev = findEvent(errEvs, eventError)
		if ev.Err == "" || !strings.Contains(ev.Err, "already completed") {
			t.Fatalf("finished cancel error = %q, want already completed", ev.Err)
		}
	}
}

// A5: reconnecting with the same client id replays the unfinished task and
// keeps streaming progress; other owners never receive it.
func TestA5_ReconnectReclaimsOwnTasksOnly(t *testing.T) {
	h := newHarness(t, 4, tickRunner{interval: 15 * time.Millisecond})
	first := h.dial("alice-a5")
	bob := h.dial("bob-a5")

	first.send(encodeMsg("", "in.mp4", "out.mp4"))
	started, err := first.waitFor(waitLong, func(s Status) bool { return s.Type == eventStarted })
	if err != nil {
		t.Fatal(err)
	}
	idA := findEvent(started, eventStarted).TaskID
	if _, err := first.waitFor(waitLong, func(s Status) bool { return s.Type == eventProgress && s.TaskID == idA }); err != nil {
		t.Fatalf("no initial progress: %v", err)
	}

	// Drop the connection, then reconnect with the same stable identity.
	_ = first.conn.Close()
	time.Sleep(200 * time.Millisecond)

	again := h.dial("alice-a5")
	snaps, err := again.waitFor(waitLong, func(s Status) bool { return s.Type == eventSnapshot && s.TaskID == idA })
	if err != nil {
		t.Fatalf("reconnect did not replay running task: %v", err)
	}
	snap := findEvent(snaps, eventSnapshot)
	if !snap.Replay || (snap.State != string(stateRunning) && snap.State != string(stateCancelling)) {
		t.Fatalf("bad snapshot: %+v", snap)
	}
	if _, err := again.waitFor(3*time.Second, func(s Status) bool {
		return s.Type == eventProgress && s.TaskID == idA
	}); err != nil {
		t.Fatalf("progress did not continue after reconnect: %v", err)
	}

	// Bob receives none of Alice's traffic.
	bob.expectSilence(300*time.Millisecond, "bob before submitting")

	// Bob's own queued task must never be delivered to Alice's new connection.
	bob.send(encodeMsg("bob-job", "b.mp4", "b-out.mp4"))
	bobAcc, err := bob.waitFor(waitLong, func(s Status) bool { return s.Type == eventAccepted })
	if err != nil {
		t.Fatal(err)
	}
	idB := findEvent(bobAcc, eventAccepted).TaskID

	leak := again.drain(350 * time.Millisecond)
	for _, st := range leak {
		if st.TaskID == idB {
			t.Fatalf("Alice reconnection saw Bob's task %s: %+v", idB, st)
		}
	}

	bob.send(map[string]any{"type": msgCancel, "taskId": idB})
	if _, err := bob.waitFor(waitLong, func(s Status) bool {
		return s.Type == eventCancelled && s.TaskID == idB
	}); err != nil {
		t.Fatalf("bob cancel queued: %v", err)
	}

	again.send(map[string]any{"type": msgCancel, "taskId": idA})
	if _, err := again.waitFor(waitLong, func(s Status) bool {
		return s.Type == eventCancelled && s.TaskID == idA
	}); err != nil {
		t.Fatalf("alice cancel after reconnect: %v", err)
	}
}

// A7: a full queue rejects submission with a clear error and leaves no
// cancellable task record behind.
func TestA7_QueueFullRejectsWithoutRecord(t *testing.T) {
	g := newGatedRunner()
	h := newHarness(t, 1, g)
	a := h.dial("alice-a7")

	a.send(encodeMsg("", "one.mp4", "one-out.mp4"))
	waitStarted(t, g, waitLong)

	a.send(encodeMsg("", "two.mp4", "two-out.mp4"))
	if _, err := a.waitFor(waitLong, func(s Status) bool { return s.Type == eventAccepted }); err != nil {
		t.Fatalf("second job should fit the queue buffer: %v", err)
	}

	a.send(encodeMsg("corr-full", "three.mp4", "three-out.mp4"))
	errEvs, err := a.waitFor(waitLong, func(s Status) bool { return s.Type == eventError })
	if err != nil {
		t.Fatalf("missing queue-full error: %v", err)
	}
	ev := findEvent(errEvs, eventError)
	if !strings.Contains(strings.ToLower(ev.Err), "queue") {
		t.Fatalf("queue-full error = %q", ev.Err)
	}
	if ev.ClientTaskID != "corr-full" {
		t.Fatalf("queue-full error did not echo client token: %+v", ev)
	}

	// The rejected token has no record and cannot be cancelled.
	if _, ok := h.s.tasks.resolve("corr-full", "alice-a7", time.Now()); ok {
		t.Fatal("rejected submission left a resolvable record")
	}
	if _, err := h.s.Cancel("corr-full", "alice-a7"); err != ErrTaskNotFound {
		t.Fatalf("cancel rejected token err = %v, want ErrTaskNotFound", err)
	}

	g.releaseAll()
}
