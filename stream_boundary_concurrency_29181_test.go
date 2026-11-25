// SPDX-FileCopyrightText: 2026 Tomi P. Hakala
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package resampler_test holds the streaming-boundary regression tests for
// task 29181. It is an external test package so these checks exercise only the
// exported API and cannot redeclare identifiers used by the repository's
// internal-package tests or by separately supplied grading tests.
package resampler_test

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"testing"

	resampler "github.com/tphakala/go-audio-resampler"
)

// c29181F64 is the exported surface shared by *resampler.SimpleResampler and
// the concrete value behind resampler.Resampler from resampler.New (the latter
// is recovered with a type assertion because ProcessInto is not on the
// resampler.Resampler interface).
type c29181F64 interface {
	Process(input []float64) ([]float64, error)
	ProcessInto(input, output []float64) (int, error)
	EstimateOutput(inputLen int) int
	Flush() ([]float64, error)
}

func c29181Config(in, out float64, q resampler.QualityPreset, channels int, parallel bool) *resampler.Config {
	return &resampler.Config{
		InputRate:      in,
		OutputRate:     out,
		Channels:       channels,
		EnableParallel: parallel,
		Quality:        resampler.QualitySpec{Preset: q},
	}
}

func c29181Sine(n int, rate float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = 0.5 * math.Sin(2*math.Pi*997*float64(i)/rate)
	}
	return out
}

func c29181DetInput(rng *rand.Rand, n int, rate float64) []float64 {
	out := make([]float64, n)
	p1 := rng.Float64() * 2 * math.Pi
	p2 := rng.Float64() * 2 * math.Pi
	for i := range out {
		t := float64(i) / rate
		out[i] = 0.7*math.Sin(2*math.Pi*440*t+p1) +
			0.2*math.Sin(2*math.Pi*1750*t+p2) +
			0.1*(rng.Float64()-0.5)
	}
	return out
}

func c29181AssertEqual[T float32 | float64](t *testing.T, label string, got, want []T) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: length %d != reference %d", label, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: sample %d differs: got=%v want=%v", label, i, got[i], want[i])
		}
	}
}

func c29181Chunks(t *testing.T, rate float64, total, lo, hi int, seed int64) [][]float64 {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	var chunks [][]float64
	for fed := 0; fed < total; {
		n := lo + rng.Intn(hi-lo+1)
		if fed+n > total {
			n = total - fed
		}
		chunks = append(chunks, c29181DetInput(rng, n, rate))
		fed += n
	}
	return chunks
}

// c29181CleanReference runs chunked Process plus one terminal Flush.
func c29181CleanReference(t *testing.T, r c29181F64, chunks [][]float64) []float64 {
	t.Helper()
	var out []float64
	for _, c := range chunks {
		o, err := r.Process(c)
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
		out = append(out, o...)
	}
	tail, err := r.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return append(out, tail...)
}

// c29181RetryRun injects undersized ProcessInto calls (which must fail without
// advancing state) before alternating chunks, then retries with an adequate
// buffer, and finally Flush.
func c29181RetryRun(t *testing.T, r c29181F64, chunks [][]float64) []float64 {
	t.Helper()
	var out []float64
	buf := make([]float64, 0)
	for ci, c := range chunks {
		if ci%2 == 0 {
			req := r.EstimateOutput(len(c))
			for _, bad := range []int{0, 1, req - 1} {
				if bad < 0 {
					continue
				}
				if _, err := r.ProcessInto(c, make([]float64, bad)); !errors.Is(err, resampler.ErrBufferTooSmall) {
					t.Fatalf("chunk %d bad=%d: got %v, want ErrBufferTooSmall", ci, bad, err)
				}
			}
		}
		req := r.EstimateOutput(len(c))
		if cap(buf) < req {
			buf = make([]float64, req)
		}
		n, err := r.ProcessInto(c, buf[:req])
		if err != nil {
			t.Fatalf("chunk %d retry: %v", ci, err)
		}
		if n > req {
			t.Fatalf("chunk %d: n=%d > buffer %d", ci, n, req)
		}
		out = append(out, buf[:n]...)
	}
	tail, err := r.Flush()
	if err != nil {
		t.Fatalf("final Flush: %v", err)
	}
	return append(out, tail...)
}

// A1/A7: NewEngine ProcessInto retry matches a clean one-buffer-per-chunk run,
// with failures interleaved after successful writes.
func TestContract29181_EngineProcessIntoRetry(t *testing.T) {
	cases := []struct {
		name          string
		in, out       float64
		q             resampler.QualityPreset
		total, lo, hi int
	}{
		{"48to16_medium", 48000, 16000, resampler.QualityMedium, 12000, 1, 2200},
		{"441to48_high", 44100, 48000, resampler.QualityHigh, 12000, 1, 2200},
		{"48to441_high", 48000, 44100, resampler.QualityHigh, 9000, 1, 1800},
		{"16to48_quick", 16000, 48000, resampler.QualityQuick, 9000, 1, 1800},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := c29181Chunks(t, tc.in, tc.total, tc.lo, tc.hi, int64(1337+i*17))

			ref, err := resampler.NewEngine(tc.in, tc.out, tc.q)
			if err != nil {
				t.Fatal(err)
			}
			want := c29181CleanReference(t, ref, chunks)

			retry, err := resampler.NewEngine(tc.in, tc.out, tc.q)
			if err != nil {
				t.Fatal(err)
			}
			got := c29181RetryRun(t, retry, chunks)
			c29181AssertEqual(t, "engine retry", got, want)
		})
	}
}

// A1/A7/A8: the New(config) concrete resampler's ProcessInto obeys the same
// atomic retry contract.
func TestContract29181_NewPathProcessIntoRetry(t *testing.T) {
	rates := [][2]float64{{48000, 16000}, {44100, 48000}, {48000, 44100}}
	for i, rr := range rates {
		t.Run(fmt.Sprintf("%gto%g", rr[0], rr[1]), func(t *testing.T) {
			chunks := c29181Chunks(t, rr[0], 9000, 1, 2000, int64(7331+i))

			refRaw, err := resampler.New(c29181Config(rr[0], rr[1], resampler.QualityHigh, 1, false))
			if err != nil {
				t.Fatal(err)
			}
			ref, ok := refRaw.(c29181F64)
			if !ok {
				t.Fatal("New result does not expose ProcessInto/EstimateOutput")
			}
			want := c29181CleanReference(t, ref, chunks)

			retryRaw, err := resampler.New(c29181Config(rr[0], rr[1], resampler.QualityHigh, 1, false))
			if err != nil {
				t.Fatal(err)
			}
			retry, ok := retryRaw.(c29181F64)
			if !ok {
				t.Fatal("New result does not expose ProcessInto/EstimateOutput")
			}
			got := c29181RetryRun(t, retry, chunks)
			c29181AssertEqual(t, "New-path retry", got, want)
		})
	}
}

// A1 for the float32-native NewEngineFloat32 entry point.
func TestContract29181_Float32ProcessIntoRetry(t *testing.T) {
	chunks64 := c29181Chunks(t, 44100, 9000, 1, 2000, 909)
	chunks := make([][]float32, len(chunks64))
	for i, c := range chunks64 {
		chunks[i] = make([]float32, len(c))
		for j, v := range c {
			chunks[i][j] = float32(v)
		}
	}

	ref, err := resampler.NewEngineFloat32(44100, 48000, resampler.QualityHigh)
	if err != nil {
		t.Fatal(err)
	}
	var want []float32
	for _, c := range chunks {
		o, err := ref.Process(c)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, o...)
	}
	tail, err := ref.Flush()
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, tail...)

	r, err := resampler.NewEngineFloat32(44100, 48000, resampler.QualityHigh)
	if err != nil {
		t.Fatal(err)
	}
	var got []float32
	for ci, c := range chunks {
		if ci%2 == 1 {
			_, err := r.ProcessInto(c, make([]float32, r.EstimateOutput(len(c))-1))
			if !errors.Is(err, resampler.ErrBufferTooSmall) {
				t.Fatalf("ci=%d: %v", ci, err)
			}
		}
		buf := make([]float32, r.EstimateOutput(len(c)))
		n, err := r.ProcessInto(c, buf)
		if err != nil {
			t.Fatalf("ci=%d: %v", ci, err)
		}
		got = append(got, buf[:n]...)
	}
	tail2, err := r.Flush()
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, tail2...)
	c29181AssertEqual(t, "float32 retry", got, want)
}

// A2/A8: chunked ProcessMulti followed by FlushMulti matches independent
// serial per-channel New resamplers, for both sequential and parallel configs.
func TestContract29181_FlushMultiDrainsAllChannels(t *testing.T) {
	const (
		in, out       = 48000.0, 32000.0
		chans         = 4
		total         = 12000
		chunkLo       = 300
		chunkHi       = 2400
		seed    int64 = 4242
	)

	channelInput := make([][]float64, chans)
	for ch := range chans {
		channelInput[ch] = c29181DetInput(rand.New(rand.NewSource(seed+int64(ch)*101)), total, in)
	}
	rng := rand.New(rand.NewSource(seed + 7))
	var chunks [][][]float64
	for pos := 0; pos < total; {
		n := chunkLo + rng.Intn(chunkHi-chunkLo+1)
		if pos+n > total {
			n = total - pos
		}
		c := make([][]float64, chans)
		for ch := range chans {
			c[ch] = channelInput[ch][pos : pos+n]
		}
		chunks = append(chunks, c)
		pos += n
	}

	want := make([][]float64, chans)
	wantTail := make([][]float64, chans)
	for ch := range chans {
		rr, err := resampler.New(c29181Config(in, out, resampler.QualityHigh, 1, false))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range chunks {
			o, err := rr.Process(c[ch])
			if err != nil {
				t.Fatal(err)
			}
			want[ch] = append(want[ch], o...)
		}
		tl, err := rr.Flush()
		if err != nil {
			t.Fatal(err)
		}
		wantTail[ch] = tl
	}

	for _, parallel := range []bool{false, true} {
		t.Run(fmt.Sprintf("parallel=%v", parallel), func(t *testing.T) {
			r, err := resampler.New(c29181Config(in, out, resampler.QualityHigh, chans, parallel))
			if err != nil {
				t.Fatal(err)
			}
			proc := make([][]float64, chans)
			for _, c := range chunks {
				outs, err := r.ProcessMulti(c)
				if err != nil {
					t.Fatal(err)
				}
				for ch := range chans {
					proc[ch] = append(proc[ch], outs[ch]...)
				}
			}
			mf, ok := r.(resampler.MultiFlusher)
			if !ok {
				t.Fatal("Resampler does not implement MultiFlusher")
			}
			tails, err := mf.FlushMulti()
			if err != nil {
				t.Fatal(err)
			}
			if len(tails) != chans {
				t.Fatalf("FlushMulti channels=%d want %d", len(tails), chans)
			}
			for ch, tl := range tails {
				if len(tl) == 0 {
					t.Errorf("channel %d: empty first tail after real input", ch)
				}
				all := append(append([]float64{}, proc[ch]...), tl...)
				w := append(append([]float64{}, want[ch]...), wantTail[ch]...)
				c29181AssertEqual(t, fmt.Sprintf("parallel=%v ch%d", parallel, ch), all, w)
			}

			// A3: immediate second FlushMulti is empty for every channel.
			again, err := mf.FlushMulti()
			if err != nil {
				t.Fatal(err)
			}
			for ch, s := range again {
				if len(s) != 0 {
					t.Fatalf("channel %d: second FlushMulti emitted %d repeated samples", ch, len(s))
				}
			}
		})
	}
}

// A3: repeated mono Flush (float64 and float32) and plain Flush after
// FlushMulti both return empty rather than the repeated tail.
func TestContract29181_RepeatedFlushIsEmpty(t *testing.T) {
	r64, err := resampler.NewEngine(48000, 16000, resampler.QualityHigh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r64.Process(c29181Sine(4800, 48000)); err != nil {
		t.Fatal(err)
	}
	first, err := r64.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("first float64 Flush unexpectedly empty")
	}
	if second, err := r64.Flush(); err != nil || len(second) != 0 {
		t.Fatalf("second float64 Flush len=%d err=%v, want empty/nil", len(second), err)
	}

	r32, err := resampler.NewEngineFloat32(44100, 48000, resampler.QualityMedium)
	if err != nil {
		t.Fatal(err)
	}
	s64 := c29181Sine(4410, 44100)
	in32 := make([]float32, len(s64))
	for i, v := range s64 {
		in32[i] = float32(v)
	}
	if _, err := r32.Process(in32); err != nil {
		t.Fatal(err)
	}
	if _, err := r32.Flush(); err != nil {
		t.Fatal(err)
	}
	if second, err := r32.Flush(); err != nil || len(second) != 0 {
		t.Fatalf("second float32 Flush len=%d err=%v, want empty", len(second), err)
	}

	mr, err := resampler.New(c29181Config(44100, 48000, resampler.QualityHigh, 3, false))
	if err != nil {
		t.Fatal(err)
	}
	in := [][]float64{c29181Sine(4410, 44100), c29181Sine(4410, 44100), c29181Sine(4410, 44100)}
	if _, err := mr.ProcessMulti(in); err != nil {
		t.Fatal(err)
	}
	mf := mr.(resampler.MultiFlusher)
	if _, err := mf.FlushMulti(); err != nil {
		t.Fatal(err)
	}
	second, err := mf.FlushMulti()
	if err != nil {
		t.Fatal(err)
	}
	for ch, s := range second {
		if len(s) != 0 {
			t.Fatalf("multi ch%d second FlushMulti len=%d", ch, len(s))
		}
	}
	if plain, err := mr.Flush(); err != nil || len(plain) != 0 {
		t.Fatalf("Flush after FlushMulti len=%d err=%v, want empty", len(plain), err)
	}
}

// A4: Reset after a flushed stream makes the next stream identical to a fresh
// instance, on both the New multi-channel path and NewEngine.
func TestContract29181_ResetAfterFlush(t *testing.T) {
	in := [][]float64{c29181Sine(4800, 48000), c29181Sine(4800, 48000)}
	r, err := resampler.New(c29181Config(48000, 24000, resampler.QualityHigh, 2, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProcessMulti(in); err != nil {
		t.Fatal(err)
	}
	if _, err := r.(resampler.MultiFlusher).FlushMulti(); err != nil {
		t.Fatal(err)
	}
	r.Reset()
	got, err := r.ProcessMulti(in)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := resampler.New(c29181Config(48000, 24000, resampler.QualityHigh, 2, true))
	if err != nil {
		t.Fatal(err)
	}
	want, err := fresh.ProcessMulti(in)
	if err != nil {
		t.Fatal(err)
	}
	for ch := range 2 {
		c29181AssertEqual(t, fmt.Sprintf("multi reset ch%d", ch), got[ch], want[ch])
	}

	e, err := resampler.NewEngine(48000, 44100, resampler.QualityMedium)
	if err != nil {
		t.Fatal(err)
	}
	chunk := c29181Sine(4800, 48000)
	if _, err := e.Process(chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	e.Reset()
	eg, err := e.Process(chunk)
	if err != nil {
		t.Fatal(err)
	}
	ef, err := resampler.NewEngine(48000, 44100, resampler.QualityMedium)
	if err != nil {
		t.Fatal(err)
	}
	ew, err := ef.Process(chunk)
	if err != nil {
		t.Fatal(err)
	}
	c29181AssertEqual(t, "engine reset", eg, ew)
}

// A5: concurrent same-instance ProcessInto callers must stay race-free (run
// under -race), never violate the buffer contract, and Reset afterwards must
// restore a fresh stream.
func TestContract29181_ConcurrentProcessInto(t *testing.T) {
	r, err := resampler.NewEngine(48000, 16000, resampler.QualityMedium)
	if err != nil {
		t.Fatal(err)
	}
	chunk := c29181Sine(333, 48000)

	const gN, iters = 8, 60
	var wg sync.WaitGroup
	for g := range gN {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			buf := make([]float64, r.EstimateOutput(len(chunk)))
			for i := range iters {
				if (g+i)%7 == 0 {
					if _, err := r.ProcessInto(chunk, make([]float64, 1)); !errors.Is(err, resampler.ErrBufferTooSmall) {
						t.Errorf("expected ErrBufferTooSmall, got %v", err)
					}
					continue
				}
				n, err := r.ProcessInto(chunk, buf)
				if err != nil {
					t.Errorf("ProcessInto: %v", err)
					return
				}
				if n < 0 || n > len(buf) {
					t.Errorf("n=%d outside cap %d", n, len(buf))
					return
				}
			}
		}(g)
	}
	wg.Wait()

	r.Reset()
	got, err := r.Process(chunk)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := resampler.NewEngine(48000, 16000, resampler.QualityMedium)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fresh.Process(chunk)
	if err != nil {
		t.Fatal(err)
	}
	c29181AssertEqual(t, "post-concurrency reset", got, want)
}

// A5: concurrent same-instance ProcessMulti callers on a parallel-enabled
// multi-channel resampler must stay race-free and preserve channel count.
func TestContract29181_ConcurrentProcessMulti(t *testing.T) {
	r, err := resampler.New(c29181Config(44100, 48000, resampler.QualityMedium, 2, true))
	if err != nil {
		t.Fatal(err)
	}
	in := [][]float64{c29181Sine(257, 44100), c29181Sine(257, 44100)}

	const gN, iters = 8, 40
	var wg sync.WaitGroup
	for range gN {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iters {
				out, err := r.ProcessMulti(in)
				if err != nil {
					t.Errorf("ProcessMulti: %v", err)
					return
				}
				if len(out) != 2 {
					t.Errorf("channels=%d want 2", len(out))
					return
				}
			}
		}()
	}
	wg.Wait()
}

// Terminal-flush lifecycle on the New path without an explicit Reset: after
// FlushMulti the stages are fresh, so a new stream matches a new instance.
func TestContract29181_FlushAllowsFreshStreamWithoutReset(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(fmt.Sprintf("parallel=%v", parallel), func(t *testing.T) {
			r, err := resampler.New(c29181Config(44100, 48000, resampler.QualityHigh, 2, parallel))
			if err != nil {
				t.Fatal(err)
			}
			first := [][]float64{c29181Sine(2000, 44100), c29181Sine(2000, 44100)}
			if _, err := r.ProcessMulti(first); err != nil {
				t.Fatal(err)
			}
			if _, err := r.(resampler.MultiFlusher).FlushMulti(); err != nil {
				t.Fatal(err)
			}
			next := [][]float64{c29181Sine(3000, 44100), c29181Sine(3000, 44100)}
			got, err := r.ProcessMulti(next)
			if err != nil {
				t.Fatal(err)
			}
			tail, err := r.(resampler.MultiFlusher).FlushMulti()
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := resampler.New(c29181Config(44100, 48000, resampler.QualityHigh, 2, false))
			if err != nil {
				t.Fatal(err)
			}
			want, err := fresh.ProcessMulti(next)
			if err != nil {
				t.Fatal(err)
			}
			wt, err := fresh.(resampler.MultiFlusher).FlushMulti()
			if err != nil {
				t.Fatal(err)
			}
			for ch := range 2 {
				g := append(append([]float64{}, got[ch]...), tail[ch]...)
				w := append(append([]float64{}, want[ch]...), wt[ch]...)
				c29181AssertEqual(t, fmt.Sprintf("ch%d reuse", ch), g, w)
			}
		})
	}
}
