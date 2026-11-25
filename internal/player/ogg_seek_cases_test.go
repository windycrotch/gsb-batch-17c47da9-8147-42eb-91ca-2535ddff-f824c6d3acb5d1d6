package player

import (
	"bytes"
	"errors"
	"testing"
)

// The acceptance bar for these tests is observable behavior: playback
// position and the exact decoded sample values, never internal counters.

// TestFakeStream_NormalPlayback is the baseline: every packet decodes in
// order and the concatenated output matches the packets' frame sequences.
func TestFakeStream_NormalPlayback(t *testing.T) {
	const packets, frameLen = 6, 100
	codec := newFakeOggCodec(0)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	d := newFakeDecoder(t, data, dataStart, codec)

	got := readAllFrames(d)
	if d.Err() != nil {
		t.Fatalf("unexpected error: %v", d.Err())
	}

	var want []float64
	for i := range packets {
		want = append(want, expectedFrames(i, frameLen)...)
	}
	if len(got) != len(want) {
		t.Fatalf("frame count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("frame %d = %v, want %v (first divergence)", i, got[i], want[i])
		}
	}
	if d.Position() != packets*frameLen {
		t.Errorf("final position = %d, want %d", d.Position(), packets*frameLen)
	}
}

// TestMultiPagePacket_JoinedBeforeDecode: a packet split over two Ogg pages
// must reach the codec as one concatenated packet, not two half-decodes.
func TestMultiPagePacket_JoinedBeforeDecode(t *testing.T) {
	codec := newFakeOggCodec(0)
	b := newOggBuilder(0x4242)
	b.writeHeaderPage([]byte("FAKE-HEADER"))
	dataStart := int64(b.buf.Len())

	// Build a large packet whose size forces three pages: 600 bytes split
	// into two 255+255 unterminated pages, then a terminating page. Padding
	// bytes are harmless: the fake codec only reads its fixed-size header.
	big := make([]byte, 620)
	copy(big, fakePacketPrefix)
	put32(big[len(fakePacketPrefix):], 0)     // packet index 0
	put32(big[len(fakePacketPrefix)+4:], 100) // 100 frames

	// Page A: first 300 bytes as 255+255, no terminator -> packet continues.
	b.writeContinuationPage(-1, big[:300], false)
	// Page B: next 300 bytes as 255+255, still unterminated.
	b.writeContinuationPage(-1, big[300:600], false)
	// Page C: final 20 bytes terminate the packet (< 255 segment), granule set.
	b.writeContinuationPage(100, big[600:], true)

	d := newFakeDecoder(t, b.bytes(), dataStart, codec)
	got := readAllFrames(d)
	if d.Err() != nil {
		t.Fatalf("unexpected error: %v", d.Err())
	}
	want := expectedFrames(0, 100)
	if len(got) != len(want) {
		t.Fatalf("frame count = %d, want %d (packet must decode once, whole)", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("frame %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestSeek_MiddleOfPage lands inside a page's sample range and trims the
// lead-in: output must start exactly at the requested sample, with no frame
// from before the seek point replayed.
func TestSeek_MiddleOfPage(t *testing.T) {
	const packets, frameLen = 8, 100
	codec := newFakeOggCodec(0)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	d := newFakeDecoder(t, data, dataStart, codec)

	// Drain the first two packets so the seek demonstrably moves backwards.
	readFrames(t, d, 2*frameLen)

	const target = 250 // inside packet 2 (frames 200..299)
	if err := d.Seek(target); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if d.Position() != target {
		t.Fatalf("position after seek = %d, want %d", d.Position(), target)
	}

	got := readFrames(t, d, 150)
	// First emitted frame must be packet 2, frame 50 (250 - 200).
	wantFirst := float64(2*10000 + 50)
	if got[0] != wantFirst {
		t.Fatalf("first frame after seek = %v, want %v (no pre-target replay)", got[0], wantFirst)
	}
	// And it must continue contiguously through packet boundaries.
	wantSecond := float64(2*10000 + 51)
	if got[1] != wantSecond {
		t.Fatalf("second frame = %v, want %v", got[1], wantSecond)
	}
}

// TestSeek_OnPageBoundary seeks exactly at a page boundary: no trimming loss
// and no overlap.
func TestSeek_OnPageBoundary(t *testing.T) {
	const packets, frameLen = 6, 100
	codec := newFakeOggCodec(0)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	d := newFakeDecoder(t, data, dataStart, codec)

	const target = 300 // first frame of packet 3
	if err := d.Seek(target); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got := readFrames(t, d, 100)
	if got[0] != float64(3*10000) {
		t.Fatalf("first frame = %v, want packet 3 frame 0", got[0])
	}
}

// TestSeek_PreskipOffset (Opus semantics): the public position excludes the
// pre-skip, so a seek to public sample p must bisect on granule p+preSkip and
// still resume at p.
func TestSeek_PreskipOffset(t *testing.T) {
	const packets, frameLen, preSkip = 6, 100, 312
	codec := newFakeOggCodec(preSkip)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	d := newFakeDecoder(t, data, dataStart, codec)

	if d.Len() != packets*frameLen {
		t.Fatalf("Len = %d, want %d (pre-skip must not count)", d.Len(), packets*frameLen)
	}
	const target = 250
	if err := d.Seek(target); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if d.Position() != target {
		t.Fatalf("position = %d, want %d", d.Position(), target)
	}
	got := readFrames(t, d, 50)
	if got[0] != float64(2*10000+50) {
		t.Fatalf("first frame = %v, want packet 2 frame 50 despite pre-skip", got[0])
	}
}

// TestSeek_ClampsNegative converges a negative target to the start.
func TestSeek_ClampsNegative(t *testing.T) {
	const packets, frameLen = 4, 100
	codec := newFakeOggCodec(0)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	d := newFakeDecoder(t, data, dataStart, codec)

	if err := d.Seek(-500); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if d.Position() != 0 {
		t.Fatalf("position = %d, want 0", d.Position())
	}
	got := readFrames(t, d, 10)
	if got[0] != float64(0) {
		t.Fatalf("first frame = %v, want packet 0 frame 0", got[0])
	}
}

// TestSeek_ToEnd converges on the end: no packets decoded, position at Len.
func TestSeek_ToEnd(t *testing.T) {
	const packets, frameLen = 4, 100
	codec := newFakeOggCodec(0)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	d := newFakeDecoder(t, data, dataStart, codec)

	if err := d.Seek(d.Len()); err != nil {
		t.Fatalf("Seek(Len): %v", err)
	}
	if d.Position() != d.Len() {
		t.Fatalf("position = %d, want Len %d", d.Position(), d.Len())
	}
	buf := make([][2]float64, 32)
	n, ok := d.Stream(buf)
	if n != 0 || ok {
		t.Fatalf("end-seek produced n=%d ok=%v, want immediate exhaustion", n, ok)
	}
	if d.Err() != nil {
		t.Fatalf("end-seek must be clean, got %v", d.Err())
	}
}

// TestSeek_BeyondEnd clamps to the end rather than erroring or replaying.
func TestSeek_BeyondEnd(t *testing.T) {
	const packets, frameLen = 4, 100
	codec := newFakeOggCodec(0)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	d := newFakeDecoder(t, data, dataStart, codec)

	if err := d.Seek(d.Len() + 9999); err != nil {
		t.Fatalf("Seek past end: %v", err)
	}
	if d.Position() != d.Len() {
		t.Fatalf("position = %d, want clamped Len %d", d.Position(), d.Len())
	}
}

// TestStream_PrimingAndPaddingCombined: priming on the first page and
// over-emission (padding) on the EOS page are both stripped, and every frame
// in between keeps its correct stream position and packet identity.
func TestStream_PrimingAndPaddingCombined(t *testing.T) {
	codec := newFakeOggCodec(0)
	codec.priming = 15
	b := newOggBuilder(0x4242)
	b.writeHeaderPage([]byte("FAKE-HEADER"))
	dataStart := int64(b.buf.Len())
	b.writePage(100, 0, [][]byte{fakePacketWith(0, 100)}, nil)          // 15 priming + 100
	b.writePage(200, oggFlagEOS, [][]byte{fakePacketWith(1, 140)}, nil) // over-emits; granule caps at 200

	d := newFakeDecoder(t, b.bytes(), dataStart, codec)
	got := readAllFrames(d)
	if d.Err() != nil {
		t.Fatalf("unexpected error: %v", d.Err())
	}
	if len(got) != 200 {
		t.Fatalf("frames=%d want exactly 200", len(got))
	}
	if got[0] != 0 {
		t.Fatalf("first frame = %v, want 0 (priming clipped)", got[0])
	}
	if got[99] != 99 {
		t.Fatalf("frame 99 = %v, want packet 0 frame 99", got[99])
	}
	if got[100] != float64(1*10000) {
		t.Fatalf("frame 100 = %v, want packet 1 frame 0", got[100])
	}
	if got[199] != float64(1*10000+99) {
		t.Fatalf("last frame = %v, want packet 1 frame 99 (padding clipped)", got[199])
	}
}

// TestSeek_ResetsCodecState: every seek must reset the codec so a packet at
// the new position is decoded with fresh decoder state, and the reader's
// carried partial packet must be cleared.
func TestSeek_ResetsCodecState(t *testing.T) {
	const packets, frameLen = 6, 100
	codec := newFakeOggCodec(0)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	d := newFakeDecoder(t, data, dataStart, codec)

	for _, target := range []int{450, 120, 560, 0} {
		before := codec.resetCalled
		if err := d.Seek(target); err != nil {
			t.Fatalf("Seek(%d): %v", target, err)
		}
		if codec.resetCalled != before+1 {
			t.Fatalf("Seek(%d) did not reset the codec (calls %d -> %d)",
				target, before, codec.resetCalled)
		}
		if d.ogg.partial != nil {
			t.Fatalf("Seek(%d) left a reader partial packet", target)
		}
	}
}

// TestSeek_DecoderPrimingIsClipped: a codec that emits leading priming
// frames before stream sample 0 (as Vorbis does) must have those frames
// clipped so the first emitted sample is stream sample 0 — both on normal
// playback and after a seek.
func TestSeek_DecoderPrimingIsClipped(t *testing.T) {
	const packets, frameLen, priming = 4, 100, 30
	codec := newFakeOggCodec(0)
	codec.priming = priming
	// Mark the initial decoder as already primed only after the first Reset;
	// the very first decode (before any seek) also goes through priming here,
	// which is exactly what we want to clip.
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	d := newFakeDecoder(t, data, dataStart, codec)

	got := readFrames(t, d, packets*frameLen)
	if got[0] != 0 {
		t.Fatalf("first frame = %v, want stream sample 0 (priming must be clipped)", got[0])
	}
	for i, v := range got {
		// Every emitted frame must be a non-negative, in-range stream sample.
		if v < 0 {
			t.Fatalf("frame %d = %v is a priming frame that leaked into output", i, v)
		}
	}
	// No negative (priming) value anywhere in the full drain.
	if d.Err() != nil {
		t.Fatalf("unexpected error: %v", d.Err())
	}

	// After a seek the codec re-primes; the lead-in must again be clipped and
	// output must resume exactly at the target.
	const target = 250
	if err := d.Seek(target); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	after := readFrames(t, d, 50)
	if after[0] != float64(2*10000+50) {
		t.Fatalf("first frame after seek = %v, want packet 2 frame 50", after[0])
	}
	for _, v := range after {
		if v < 0 {
			t.Fatalf("priming frame leaked after seek: %v", v)
		}
	}
}

// TestStream_ExactLength: a full sequential drain must emit exactly Len()
// frames, never priming or padding extras, and end without an error.
func TestStream_ExactLength(t *testing.T) {
	const packets, frameLen = 5, 100
	codec := newFakeOggCodec(0)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	d := newFakeDecoder(t, data, dataStart, codec)

	got := readAllFrames(d)
	if d.Err() != nil {
		t.Fatalf("unexpected error: %v", d.Err())
	}
	if len(got) != d.Len() {
		t.Fatalf("emitted %d frames, want Len %d", len(got), d.Len())
	}
	if d.Position() != d.Len() {
		t.Fatalf("final position %d != Len %d", d.Position(), d.Len())
	}
}

// TestStream_TrailingPaddingClipped: when the codec over-emits on the final
// (EOS) page, the surplus frames beyond the last granule must be clipped.
func TestStream_TrailingPaddingClipped(t *testing.T) {
	codec := newFakeOggCodec(0)
	b := newOggBuilder(0x4242)
	b.writeHeaderPage([]byte("FAKE-HEADER"))
	dataStart := int64(b.buf.Len())

	// Two packets of 100 stream frames each, but make the codec over-emit by
	// declaring a larger frame count on the last packet while the granule
	// still says the stream is 200 samples long.
	p0 := fakePacketWith(0, 100)
	p1Over := fakePacketWith(1, 130) // 30 frames of decoder padding
	b.writePage(100, 0, [][]byte{p0}, nil)
	b.writePage(200, oggFlagEOS, [][]byte{p1Over}, nil) // granule caps stream at 200

	d := newFakeDecoder(t, b.bytes(), dataStart, codec)
	got := readAllFrames(d)
	if d.Err() != nil {
		t.Fatalf("unexpected error: %v", d.Err())
	}
	if len(got) != 200 {
		t.Fatalf("emitted %d frames, want exactly 200 (padding clipped)", len(got))
	}
	// The last valid frame must be packet 1, frame 99 — not padding.
	last := got[len(got)-1]
	if last != float64(1*10000+99) {
		t.Fatalf("last frame = %v, want packet 1 frame 99", last)
	}
}

// TestSeek_RapidSuccession: a burst of seeks must leave the decoder at the
// last requested position, with no half-packet or old page state surviving.
func TestSeek_RapidSuccession(t *testing.T) {
	const packets, frameLen = 10, 100
	codec := newFakeOggCodec(0)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	d := newFakeDecoder(t, data, dataStart, codec)

	targets := []int{50, 350, 120, 780, 250, 640}
	for _, target := range targets {
		if err := d.Seek(target); err != nil {
			t.Fatalf("Seek(%d): %v", target, err)
		}
		// A seek must reset all buffered PCM; position is exact.
		if d.Position() != target {
			t.Fatalf("after Seek(%d) position = %d", target, d.Position())
		}
		if len(d.pcmBuffer) != 0 || d.pcmPos != 0 {
			t.Fatalf("Seek(%d) left stale buffered PCM", target)
		}
	}

	final := targets[len(targets)-1]
	got := readFrames(t, d, 60)
	// 640 is inside packet 6 (600..699), frame offset 40.
	if got[0] != float64(final/100*10000+(final%100)) {
		t.Fatalf("first frame after burst = %v, want packet %d frame %d", got[0], final/100, final%100)
	}
	// No frame earlier than the final target may appear anywhere.
	minAllowed := float64(6*10000 + 40)
	for i, v := range got {
		if v < minAllowed {
			t.Fatalf("frame %d = %v replays content before final target", i, v)
		}
	}
}

// TestSeek_ContinuedPageDropsStaleFragment: when the landing page starts with
// a continued packet whose head is before the seek point, that fragment must
// be dropped instead of being joined into the new position's first packet.
func TestSeek_ContinuedPageDropsStaleFragment(t *testing.T) {
	codec := newFakeOggCodec(0)
	b := newOggBuilder(0x4242)
	b.writeHeaderPage([]byte("FAKE-HEADER"))
	dataStart := int64(b.buf.Len())

	// Packets 0..3; packet 2 is a 620-byte packet split over two pages so its
	// tail lands on the same page that starts packet 3.
	p0 := fakePacketWith(0, 100)
	p1 := fakePacketWith(1, 100)
	p2 := make([]byte, 620)
	copy(p2, fakePacketPrefix)
	put32(p2[len(fakePacketPrefix):], 2)     // packet index 2
	put32(p2[len(fakePacketPrefix)+4:], 100) // 100 frames
	p3 := fakePacketWith(3, 100)

	// Page 2: packets 0 and 1, granule at end of packet 1.
	b.writePage(200, 0, [][]byte{p0, p1}, nil)
	// Page 3: head of packet 2, unterminated, granule-less.
	b.writeContinuationPage(-1, p2[:300], false)
	// Page 4: tail of packet 2 (continued and now terminated) then packet 3.
	b.writePage(400, oggFlagEOS, [][]byte{p3}, p2[300:])

	d := newFakeDecoder(t, b.bytes(), dataStart, codec)

	// Seek to sample 350 (inside packet 3's 300..399 range). The latest page
	// not later than 350 is page 4; its first segment is the stale tail of
	// packet 2 and must be discarded, not prepended to packet 3.
	if err := d.Seek(350); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got := readFrames(t, d, 50)
	if d.Err() != nil {
		t.Fatalf("stale fragment poisoned the new packet: %v", d.Err())
	}
	if got[0] != float64(3*10000+50) {
		t.Fatalf("first frame = %v, want packet 3 frame 50", got[0])
	}
}

// TestSeek_CorruptPageAfterSeekStops: a bad checksum on the first page read
// after a seek must stop the streamer and expose the error — no silence, no
// old buffer.
func TestSeek_CorruptPageAfterSeekStops(t *testing.T) {
	const packets, frameLen = 8, 100
	codec := newFakeOggCodec(0)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)

	// Corrupt a byte inside a later page's body.
	corrupt := bytes.Clone(data)
	// Locate the 5th "OggS" occurrence (4 audio pages in) and flip a body byte.
	pageOffsets := findPageOffsets(corrupt)
	targetPage := pageOffsets[4] // header page is [0]; audio pages follow
	corrupt[targetPage+40] ^= 0xFF

	d := newFakeDecoder(t, corrupt, dataStart, codec)
	// Target before the corrupted page: the seek lands on a valid page, then
	// streaming reaches the corrupt page and must stop with an error.
	if err := d.Seek(250); err != nil {
		t.Fatalf("Seek should land on a valid page before the corrupt one: %v", err)
	}
	buf := make([][2]float64, 400)
	for {
		_, ok := d.Stream(buf)
		if !ok {
			break
		}
	}
	if d.Err() == nil {
		t.Fatal("expected an error when streaming reaches the corrupt page")
	}
	if !errors.Is(d.Err(), errInvalidOggChecksum) {
		t.Fatalf("error = %v, want errInvalidOggChecksum", d.Err())
	}
}

// TestStream_CorruptPageStopsPlayback: corruption during ordinary playback
// (no seek) is likewise fatal.
func TestStream_CorruptPageStopsPlayback(t *testing.T) {
	const packets, frameLen = 6, 100
	codec := newFakeOggCodec(0)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	corrupt := bytes.Clone(data)
	pageOffsets := findPageOffsets(corrupt)
	corrupt[pageOffsets[3]+35] ^= 0xAA

	d := newFakeDecoder(t, corrupt, dataStart, codec)
	buf := make([][2]float64, 64)
	for {
		_, ok := d.Stream(buf)
		if !ok {
			break
		}
	}
	if !errors.Is(d.Err(), errInvalidOggChecksum) {
		t.Fatalf("error = %v, want checksum error", d.Err())
	}
}

// TestStream_CorruptPacketStopsPlayback: a structurally valid page whose
// packet fails to decode must also stop and expose the codec error.
func TestStream_CorruptPacketStopsPlayback(t *testing.T) {
	codec := newFakeOggCodec(0)
	b := newOggBuilder(0x4242)
	b.writeHeaderPage([]byte("FAKE-HEADER"))
	dataStart := int64(b.buf.Len())
	b.writePage(100, 0, [][]byte{fakePacketWith(0, 100)}, nil)
	b.writePage(200, 0, [][]byte{corruptPacket()}, nil)
	b.writePage(300, oggFlagEOS, [][]byte{fakePacketWith(2, 100)}, nil)

	d := newFakeDecoder(t, b.bytes(), dataStart, codec)
	got := readAllFrames(d)
	// Exactly packet 0's frames, then a hard stop.
	if len(got) != 100 {
		t.Fatalf("emitted %d frames, want 100 before the corrupt packet stops it", len(got))
	}
	if d.Err() == nil {
		t.Fatal("expected the corrupt packet to surface an error")
	}
}

// TestSeek_FailureLeavesStoppedStreamer: if no valid page exists before the
// target (truncated/corrupt landing zone), Seek returns an error and the
// streamer subsequently emits nothing.
func TestSeek_FailureLeavesStoppedStreamer(t *testing.T) {
	const packets, frameLen = 4, 100
	codec := newFakeOggCodec(0)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	ogr0, err := NewOggReader(bytes.NewReader(data), codec.sampleRate, codec.preSkip)
	if err != nil {
		t.Fatal(err)
	}
	ogr0.SetDataStart(dataStart)
	if err := ogr0.ScanLastGranule(); err != nil {
		t.Fatal(err)
	}
	lastGran := ogr0.LastGranule()

	// Flip a body byte inside every audio page so every page's CRC fails while
	// the headers (and thus the granule index) stay intact.
	wrecked := bytes.Clone(data)
	for _, off := range findPageOffsets(wrecked) {
		if off < dataStart {
			continue // keep the header page readable
		}
		segCount := int(wrecked[off+26])
		bodyOff := off + int64(27+segCount)
		wrecked[bodyOff] ^= 0xFF
	}

	d := newFakeDecoder(t, wrecked, dataStart, codec)
	if d.ogg.LastGranule() != lastGran {
		t.Fatalf("last granule changed: %d vs %d", d.ogg.LastGranule(), lastGran)
	}
	if err := d.Seek(150); err == nil {
		t.Fatal("expected seek to fail when every candidate page fails checksum")
	}
	buf := make([][2]float64, 16)
	n, ok := d.Stream(buf)
	if n != 0 || ok {
		t.Fatalf("failed seek left a producing streamer: n=%d ok=%v", n, ok)
	}
	if d.Err() == nil {
		t.Fatal("failed seek must leave an error recorded")
	}
}

// TestSeek_ReaderHalfPacketCleared exercises the OggReader boundary directly:
// a partial packet pending from sequential reading must be discarded by the
// next seek, and a continued fragment at the landing page dropped.
func TestSeek_ReaderHalfPacketCleared(t *testing.T) {
	const packets, frameLen = 6, 100
	codec := newFakeOggCodec(0)
	data, dataStart := buildFakeStream(t, codec, packets, frameLen)
	ogr := newTestOggReader(t, testOggFile{
		data: data, dataStart: dataStart, sampleRate: codec.sampleRate, preSkip: codec.preSkip,
	})

	page, err := ogr.ReadPage()
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if len(page.Packets) != 1 {
		t.Fatalf("got %d packets, want 1", len(page.Packets))
	}

	// Seek must clear any reader-level partial regardless of prior state.
	if err := ogr.SeekToGranule(350); err != nil {
		t.Fatalf("SeekToGranule: %v", err)
	}
	if ogr.partial != nil {
		t.Fatalf("seek left partial packet of %d bytes", len(ogr.partial))
	}
	page2, err := ogr.ReadPage()
	if err != nil {
		t.Fatalf("ReadPage after seek: %v", err)
	}
	if len(page2.Packets) != 1 {
		t.Fatalf("post-seek page has %d packets, want 1 complete packet", len(page2.Packets))
	}
}

// TestOggChecksum_KnownVector pins the CRC implementation against a hand-built
// page: flipping any byte must change the checksum.
func TestOggChecksum_ChangesWithContent(t *testing.T) {
	b := newOggBuilder(7)
	b.writePage(123, 0, [][]byte{[]byte("hello")}, nil)
	page := b.bytes()
	first := oggChecksum(withZeroChecksum(page))
	if first == 0 {
		t.Fatal("non-trivial page produced zero checksum")
	}
	// Stored checksum must equal the computed one.
	stored := le32(page[22:26])
	if stored != first {
		t.Fatalf("stored CRC %d != computed %d", stored, first)
	}
	tweaked := bytes.Clone(page)
	tweaked[len(tweaked)-1] ^= 0x01
	if oggChecksum(withZeroChecksum(tweaked)) == first {
		t.Fatal("checksum did not change after a body mutation")
	}
}

// --- helpers ---

func readFrames(t *testing.T, d *oggDecoder, count int) []float64 {
	t.Helper()
	out := make([]float64, 0, count)
	buf := make([][2]float64, 32)
	for len(out) < count {
		n, ok := d.Stream(buf)
		for i := range n {
			out = append(out, buf[i][0])
			if len(out) == count {
				break
			}
		}
		if !ok {
			t.Fatalf("stream ended after %d frames, wanted %d (err=%v)", len(out), count, d.Err())
		}
	}
	return out
}

func put32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func fakePacketWith(idx uint32, frames int) []byte {
	pkt := make([]byte, len(fakePacketPrefix)+8)
	copy(pkt, fakePacketPrefix)
	put32(pkt[len(fakePacketPrefix):], idx)
	put32(pkt[len(fakePacketPrefix)+4:], uint32(frames))
	return pkt
}

func findPageOffsets(data []byte) []int64 {
	var offs []int64
	for i := 0; i+4 <= len(data); {
		if bytes.Equal(data[i:i+4], []byte(oggMagic)) {
			offs = append(offs, int64(i))
		}
		i++
	}
	return offs
}

func withZeroChecksum(page []byte) []byte {
	out := bytes.Clone(page)
	for i := 22; i < 26; i++ {
		out[i] = 0
	}
	return out
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
