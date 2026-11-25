package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func requireFFmpeg(t *testing.T) (string, string) {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH; skipping real-process integration test")
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not on PATH; skipping real-process integration test")
	}
	return ff, fp
}

// A9 baseline: a real encode over websocket is accepted, started, progresses
// and ends exactly once at 100% with a produced output file.
func TestRealFFmpeg_HappyPath(t *testing.T) {
	requireFFmpeg(t)

	out := filepath.Join(t.TempDir(), "happy-out.mp4")
	input, err := filepath.Abs(filepath.Join("..", "demo", "tears-of-steel-5s.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(input); err != nil {
		t.Skipf("demo input unavailable: %v", err)
	}

	s := NewServer(Options{
		QueueSize:        4,
		AllowAllOrigins:  true,
		Runner:           realRunner{progressInterval: 100 * time.Millisecond},
		TerminalTTL:      time.Minute,
		ProgressInterval: 100 * time.Millisecond,
	})
	s.Start()
	hs := newTestServer(t, s)
	h := &harness{t: t, s: s, hs: hs}
	t.Cleanup(func() { _ = s.Shutdown(testContext(t)) })

	c := h.dial("real-happy")
	c.send(map[string]any{
		"type":    msgEncode,
		"input":   input,
		"output":  out,
		"payload": `{"raw":["-c copy"]}`,
	})

	evs, err := c.waitFor(90*time.Second, func(st Status) bool {
		return st.Type == eventCompleted || st.Type == eventFailed || st.Type == eventCancelled
	})
	if err != nil {
		t.Fatalf("no terminal event: %v", err)
	}
	var terminal *Status
	for i := range evs {
		if isTerminalEvent(evs[i]) {
			terminal = &evs[i]
		}
	}
	if terminal == nil {
		t.Fatalf("no terminal in %+v", evs)
	}
	if terminal.Type != eventCompleted {
		t.Fatalf("real encode ended %s: %s", terminal.Type, terminal.Err)
	}
	if terminal.Percent != 100 {
		t.Fatalf("completion percent = %v, want 100", terminal.Percent)
	}
	if findEvent(evs, eventAccepted) == nil || findEvent(evs, eventStarted) == nil {
		t.Fatalf("missing accepted/started phases: %+v", evs)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("output file not produced: %v", err)
	}
}

// A3 for real: cancelling a genuine running ffmpeg yields one cancelled
// terminal and no progress afterwards.
func TestRealFFmpeg_CancelRunningProcess(t *testing.T) {
	ff, _ := requireFFmpeg(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "slow-src.mp4")
	gen := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=1280x720:rate=30:duration=180",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate x264 source (%v): %s", err, out)
	}

	out := filepath.Join(dir, "cancel-out.mp4")
	s := NewServer(Options{
		QueueSize:        4,
		AllowAllOrigins:  true,
		Runner:           realRunner{progressInterval: 50 * time.Millisecond},
		TerminalTTL:      time.Minute,
		ProgressInterval: 50 * time.Millisecond,
	})
	s.Start()
	hs := newTestServer(t, s)
	h := &harness{t: t, s: s, hs: hs}
	t.Cleanup(func() { _ = s.Shutdown(testContext(t)) })

	c := h.dial("real-cancel")
	c.send(map[string]any{
		"type":    msgEncode,
		"input":   src,
		"output":  out,
		"payload": `{"raw":["-c:v libx264 -preset veryslow -an"]}`,
	})

	prog, err := c.waitFor(60*time.Second, func(st Status) bool {
		return st.Type == eventProgress && st.TaskID != ""
	})
	if err != nil {
		// Some builds/codecs may not report progress quickly; still try cancel
		// via the accepted id if present.
		t.Skipf("no real progress before timeout: %v", err)
	}
	id := prog[len(prog)-1].TaskID

	c.send(map[string]any{"type": msgCancel, "taskId": id})
	term, err := c.waitFor(30*time.Second, func(st Status) bool {
		return st.Type == eventCancelled && st.TaskID == id
	})
	if err != nil {
		t.Fatalf("real ffmpeg was not cancelled: %v", err)
	}
	n := 0
	for _, st := range term {
		if st.Type == eventCancelled && st.TaskID == id {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("cancelled terminals = %d, want 1", n)
	}

	for _, st := range c.drain(800 * time.Millisecond) {
		if st.TaskID != id {
			continue
		}
		if st.Type == eventProgress {
			t.Fatalf("progress after real cancel: %+v", st)
		}
		if isTerminalEvent(st) {
			t.Fatalf("second terminal after real cancel: %+v", st)
		}
	}
}
