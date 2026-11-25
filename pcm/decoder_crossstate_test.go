package pcm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	flac "github.com/tphakala/go-flac"
)

// This file pins the state-transition contracts of a pooled Decoder: every Reset
// must behave like a fresh NewDecoder (options from that call only, defaults
// restored for everything not passed), a failed Reset poisons the decoder until
// another Reset, argument/capability seek failures must not touch readable state,
// and once a seek passes those checks any later failure must stay visible to Read
// and WriteTo until a successful seek or Reset clears it. All streams are built
// locally with the package's own encoder.

// crossStream is one self-made FLAC stream plus the facts tests assert on.
type crossStream struct {
	name    string
	cfg     Config
	samples int
	data    []byte
	want    []byte // native interleaved PCM a full forward decode must produce
}

func buildCrossStreams(t *testing.T) []crossStream {
	t.Helper()
	shapes := []struct {
		name            string
		ch, bits, nSamp int
	}{
		{"s16stereo", 2, 16, 3*encoderBlockSize + 77},
		{"s24mono", 1, 24, 2*encoderBlockSize + 4095},
		{"s8quad", 4, 8, 2 * encoderBlockSize},
		{"s12stereo", 2, 12, encoderBlockSize + 1234},
	}
	out := make([]crossStream, 0, len(shapes))
	for _, s := range shapes {
		cfg := Config{SampleRate: 44100, BitDepth: s.bits, Channels: s.ch, CompressionLevel: 5}
		// In-range samples only: for sub-byte depths (12 bits) arbitrary bytes would
		// overflow the sample range and be truncated by the encoder, making a lossy
		// round trip. genInRange masks and sign-extends to the exact depth.
		vals := genInRange(s.bits, s.ch, s.nSamp)
		data := encodeFLAC(t, cfg, packLE(vals, s.bits))
		d, err := NewDecoder(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("reference NewDecoder(%s): %v", s.name, err)
		}
		want, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("reference decode(%s): %v", s.name, err)
		}
		out = append(out, crossStream{
			name: s.name, cfg: cfg, samples: s.nSamp, data: data, want: want,
		})
	}
	return out
}

// TestCrossResetOptionsReDerivedPerCall drives one pooled decoder through many
// streams of different bit depths and per-call option sets, asserting after every
// Reset that Info, OutputBitDepth, tags, MD5 policy and seekability match a fresh
// decoder and that the emitted PCM is byte-identical to it. Unpassed options must
// revert to defaults: a SkipMD5Verification/WithConvertTo call followed by a plain
// Reset must verify MD5 again and emit native-width samples again.
func TestCrossResetOptionsReDerivedPerCall(t *testing.T) {
	streams := buildCrossStreams(t)

	// A tagged stream and a corrupted-MD5 stream let the test observe policy, not
	// just bytes.
	taggedCfg := Config{SampleRate: 44100, BitDepth: 16, Channels: 2, CompressionLevel: 5,
		Tags: []flac.Tag{{Name: tagTitle, Value: "tagged"}}, Vendor: "vend-or"}
	tagged := makeStream(t, taggedCfg, encoderBlockSize+500)
	badMD5 := corruptMD5(streams[0].data)

	type step struct {
		stream []byte
		opts   []DecoderOption
		// expectations
		outBits   int   // OutputBitDepth after Reset
		wantErr   bool  // Reset must fail
		decodeErr error // nil: full decode must succeed
	}
	steps := []step{
		{stream: streams[0].data, outBits: 16},
		{stream: streams[1].data, opts: []DecoderOption{WithConvertTo(32)}, outBits: 32},
		// No option this call: conversion must be forgotten, output back at native 8.
		{stream: streams[2].data, outBits: 8},
		{stream: streams[3].data, opts: []DecoderOption{WithConvertTo(16)}, outBits: 16},
		// Skip must be forgotten next call: the corrupted stream then has to fail MD5.
		{stream: badMD5, opts: []DecoderOption{SkipMD5Verification()}, outBits: 16},
		{stream: badMD5, outBits: 16, decodeErr: flac.ErrMD5Mismatch},
		// Tags must come from the current stream only.
		{stream: tagged, outBits: 16},
		// Back to an untagged stream: the tags must not linger.
		{stream: streams[1].data, outBits: 24},
		// Invalid option poisons the decoder.
		{stream: streams[0].data, opts: []DecoderOption{WithConvertTo(13)}, outBits: 0, wantErr: true},
		// Recovery through a good Reset.
		{stream: streams[2].data, opts: []DecoderOption{SkipMD5Verification()}, outBits: 8},
		// Skip forgotten again; native width restored on a different depth.
		{stream: streams[3].data, outBits: 12},
	}

	d := new(Decoder) // zero-value, the sync.Pool entry point
	for i, st := range steps {
		err := d.Reset(bytes.NewReader(st.stream), st.opts...)
		if st.wantErr {
			if err == nil {
				t.Fatalf("step %d: Reset want error, got nil", i)
			}
			// Poisoned: Read and WriteTo keep surfacing the error, Info is zero.
			if _, rerr := d.Read(make([]byte, 32)); !errors.Is(rerr, err) {
				t.Fatalf("step %d: Read after failed Reset = %v, want sticky %v", i, rerr, err)
			}
			if _, werr := d.WriteTo(io.Discard); !errors.Is(werr, err) {
				t.Fatalf("step %d: WriteTo after failed Reset = %v, want sticky %v", i, werr, err)
			}
			if info := d.Info(); info != (flac.StreamInfo{}) {
				t.Fatalf("step %d: Info after failed Reset = %+v, want zero", i, info)
			}
			continue
		}
		if err != nil {
			t.Fatalf("step %d: Reset: %v", i, err)
		}
		if got := d.OutputBitDepth(); got != st.outBits {
			t.Fatalf("step %d: OutputBitDepth = %d, want %d (stale option from a previous Reset?)", i, got, st.outBits)
		}

		// Tag policy reflects only the current stream.
		wantTags := 0
		if bytes.Equal(st.stream, tagged) {
			wantTags = 1
		}
		if got := len(d.Tags()); got != wantTags {
			t.Fatalf("step %d: Tags() has %d entries, want %d", i, got, wantTags)
		}
		if wantTags == 1 && d.Vendor() != "vend-or" {
			t.Fatalf("step %d: Vendor = %q, want vend-or", i, d.Vendor())
		}

		// Compare against a fresh decoder built with the same options.
		fresh, ferr := NewDecoder(bytes.NewReader(st.stream), st.opts...)
		if ferr != nil {
			t.Fatalf("step %d: fresh NewDecoder: %v", i, ferr)
		}
		if d.Info() != fresh.Info() {
			t.Fatalf("step %d: pooled Info %+v != fresh %+v", i, d.Info(), fresh.Info())
		}
		got, gerr := io.ReadAll(d)
		want, ferr := io.ReadAll(fresh)
		if st.decodeErr != nil {
			if !errors.Is(gerr, st.decodeErr) {
				t.Fatalf("step %d: decode err = %v, want %v", i, gerr, st.decodeErr)
			}
			continue
		}
		if gerr != nil {
			t.Fatalf("step %d: pooled decode: %v", i, gerr)
		}
		if ferr != nil {
			t.Fatalf("step %d: fresh decode: %v", i, ferr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("step %d: pooled output (%d bytes) differs from fresh (%d bytes)", i, len(got), len(want))
		}
	}
}

// TestCrossResetBadHeaderThenRecover interleaves header failures between good
// streams on one decoder. Each failed Reset must leave zeroed Info, a sticky
// error and no readable audio; the next good Reset must fully recover and decode
// byte-identically to a fresh decoder, including when the failed Reset carried
// options that must not survive it.
func TestCrossResetBadHeaderThenRecover(t *testing.T) {
	streams := buildCrossStreams(t)
	d := new(Decoder)

	for cycle := range 3 {
		good := streams[cycle%len(streams)]
		if err := d.Reset(bytes.NewReader(good.data), WithConvertTo(good.cfg.BitDepth)); err != nil {
			t.Fatalf("cycle %d: good Reset: %v", cycle, err)
		}
		if _, err := io.CopyN(io.Discard, d, 128); err != nil {
			t.Fatalf("cycle %d: partial decode: %v", cycle, err)
		}

		// A bad header, with an option attached: neither the half-parsed stream nor
		// the option may survive.
		rerr := d.Reset(bytes.NewReader([]byte("fLaC-but-then-garbage")), SkipMD5Verification())
		if rerr == nil {
			t.Fatalf("cycle %d: bad-header Reset want error", cycle)
		}
		if info := d.Info(); info != (flac.StreamInfo{}) {
			t.Fatalf("cycle %d: Info after bad Reset = %+v, want zero", cycle, info)
		}
		if d.OutputBitDepth() != 0 {
			t.Fatalf("cycle %d: OutputBitDepth after bad Reset = %d, want 0", cycle, d.OutputBitDepth())
		}
		if n, err := d.Read(make([]byte, 16)); n != 0 || !errors.Is(err, rerr) {
			t.Fatalf("cycle %d: Read after bad Reset = (%d, %v), want (0, %v)", cycle, n, err, rerr)
		}
		if n, err := d.WriteTo(io.Discard); n != 0 || !errors.Is(err, rerr) {
			t.Fatalf("cycle %d: WriteTo after bad Reset = (%d, %v), want (0, %v)", cycle, n, err, rerr)
		}

		// Recover on a different shape, with no options: MD5 verification must run.
		next := streams[(cycle+1)%len(streams)]
		if err := d.Reset(bytes.NewReader(next.data)); err != nil {
			t.Fatalf("cycle %d: recovery Reset: %v", cycle, err)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("cycle %d: recovered decode: %v", cycle, err)
		}
		if !bytes.Equal(got, next.want) {
			t.Fatalf("cycle %d: recovered output %d bytes, want %d", cycle, len(got), len(next.want))
		}
	}
}

// TestCrossResetNilReaderLeavesStreamIntact pins that Reset(nil) is rejected
// before any state change: after a partial decode, Reset(nil) fails and the same
// decoder continues the in-progress stream to the end, byte-identical to a full
// fresh decode.
func TestCrossResetNilReaderLeavesStreamIntact(t *testing.T) {
	streams := buildCrossStreams(t)
	s := streams[1] // 24-bit mono
	d, err := NewDecoder(bytes.NewReader(s.data))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	var got bytes.Buffer
	if _, err := io.CopyN(&got, d, 256); err != nil {
		t.Fatalf("partial decode: %v", err)
	}
	if err := d.Reset(nil); err == nil {
		t.Fatal("Reset(nil): want error, got nil")
	}
	if info := d.Info(); info.BitDepth != s.cfg.BitDepth {
		t.Fatalf("Reset(nil) disturbed Info: %+v", info)
	}
	if _, err := io.Copy(&got, d); err != nil {
		t.Fatalf("continue after Reset(nil): %v", err)
	}
	if !bytes.Equal(got.Bytes(), s.want) {
		t.Fatalf("stream after Reset(nil) = %d bytes, want %d", got.Len(), len(s.want))
	}
}

// TestCrossSeekArgumentFailuresPreserveState proves the two pre-check failures
// touch no decoder state: ErrSeekUnsupported on a forward-only source and
// ErrInvalidSeek for a negative index leave the pending audio and the decoder's
// position exactly where they were, so the next Read continues the same stream.
func TestCrossSeekArgumentFailuresPreserveState(t *testing.T) {
	streams := buildCrossStreams(t)
	s := streams[0]

	t.Run("unsupported seek on a non-seekable source", func(t *testing.T) {
		d, err := NewDecoder(readerOnly{bytes.NewReader(s.data)})
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		var before bytes.Buffer
		if _, err := io.CopyN(&before, d, 512); err != nil {
			t.Fatalf("partial decode: %v", err)
		}
		if _, serr := d.SeekToSample(10); !errors.Is(serr, flac.ErrSeekUnsupported) {
			t.Fatalf("SeekToSample = %v, want ErrSeekUnsupported", serr)
		}
		// Repeating it must fail the same way and still change nothing.
		if _, serr := d.SeekToSample(0); !errors.Is(serr, flac.ErrSeekUnsupported) {
			t.Fatalf("second SeekToSample = %v, want ErrSeekUnsupported", serr)
		}
		var rest bytes.Buffer
		if _, err := io.Copy(&rest, d); err != nil {
			t.Fatalf("decode after rejected seek: %v", err)
		}
		if !bytes.Equal(append(before.Bytes(), rest.Bytes()...), s.want) {
			t.Fatal("a rejected unsupported seek disturbed the forward decode")
		}
	})

	t.Run("negative seek on a seekable source", func(t *testing.T) {
		d, err := NewDecoder(bytes.NewReader(s.data))
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		var before bytes.Buffer
		if _, err := io.CopyN(&before, d, 512); err != nil {
			t.Fatalf("partial decode: %v", err)
		}
		if _, serr := d.SeekToSample(-7); !errors.Is(serr, flac.ErrInvalidSeek) {
			t.Fatalf("SeekToSample(-7) = %v, want ErrInvalidSeek", serr)
		}
		var rest bytes.Buffer
		if _, err := io.Copy(&rest, d); err != nil {
			t.Fatalf("decode after rejected seek: %v", err)
		}
		if !bytes.Equal(append(before.Bytes(), rest.Bytes()...), s.want) {
			t.Fatal("a rejected negative seek disturbed the forward decode")
		}
	})
}

// TestCrossSeekFailureIsStickyForReadAndWriteTo forces a real I/O failure at every
// stage of a seek (the sweep in allow counts), then disarms the source. Every
// failed seek must leave the decoder hard-failed: both Read and WriteTo keep
// returning the seek error with zero bytes, even though the source itself is
// healthy again. A subsequent successful seek clears it; a Reset clears it too.
func TestCrossSeekFailureIsStickyForReadAndWriteTo(t *testing.T) {
	data, total := encodeRamp(t, 2, 16, 6*encoderBlockSize)
	for allow := range 14 {
		t.Run(fmt.Sprintf("allow=%d", allow), func(t *testing.T) {
			src := newFailingSource(data)
			d, err := NewDecoder(src)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			// Have pending audio live when the seek fails.
			if _, err := io.ReadFull(d, make([]byte, 128)); err != nil {
				t.Fatalf("initial read: %v", err)
			}
			src.arm(allow)
			_, seekErr := d.SeekToSample(int64(3 * encoderBlockSize))
			src.disarm()
			if seekErr == nil {
				return // failure point not reached; nothing to assert at this allow
			}
			if n, err := d.Read(make([]byte, 64)); n != 0 || !errors.Is(err, seekErr) {
				t.Fatalf("Read after failed seek = (%d, %v), want (0, %v)", n, err, seekErr)
			}
			if n, err := d.WriteTo(io.Discard); n != 0 || !errors.Is(err, seekErr) {
				t.Fatalf("WriteTo after failed seek = (%d, %v), want (0, %v)", n, err, seekErr)
			}
			// The error must remain sticky across repeated calls.
			if n, err := d.Read(make([]byte, 64)); n != 0 || !errors.Is(err, seekErr) {
				t.Fatalf("second Read = (%d, %v), still want the sticky seek error", n, err)
			}

			// A successful seek on the healed source recovers the decoder.
			target := int64(2 * encoderBlockSize)
			landed, err := d.SeekToSample(target)
			if err != nil {
				t.Fatalf("recovery seek: %v", err)
			}
			if landed != target {
				t.Fatalf("recovery seek landed at %d, want %d", landed, target)
			}
			got, err := io.ReadAll(d)
			if err != nil {
				t.Fatalf("read after recovery: %v", err)
			}
			ref, err := NewDecoder(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("reference NewDecoder: %v", err)
			}
			if _, err := ref.SeekToSample(target); err != nil {
				t.Fatalf("reference seek: %v", err)
			}
			want, err := io.ReadAll(ref)
			if err != nil {
				t.Fatalf("reference read: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("recovered tail %d bytes, want %d (total %d)", len(got), len(want), total)
			}
		})
	}
}

// TestCrossFailedSeekRecoveredByReset is the other recovery path: after a hard
// seek failure, Reset onto a healthy different-depth stream must clear the sticky
// error and the decoder decodes the new stream cleanly with its own MD5 check.
func TestCrossFailedSeekRecoveredByReset(t *testing.T) {
	streams := buildCrossStreams(t)
	src := newFailingSource(streams[0].data)
	d, err := NewDecoder(src)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	src.arm(0)
	if _, err := d.SeekToSample(int64(encoderBlockSize)); err == nil {
		t.Fatal("armed seek: want failure")
	}
	src.disarm()
	if n, err := d.Read(make([]byte, 16)); err == nil || n != 0 {
		t.Fatalf("Read after failed seek = (%d, %v), want failure", n, err)
	}

	next := streams[1] // 24-bit mono, different depth
	if err := d.Reset(bytes.NewReader(next.data)); err != nil {
		t.Fatalf("Reset after failed seek: %v", err)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("decode after Reset: %v", err)
	}
	if !bytes.Equal(got, next.want) {
		t.Fatalf("post-Reset output %d bytes, want %d", len(got), len(next.want))
	}
}

// TestCrossSeekLandsExactlyOnTargetAndDropsEarlierSamples is the sample-accuracy
// guard across depths and channel counts: after a seek (including a backward seek
// following a forward one, and seeks while converted output is selected), the very
// next sample must be the target and nothing before it may appear. The full tail
// must equal the reference tail byte for byte.
func TestCrossSeekLandsExactlyOnTargetAndDropsEarlierSamples(t *testing.T) {
	for _, s := range buildCrossStreams(t) {
		t.Run(s.name, func(t *testing.T) {
			native := unpackLE(s.want, (s.cfg.BitDepth+7)/8)
			// Targets: frame boundary, mid-frame, one sample in, the last sample.
			block := encoderBlockSize
			targets := []int64{0, 1, int64(block / 2), int64(block), int64(block + 1),
				int64(s.samples - 1)}
			for _, tg := range targets {
				d, err := NewDecoder(bytes.NewReader(s.data))
				if err != nil {
					t.Fatalf("NewDecoder: %v", err)
				}
				landed, err := d.SeekToSample(tg)
				if err != nil || landed != tg {
					t.Fatalf("SeekToSample(%d) = (%d, %v), want (%d, nil)", tg, landed, err, tg)
				}
				got, err := io.ReadAll(d)
				if err != nil {
					t.Fatalf("read after seek %d: %v", tg, err)
				}
				wantTail := s.want[int(tg)*s.cfg.Channels*((s.cfg.BitDepth+7)/8):]
				if !bytes.Equal(got, wantTail) {
					t.Fatalf("seek %d: tail %d bytes, want %d; pre-target samples mixed in or wrong landing",
						tg, len(got), len(wantTail))
				}
				// First inter-channel sample's channel 0 must be the target's value.
				first := unpackLE(got, (s.cfg.BitDepth+7)/8)
				if first[0] != native[int(tg)*s.cfg.Channels] {
					t.Fatalf("seek %d: first sample = %d, want %d", tg, first[0], native[int(tg)*s.cfg.Channels])
				}
			}

			// Forward seek, consume a little, then a backward seek: the second landing
			// must restart exactly at its target with nothing carried over. The forward
			// target stays inside even the shortest (sub-two-block) stream.
			d, err := NewDecoder(bytes.NewReader(s.data))
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			fwd := int64(s.samples / 2)
			if _, err := d.SeekToSample(fwd); err != nil {
				t.Fatalf("forward seek: %v", err)
			}
			frameBytes := s.cfg.Channels * ((s.cfg.BitDepth + 7) / 8)
			consume := int64(64) * int64(frameBytes)
			if maxConsume := int64(s.samples-int(fwd)) * int64(frameBytes); consume > maxConsume {
				consume = maxConsume
			}
			if _, err := io.CopyN(io.Discard, d, consume); err != nil {
				t.Fatalf("consume between seeks: %v", err)
			}
			back := fwd / 2
			if _, err := d.SeekToSample(back); err != nil {
				t.Fatalf("backward seek: %v", err)
			}
			got, err := io.ReadAll(d)
			if err != nil {
				t.Fatalf("read after backward seek: %v", err)
			}
			wantTail := s.want[int(back)*frameBytes:]
			if !bytes.Equal(got, wantTail) {
				t.Fatalf("backward seek to %d: tail mismatch (%d vs %d bytes)", back, len(got), len(wantTail))
			}
		})
	}
}

// TestCrossSeekWithConvertToLandsAtTargetAtConvertedWidth combines seeking and
// width conversion: after a seek under WithConvertTo, the output stays at the
// converted width, starts exactly at the target sample, and matches a converted
// fresh-decoder tail. Info keeps the native depth.
func TestCrossSeekWithConvertToLandsAtTargetAtConvertedWidth(t *testing.T) {
	streams := buildCrossStreams(t)
	cases := []struct {
		s   crossStream
		dst int
	}{
		{streams[0], 32}, // 16 -> 32 widen
		{streams[1], 16}, // 24 -> 16 narrow
		{streams[3], 24}, // 12 -> 24 widen
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s_to_%d", c.s.name, c.dst), func(t *testing.T) {
			native := unpackLE(c.s.want, (c.s.cfg.BitDepth+7)/8)
			target := int64(encoderBlockSize + 333)
			d, err := NewDecoder(bytes.NewReader(c.s.data), WithConvertTo(c.dst))
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			if d.Info().BitDepth != c.s.cfg.BitDepth {
				t.Fatalf("Info().BitDepth = %d, want native %d", d.Info().BitDepth, c.s.cfg.BitDepth)
			}
			if d.OutputBitDepth() != c.dst {
				t.Fatalf("OutputBitDepth = %d, want %d", d.OutputBitDepth(), c.dst)
			}
			landed, err := d.SeekToSample(target)
			if err != nil || landed != target {
				t.Fatalf("seek = (%d, %v), want (%d, nil)", landed, err, target)
			}
			got, err := io.ReadAll(d)
			if err != nil {
				t.Fatalf("read after seek: %v", err)
			}
			tailNative := native[int(target)*c.s.cfg.Channels:]
			want := packLE(applyRef(tailNative, c.s.cfg.BitDepth, c.dst), c.dst)
			if !bytes.Equal(got, want) {
				t.Fatalf("converted seek tail %d bytes, want %d", len(got), len(want))
			}
			// Alignment: output length must be a whole number of converted samples.
			if len(got)%(c.dst/8*c.s.cfg.Channels) != 0 {
				t.Fatalf("converted output %d bytes is not sample-aligned", len(got))
			}
		})
	}
}

// TestCrossSeekToEndClearsStickyFailureThenResetRepositions covers the end-of-stream
// recovery: after a hard seek failure, seeking to TotalSamples must clear the
// sticky error and yield a clean io.EOF; a Reset afterwards must seek normally
// again on a fresh stream.
func TestCrossSeekToEndClearsStickyFailureThenResetRepositions(t *testing.T) {
	data, total := encodeRamp(t, 2, 16, 4*encoderBlockSize)
	src := newFailingSource(data)
	d, err := NewDecoder(src)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	src.arm(0)
	if _, err := d.SeekToSample(int64(2 * encoderBlockSize)); !errors.Is(err, errSourceIO) {
		t.Fatalf("armed seek: %v", err)
	}
	src.disarm()

	landed, err := d.SeekToSample(int64(total))
	if err != nil {
		t.Fatalf("seek to end: %v", err)
	}
	if landed != int64(total) {
		t.Fatalf("landed %d, want total %d", landed, total)
	}
	if n, err := d.Read(make([]byte, 16)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read at end = (%d, %v), want (0, io.EOF)", n, err)
	}

	// Reset onto another stream restores ordinary seeking.
	streams := buildCrossStreams(t)
	next := streams[2]
	if err := d.Reset(bytes.NewReader(next.data)); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	tg := int64(encoderBlockSize / 2)
	if got, err := d.SeekToSample(tg); err != nil || got != tg {
		t.Fatalf("seek after Reset = (%d, %v), want (%d, nil)", got, err, tg)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("read after seek: %v", err)
	}
	want := next.want[int(tg)*next.cfg.Channels:]
	if !bytes.Equal(got, want) {
		t.Fatalf("tail after Reset+seek %d bytes, want %d", len(got), len(want))
	}
}

// TestCrossFullDecodeStillVerifiesMD5AndCount guards the normal path through all
// the state machinery: a full forward decode of an untouched seekable stream
// passes MD5 and sample-count checks (a corrupted one fails MD5; a zero-MD5
// truncated one fails the count check), and a non-seekable source decodes the
// same bytes.
func TestCrossFullDecodeStillVerifiesMD5AndCount(t *testing.T) {
	streams := buildCrossStreams(t)
	for _, s := range streams {
		// Seekable, intact: clean decode equal to the reference PCM.
		if got, err := io.ReadAll(mustDecoder(t, bytes.NewReader(s.data))); err != nil {
			t.Fatalf("%s seekable full decode: %v", s.name, err)
		} else if !bytes.Equal(got, s.want) {
			t.Fatalf("%s seekable decode mismatch", s.name)
		}
		// Non-seekable source: same bytes, sequential-only.
		d, err := NewDecoder(readerOnly{bytes.NewReader(s.data)})
		if err != nil {
			t.Fatalf("%s non-seekable NewDecoder: %v", s.name, err)
		}
		if got, err := io.ReadAll(d); err != nil {
			t.Fatalf("%s non-seekable decode: %v", s.name, err)
		} else if !bytes.Equal(got, s.want) {
			t.Fatalf("%s non-seekable decode mismatch", s.name)
		}
		if _, err := d.SeekToSample(0); !errors.Is(err, flac.ErrSeekUnsupported) {
			t.Fatalf("%s: seek on drained non-seekable decoder = %v, want ErrSeekUnsupported", s.name, err)
		}
		// Corrupted MD5: the end-of-stream check must still fire on a full decode.
		bad := corruptMD5(s.data)
		if _, err := io.Copy(io.Discard, mustDecoder(t, bytes.NewReader(bad))); !errors.Is(err, flac.ErrMD5Mismatch) {
			t.Fatalf("%s corrupted MD5: err = %v, want ErrMD5Mismatch", s.name, err)
		}
		// ...but SkipMD5Verification suppresses exactly that check, same audio.
		skip, err := NewDecoder(bytes.NewReader(bad), SkipMD5Verification())
		if err != nil {
			t.Fatalf("%s skip NewDecoder: %v", s.name, err)
		}
		if got, err := io.ReadAll(skip); err != nil {
			t.Fatalf("%s skip decode: %v", s.name, err)
		} else if !bytes.Equal(got, s.want) {
			t.Fatalf("%s skip decode mismatch", s.name)
		}
	}

	// Sample-count guard on a zero-MD5 stream cut between frames.
	data, _ := encodeRamp(t, 1, 16, 3*encoderBlockSize)
	zeroStreamInfoMD5(data)
	cut := truncateAtFrameBoundary(t, data, 2)
	if _, err := io.Copy(io.Discard, mustDecoder(t, bytes.NewReader(cut))); !errors.Is(err, flac.ErrTruncatedStream) {
		t.Fatalf("zero-MD5 inter-frame cut: err = %v, want ErrTruncatedStream", err)
	}
}

// TestCrossResetClearsSeekOnlyFields pins that seekability belongs to the current
// stream, not the decoder: a SEEKTABLE-bearing seekable stream followed by a plain
// seekable one drops the old seek points and re-bases the byte window, and a
// following non-seekable Reset drops the ReadSeeker entirely so seeking is
// unsupported. The reverse flip restores everything on the next seekable Reset.
func TestCrossResetClearsSeekOnlyFields(t *testing.T) {
	const block = encoderBlockSize
	withTable := encodeRampSeektable(t, 2, 16, 6*block, block)
	plain, _ := encodeRamp(t, 1, 16, 3*block)

	d := new(Decoder)
	if err := d.Reset(bytes.NewReader(withTable)); err != nil {
		t.Fatalf("Reset seektable stream: %v", err)
	}
	if !d.seekable || len(d.seekPoints) == 0 {
		t.Fatalf("precondition: seekable=%v seekPoints=%d", d.seekable, len(d.seekPoints))
	}
	// A table-narrowed seek must land exactly.
	if got, err := d.SeekToSample(int64(2 * block)); err != nil || got != 2*block {
		t.Fatalf("table seek = (%d, %v)", got, err)
	}

	// Reset to a seekable stream without a table: the old points and byte window
	// must be replaced by the new stream's.
	if err := d.Reset(bytes.NewReader(plain)); err != nil {
		t.Fatalf("Reset plain seekable: %v", err)
	}
	if !d.seekable {
		t.Fatal("seekability lost after a seekable Reset")
	}
	if d.seekPoints != nil {
		t.Fatalf("stale seek points survived Reset: %d entries", len(d.seekPoints))
	}
	if d.info.Channels != 1 {
		t.Fatalf("Info channels = %d, want 1 (current stream only)", d.info.Channels)
	}
	tg := int64(block - 1)
	if got, err := d.SeekToSample(tg); err != nil || got != tg {
		t.Fatalf("seek after table->plain Reset = (%d, %v), want %d", got, err, tg)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	fresh, _ := NewDecoder(bytes.NewReader(plain))
	if _, err := fresh.SeekToSample(tg); err != nil {
		t.Fatalf("fresh seek: %v", err)
	}
	want, _ := io.ReadAll(fresh)
	if !bytes.Equal(got, want) {
		t.Fatalf("post-Reset seek tail %d bytes, want %d", len(got), len(want))
	}

	// Reset to a forward-only source: the ReadSeeker must be dropped entirely.
	if err := d.Reset(readerOnly{bytes.NewReader(plain)}); err != nil {
		t.Fatalf("Reset non-seekable: %v", err)
	}
	if d.seekable || d.rs != nil {
		t.Fatalf("stale seeker survived a non-seekable Reset: seekable=%v rs=%v", d.seekable, d.rs)
	}
	if _, err := d.SeekToSample(0); !errors.Is(err, flac.ErrSeekUnsupported) {
		t.Fatalf("seek after non-seekable Reset = %v, want ErrSeekUnsupported", err)
	}
	if _, err := io.Copy(io.Discard, d); err != nil {
		t.Fatalf("sequential decode on the non-seekable stream: %v", err)
	}

	// Flip back to seekable: a new ReadSeeker and window are installed.
	if err := d.Reset(bytes.NewReader(withTable)); err != nil {
		t.Fatalf("final seekable Reset: %v", err)
	}
	if !d.seekable || len(d.seekPoints) == 0 {
		t.Fatalf("seekability/table not restored: seekable=%v points=%d", d.seekable, len(d.seekPoints))
	}

	// A failed Reset must not leave the previous stream's seek points behind: the
	// success-path assignment is skipped on a bad header, so only the pre-parse
	// clear keeps them from leaking. A following good Reset recovers cleanly.
	if rerr := d.Reset(bytes.NewReader([]byte("fLaC-but-then-garbage"))); rerr == nil {
		t.Fatal("bad-header Reset: want error")
	}
	if d.seekPoints != nil {
		t.Fatalf("stale seek points survived a failed Reset: %d entries", len(d.seekPoints))
	}
	if d.seekable || d.rs != nil {
		t.Fatalf("stale seeker survived a failed Reset: seekable=%v rs=%v", d.seekable, d.rs)
	}
	if err := d.Reset(bytes.NewReader(plain)); err != nil {
		t.Fatalf("recovery Reset: %v", err)
	}
	if !d.seekable || d.seekPoints != nil {
		t.Fatalf("recovery wrong seek state: seekable=%v points=%v", d.seekable, d.seekPoints)
	}
}

// TestCrossPooledDecoderReusedAcrossStreams is the end-to-end sync.Pool scenario:
// many goroutines reuse pooled decoders over streams of different depths and
// options, including seeks and failed Resets, and every output matches a fresh
// decoder's. A stale strategy or a cross-stream buffer leak shows up as a byte
// mismatch (and the race builder catches shared-state races).
func TestCrossPooledDecoderReusedAcrossStreams(t *testing.T) {
	streams := buildCrossStreams(t)
	type job struct {
		s    crossStream
		opts []DecoderOption
		seek int64 // < 0 means full forward decode
	}
	jobs := make([]job, 0, len(streams)*2)
	for i, s := range streams {
		jobs = append(jobs, job{s: s, seek: -1})
		dst := []int{8, 16, 24, 32}[i%4]
		jobs = append(jobs, job{s: s, opts: []DecoderOption{WithConvertTo(dst)}, seek: int64(encoderBlockSize / 2)})
	}

	pool := sync.Pool{New: func() any { return new(Decoder) }}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for round := range 6 {
				j := jobs[(g+round)%len(jobs)]
				d := pool.Get().(*Decoder)
				if err := d.Reset(bytes.NewReader(j.s.data), j.opts...); err != nil {
					t.Errorf("pooled Reset: %v", err)
					pool.Put(d)
					return
				}
				if j.seek >= 0 {
					if landed, err := d.SeekToSample(j.seek); err != nil || landed != j.seek {
						t.Errorf("pooled seek = (%d, %v), want %d", landed, err, j.seek)
						pool.Put(d)
						return
					}
				}
				got, err := io.ReadAll(d)
				if err != nil {
					t.Errorf("pooled decode: %v", err)
					pool.Put(d)
					return
				}
				fresh, err := NewDecoder(bytes.NewReader(j.s.data), j.opts...)
				if err != nil {
					t.Errorf("fresh NewDecoder: %v", err)
					pool.Put(d)
					return
				}
				var want []byte
				if j.seek >= 0 {
					if _, err := fresh.SeekToSample(j.seek); err != nil {
						t.Errorf("fresh seek: %v", err)
						pool.Put(d)
						return
					}
				}
				want, err = io.ReadAll(fresh)
				if err != nil {
					t.Errorf("fresh decode: %v", err)
					pool.Put(d)
					return
				}
				if !bytes.Equal(got, want) {
					t.Errorf("pooled output %d bytes != fresh %d bytes", len(got), len(want))
				}
				pool.Put(d)

				// Every goroutine also poisons and recovers one pooled decoder to
				// exercise the failure contract under concurrency.
				if round == 1 {
					bad := pool.Get().(*Decoder)
					if rerr := bad.Reset(bytes.NewReader([]byte("not a flac stream"))); rerr == nil {
						t.Error("poison Reset: want error")
					}
					if n, rerr := bad.Read(make([]byte, 8)); rerr == nil || n != 0 {
						t.Errorf("poisoned Read = (%d, %v), want error", n, rerr)
					}
					if err := bad.Reset(bytes.NewReader(j.s.data)); err != nil {
						t.Errorf("recovery Reset: %v", err)
					}
					if _, err := io.Copy(io.Discard, bad); err != nil {
						t.Errorf("decode after recovery: %v", err)
					}
					pool.Put(bad)
				}
			}
		})
	}
	wg.Wait()
}
