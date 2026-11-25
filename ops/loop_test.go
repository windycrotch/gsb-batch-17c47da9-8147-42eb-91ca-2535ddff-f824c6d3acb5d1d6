// Copyright 2018 The ZikiChombo Authors. All rights reserved.  Use of this source
// code is governed by a license that can be found in the License file.

package ops

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/zikichombo/sound/freq"
	"github.com/zikichombo/sound/sndbuf"
)

// drainLoop reads src to EOF using blocks of blockFrames frames and
// returns the deinterleaved per-channel frame slices.
func drainLoop(t *testing.T, src interface {
	Receive([]float64) (int, error)
	Channels() int
}, blockFrames int) ([][]float64, error) {
	nC := src.Channels()
	out := make([][]float64, nC)
	dst := make([]float64, blockFrames*nC)
	for {
		n, err := src.Receive(dst)
		if err == io.EOF {
			if n != 0 {
				t.Fatalf("EOF returned with %d frames", n)
			}
			return out, nil
		}
		if err != nil {
			return out, err
		}
		if n <= 0 || n > blockFrames {
			t.Fatalf("bad frame count %d (block %d)", n, blockFrames)
		}
		for c := 0; c < nC; c++ {
			out[c] = append(out[c], dst[c*blockFrames:c*blockFrames+n]...)
		}
	}
}

// checkLooped verifies that got[c] equals srcFrames repeated plays times,
// with the given per-frame pattern value(c, frameInPlay).
func checkLooped(t *testing.T, got [][]float64, nC, srcFrames, plays int, value func(c, f int) float64) {
	t.Helper()
	want := srcFrames * plays
	for c := 0; c < nC; c++ {
		if len(got[c]) != want {
			t.Fatalf("ch%d got %d frames, want %d", c, len(got[c]), want)
		}
		for f := 0; f < want; f++ {
			wantV := value(c, f%srcFrames)
			if got[c][f] != wantV {
				t.Errorf("ch%d frame %d = %v, want %v", c, f, got[c][f], wantV)
			}
		}
	}
}

func monoPattern() ([]float64, int) {
	d := make([]float64, 10)
	for i := 0; i < 5; i++ {
		d[i] = -1.0
	}
	for i := 5; i < 10; i++ {
		d[i] = 1.0
	}
	return d, 10
}

func monoValue(c, f int) float64 {
	if f < 5 {
		return -1.0
	}
	return 1.0
}

func TestLoopMono(t *testing.T) {
	d, srcFrames := monoPattern()
	src := sndbuf.FromSlice(d, freq.Hertz)
	loop := Loop(src, 3)

	// A buffer large enough to hold more than all plays (30 frames).
	e := make([]float64, 40)
	n, err := loop.Receive(e)
	if err != nil {
		t.Fatal(err)
	}
	if n != 30 {
		t.Fatalf("got %d frames not 30 (no play may be dropped)", n)
	}
	for i := 0; i < 30; i++ {
		want := monoValue(0, i%srcFrames)
		if e[i] != want {
			t.Errorf("frame %d got %f want %f", i, e[i], want)
		}
	}
	// All finite plays delivered: the next receive is a stable EOF.
	if n, err := loop.Receive(make([]float64, 10)); err != io.EOF || n != 0 {
		t.Errorf("terminal read got (%d, %v), want (0, EOF)", n, err)
	}
}

func TestLoopMonoSmallRecv(t *testing.T) {
	d, srcFrames := monoPattern()
	src := sndbuf.FromSlice(d, freq.Hertz)
	loop := Loop(src, 3)

	got, err := drainLoop(t, loop, 1)
	if err != nil {
		t.Fatal(err)
	}
	checkLooped(t, got, 1, srcFrames, 3, monoValue)
}

// TestLoopMonoBlockSizes sweeps receive block sizes, including sizes that
// straddle play boundaries and the exact boundary, to prove no frame is
// lost or duplicated on rewind.
func TestLoopMonoBlockSizes(t *testing.T) {
	for _, block := range []int{1, 2, 3, 7, 10, 11, 15, 25, 30, 31, 64} {
		d, srcFrames := monoPattern()
		src := sndbuf.FromSlice(d, freq.Hertz)
		loop := Loop(src, 3)
		got, err := drainLoop(t, loop, block)
		if err != nil {
			t.Fatalf("block %d: %v", block, err)
		}
		checkLooped(t, got, 1, srcFrames, 3, monoValue)
	}
}

func TestLoopStereo(t *testing.T) {
	const srcFrames = 10
	d := make([]float64, 2*srcFrames)
	for i := 0; i < 5; i++ {
		d[2*i] = -1.0
		d[2*i+1] = -2.0
	}
	for i := 5; i < 10; i++ {
		d[2*i] = 1.0
		d[2*i+1] = 2.0
	}
	for _, block := range []int{1, 3, 7, 10, 13, 25, 30, 40} {
		d2 := make([]float64, 2*srcFrames)
		copy(d2, d)
		s := sndbuf.FromSliceChans(d2, 2, freq.Hertz)
		lp := Loop(s, 3)
		got, err := drainLoop(t, lp, block)
		if err != nil {
			t.Fatalf("block %d: %v", block, err)
		}
		val := func(c, f int) float64 {
			neg := -1.0
			pos := 1.0
			if c == 1 {
				neg, pos = -2.0, 2.0
			}
			if f < 5 {
				return neg
			}
			return pos
		}
		checkLooped(t, got, 2, srcFrames, 3, val)
	}
}

// TestLoopZeroAndOne covers the n == 0 (immediate EOF) and n == 1
// (single play, no rewind) edge cases.
func TestLoopZeroAndOne(t *testing.T) {
	d, srcFrames := monoPattern()

	zero := Loop(sndbuf.FromSlice(append([]float64(nil), d...), freq.Hertz), 0)
	n, err := zero.Receive(make([]float64, 10))
	if err != io.EOF || n != 0 {
		t.Errorf("n=0 got (%d, %v), want (0, EOF)", n, err)
	}

	one := Loop(sndbuf.FromSlice(append([]float64(nil), d...), freq.Hertz), 1)
	got, err := drainLoop(t, one, 3)
	if err != nil {
		t.Fatal(err)
	}
	checkLooped(t, got, 1, srcFrames, 1, monoValue)
}

// TestLoopExactBoundaryRewind forces a play to end precisely on a receive
// boundary so the next call starts at a fresh, rewound play.  This is the
// off-by-one the original implementation got wrong.
func TestLoopExactBoundaryRewind(t *testing.T) {
	d, srcFrames := monoPattern()
	src := sndbuf.FromSlice(d, freq.Hertz)
	loop := Loop(src, 3)

	buf := make([]float64, srcFrames)
	for play := 0; play < 3; play++ {
		n, err := loop.Receive(buf)
		if err != nil {
			t.Fatalf("play %d: %v", play, err)
		}
		if n != srcFrames {
			t.Fatalf("play %d got %d frames, want %d", play, n, srcFrames)
		}
		for f := 0; f < srcFrames; f++ {
			if buf[f] != monoValue(0, f) {
				t.Errorf("play %d frame %d = %v, want %v", play, f, buf[f], monoValue(0, f))
			}
		}
	}
	if n, err := loop.Receive(buf); err != io.EOF || n != 0 {
		t.Errorf("post-loop got (%d, %v), want (0, EOF)", n, err)
	}
}

// TestLoopInfiniteClose verifies an infinite loop keeps producing correct,
// non-duplicated frames and stops promptly once Close is called, after
// which reads return EOF.
func TestLoopInfiniteClose(t *testing.T) {
	const srcFrames = 6
	dat := make([]float64, srcFrames)
	for i := range dat {
		dat[i] = float64(i)
	}
	m := newMockSource(append([]float64(nil), dat...), 1, freq.Hertz)
	loop := Loop(m, -1)

	// Read enough to span several plays with an awkward block size.
	const plays = 4
	buf := make([]float64, 4)
	var collected []float64
	wantTotal := srcFrames * plays
	for len(collected) < wantTotal {
		n, err := loop.Receive(buf)
		if err != nil {
			t.Fatal(err)
		}
		collected = append(collected, buf[:n]...)
	}
	for i := 0; i < wantTotal; i++ {
		if collected[i] != float64(i%srcFrames) {
			t.Errorf("frame %d = %v, want %v", i, collected[i], float64(i%srcFrames))
		}
	}

	if err := loop.Close(); err != nil {
		t.Fatal(err)
	}
	if !m.closed {
		t.Error("Close did not close the underlying source")
	}
	n, err := loop.Receive(make([]float64, 4))
	if err != io.EOF || n != 0 {
		t.Errorf("post-close got (%d, %v), want (0, EOF)", n, err)
	}
}

// TestLoopInfiniteCloseWhileBlocked makes the underlying source block so
// that an in-flight Receive is stuck, then closes the loop and requires
// the blocked read to return promptly.
func TestLoopInfiniteCloseWhileBlocked(t *testing.T) {
	const srcFrames = 4
	dat := make([]float64, srcFrames)
	m := newMockSource(dat, 1, freq.Hertz)
	release := make(chan struct{})
	m.block = release
	loop := Loop(m, -1)

	done := make(chan struct{})
	var n int
	var err error
	go func() {
		n, err = loop.Receive(make([]float64, srcFrames))
		close(done)
	}()

	// Give the goroutine time to enter the blocked Receive.
	time.Sleep(20 * time.Millisecond)
	// Closing must unblock the stuck reader.  Release is also closed to
	// cover sources whose Close does not interrupt in-flight reads.
	if cerr := loop.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Receive did not return after Close")
	}
	if err != io.EOF || n != 0 {
		t.Errorf("blocked read after close got (%d, %v), want (0, EOF)", n, err)
	}
}

// TestLoopRecvError ensures a non-EOF error from the underlying source is
// returned rather than swallowed.
func TestLoopRecvError(t *testing.T) {
	boom := errors.New("mock: loop receive boom")
	m := newMockSource([]float64{1, 2, 3}, 1, freq.Hertz)
	m.recvErr = boom
	m.failAfter = 1
	loop := Loop(m, 5)

	dst := make([]float64, 2)
	saw := false
	for i := 0; i < 10; i++ {
		_, err := loop.Receive(dst)
		if err == boom {
			saw = true
			break
		}
		if err != nil {
			t.Fatalf("unexpected error %v", err)
		}
	}
	if !saw {
		t.Error("underlying receive error was swallowed")
	}
}

// TestLoopSeekError proves a failed rewind is reported and the loop does
// not keep reading from the stale, exhausted position.
func TestLoopSeekError(t *testing.T) {
	seekBoom := errors.New("mock: seek boom")
	m := newMockSource([]float64{1, 2, 3}, 1, freq.Hertz)
	m.seekErr = seekBoom
	loop := Loop(m, 3)

	// Request more frames than one play holds so a rewind is attempted.
	dst := make([]float64, 10)
	var saw error
	for i := 0; i < 6; i++ {
		_, err := loop.Receive(dst)
		if err != nil {
			saw = err
			break
		}
	}
	if saw != seekBoom {
		t.Fatalf("got %v, want seek error %v", saw, seekBoom)
	}
	pos := m.Pos()
	// A subsequent read must not silently advance from the old position.
	_, err := loop.Receive(dst)
	if err == nil {
		t.Error("read after failed seek returned no error")
	}
	if m.Pos() != pos {
		t.Errorf("position changed after failed seek: %d -> %d", pos, m.Pos())
	}
}

// TestLoopConcurrentReads serializes concurrent Receive calls and checks
// that the finite number of frames is delivered exactly once (no loss,
// no duplication), and that every call returns coherent frame counts.
// Content correctness across the loop is covered by the deterministic
// single-goroutine tests above; here goroutines share one source, so no
// single goroutine sees a contiguous sequence.
func TestLoopConcurrentReads(t *testing.T) {
	const srcFrames = 8
	const plays = 3
	dat := make([]float64, srcFrames)
	for i := range dat {
		dat[i] = float64(i)
	}
	m := newMockSource(append([]float64(nil), dat...), 1, freq.Hertz)
	loop := Loop(m, plays)

	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	var failCount int
	for g := 0; g < 6; g++ {
		block := 1 + g*2
		wg.Add(1)
		go func(block int) {
			defer wg.Done()
			dst := make([]float64, block)
			local := 0
			for {
				n, err := loop.Receive(dst)
				if err == io.EOF {
					mu.Lock()
					total += local
					mu.Unlock()
					return
				}
				if err != nil {
					mu.Lock()
					failCount++
					mu.Unlock()
					return
				}
				if n <= 0 || n > block {
					mu.Lock()
					failCount++
					mu.Unlock()
					return
				}
				// Every frame value must be a valid pattern index.
				for i := 0; i < n; i++ {
					v := dst[i]
					if v < 0 || v >= srcFrames || v != float64(int(v)) {
						mu.Lock()
						failCount++
						mu.Unlock()
						return
					}
				}
				local += n
			}
		}(block)
	}
	wg.Wait()
	if failCount != 0 {
		t.Fatalf("concurrent reads had %d failures", failCount)
	}
	if total != srcFrames*plays {
		t.Errorf("total frames %d, want %d", total, srcFrames*plays)
	}
}
