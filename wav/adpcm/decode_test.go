package adpcm_test

import (
	"bytes"
	"io"
	"sync"
	"testing"

	"github.com/qiniu/audio/wav"
	_ "github.com/qiniu/audio/wav/adpcm"
)

type wavBuilder struct {
	formatTag     uint16
	channels      uint16
	sampleRate    uint32
	bitsPerSample uint16
	blockAlign    uint16
	data          []byte
	declaredData  uint32
	extraChunks   [][]byte
}

func (b wavBuilder) build() []byte {
	dataSize := uint32(len(b.data))
	if b.declaredData != 0 {
		dataSize = b.declaredData
	}
	fmtSize := uint32(16)
	var extraSize uint32
	for _, c := range b.extraChunks {
		extraSize += uint32(len(c))
	}
	riffSize := uint32(4+(8+fmtSize)+(8+dataSize)) + extraSize
	buf := new(bytes.Buffer)
	put32 := func(v uint32) {
		buf.WriteByte(byte(v))
		buf.WriteByte(byte(v >> 8))
		buf.WriteByte(byte(v >> 16))
		buf.WriteByte(byte(v >> 24))
	}
	put16 := func(v uint16) { buf.WriteByte(byte(v)); buf.WriteByte(byte(v >> 8)) }
	buf.WriteString("RIFF")
	put32(riffSize)
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	put32(fmtSize)
	put16(b.formatTag)
	put16(b.channels)
	put32(b.sampleRate)
	put32(b.sampleRate * uint32(b.blockAlign))
	put16(b.blockAlign)
	put16(b.bitsPerSample)
	for _, c := range b.extraChunks {
		buf.Write(c)
	}
	buf.WriteString("data")
	put32(dataSize)
	buf.Write(b.data)
	return buf.Bytes()
}

// adpcmBlock builds one block of blockAlign bytes with a predictor header
// and pseudo-random nibble payload.
func adpmBlock(channels, blockAlign int, seed byte) []byte {
	blk := make([]byte, blockAlign)
	blk[0] = seed
	blk[1] = seed ^ 0x55
	blk[2] = 20 // step index
	if channels == 2 {
		blk[4] = seed ^ 0x0f
		blk[5] = seed ^ 0x33
		blk[6] = 30
	}
	for i := channels * 4; i < blockAlign; i++ {
		blk[i] = seed*byte(i) + byte(i>>3) ^ seed
	}
	return blk
}

func adpcmFile(channels, blockAlign, blocks int, extra int) []byte {
	var data []byte
	for i := 0; i < blocks; i++ {
		data = append(data, adpmBlock(channels, blockAlign, byte(i+1))...)
	}
	data = append(data, make([]byte, extra)...) // trailing partial block
	return wavBuilder{
		formatTag: 0x11, channels: uint16(channels), sampleRate: 8000,
		bitsPerSample: 4, blockAlign: uint16(blockAlign), data: data,
	}.build()
}

func decodeAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return out
}

func samplesPerBlock(channels, blockAlign int) int {
	return (blockAlign << 1) - channels*7
}

func TestADPCMLengthExcludesPartialBlock(t *testing.T) {
	const channels, blockAlign, blocks, extra = 1, 36, 4, 10
	raw := adpcmFile(channels, blockAlign, blocks, extra)
	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	spb := samplesPerBlock(channels, blockAlign)
	wantLen := int64(blocks * spb * 2)
	if d.Length() != wantLen {
		t.Fatalf("Length = %d, want %d (complete blocks only)", d.Length(), wantLen)
	}
}

func TestADPCMFullReadThenEOFNoReplay(t *testing.T) {
	const channels, blockAlign, blocks = 2, 256, 3
	raw := adpcmFile(channels, blockAlign, blocks, 0)
	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	out := decodeAll(t, d)
	spb := samplesPerBlock(channels, blockAlign)
	if len(out) != blocks*spb*2 {
		t.Fatalf("decoded bytes = %d, want %d", len(out), blocks*spb*2)
	}
	// No zero-length garbage and no replay: repeated reads return io.EOF.
	for i := 0; i < 3; i++ {
		n, err := d.Read(make([]byte, 64))
		if n != 0 || err != io.EOF {
			t.Fatalf("post-EOF Read #%d = (%d,%v), want (0,EOF)", i, n, err)
		}
	}
}

func TestADPCMTruncatedBlockNoPanic(t *testing.T) {
	const channels, blockAlign, blocks, extra = 1, 36, 2, 7
	raw := adpcmFile(channels, blockAlign, blocks, extra)
	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	// Must not panic; complete blocks come through, then clean EOF.
	out := decodeAll(t, d)
	spb := samplesPerBlock(channels, blockAlign)
	minWant := blocks * spb * 2
	if len(out) < minWant {
		t.Fatalf("decoded %d bytes, want at least %d from complete blocks", len(out), minWant)
	}
	if n, err := d.Read(make([]byte, 8)); n != 0 || err != io.EOF {
		t.Fatalf("final Read = (%d,%v), want (0,EOF)", n, err)
	}
}

func TestADPCMTruncatedHeaderOnly(t *testing.T) {
	// A fragment too short even for the predictor header yields no sample,
	// only EOF, without panicking.
	data := adpmBlock(1, 36, 1)[:3] // keep just 3 bytes
	raw := wavBuilder{
		formatTag: 0x11, channels: 1, sampleRate: 8000,
		bitsPerSample: 4, blockAlign: 36, data: data,
	}.build()
	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if d.Length() != 0 {
		t.Fatalf("Length = %d, want 0 (no complete block)", d.Length())
	}
	out := decodeAll(t, d)
	if len(out) != 0 {
		t.Fatalf("decoded %d bytes from header-only fragment, want 0", len(out))
	}
}

func TestADPCMSeekModesAndClamp(t *testing.T) {
	const channels, blockAlign, blocks = 1, 36, 5
	raw := adpcmFile(channels, blockAlign, blocks, 0)
	baseline, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode baseline: %v", err)
	}
	reference := decodeAll(t, baseline)

	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	length := d.Length()
	blockBytes := int64(samplesPerBlock(channels, blockAlign) * 2)

	checkSeekRead := func(off int64, whence int, wantPos int64) {
		t.Helper()
		pos, serr := d.Seek(off, whence)
		if serr != nil {
			t.Fatalf("Seek(%d,%d): %v", off, whence, serr)
		}
		if pos != wantPos {
			t.Fatalf("Seek(%d,%d) = %d, want %d", off, whence, pos, wantPos)
		}
		buf := make([]byte, 40)
		n, rerr := d.Read(buf)
		if rerr != nil && rerr != io.EOF {
			t.Fatalf("Read after seek: %v", rerr)
		}
		if !bytes.Equal(buf[:n], reference[pos:pos+int64(n)]) {
			t.Fatalf("Read after Seek(%d,%d) does not match a fresh decode at that position", off, whence)
		}
	}

	// Block boundary.
	checkSeekRead(2*blockBytes, io.SeekStart, 2*blockBytes)
	// Inside the same block: first byte must be the correct in-block sample.
	// Each check also reads 40 bytes, so the next SeekCurrent starts 40 bytes
	// after the position asserted here.
	checkSeekRead(10, io.SeekStart, 10)
	checkSeekRead(6, io.SeekCurrent, 56)
	checkSeekRead(-16, io.SeekCurrent, 80)
	checkSeekRead(0, io.SeekEnd, length)
	checkSeekRead(-blockBytes, io.SeekEnd, length-blockBytes)

	// Clamping on both ends.
	checkSeekRead(1<<30, io.SeekStart, length)
	checkSeekRead(-(1 << 30), io.SeekStart, 0)
	pos, err := d.Seek(length+50, io.SeekStart)
	if err != nil || pos != length {
		t.Fatalf("overshoot Seek = (%d,%v), want %d,nil", pos, err, length)
	}
	if n, rerr := d.Read(make([]byte, 4)); n != 0 || rerr != io.EOF {
		t.Fatalf("Read at end = (%d,%v), want (0,EOF)", n, rerr)
	}

	// Seek to an unaligned target: returned position is frame-aligned and the
	// following bytes match the reference decode.
	pos, err = d.Seek(blockBytes+3, io.SeekStart)
	if err != nil {
		t.Fatalf("unaligned Seek: %v", err)
	}
	if pos != blockBytes+2 { // mono frame = 2 bytes
		t.Fatalf("unaligned Seek pos = %d, want %d", pos, blockBytes+2)
	}
	buf := make([]byte, 10)
	n, _ := d.Read(buf)
	if !bytes.Equal(buf[:n], reference[pos:pos+int64(n)]) {
		t.Fatal("unaligned seek produced wrong samples")
	}

	if _, err := d.Seek(0, 42); err == nil {
		t.Fatal("expected error for invalid whence")
	}
}

func TestADPCMSeekThenReadToEnd(t *testing.T) {
	const channels, blockAlign, blocks = 2, 256, 4
	raw := adpcmFile(channels, blockAlign, blocks, 0)
	reference := decodeAll(t, mustDecode(t, raw))

	d := mustDecode(t, raw)
	blockBytes := int64(samplesPerBlock(channels, blockAlign) * 2)
	if pos, err := d.Seek(blockBytes, io.SeekStart); err != nil || pos != blockBytes {
		t.Fatalf("Seek = (%d,%v)", pos, err)
	}
	rest := decodeAll(t, d)
	if !bytes.Equal(rest, reference[blockBytes:]) {
		t.Fatal("read-after-seek tail does not match reference decode")
	}
}

func TestADPCMConcurrentSeekRead(t *testing.T) {
	const channels, blockAlign, blocks = 1, 36, 8
	raw := adpcmFile(channels, blockAlign, blocks, 0)
	d := mustDecode(t, raw)
	length := d.Length()

	// Internal synchronization makes each individual Seek/Read atomic, so
	// under contention the stream must never report an out-of-range position,
	// an out-of-range read, a non-EOF error or panic. Pairwise seek→read
	// consistency across goroutines is the caller's job (serialization), and
	// is covered separately by TestADPCMSeekThenReadToEnd.
	var wg sync.WaitGroup
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			buf := make([]byte, 16)
			for i := 0; i < 300; i++ {
				off := int64((seed*977 + i*53) % int(length))
				pos, err := d.Seek(off, io.SeekStart)
				if err != nil {
					t.Errorf("Seek: %v", err)
					return
				}
				if pos < 0 || pos > length {
					t.Errorf("illegal position %d", pos)
					return
				}
				n, err := d.Read(buf)
				if err != nil && err != io.EOF {
					t.Errorf("Read: %v", err)
					return
				}
				if n < 0 || n > len(buf) {
					t.Errorf("illegal read count %d", n)
					return
				}
			}
		}(g + 1)
	}
	wg.Wait()

	// The stream must still be usable and report its length correctly after
	// the concurrent hammering.
	if d.Length() != length {
		t.Fatalf("Length changed after concurrency: %d != %d", d.Length(), length)
	}
	pos, err := d.Seek(0, io.SeekStart)
	if err != nil || pos != 0 {
		t.Fatalf("final Seek = (%d,%v), want 0,nil", pos, err)
	}
}

func mustDecode(t *testing.T, raw []byte) interface {
	io.ReadSeeker
	Length() int64
} {
	t.Helper()
	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return d
}

func TestADPCMSeekWithExtraChunks(t *testing.T) {
	const channels, blockAlign, blocks = 1, 36, 4
	var data []byte
	for i := 0; i < blocks; i++ {
		data = append(data, adpmBlock(channels, blockAlign, byte(i+1))...)
	}
	list := make([]byte, 30)
	copy(list, "LIST")
	list[4], list[5], list[6], list[7] = 22, 0, 0, 0

	raw := wavBuilder{
		formatTag: 0x11, channels: channels, sampleRate: 8000,
		bitsPerSample: 4, blockAlign: blockAlign, data: data,
		extraChunks: [][]byte{list},
	}.build()
	reference := decodeAll(t, mustDecode(t, raw))

	d := mustDecode(t, raw)
	blockBytes := int64(samplesPerBlock(channels, blockAlign) * 2)
	pos, err := d.Seek(2*blockBytes, io.SeekStart)
	if err != nil || pos != 2*blockBytes {
		t.Fatalf("Seek = (%d,%v), want %d,nil", pos, err, 2*blockBytes)
	}
	out := decodeAll(t, d)
	if !bytes.Equal(out, reference[2*blockBytes:]) {
		t.Fatal("seek with a non-44-byte header landed on the wrong block")
	}
}

func TestADPCMZeroLength(t *testing.T) {
	// No data at all: Length is zero, every Read ends with io.EOF, Seek only
	// ever clamps to 0.
	raw := wavBuilder{
		formatTag: 0x11, channels: 1, sampleRate: 8000,
		bitsPerSample: 4, blockAlign: 36, data: nil,
	}.build()
	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if d.Length() != 0 {
		t.Fatalf("Length = %d, want 0", d.Length())
	}
	if n, rerr := d.Read(make([]byte, 8)); n != 0 || rerr != io.EOF {
		t.Fatalf("Read = (%d,%v), want (0,EOF)", n, rerr)
	}
	if pos, serr := d.Seek(10, io.SeekStart); serr != nil || pos != 0 {
		t.Fatalf("Seek = (%d,%v), want 0,nil", pos, serr)
	}
}
