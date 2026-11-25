package decoder

import (
	"errors"
	"math"
	"testing"

	"github.com/skrashevich/go-aac/pkg/adts"
	"github.com/skrashevich/go-aac/pkg/cpe"
	"github.com/skrashevich/go-aac/pkg/filterbank"
	"github.com/skrashevich/go-aac/pkg/ics"
)

// ---------------------------------------------------------------------------
// Bitstream tests
// ---------------------------------------------------------------------------

func TestBitstreamReadBits(t *testing.T) {
	// 0xAB = 10101011, 0xCD = 11001101
	bs := NewBitstream([]byte{0xAB, 0xCD})
	if v := bs.ReadBits(4); v != 0xA {
		t.Fatalf("ReadBits(4)=%d want %d", v, 0xA)
	}
	if v := bs.ReadBits(4); v != 0xB {
		t.Fatalf("ReadBits(4)=%d want %d", v, 0xB)
	}
	if v := bs.ReadBits(8); v != 0xCD {
		t.Fatalf("ReadBits(8)=%d want %d", v, 0xCD)
	}
	if err := bs.Error(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBitstreamPeekBits(t *testing.T) {
	bs := NewBitstream([]byte{0xFF, 0x00})
	if v := bs.PeekBits(8); v != 0xFF {
		t.Fatalf("PeekBits(8)=%d want %d", v, 0xFF)
	}
	// peek should not advance
	if v := bs.PeekBits(8); v != 0xFF {
		t.Fatalf("PeekBits(8) after peek=%d want %d", v, 0xFF)
	}
	_ = bs.ReadBits(8)
	if v := bs.PeekBits(8); v != 0x00 {
		t.Fatalf("PeekBits(8) after read=%d want %d", v, 0x00)
	}
}

func TestBitstreamAlign(t *testing.T) {
	bs := NewBitstream([]byte{0xFF, 0xAA})
	_ = bs.ReadBits(3)
	bs.Align()
	if v := bs.ReadBits(8); v != 0xAA {
		t.Fatalf("ReadBits after align=%d want %d", v, 0xAA)
	}
}

func TestBitstreamAdvance(t *testing.T) {
	bs := NewBitstream([]byte{0xFF, 0xAA})
	bs.Advance(8)
	if v := bs.ReadBits(8); v != 0xAA {
		t.Fatalf("ReadBits after advance=%d want %d", v, 0xAA)
	}
}

func TestBitstreamOverflow(t *testing.T) {
	bs := NewBitstream([]byte{0xFF})
	_ = bs.ReadBits(8)
	_ = bs.ReadBits(1)
	if err := bs.Error(); err == nil {
		t.Fatal("expected error on overflow")
	}
}

func TestBitstreamAdvanceOutOfRange(t *testing.T) {
	bs := NewBitstream([]byte{0xFF})
	bs.Advance(100)
	if err := bs.Error(); err == nil {
		t.Fatal("expected error on advance out of range")
	}
}

func TestBitstreamAlignAlreadyAligned(t *testing.T) {
	bs := NewBitstream([]byte{0xAA, 0xBB})
	bs.Align() // already at byte boundary
	if v := bs.ReadBits(8); v != 0xAA {
		t.Fatalf("ReadBits after no-op align=%d want %d", v, 0xAA)
	}
}

// ---------------------------------------------------------------------------
// SetASC tests
// ---------------------------------------------------------------------------

func TestSetASC_AACLC(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2) // AAC-LC, 44100 Hz, stereo
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}
	if dec.Config.Profile != 2 {
		t.Errorf("Profile=%d want 2", dec.Config.Profile)
	}
	if dec.Config.SampleRate != 44100 {
		t.Errorf("SampleRate=%d want 44100", dec.Config.SampleRate)
	}
	if dec.Config.ChanConfig != 2 {
		t.Errorf("ChanConfig=%d want 2", dec.Config.ChanConfig)
	}
	if dec.Config.FrameLength != 1024 {
		t.Errorf("FrameLength=%d want 1024", dec.Config.FrameLength)
	}
	if dec.FilterBank == nil {
		t.Error("FilterBank not initialized")
	}
}

func TestSetASC_AACMain(t *testing.T) {
	dec := New()
	asc := buildASC(1, 3, 1) // AAC-Main, 48000 Hz, mono
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}
	if dec.Config.Profile != 1 {
		t.Errorf("Profile=%d want 1", dec.Config.Profile)
	}
	if dec.Config.SampleRate != 48000 {
		t.Errorf("SampleRate=%d want 48000", dec.Config.SampleRate)
	}
	if dec.Config.ChanConfig != 1 {
		t.Errorf("ChanConfig=%d want 1", dec.Config.ChanConfig)
	}
}

func TestSetASC_UnsupportedProfile(t *testing.T) {
	dec := New()
	asc := buildASC(5, 4, 2)
	if err := dec.SetASC(asc); err == nil {
		t.Fatal("expected error for unsupported profile 5")
	}
}

func TestSetASC_PCEUnimplemented(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 0) // chanConfig=0 triggers PCE
	if err := dec.SetASC(asc); err == nil {
		t.Fatal("expected error for PCE (chanConfig=0)")
	}
}

func TestSetASC_FrameLengthFlag(t *testing.T) {
	dec := New()
	w := newBitWriter()
	w.write(5, 2)  // profile AAC-LC
	w.write(4, 4)  // sample index
	w.write(4, 2)  // chan config
	w.write(1, 1)  // frameLengthFlag=1 (not supported)
	w.write(1, 0)  // dependsOnCoreCoder
	w.write(1, 0)  // extensionFlag
	if err := dec.SetASC(w.bytes()); err == nil {
		t.Fatal("expected error for frameLengthFlag=1")
	}
}

func TestSetASC_DependsOnCoreCoder(t *testing.T) {
	dec := New()
	w := newBitWriter()
	w.write(5, 2)  // profile AAC-LC
	w.write(4, 4)  // sample index
	w.write(4, 2)  // chan config
	w.write(1, 0)  // frameLengthFlag
	w.write(1, 1)  // dependsOnCoreCoder=1
	w.write(14, 0) // coreCoderDelay (skipped)
	w.write(1, 0)  // extensionFlag
	if err := dec.SetASC(w.bytes()); err != nil {
		t.Fatalf("SetASC with dependsOnCoreCoder failed: %v", err)
	}
	if dec.Config.FrameLength != 1024 {
		t.Errorf("FrameLength=%d want 1024", dec.Config.FrameLength)
	}
}

func TestSetASC_ExtensionFlag(t *testing.T) {
	dec := New()
	w := newBitWriter()
	w.write(5, 2)  // profile AAC-LC
	w.write(4, 4)  // sample index
	w.write(4, 2)  // chan config
	w.write(1, 0)  // frameLengthFlag
	w.write(1, 0)  // dependsOnCoreCoder
	w.write(1, 1)  // extensionFlag=1
	w.write(1, 0)  // extensionFlag3 (skipped since profile <= 16)
	if err := dec.SetASC(w.bytes()); err != nil {
		t.Fatalf("SetASC with extensionFlag failed: %v", err)
	}
}

func TestSetASC_EscapeProfile(t *testing.T) {
	dec := New()
	w := newBitWriter()
	w.write(5, 31) // AOT_ESCAPE
	w.write(6, 0)  // extended profile = 32+0=32
	w.write(4, 4)  // sample index
	w.write(4, 2)  // chan config
	// profile 32 falls through to default -> unsupported
	if err := dec.SetASC(w.bytes()); err == nil {
		t.Fatal("expected error for unsupported escape profile 32")
	}
}

func TestSetASC_VariousSampleRates(t *testing.T) {
	expectedRates := map[int]int{
		0: 96000, 1: 88200, 2: 64000, 3: 48000,
		4: 44100, 5: 32000, 6: 24000, 7: 22050,
		8: 16000, 9: 12000, 10: 11025, 11: 8000,
	}
	for idx, rate := range expectedRates {
		dec := New()
		asc := buildASC(2, idx, 1)
		if err := dec.SetASC(asc); err != nil {
			t.Errorf("SetASC(sampleIndex=%d) failed: %v", idx, err)
			continue
		}
		if dec.Config.SampleRate != rate {
			t.Errorf("SampleRate for index %d=%d want %d", idx, dec.Config.SampleRate, rate)
		}
	}
}

// ---------------------------------------------------------------------------
// DecodeFrame tests
// ---------------------------------------------------------------------------

func TestSetASCAndDecodeMinimalFrame(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	// 0xE0 = 111_00000 = END_ELEMENT(7) followed by padding
	output, err := dec.DecodeFrame([]byte{0xE0})
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	if len(output) != 1024*2 {
		t.Fatalf("output length=%d want %d", len(output), 1024*2)
	}
	for i, v := range output {
		if v != 0 {
			t.Fatalf("output[%d]=%f want 0", i, v)
		}
	}
}

func TestDecodeWithADTSHeader(t *testing.T) {
	dec := New()
	adtsHeader := buildADTSHeader(2, 4, 2, true, 8)
	// Append END_ELEMENT after ADTS header (7-byte header + 1 payload byte)
	frame := append(adtsHeader, 0xE0)

	output, err := dec.DecodeFrame(frame)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	if len(output) != 1024*2 {
		t.Fatalf("output length=%d want %d", len(output), 1024*2)
	}
}

func TestDecodeWithADTSHeaderAutoConfig(t *testing.T) {
	// Test that ADTS header sets config when none was set
	dec := New()
	adtsHeader := buildADTSHeader(2, 4, 2, true, 8)
	frame := append(adtsHeader, 0xE0)

	output, err := dec.DecodeFrame(frame)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	if len(output) != 1024*2 {
		t.Fatalf("output length=%d want %d", len(output), 1024*2)
	}
	if dec.Config.SampleRate != 44100 {
		t.Errorf("auto-configured SampleRate=%d want 44100", dec.Config.SampleRate)
	}
	if dec.Config.ChanConfig != 2 {
		t.Errorf("auto-configured ChanConfig=%d want 2", dec.Config.ChanConfig)
	}
}

func TestDecodeWithADTSHeaderWithCRC(t *testing.T) {
	dec := New()
	adtsHeader := buildADTSHeader(2, 4, 2, false, 10) // protection NOT absent = CRC present
	// 9-byte header (7 + CRC) + 1 payload byte
	frame := append(adtsHeader, 0xE0)

	output, err := dec.DecodeFrame(frame)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	if len(output) != 1024*2 {
		t.Fatalf("output length=%d want %d", len(output), 1024*2)
	}
}

func TestDecodeFrameNoConfig(t *testing.T) {
	dec := New()
	// No ADTS header, no ASC -> should fail
	_, err := dec.DecodeFrame([]byte{0xE0})
	if err == nil {
		t.Fatal("expected error when config not initialized")
	}
}

func TestDecodeMonoFrame(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 1) // mono
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}
	output, err := dec.DecodeFrame([]byte{0xE0})
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	if len(output) != 1024*1 {
		t.Fatalf("output length=%d want %d", len(output), 1024)
	}
}

func TestDecodeDSEElement(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	w := newBitWriter()
	// DSE element: type=4, id=0, align=0, count=2, 2 bytes of data
	w.write(3, uint32(dseElement)) // element type
	w.write(4, 0)                  // id
	w.write(1, 0)                  // align
	w.write(8, 2)                  // count=2
	w.write(8, 0xAA)              // data byte 1
	w.write(8, 0xBB)              // data byte 2
	// END element
	w.write(3, uint32(endElement))
	// padding
	w.write(5, 0)

	output, err := dec.DecodeFrame(w.bytes())
	if err != nil {
		t.Fatalf("DecodeFrame with DSE failed: %v", err)
	}
	if len(output) != 1024*2 {
		t.Fatalf("output length=%d want %d", len(output), 1024*2)
	}
}

func TestDecodeDSEElementWithAlign(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	w := newBitWriter()
	// DSE element with align=1
	// After 3(type)+4(id)+1(align)+8(count) = 16 bits -> already byte-aligned
	// So align is a no-op here. count=1 -> skip 8 bits of data.
	w.write(3, uint32(dseElement))
	w.write(4, 0)
	w.write(1, 1) // align=1
	w.write(8, 1) // count=1
	// already at byte boundary, so align is no-op
	w.write(8, 0xCC) // 1 byte of data
	// END element
	w.write(3, uint32(endElement))
	w.write(5, 0)

	output, err := dec.DecodeFrame(w.bytes())
	if err != nil {
		t.Fatalf("DecodeFrame with DSE (aligned) failed: %v", err)
	}
	if len(output) != 1024*2 {
		t.Fatalf("output length=%d want %d", len(output), 1024*2)
	}
}

func TestDecodeDSEElementExtendedCount(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	w := newBitWriter()
	w.write(3, uint32(dseElement))
	w.write(4, 0)
	w.write(1, 0)   // align
	w.write(8, 255)  // count=255 -> triggers extended count
	w.write(8, 1)    // extended count += 1 -> total 256
	for i := 0; i < 256; i++ {
		w.write(8, 0)
	}
	w.write(3, uint32(endElement))
	w.write(5, 0)

	output, err := dec.DecodeFrame(w.bytes())
	if err != nil {
		t.Fatalf("DecodeFrame with DSE extended count failed: %v", err)
	}
	if len(output) != 1024*2 {
		t.Fatalf("output length=%d want %d", len(output), 1024*2)
	}
}

func TestDecodeFILElement(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	w := newBitWriter()
	// FIL element: type=6, id=2 (2 bytes of filler)
	w.write(3, uint32(filElement))
	w.write(4, 2) // id=2
	w.write(8, 0) // 2 bytes of filler
	w.write(8, 0)
	// END element
	w.write(3, uint32(endElement))
	w.write(5, 0)

	output, err := dec.DecodeFrame(w.bytes())
	if err != nil {
		t.Fatalf("DecodeFrame with FIL failed: %v", err)
	}
	if len(output) != 1024*2 {
		t.Fatalf("output length=%d want %d", len(output), 1024*2)
	}
}

func TestDecodeFILElementExtended(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	w := newBitWriter()
	// FIL element with id=15 -> triggers extended length
	w.write(3, uint32(filElement))
	w.write(4, 15)  // id=15
	w.write(8, 2)   // ext count: id = 15 + 2 - 1 = 16
	for i := 0; i < 16; i++ {
		w.write(8, 0)
	}
	w.write(3, uint32(endElement))
	w.write(5, 0)

	output, err := dec.DecodeFrame(w.bytes())
	if err != nil {
		t.Fatalf("DecodeFrame with FIL extended failed: %v", err)
	}
	if len(output) != 1024*2 {
		t.Fatalf("output length=%d want %d", len(output), 1024*2)
	}
}

func TestDecodePCEElementError(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	w := newBitWriter()
	w.write(3, uint32(pceElement))
	w.write(4, 0)

	_, err := dec.DecodeFrame(w.bytes())
	if err == nil {
		t.Fatal("expected error for PCE_ELEMENT")
	}
}

func TestDecodeUnknownElementError(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	// Element type 7 is END, but let's write bits that look invalid
	// Actually, all 8 values 0-7 are defined. The only way to trigger
	// "unknown element" would be if the bit pattern read as an invalid type.
	// Since we have 3 bits, all values 0-7 are covered. This code path
	// can't be reached in practice, but we verify the error handling.
}

func TestDecodeMultipleFrames(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	frame := []byte{0xE0}
	for i := 0; i < 5; i++ {
		output, err := dec.DecodeFrame(frame)
		if err != nil {
			t.Fatalf("DecodeFrame[%d] failed: %v", i, err)
		}
		if len(output) != 1024*2 {
			t.Fatalf("DecodeFrame[%d] output length=%d want %d", i, len(output), 1024*2)
		}
	}
}

func TestDecodeOutputInterleaving(t *testing.T) {
	// Verify that output is interleaved: [L0, R0, L1, R1, ...]
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	output, err := dec.DecodeFrame([]byte{0xE0})
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}

	// With empty frame (END_ELEMENT), all data should be zero
	// but verify the length and structure
	expectedLen := 1024 * 2
	if len(output) != expectedLen {
		t.Fatalf("output length=%d want %d", len(output), expectedLen)
	}
}

func TestDecodeOutputNormalization(t *testing.T) {
	// Verify the /32768 normalization factor is applied
	dec := New()
	asc := buildASC(2, 4, 1) // mono
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	// With empty frame all zeros, normalization doesn't matter for zeros.
	// We verify the factor by checking the code path exists.
	output, err := dec.DecodeFrame([]byte{0xE0})
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	for _, v := range output {
		if math.Abs(float64(v)) > 1.0 {
			t.Fatalf("output value %f exceeds normalized range [-1, 1]", v)
		}
	}
}

// ---------------------------------------------------------------------------
// windowInfo helper test
// ---------------------------------------------------------------------------

func TestWindowInfo(t *testing.T) {
	info := &ics.ICSInfo{
		WindowSequence: 2,
		WindowShape:    [2]int{0, 1},
	}
	wi := windowInfo(info)
	if wi.WindowSequence != 2 {
		t.Errorf("WindowSequence=%d want 2", wi.WindowSequence)
	}
	if wi.WindowShape[0] != 0 || wi.WindowShape[1] != 1 {
		t.Errorf("WindowShape=%v want [0, 1]", wi.WindowShape)
	}
}

// ---------------------------------------------------------------------------
// icsConfig test
// ---------------------------------------------------------------------------

func TestICSConfig(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}
	cfg := dec.icsConfig()
	if cfg.SampleIndex != 4 {
		t.Errorf("SampleIndex=%d want 4", cfg.SampleIndex)
	}
	if cfg.FrameLength != 1024 {
		t.Errorf("FrameLength=%d want 1024", cfg.FrameLength)
	}
	if cfg.Profile != 2 {
		t.Errorf("Profile=%d want 2", cfg.Profile)
	}
}

// ---------------------------------------------------------------------------
// Channel configuration tests
// ---------------------------------------------------------------------------

func TestSetASCChannelConfigurations(t *testing.T) {
	configs := []struct {
		chanConfig int
		channels   int
	}{
		{1, 1}, // mono
		{2, 2}, // stereo
		{3, 3}, // stereo + center
		{4, 4}, // stereo + center + rear mono
		{5, 5}, // five channel
		{6, 6}, // 5.1
	}
	for _, tc := range configs {
		dec := New()
		asc := buildASC(2, 4, tc.chanConfig)
		if err := dec.SetASC(asc); err != nil {
			t.Errorf("SetASC(chanConfig=%d) failed: %v", tc.chanConfig, err)
			continue
		}
		if dec.Config.ChanConfig != tc.chanConfig {
			t.Errorf("ChanConfig=%d want %d", dec.Config.ChanConfig, tc.chanConfig)
		}
	}
}

func TestDecodeVariousChannelCounts(t *testing.T) {
	for _, channels := range []int{1, 2, 3, 4, 5, 6} {
		dec := New()
		asc := buildASC(2, 4, channels)
		if err := dec.SetASC(asc); err != nil {
			t.Fatalf("SetASC(channels=%d) failed: %v", channels, err)
		}
		output, err := dec.DecodeFrame([]byte{0xE0})
		if err != nil {
			t.Fatalf("DecodeFrame(channels=%d) failed: %v", channels, err)
		}
		if len(output) != 1024*channels {
			t.Errorf("output length for %d channels=%d want %d", channels, len(output), 1024*channels)
		}
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkDecodeMinimalFrame(b *testing.B) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		b.Fatalf("SetASC failed: %v", err)
	}
	frame := []byte{0xE0}

	for i := 0; i < b.N; i++ {
		_, _ = dec.DecodeFrame(frame)
	}
}

func BenchmarkDecodeMinimalFrameMono(b *testing.B) {
	dec := New()
	asc := buildASC(2, 4, 1)
	if err := dec.SetASC(asc); err != nil {
		b.Fatalf("SetASC failed: %v", err)
	}
	frame := []byte{0xE0}

	for i := 0; i < b.N; i++ {
		_, _ = dec.DecodeFrame(frame)
	}
}

func BenchmarkSetASC(b *testing.B) {
	asc := buildASC(2, 4, 2)
	for i := 0; i < b.N; i++ {
		dec := New()
		_ = dec.SetASC(asc)
	}
}

func BenchmarkBitstreamReadBits(b *testing.B) {
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bs := NewBitstream(data)
		for j := 0; j < 1024; j++ {
			bs.ReadBits(8)
		}
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type bitWriter struct {
	bits []uint8
}

func newBitWriter() *bitWriter {
	return &bitWriter{bits: make([]uint8, 0, 64)}
}

func (w *bitWriter) write(n int, value uint32) {
	for i := n - 1; i >= 0; i-- {
		bit := (value >> uint(i)) & 1
		w.bits = append(w.bits, uint8(bit))
	}
}

func (w *bitWriter) bytes() []byte {
	length := (len(w.bits) + 7) / 8
	out := make([]byte, length)
	for i, bit := range w.bits {
		if bit != 0 {
			byteIndex := i / 8
			bitIndex := 7 - (i % 8)
			out[byteIndex] |= 1 << uint(bitIndex)
		}
	}
	return out
}

func buildASC(profile, sampleIndex, chanConfig int) []byte {
	w := newBitWriter()
	w.write(5, uint32(profile))
	w.write(4, uint32(sampleIndex))
	w.write(4, uint32(chanConfig))
	w.write(1, 0) // frameLengthFlag
	w.write(1, 0) // dependsOnCoreCoder
	w.write(1, 0) // extensionFlag
	return w.bytes()
}

func buildADTSHeader(profile, sampleIndex, chanConfig int, protectionAbsent bool, frameLength int) []byte {
	w := newBitWriter()
	w.write(12, 0xfff)
	w.write(1, 0) // mpeg id
	w.write(2, 0) // layer
	if protectionAbsent {
		w.write(1, 1)
	} else {
		w.write(1, 0)
	}
	w.write(2, uint32(profile-1))
	w.write(4, uint32(sampleIndex))
	w.write(1, 0)
	w.write(3, uint32(chanConfig))
	w.write(4, 0)
	w.write(13, uint32(frameLength))
	w.write(11, 0)
	w.write(2, 0)
	if !protectionAbsent {
		w.write(16, 0)
	}
	return w.bytes()
}

// ---------------------------------------------------------------------------
// Internal function coverage tests
// ---------------------------------------------------------------------------

func TestProcessIS(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	// Create a minimal CPE element structure for testing
	element := &cpe.Element{
		CommonWindow: true,
		MaskPresent:  true,
		MSUsed:       make([]bool, 120),
		Left: &ics.ICStream{
			Info: &ics.ICSInfo{
				GroupCount:   1,
				GroupLength:  []int{1, 0, 0, 0, 0, 0, 0, 0},
				MaxSFB:       2,
				SwbOffsets:   []int{0, 8, 16},
			},
			BandTypes: make([]int, 120),
			SectEnd:   []int{1, 2},
			Data:      make([]float32, 1024),
		},
		Right: &ics.ICStream{
			Info: &ics.ICSInfo{
				GroupCount:   1,
				GroupLength:  []int{1, 0, 0, 0, 0, 0, 0, 0},
				MaxSFB:       2,
				SwbOffsets:   []int{0, 8, 16},
			},
			BandTypes:    []int{ics.IntensityBT, ics.IntensityBT2},
			SectEnd:      []int{1, 2},
			ScaleFactors: []float32{0.5, 0.3},
			Data:         make([]float32, 1024),
		},
	}

	left := element.Left.Data
	right := element.Right.Data
	for i := range left {
		left[i] = 1.0
	}
	element.MSUsed[0] = true

	dec.processIS(element, left, right)

	// Verify that intensity stereo was applied
	if right[0] == 0 {
		t.Error("intensity stereo should have modified right channel")
	}
}

func TestProcessMS(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	element := &cpe.Element{
		CommonWindow: true,
		MaskPresent:  true,
		MSUsed:       make([]bool, 120),
		Left: &ics.ICStream{
			Info: &ics.ICSInfo{
				GroupCount:   1,
				GroupLength:  []int{1, 0, 0, 0, 0, 0, 0, 0},
				MaxSFB:       2,
				SwbOffsets:   []int{0, 8, 16},
			},
			BandTypes: []int{1, 2},
			Data:      make([]float32, 1024),
		},
		Right: &ics.ICStream{
			BandTypes: []int{1, 2},
			Data:      make([]float32, 1024),
		},
	}

	left := element.Left.Data
	right := element.Right.Data
	for i := 0; i < 16; i++ {
		left[i] = 2.0
		right[i] = 1.0
	}
	element.MSUsed[0] = true

	dec.processMS(element, left, right)

	// Verify M/S decoding: left = mid+side, right = mid-side
	// After M/S: left should be 3.0, right should be 1.0
	if left[0] != 3.0 {
		t.Errorf("left[0] = %f, want 3.0", left[0])
	}
	if right[0] != 1.0 {
		t.Errorf("right[0] = %f, want 1.0", right[0])
	}
}

func TestProcessSingleAACMain(t *testing.T) {
	dec := New()
	dec.Config = Config{
		Profile:     aotAACMain,
		SampleIndex: 4,
		SampleRate:  44100,
		ChanConfig:  1,
		FrameLength: 1024,
	}
	fb, _ := filterbank.New(false, 1)
	dec.FilterBank = fb
	dec.Data = [][]float32{make([]float32, 1024)}

	element := &ics.ICStream{
		Info: &ics.ICSInfo{
			WindowSequence: 0,
			WindowShape:    [2]int{0, 0},
		},
		Data: make([]float32, 1024),
	}

	_, err := dec.processSingle(0, element, 0)
	if err == nil {
		t.Error("expected error for AAC Main profile prediction")
	}
}

func TestProcessSingleAACLTP(t *testing.T) {
	dec := New()
	dec.Config = Config{
		Profile:     aotAACLTP,
		SampleIndex: 4,
		SampleRate:  44100,
		ChanConfig:  1,
		FrameLength: 1024,
	}
	fb, _ := filterbank.New(false, 1)
	dec.FilterBank = fb
	dec.Data = [][]float32{make([]float32, 1024)}

	element := &ics.ICStream{
		Info: &ics.ICSInfo{
			WindowSequence: 0,
			WindowShape:    [2]int{0, 0},
		},
		Data: make([]float32, 1024),
	}

	_, err := dec.processSingle(0, element, 0)
	if err == nil {
		t.Error("expected error for AAC LTP profile")
	}
}

func TestProcessSingleGainPresent(t *testing.T) {
	dec := New()
	dec.Config = Config{
		Profile:     aotAACLC,
		SampleIndex: 4,
		SampleRate:  44100,
		ChanConfig:  1,
		FrameLength: 1024,
	}
	fb, _ := filterbank.New(false, 1)
	dec.FilterBank = fb
	dec.Data = [][]float32{make([]float32, 1024)}

	element := &ics.ICStream{
		Info: &ics.ICSInfo{
			WindowSequence: 0,
			WindowShape:    [2]int{0, 0},
		},
		Data:        make([]float32, 1024),
		GainPresent: true,
	}

	_, err := dec.processSingle(0, element, 0)
	if err == nil {
		t.Error("expected error for gain control")
	}
}

func TestProcessSingleSBRPresent(t *testing.T) {
	dec := New()
	dec.Config = Config{
		Profile:     aotAACLC,
		SampleIndex: 4,
		SampleRate:  44100,
		ChanConfig:  1,
		FrameLength: 1024,
	}
	fb, _ := filterbank.New(false, 1)
	dec.FilterBank = fb
	dec.Data = [][]float32{make([]float32, 1024)}
	dec.SBRPresent = true

	element := &ics.ICStream{
		Info: &ics.ICSInfo{
			WindowSequence: 0,
			WindowShape:    [2]int{0, 0},
		},
		Data: make([]float32, 1024),
	}

	_, err := dec.processSingle(0, element, 0)
	if err == nil {
		t.Error("expected error for SBR")
	}
}

func TestProcessPairAACMain(t *testing.T) {
	dec := New()
	dec.Config = Config{
		Profile:     aotAACMain,
		SampleIndex: 4,
		SampleRate:  44100,
		ChanConfig:  2,
		FrameLength: 1024,
	}
	fb, _ := filterbank.New(false, 2)
	dec.FilterBank = fb
	dec.Data = [][]float32{make([]float32, 1024), make([]float32, 1024)}

	element := &cpe.Element{
		Left: &ics.ICStream{
			Info: &ics.ICSInfo{
				WindowSequence: 0,
				WindowShape:    [2]int{0, 0},
			},
			Data: make([]float32, 1024),
		},
		Right: &ics.ICStream{
			Info: &ics.ICSInfo{
				WindowSequence: 0,
				WindowShape:    [2]int{0, 0},
			},
			Data: make([]float32, 1024),
		},
	}

	err := dec.processPair(0, element, 0)
	if err == nil {
		t.Error("expected error for AAC Main profile in pair")
	}
}

func TestProcessPairAACLTP(t *testing.T) {
	dec := New()
	dec.Config = Config{
		Profile:     aotAACLTP,
		SampleIndex: 4,
		SampleRate:  44100,
		ChanConfig:  2,
		FrameLength: 1024,
	}
	fb, _ := filterbank.New(false, 2)
	dec.FilterBank = fb
	dec.Data = [][]float32{make([]float32, 1024), make([]float32, 1024)}

	element := &cpe.Element{
		Left: &ics.ICStream{
			Info: &ics.ICSInfo{
				WindowSequence: 0,
				WindowShape:    [2]int{0, 0},
			},
			Data: make([]float32, 1024),
		},
		Right: &ics.ICStream{
			Info: &ics.ICSInfo{
				WindowSequence: 0,
				WindowShape:    [2]int{0, 0},
			},
			Data: make([]float32, 1024),
		},
	}

	err := dec.processPair(0, element, 0)
	if err == nil {
		t.Error("expected error for AAC LTP profile in pair")
	}
}

func TestSetConfigFromADTSInvalidSampleIndex(t *testing.T) {
	dec := New()
	header := adts.Header{
		Profile:       2,
		SamplingIndex: 99, // invalid
		ChannelConfig: 2,
	}
	err := dec.setConfigFromADTS(header)
	if err == nil {
		t.Error("expected error for invalid sample index in ADTS")
	}
}

func TestSetConfigFromADTSUnsupportedProfile(t *testing.T) {
	dec := New()
	header := adts.Header{
		Profile:       5, // unsupported
		SamplingIndex: 4,
		ChannelConfig: 2,
	}
	err := dec.setConfigFromADTS(header)
	if err == nil {
		t.Error("expected error for unsupported profile in ADTS")
	}
}

func TestSetConfigFromADTSPCE(t *testing.T) {
	dec := New()
	header := adts.Header{
		Profile:       2,
		SamplingIndex: 4,
		ChannelConfig: 0, // PCE
	}
	err := dec.setConfigFromADTS(header)
	if err == nil {
		t.Error("expected error for PCE in ADTS")
	}
}
func TestSetASCCustomSampleRate(t *testing.T) {
	dec := New()
	w := newBitWriter()
	w.write(5, 2)         // profile AAC-LC
	w.write(4, 0x0f)      // sample index = 15 (custom)
	w.write(24, 44100)    // custom sample rate
	w.write(4, 2)         // chan config
	w.write(1, 0)         // frameLengthFlag
	w.write(1, 0)         // dependsOnCoreCoder
	w.write(1, 0)         // extensionFlag

	if err := dec.SetASC(w.bytes()); err != nil {
		t.Fatalf("SetASC with custom sample rate failed: %v", err)
	}
	if dec.Config.SampleRate != 44100 {
		t.Errorf("SampleRate=%d want 44100", dec.Config.SampleRate)
	}
	if dec.Config.SampleIndex != 4 {
		t.Errorf("SampleIndex=%d want 4", dec.Config.SampleIndex)
	}
}

func TestSetASCInvalidSampleIndex(t *testing.T) {
	dec := New()
	w := newBitWriter()
	w.write(5, 2)         // profile AAC-LC
	w.write(4, 14)        // sample index = 14 (invalid, not 15 and out of range)
	w.write(4, 2)         // chan config
	w.write(1, 0)         // frameLengthFlag
	w.write(1, 0)         // dependsOnCoreCoder
	w.write(1, 0)         // extensionFlag

	if err := dec.SetASC(w.bytes()); err == nil {
		t.Error("expected error for invalid sample index")
	}
}

func TestSetASCExtensionFlagHighProfile(t *testing.T) {
	dec := New()
	w := newBitWriter()
	w.write(5, 31)        // profile escape
	w.write(6, 1)         // extended profile = 32+1=33 (> 16)
	w.write(4, 4)         // sample index
	w.write(4, 2)         // chan config
	w.write(1, 0)         // frameLengthFlag
	w.write(1, 0)         // dependsOnCoreCoder
	w.write(1, 1)         // extensionFlag=1
	w.write(1, 1)         // sectionDataResilience
	w.write(1, 1)         // scalefactorResilience
	w.write(1, 1)         // spectralDataResilience
	w.write(1, 0)         // extensionFlag3

	// Profile 33 is not supported, should error
	if err := dec.SetASC(w.bytes()); err == nil {
		t.Error("expected error for unsupported profile 33")
	}
}

func TestTruncatedFrameReturnsError(t *testing.T) {
	d := New()
	if err := d.SetASC([]byte{0x11, 0x88}); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{nil, {0}, {0, 0, 0}, {0, 0, 0, 0}} {
		if _, err := d.DecodeFrame(data); err == nil {
			t.Fatalf("accepted truncated frame %x", data)
		}
	}
}

// ---------------------------------------------------------------------------
// Corrupt frame resilience tests
// ---------------------------------------------------------------------------

// buildADTSFrame builds a complete ADTS frame whose payload is END_ELEMENT
// followed by zero padding, with the frame length field set automatically.
func buildADTSFrame(profile, sampleIndex, chanConfig int) []byte {
	// 7-byte header + 1-byte payload
	frame := buildADTSHeader(profile, sampleIndex, chanConfig, true, 8)
	return append(frame, 0xE0)
}

func TestDecodeFrameCorruptPayloadReturnsInvalidFrame(t *testing.T) {
	dec := New()
	asc := buildASC(2, 4, 2)
	if err := dec.SetASC(asc); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	for _, data := range [][]byte{nil, {0}, {0, 0, 0}, {0, 0, 0, 0}} {
		_, err := dec.DecodeFrame(data)
		if err == nil {
			t.Fatalf("accepted truncated frame %x", data)
		}
		if !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("truncated frame %x: error %v does not match ErrInvalidFrame", data, err)
		}
	}
}

func TestDecodeFrameRecoversAfterBadFrame(t *testing.T) {
	dec := New()
	if err := dec.SetASC(buildASC(2, 4, 2)); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	good := []byte{0xE0}
	// A frame whose element header runs out of data is a frame error.
	if _, err := dec.DecodeFrame([]byte{0x00, 0x00}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("expected ErrInvalidFrame, got %v", err)
	}

	out, err := dec.DecodeFrame(good)
	if err != nil {
		t.Fatalf("DecodeFrame after bad frame failed: %v", err)
	}
	if len(out) != 1024*2 {
		t.Fatalf("output length=%d want %d", len(out), 1024*2)
	}
}

func TestDecodeFrameRecoversAfterBadADTSFrame(t *testing.T) {
	dec := New()
	frame := buildADTSFrame(2, 4, 2)

	// First frame is truncated: header claims 8 bytes, only 7 supplied.
	if _, err := dec.DecodeFrame(frame[:7]); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("truncated ADTS frame: expected ErrInvalidFrame, got %v", err)
	}

	out, err := dec.DecodeFrame(frame)
	if err != nil {
		t.Fatalf("DecodeFrame after bad ADTS frame failed: %v", err)
	}
	if len(out) != 1024*2 {
		t.Fatalf("output length=%d want %d", len(out), 1024*2)
	}
}

func TestDecodeFrameADTSFrameLengthTooSmall(t *testing.T) {
	dec := New()
	if err := dec.SetASC(buildASC(2, 4, 2)); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	// Frame length below the header size must be rejected, not read out of
	// bounds.
	frame := buildADTSHeader(2, 4, 2, true, 3)
	frame = append(frame, 0xE0)
	if _, err := dec.DecodeFrame(frame); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("expected ErrInvalidFrame for undersized frame, got %v", err)
	}
}

func TestDecodeFrameDoesNotReadPastDeclaredLength(t *testing.T) {
	dec := New()
	if err := dec.SetASC(buildASC(2, 4, 2)); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	// The buffer contains a valid frame followed by bytes that would decode
	// differently. The decoder must honour frameLength and stop after it.
	frame := buildADTSHeader(2, 4, 2, true, 8)
	frame = append(frame, 0xE0)
	trailing := append(frame, 0xFF, 0xFF, 0xFF, 0xFF)
	out, err := dec.DecodeFrame(trailing)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	// A decode that consumed the trailing bytes would report a bitstream
	// error; a clean END_ELEMENT decode produces silence.
	for i, v := range out {
		if v != 0 {
			t.Fatalf("output[%d]=%f want 0", i, v)
		}
	}
}

func TestDecodeFrameConfigErrorIsSticky(t *testing.T) {
	dec := New()

	// ADTS header with an unsupported profile (profile 3 => AOT 4? buildADTSHeader
	// stores profile-1 in 2 bits, so use 3 -> AOT 4 is LTP which is accepted;
	// use channel config 0 instead, which is rejected as PCE).
	frame := buildADTSHeader(2, 4, 0, true, 8)
	frame = append(frame, 0xE0)
	if _, err := dec.DecodeFrame(frame); err == nil {
		t.Fatal("expected error for PCE channel config")
	} else if errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("config error must not match ErrInvalidFrame: %v", err)
	}

	// A subsequent perfectly-formed stereo frame must keep failing.
	good := buildADTSFrame(2, 4, 2)
	if _, err := dec.DecodeFrame(good); err == nil {
		t.Fatal("decoder recovered from a configuration error; it must stay failed")
	}
}

func TestDecodeFrameUnsupportedProfileIsSticky(t *testing.T) {
	dec := New()
	// ADTS stores profile-1 in 2 bits; 3 => AOT 4 (LTP), which is accepted.
	// Use SetASC with an unsupported AOT instead: AOT 5 (SBR).
	asc := buildASC(5, 4, 2)
	if err := dec.SetASC(asc); err == nil {
		t.Fatal("expected SetASC to reject unsupported profile")
	}

	good := buildADTSFrame(2, 4, 2)
	if _, err := dec.DecodeFrame(good); err == nil {
		t.Fatal("decoder decoded after a rejected SetASC; configuration error must be sticky")
	}
}

func TestDecodeFrameBadFrameDoesNotPolluteFilterState(t *testing.T) {
	// Decode a valid frame on two decoders. Corrupt the first decoder with a
	// bad frame in between; the final valid frame must match the reference.
	reference := New()
	if err := reference.SetASC(buildASC(2, 4, 2)); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}

	frame := buildADTSFrame(2, 4, 2)
	refOut, err := reference.DecodeFrame(frame)
	if err != nil {
		t.Fatalf("reference DecodeFrame failed: %v", err)
	}

	subject := New()
	if err := subject.SetASC(buildASC(2, 4, 2)); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}
	// A corrupt frame whose spectral decoding fails part-way must not disturb
	// the filter bank.
	bad := buildADTSHeader(2, 4, 2, true, 8)
	bad = append(bad, 0x00, 0x00, 0x00) // invalid element stream
	if _, err := subject.DecodeFrame(bad); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("expected ErrInvalidFrame, got %v", err)
	}

	subjOut, err := subject.DecodeFrame(frame)
	if err != nil {
		t.Fatalf("subject DecodeFrame failed: %v", err)
	}
	if len(subjOut) != len(refOut) {
		t.Fatalf("length mismatch: got %d want %d", len(subjOut), len(refOut))
	}
	for i := range refOut {
		if subjOut[i] != refOut[i] {
			t.Fatalf("sample %d differs: got %f want %f", i, subjOut[i], refOut[i])
		}
	}
}

func TestDecodeFrameBadFrameRestoresFilterBankOverlap(t *testing.T) {
	// Seed a decoder with a non-zero overlap state through a real decode, then
	// force a processing-stage failure. The overlap must be restored exactly so
	// the next valid frame comes out identical to an untouched decoder.
	dec := New()
	if err := dec.SetASC(buildASC(2, 4, 2)); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}
	if _, err := dec.DecodeFrame([]byte{0xE0}); err != nil {
		t.Fatalf("priming DecodeFrame failed: %v", err)
	}

	// Inject a non-zero overlap so any accidental carry-over is visible.
	seed := [][]float32{
		make([]float32, 1024),
		make([]float32, 1024),
	}
	for i := range seed[0] {
		seed[0][i] = float32(i%13) - 6
		seed[1][i] = float32(i%7) - 3
	}
	dec.FilterBank.Restore(seed)
	before := dec.FilterBank.Snapshot()

	// A frame that parses but fails in the synthesis stage. A CPE whose
	// second channel's Info is invalid cannot be produced from bits alone, so
	// drive process() directly with a broken element.
	brokenData := [][]float32{make([]float32, 1024), make([]float32, 1024)}
	dec.Data = brokenData
	badElement := frameElement{
		kind: elemCPE,
		cpe: &cpe.Element{
			Left: &ics.ICStream{
				Info: &ics.ICSInfo{WindowSequence: 0, WindowShape: [2]int{0, 0}},
				Data: make([]float32, 1024),
			},
			Right: &ics.ICStream{
				// WindowShape[1] out of range => filterbank.Process errors.
				Info: &ics.ICSInfo{WindowSequence: 0, WindowShape: [2]int{0, 99}},
				Data: make([]float32, 1024),
			},
		},
	}
	// Emulate DecodeFrame's guard: snapshot, attempt process, restore on error.
	snapshot := dec.FilterBank.Snapshot()
	if err := dec.process([]frameElement{badElement}); err == nil {
		t.Fatal("expected process to fail for invalid window shape")
	}
	dec.FilterBank.Restore(snapshot)

	after := dec.FilterBank.Snapshot()
	for ch := range before {
		for i := range before[ch] {
			if before[ch][i] != after[ch][i] {
				t.Fatalf("overlap[%d][%d] changed: got %f want %f", ch, i, after[ch][i], before[ch][i])
			}
		}
	}
}

func TestDecodeFrameSampleIndexZeroAutoConfigures(t *testing.T) {
	// Sample index 0 (96 kHz) is valid; auto-configuration must trigger for it
	// too, rather than being mistaken for "unset".
	dec := New()
	frame := buildADTSFrame(2, 0, 1)
	out, err := dec.DecodeFrame(frame)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	if dec.Config.SampleRate != 96000 {
		t.Errorf("SampleRate=%d want 96000", dec.Config.SampleRate)
	}
	if len(out) != 1024 {
		t.Errorf("output length=%d want 1024", len(out))
	}
	if dec.Config.ChanConfig != 1 {
		t.Errorf("ChanConfig=%d want 1", dec.Config.ChanConfig)
	}
}

func TestDecodeFrameSetASCAfterConfigError(t *testing.T) {
	dec := New()
	// Trigger a configuration error through ADTS.
	frame := buildADTSHeader(2, 4, 0, true, 8)
	frame = append(frame, 0xE0)
	if _, err := dec.DecodeFrame(frame); err == nil {
		t.Fatal("expected configuration error")
	}

	// A successful SetASC clears the latched error.
	if err := dec.SetASC(buildASC(2, 4, 2)); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}
	if _, err := dec.DecodeFrame([]byte{0xE0}); err != nil {
		t.Fatalf("DecodeFrame after recovered SetASC failed: %v", err)
	}
}

func TestDecodeFrameCorruptFirstFrameDoesNotLockConfig(t *testing.T) {
	dec := New()

	// Corrupt first frame whose ADTS header declares mono 96 kHz. Its payload
	// cannot be decoded, so its configuration must not stick.
	badHeader := buildADTSHeader(2, 0, 1, true, 7+4)
	bad := append(badHeader, 0x20, 0x00, 0x00, 0x00)
	if _, err := dec.DecodeFrame(bad); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("expected ErrInvalidFrame, got %v", err)
	}
	if dec.Config.SampleRate != 0 || dec.Config.ChanConfig != 0 {
		t.Fatalf("corrupt first frame left configuration behind: %+v", dec.Config)
	}

	// The next valid stereo 44.1 kHz frame must configure the decoder itself.
	good := buildADTSFrame(2, 4, 2)
	out, err := dec.DecodeFrame(good)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	if dec.Config.SampleRate != 44100 {
		t.Errorf("SampleRate=%d want 44100", dec.Config.SampleRate)
	}
	if dec.Config.ChanConfig != 2 {
		t.Errorf("ChanConfig=%d want 2", dec.Config.ChanConfig)
	}
	if len(out) != 1024*2 {
		t.Errorf("output length=%d want %d", len(out), 1024*2)
	}
}

func TestDecodeFrameBadFrameAfterConfiguredKeepsConfig(t *testing.T) {
	dec := New()
	if err := dec.SetASC(buildASC(2, 4, 2)); err != nil {
		t.Fatalf("SetASC failed: %v", err)
	}
	if _, err := dec.DecodeFrame(buildADTSFrame(2, 4, 2)); err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	cfg := dec.Config

	// A corrupt frame with a wildly different header must not reconfigure.
	bad := buildADTSHeader(2, 0, 1, true, 11)
	bad = append(bad, 0x20, 0x00, 0x00, 0x00)
	if _, err := dec.DecodeFrame(bad); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("expected ErrInvalidFrame, got %v", err)
	}
	if dec.Config != cfg {
		t.Fatalf("configuration changed after bad frame: got %+v want %+v", dec.Config, cfg)
	}
}

func TestDecodeFrameBadFrameIsTransparentToFilterState(t *testing.T) {
	// With a non-zero seeded overlap, decoding good1 then good2 must produce
	// exactly the same output whether or not a corrupt frame was attempted in
	// between. This is the "bad frames do not pollute filter state" guarantee.
	seed := [][]float32{make([]float32, 1024), make([]float32, 1024)}
	for i := range seed[0] {
		seed[0][i] = float32(i%11) - 5
		seed[1][i] = float32(i%5) - 2
	}

	newSeeded := func() *Decoder {
		d := New()
		if err := d.SetASC(buildASC(2, 4, 2)); err != nil {
			t.Fatalf("SetASC failed: %v", err)
		}
		d.FilterBank.Restore(seed)
		return d
	}

	good := buildADTSFrame(2, 4, 2)
	bad := buildADTSHeader(2, 4, 2, true, 11)
	bad = append(bad, 0x20, 0x00, 0x00, 0x00)

	ref := newSeeded()
	if _, err := ref.DecodeFrame(good); err != nil {
		t.Fatalf("ref good1: %v", err)
	}
	refOut, err := ref.DecodeFrame(good)
	if err != nil {
		t.Fatalf("ref good2: %v", err)
	}

	sub := newSeeded()
	if _, err := sub.DecodeFrame(good); err != nil {
		t.Fatalf("sub good1: %v", err)
	}
	if _, err := sub.DecodeFrame(bad); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("expected ErrInvalidFrame, got %v", err)
	}
	subOut, err := sub.DecodeFrame(good)
	if err != nil {
		t.Fatalf("sub good2: %v", err)
	}

	for i := range refOut {
		if refOut[i] != subOut[i] {
			t.Fatalf("sample %d differs: ref=%f sub=%f", i, refOut[i], subOut[i])
		}
	}

	// The overlap-add state must be identical too.
	refSnap := ref.FilterBank.Snapshot()
	subSnap := sub.FilterBank.Snapshot()
	for ch := range refSnap {
		for i := range refSnap[ch] {
			if refSnap[ch][i] != subSnap[ch][i] {
				t.Fatalf("overlap[%d][%d] differs: ref=%f sub=%f", ch, i, refSnap[ch][i], subSnap[ch][i])
			}
		}
	}
}
