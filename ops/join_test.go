// Copyright 2018 The ZikiChombo Authors. All rights reserved.  Use of this source
// code is governed by a license that can be found in the License file.

package ops

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zikichombo/sound"
	"github.com/zikichombo/sound/freq"
	"github.com/zikichombo/sound/sndbuf"
)

// drainJoin reads src until io.EOF using buffers of blockFrames frames
// and returns every frame collected in deinterleaved layout: result[c][f].
func drainJoin(t *testing.T, src sound.Source, nChans, blockFrames int) ([][]float64, error) {
	t.Helper()
	out := make([][]float64, nChans)
	dst := make([]float64, blockFrames*nChans)
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
		if n <= 0 {
			t.Fatalf("non-EOF call returned %d frames", n)
		}
		if n > blockFrames {
			t.Fatalf("returned %d frames for buffer holding %d", n, blockFrames)
		}
		for c := 0; c < nChans; c++ {
			out[c] = append(out[c], dst[c*blockFrames:c*blockFrames+n]...)
		}
	}
}

// expectedValue gives the value channel c is expected to hold at frame f
// for the uneven-length join fixtures.  Each channel c is a ramp of
// (c+1); frames past its length must be zero.
func expectedValue(c, f, length int) float64 {
	if f >= length {
		return 0
	}
	return float64(c + 1)
}

// TestJoin keeps the original equal-length mono coverage and verifies the
// deinterleaved "every frame has every channel" invariant over several
// receive block sizes.
func TestJoin(t *testing.T) {
	ones := make([]float64, 130)
	for i := range ones {
		ones[i] = 1
	}
	twos := make([]float64, 130)
	for i := range twos {
		twos[i] = 2
	}
	one := sndbuf.FromSlice(ones, 44100*freq.Hertz)
	two := sndbuf.FromSlice(twos, 44100*freq.Hertz)
	src, err := Join(one, two)
	if err != nil {
		t.Fatal(err)
	}

	for _, block := range []int{1, 3, 7, 32, 64, 129, 130, 256} {
		d := make([]float64, 2*block)
		ttl := 0
		for {
			n, e := src.Receive(d)
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatal(e)
			}
			ttl += n
			for i := 0; i < n; i++ {
				if d[i] != 1.0 {
					t.Errorf("block %d frame %d ch0: got %f want 1.0", block, i, d[i])
				}
			}
			for i := 0; i < n; i++ {
				if d[block+i] != 2.0 {
					t.Errorf("block %d frame %d ch1: got %f want 2.0", block, i, d[block+i])
				}
			}
		}
		if ttl != 130 {
			t.Errorf("block %d: ttl %d not 130", block, ttl)
		}
		if err := src.Close(); err != nil {
			t.Fatal(err)
		}
		// Recreate because Close on sndbuf is harmless but position is
		// exhausted anyway.
		one = sndbuf.FromSlice(append([]float64(nil), ones...), 44100*freq.Hertz)
		two = sndbuf.FromSlice(append([]float64(nil), twos...), 44100*freq.Hertz)
		src, _ = Join(one, two)
	}
}

// TestJoinUnevenLength covers the core defect: sources ending at
// different times.  The shorter channel is zero padded past its end,
// valid samples are never overwritten, the frame count equals the
// longest source, and reads continue from the correct position.
func TestJoinUnevenLength(t *testing.T) {
	lengths := []int{5, 11, 3} // frames per source, longest is 11
	const nChans = 3

	build := func() sound.Source {
		srcs := make([]sound.Source, nChans)
		for c, ln := range lengths {
			dat := make([]float64, ln)
			for f := range dat {
				dat[f] = float64(c + 1)
			}
			srcs[c] = sndbuf.FromSlice(dat, freq.Hertz)
		}
		src, err := Join(srcs...)
		if err != nil {
			t.Fatal(err)
		}
		return src
	}

	for _, block := range []int{1, 2, 3, 5, 11, 12, 32} {
		src := build()
		got, err := drainJoin(t, src, nChans, block)
		if err != nil {
			t.Fatalf("block %d: %v", block, err)
		}
		wantFrames := 11
		for c := 0; c < nChans; c++ {
			if len(got[c]) != wantFrames {
				t.Fatalf("block %d ch%d: got %d frames, want %d", block, c, len(got[c]), wantFrames)
			}
			for f := 0; f < wantFrames; f++ {
				want := expectedValue(c, f, lengths[c])
				if got[c][f] != want {
					t.Errorf("block %d ch%d frame %d: got %v want %v", block, c, f, got[c][f], want)
				}
			}
		}
		// The next call must be a clean EOF.
		n, err := src.Receive(make([]float64, nChans))
		if err != io.EOF || n != 0 {
			t.Errorf("block %d: post-exhaustion read got (%d, %v), want (0, EOF)", block, n, err)
		}
	}
}

// TestJoinMultiChannel exercises sources that themselves carry several
// channels; their channels are concatenated in order.
func TestJoinMultiChannel(t *testing.T) {
	const srcChans = 2
	const frames = 7
	const nSrcs = 2
	const nChans = srcChans * nSrcs

	a := make([]float64, frames*srcChans) // interleaved storage for FromSliceChans
	b := make([]float64, frames*srcChans)
	for f := 0; f < frames; f++ {
		for c := 0; c < srcChans; c++ {
			a[f*srcChans+c] = float64(10 + c)
			b[f*srcChans+c] = float64(20 + c)
		}
	}
	sA := sndbuf.FromSliceChans(a, srcChans, freq.Hertz)
	sB := sndbuf.FromSliceChans(b, srcChans, freq.Hertz)
	src, err := Join(sA, sB)
	if err != nil {
		t.Fatal(err)
	}
	if src.Channels() != nChans {
		t.Fatalf("Channels() = %d, want %d", src.Channels(), nChans)
	}
	got, err := drainJoin(t, src, nChans, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := [nChans]float64{10, 11, 20, 21}
	for c := 0; c < nChans; c++ {
		if len(got[c]) != frames {
			t.Fatalf("ch%d frames = %d, want %d", c, len(got[c]), frames)
		}
		for f := 0; f < frames; f++ {
			if got[c][f] != want[c] {
				t.Errorf("ch%d frame %d = %v, want %v", c, f, got[c][f], want[c])
			}
		}
	}
}

// TestJoinMultiChannelUneven combines multi-channel sources of different
// lengths.  The shorter source's channels are zero padded past its end
// while staying frame-aligned with the still-running source.
func TestJoinMultiChannelUneven(t *testing.T) {
	const srcChans = 2

	mk := func(frames int, base float64) sound.Source {
		dat := make([]float64, frames*srcChans) // interleaved storage
		for f := 0; f < frames; f++ {
			for c := 0; c < srcChans; c++ {
				dat[f*srcChans+c] = base + float64(c)
			}
		}
		return sndbuf.FromSliceChans(dat, srcChans, freq.Hertz)
	}

	short := mk(3, 10) // 3 frames, values 10,11
	long := mk(7, 20)  // 7 frames, values 20,21
	src, err := Join(short, long)
	if err != nil {
		t.Fatal(err)
	}
	const nChans = 4 // two stereo sources
	got, err := drainJoin(t, src, nChans, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Output channels: short ch0, short ch1, long ch0, long ch1.
	wantLen := []int{7, 7, 7, 7}
	for c := 0; c < nChans; c++ {
		if len(got[c]) != wantLen[c] {
			t.Fatalf("ch%d length = %d, want %d", c, len(got[c]), wantLen[c])
		}
	}
	for f := 0; f < 7; f++ {
		want := [4]float64{0, 0, 20, 21}
		if f < 3 {
			want[0], want[1] = 10, 11
		}
		for c := 0; c < nChans; c++ {
			if got[c][f] != want[c] {
				t.Errorf("frame %d ch%d = %v, want %v", f, c, got[c][f], want[c])
			}
		}
	}
}

// TestJoinAlignment rejects buffers whose length is not a multiple of the
// output channel count.
func TestJoinAlignment(t *testing.T) {
	srcs := []sound.Source{
		sndbuf.FromSlice(make([]float64, 4), freq.Hertz),
		sndbuf.FromSlice(make([]float64, 4), freq.Hertz),
	}
	src, err := Join(srcs...)
	if err != nil {
		t.Fatal(err)
	}
	n, err := src.Receive(make([]float64, 3)) // not divisible by 2
	if err != sound.ErrChannelAlignment || n != 0 {
		t.Fatalf("got (%d, %v), want (0, ErrChannelAlignment)", n, err)
	}
}

// TestJoinCloseAllAndAggregate verifies Close reaches every underlying
// source and aggregates every close error.
func TestJoinCloseAllAndAggregate(t *testing.T) {
	m1 := newMockSource([]float64{1, 2, 3}, 1, freq.Hertz)
	m2 := newMockSource([]float64{4, 5, 6}, 1, freq.Hertz)
	m3 := newMockSource([]float64{7, 8, 9}, 1, freq.Hertz)
	m2.closeErr = errMockClose
	m3.closeErr = errors.New("mock: another close failure")

	src, err := Join(m1, m2, m3)
	if err != nil {
		t.Fatal(err)
	}
	err = src.Close()
	if err == nil {
		t.Fatal("Close returned nil, want aggregated error")
	}
	for _, m := range []*mockSource{m1, m2, m3} {
		if !m.closed {
			t.Errorf("underlying source %p was not closed", m)
		}
	}
	// Both failing sources must be represented.
	msg := err.Error()
	if !strings.Contains(msg, "close failed") || !strings.Contains(msg, "another close failure") {
		t.Errorf("aggregated error %q does not mention both failures", msg)
	}

	// Re-entry after close: EOF, no data, and no further source reads.
	before := m1.reads
	n, rerr := src.Receive(make([]float64, 3))
	if rerr != io.EOF || n != 0 {
		t.Errorf("post-close Receive got (%d, %v), want (0, EOF)", n, rerr)
	}
	if m1.reads != before {
		t.Error("post-close Receive read from an underlying source")
	}
	// Closing again is idempotent and returns nil.
	if err := src.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestJoinRecvError ensures a non-EOF error from a source is surfaced and
// not swallowed or turned into zero padding.
func TestJoinRecvError(t *testing.T) {
	good := newMockSource([]float64{1, 2, 3, 4}, 1, freq.Hertz)
	bad := newMockSource([]float64{5, 6, 7, 8}, 1, freq.Hertz)
	boom := errors.New("mock: receive boom")
	bad.recvErr = boom
	bad.failAfter = 1

	src, err := Join(good, bad)
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]float64, 4) // request 4 frames; bad fails on its 2nd read
	saw := false
	for {
		_, rerr := src.Receive(dst)
		if rerr == io.EOF {
			break
		}
		if rerr == boom {
			saw = true
			break
		}
		if rerr != nil {
			t.Fatalf("unexpected error %v", rerr)
		}
	}
	if !saw {
		t.Error("underlying receive error was not propagated")
	}
}

// TestJoinConcurrentReads exercises many goroutines reading the same join
// concurrently with varied block sizes.  Run with -race to detect data
// races.  Reads are serialized internally, so every available frame is
// delivered exactly once and every returned frame carries all channels
// from the same point in time.
func TestJoinConcurrentReads(t *testing.T) {
	const nSrcs = 3
	const frames = 60
	srcs := make([]sound.Source, nSrcs)
	for c := 0; c < nSrcs; c++ {
		dat := make([]float64, frames)
		for f := range dat {
			dat[f] = float64(c + 1)
		}
		srcs[c] = newMockSource(dat, 1, freq.Hertz)
	}
	src, err := Join(srcs...)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	totals := make([]int, nSrcs)
	var failCount int

	for g := 0; g < 8; g++ {
		block := 1 + g*3
		wg.Add(1)
		go func(block int) {
			defer wg.Done()
			dst := make([]float64, block*nSrcs)
			local := make([]int, nSrcs)
			for {
				n, rerr := src.Receive(dst)
				if rerr == io.EOF {
					mu.Lock()
					for c := 0; c < nSrcs; c++ {
						totals[c] += local[c]
					}
					mu.Unlock()
					return
				}
				if rerr != nil {
					mu.Lock()
					failCount++
					mu.Unlock()
					return
				}
				// Every returned frame must carry all channels and the
				// channel values must match the expected ramp.
				for c := 0; c < nSrcs; c++ {
					for f := 0; f < n; f++ {
						if dst[c*block+f] != float64(c+1) {
							mu.Lock()
							failCount++
							mu.Unlock()
							return
						}
					}
					local[c] += n
				}
			}
		}(block)
	}

	wg.Wait()

	if failCount != 0 {
		t.Fatalf("concurrent readers recorded %d failures", failCount)
	}
	// Exactly `frames` frames exist per channel, delivered once each.
	for c := 0; c < nSrcs; c++ {
		if totals[c] != frames {
			t.Errorf("ch%d collected %d frames, want exactly %d", c, totals[c], frames)
		}
	}
	// After natural exhaustion, every re-entrant read is a clean EOF.
	for i := 0; i < 5; i++ {
		n, rerr := src.Receive(make([]float64, nSrcs))
		if rerr != io.EOF || n != 0 {
			t.Fatalf("post-EOF read %d got (%d, %v), want (0, EOF)", i, n, rerr)
		}
	}
}

// TestJoinCloseWhileBlocked ensures Close makes progress and unblocks a
// Receive stuck inside a blocking underlying source, after which reads
// return io.EOF.
func TestJoinCloseWhileBlocked(t *testing.T) {
	blocked := newMockSource([]float64{1, 2, 3, 4}, 1, freq.Hertz)
	release := make(chan struct{})
	blocked.block = release
	other := newMockSource([]float64{5, 6, 7, 8}, 1, freq.Hertz)

	src, err := Join(blocked, other)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var n int
	var rerr error
	go func() {
		n, rerr = src.Receive(make([]float64, 4))
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	if cerr := src.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Receive did not return after Close")
	}
	if rerr != io.EOF || n != 0 {
		t.Errorf("blocked read after close got (%d, %v), want (0, EOF)", n, rerr)
	}
	if !blocked.closed || !other.closed {
		t.Error("Close did not close every underlying source")
	}
}

// TestJoinSinglePassthrough guarantees the legacy single-source behavior:
// Join returns the exact source it was given.
func TestJoinSinglePassthrough(t *testing.T) {
	only := sndbuf.FromSlice([]float64{1, 2, 3}, freq.Hertz)
	src, err := Join(only)
	if err != nil {
		t.Fatal(err)
	}
	if src != sound.Source(only) {
		t.Fatal("single-source Join did not pass the source through unchanged")
	}
}
