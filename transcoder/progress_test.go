package transcoder

import (
	"io"
	"testing"
	"time"

	"github.com/xfrr/goffmpeg/media"
)

const sampleProgressLine = "frame=  120 fps= 25 q=28.0 size=     256kB time=00:00:04.80 bitrate= 436.9kbits/s speed= 1.0x"

func newMediaWithDuration(duration string) *media.File {
	mf := &media.File{}
	md := media.Metadata{}
	md.Format.Duration = duration
	mf.SetMetadata(md)
	return mf
}

func TestParseProgressLine_PopulatesAllFields(t *testing.T) {
	mf := newMediaWithDuration("10")
	p, ok := parseProgressLine(sampleProgressLine, mf)
	if !ok {
		t.Fatal("expected line to be recognized as a progress line")
	}
	if p.FramesProcessed != "120" {
		t.Errorf("frames: got %q want %q", p.FramesProcessed, "120")
	}
	if p.CurrentTime != "00:00:04.80" {
		t.Errorf("time: got %q want %q", p.CurrentTime, "00:00:04.80")
	}
	if p.CurrentBitrate != "436.9kbits/s" {
		t.Errorf("bitrate: got %q want %q", p.CurrentBitrate, "436.9kbits/s")
	}
	if p.Speed != "1.0x" {
		t.Errorf("speed: got %q want %q", p.Speed, "1.0x")
	}
	// 4.8s of 10s => 48%.
	if p.Progress < 47.99 || p.Progress > 48.01 {
		t.Errorf("percentage: got %v want ~48", p.Progress)
	}
	if p.Err != nil {
		t.Errorf("unexpected error: %v", p.Err)
	}
}

func TestParseProgressLine_IgnoresNonProgressLines(t *testing.T) {
	for _, line := range []string{
		"ffmpeg version 6.0",
		"frame= 10",
		"time=00:00:01.00 bitrate=100kbits/s", // missing frame=
		"",
	} {
		if _, ok := parseProgressLine(line, newMediaWithDuration("10")); ok {
			t.Errorf("line %q should not be parsed as progress", line)
		}
	}
}

// Unknown/zero duration (e.g. a live stream) must not divide by zero and must
// keep reporting every other field while leaving the percentage at zero.
func TestParseProgressLine_UnknownDurationNoDivideByZero(t *testing.T) {
	cases := []struct {
		name     string
		duration string
	}{
		{"zero duration", "0"},
		{"empty duration", ""},
		{"non numeric duration", "N/A"},
		{"negative duration", "-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mf := newMediaWithDuration(tc.duration)
			p, ok := parseProgressLine(sampleProgressLine, mf)
			if !ok {
				t.Fatal("expected progress line to be recognized")
			}
			if p.Progress != 0 {
				t.Errorf("percentage must stay zero for unknown duration, got %v", p.Progress)
			}
			if p.FramesProcessed != "120" || p.CurrentTime != "00:00:04.80" ||
				p.CurrentBitrate != "436.9kbits/s" || p.Speed != "1.0x" {
				t.Errorf("other fields must still be reported, got %+v", p)
			}
		})
	}
}

func TestParseProgressLine_NilMediaFileDoesNotPanic(t *testing.T) {
	p, ok := parseProgressLine(sampleProgressLine, nil)
	if !ok {
		t.Fatal("expected progress line to be recognized")
	}
	if p.Progress != 0 {
		t.Errorf("percentage must be zero without a media file, got %v", p.Progress)
	}
	if p.CurrentTime != "00:00:04.80" {
		t.Errorf("time field expected, got %q", p.CurrentTime)
	}
}

func TestScanProgress_EmitsAndClosesOnEOF(t *testing.T) {
	r, w := io.Pipe()
	mf := newMediaWithDuration("10")
	stop := make(chan struct{})
	out := scanProgress(r, mf, stop)

	go func() {
		_, _ = io.WriteString(w, sampleProgressLine+"\r")
		_, _ = io.WriteString(w, "ignored log line\n")
		_, _ = io.WriteString(w, sampleProgressLine+"\n")
		_ = w.Close()
	}()

	var got []Progress
	for p := range out {
		got = append(got, p)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 progress updates, got %d", len(got))
	}
	for i, p := range got {
		if p.FramesProcessed != "120" {
			t.Errorf("update %d frames = %q", i, p.FramesProcessed)
		}
	}
}

// Closing stop plus releasing the reader (the pair finalize always performs)
// must close the progress channel promptly even when the writer side stays open.
func TestScanProgress_StopAndReaderCloseUnblocks(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close() // the producer side stays open
	stop := make(chan struct{})
	out := scanProgress(r, newMediaWithDuration("10"), stop)

	_, _ = io.WriteString(w, sampleProgressLine+"\r")
	if p, ok := <-out; !ok || p.FramesProcessed != "120" {
		t.Fatalf("missing initial progress: %+v ok=%v", p, ok)
	}

	// Mirror finalize: signal stop, then close the reader to end the blocked scan.
	close(stop)
	if err := r.Close(); err != nil {
		t.Fatalf("reader close: %v", err)
	}

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected progress channel to be closed after stop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("progress channel did not close promptly after stop")
	}
}

func TestIsClosed(t *testing.T) {
	var nilCh chan struct{}
	if isClosed(nilCh) {
		t.Error("nil channel must report not-closed")
	}
	ch := make(chan struct{})
	if isClosed(ch) {
		t.Error("open channel must report not-closed")
	}
	close(ch)
	if !isClosed(ch) {
		t.Error("closed channel must report closed")
	}
}
