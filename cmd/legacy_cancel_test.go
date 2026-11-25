package cmd

import (
	"testing"
	"time"
)

// The historical client sends {"type":"cancel"} with no id; that must still
// cancel the submitter's running task.
func TestA3_LegacyIdlessCancel(t *testing.T) {
	h := newHarness(t, 4, tickRunner{interval: 5 * time.Millisecond})
	a := h.dial("legacy-owner")

	a.send(encodeMsg("", "in.mp4", "out.mp4"))
	if _, err := a.waitFor(waitLong, func(s Status) bool { return s.Type == eventProgress }); err != nil {
		t.Fatalf("no progress: %v", err)
	}

	a.send(map[string]any{"type": msgCancel}) // no taskId
	term, err := a.waitFor(waitLong, func(s Status) bool { return s.Type == eventCancelled })
	if err != nil {
		t.Fatalf("id-less cancel did not cancel owner task: %v", err)
	}
	if len(term) == 0 || !term[len(term)-1].Cancelled {
		t.Fatalf("expected cancelled terminal, got %+v", term)
	}
}

// A client-supplied numeric seq reference can also address a task.
func TestA4_CancelByNumericSeq(t *testing.T) {
	h := newHarness(t, 4, funcRunner(func(*Task, ProgressFunc) error { return nil }))
	a := h.dial("seq-owner")

	a.send(encodeMsg("", "in.mp4", "out.mp4"))
	done, err := a.waitFor(waitLong, func(s Status) bool { return s.Type == eventCompleted })
	if err != nil {
		t.Fatal(err)
	}
	seq := findEvent(done, eventCompleted).Seq
	if seq == 0 {
		t.Fatal("expected numeric seq")
	}

	a.send(map[string]any{"type": msgCancel, "seq": seq})
	errEv, err := a.waitFor(waitLong, func(s Status) bool { return s.Type == eventError })
	if err != nil {
		t.Fatalf("cancel by numeric seq should resolve to finished task: %v", err)
	}
	if findEvent(errEv, eventError).Err == "" {
		t.Fatal("expected explicit already-finished error")
	}
}
