//go:build !ios && !android && (amd64 || arm64)

package ffgo

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestEncoderCloseIdempotent verifies that closing an encoder repeatedly is a
// no-op after the first call: the trailer is written once and the same result
// is returned every time.
func TestEncoderCloseIdempotent(t *testing.T) {
	if !requireFFmpeg(t) {
		return
	}
	out := filepath.Join(t.TempDir(), "out.mp4")
	enc, err := NewEncoder(out, EncoderConfig{
		Width:     64,
		Height:    64,
		FrameRate: 10,
		BitRate:   200000,
		GOPSize:   5,
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}

	for i := 0; i < 10; i++ {
		frame := hlsTestFrame(t, 64, 64, i)
		err := enc.WriteFrame(frame)
		_ = FrameFree(&frame)
		if err != nil {
			_ = enc.Close()
			t.Fatalf("WriteFrame(%d): %v", i, err)
		}
	}

	if err := enc.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Repeated closes must return the same (nil) result and not re-write the
	// trailer or double free resources.
	for i := 0; i < 3; i++ {
		if err := enc.Close(); err != nil {
			t.Errorf("Close #%d after successful close: %v", i+2, err)
		}
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("output missing: %v", err)
	}
	if info.Size() == 0 {
		t.Error("output is empty")
	}

	// A write after close must fail deterministically.
	frame := hlsTestFrame(t, 64, 64, 99)
	err = enc.WriteFrame(frame)
	_ = FrameFree(&frame)
	if err == nil {
		t.Error("WriteFrame after Close succeeded")
	}
}

// TestEncoderWriteCloseConcurrent closes the encoder while another goroutine
// is still writing frames. It must not deadlock, must finalize exactly once,
// and both closers must observe the same result.
func TestEncoderWriteCloseConcurrent(t *testing.T) {
	if !requireFFmpeg(t) {
		return
	}
	out := filepath.Join(t.TempDir(), "out.mp4")
	enc, err := NewEncoder(out, EncoderConfig{
		Width:     64,
		Height:    64,
		FrameRate: 10,
		BitRate:   200000,
		GOPSize:   5,
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.WriteHeader(); err != nil {
		_ = enc.Close()
		t.Fatalf("WriteHeader: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 500; i++ {
			frame, ok := hlsTestFrameNoAssert(64, 64, i)
			if !ok {
				return
			}
			err := enc.WriteFrame(frame)
			_ = FrameFree(&frame)
			if err != nil {
				return
			}
		}
	}()

	const closers = 2
	results := make([]error, closers)
	for i := 0; i < closers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			time.Sleep(time.Duration(idx) * time.Millisecond)
			results[idx] = enc.Close()
		}(i)
	}

	close(start)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Encoder Close/write deadlocked")
	}

	if (results[0] == nil) != (results[1] == nil) {
		t.Errorf("inconsistent Close results: %v vs %v", results[0], results[1])
	}
	if results[0] != nil && results[1] != nil && results[0].Error() != results[1].Error() {
		t.Errorf("Close results differ: %q vs %q", results[0], results[1])
	}
}
