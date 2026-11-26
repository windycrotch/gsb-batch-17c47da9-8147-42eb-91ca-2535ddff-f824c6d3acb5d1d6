package wav_test

import (
	"bytes"
	"io"
	"sync"
	"testing"

	"github.com/qiniu/audio/wav"
)

// wavBuilder assembles a minimal RIFF/WAVE file in memory.
type wavBuilder struct {
	formatTag     uint16
	channels      uint16
	sampleRate    uint32
	bitsPerSample uint16
	blockAlign    uint16
	data          []byte
	// declaredData overrides the 'data' chunk size field (0 => len(data)).
	declaredData uint32
	// extraChunks are inserted between 'fmt ' and 'data'.
	extraChunks [][]byte
}

func (b wavBuilder) build() []byte {
	dataSize := uint32(len(b.data))
	if b.declaredData != 0 {
		dataSize = b.declaredData
	}
	ba := b.blockAlign
	if ba == 0 {
		ba = b.channels * b.bitsPerSample / 8
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
	put32(b.sampleRate * uint32(ba) * 8 / uint32(b.bitsPerSample)) // avg bytes/sec
	put16(ba)
	put16(b.bitsPerSample)
	for _, c := range b.extraChunks {
		buf.Write(c)
	}
	buf.WriteString("data")
	put32(dataSize)
	buf.Write(b.data)
	return buf.Bytes()
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return out
}

func TestPCM16FullReadAndLength(t *testing.T) {
	payload := make([]byte, 1000)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	raw := wavBuilder{formatTag: 1, channels: 1, sampleRate: 8000, bitsPerSample: 16, data: payload}.build()
	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if d.Length() != int64(len(payload)) {
		t.Fatalf("Length = %d, want %d", d.Length(), len(payload))
	}
	got := readAll(t, d)
	if !bytes.Equal(got, payload) {
		t.Fatal("full read mismatch")
	}
	// The next read must report io.EOF, and must not replay buffered data.
	n, err := d.Read(make([]byte, 16))
	if n != 0 || err != io.EOF {
		t.Fatalf("post-EOF Read = (%d, %v), want (0, EOF)", n, err)
	}
}

func TestPCM8FullRead(t *testing.T) {
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(255 - i)
	}
	raw := wavBuilder{formatTag: 1, channels: 2, sampleRate: 11025, bitsPerSample: 8, data: payload}.build()
	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if d.BytesPerSample() != 1 || d.Channels() != 2 {
		t.Fatalf("unexpected config: bps=%d ch=%d", d.BytesPerSample(), d.Channels())
	}
	if got := readAll(t, d); !bytes.Equal(got, payload) {
		t.Fatal("8-bit full read mismatch")
	}
}

func TestPCMTruncatedLength(t *testing.T) {
	payload := make([]byte, 500)
	// The header claims 1000 bytes but only 500 are present.
	raw := wavBuilder{
		formatTag: 1, channels: 1, sampleRate: 8000, bitsPerSample: 16,
		data: payload, declaredData: 1000,
	}.build()
	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if d.Length() != 500 {
		t.Fatalf("Length = %d, want 500 (actual readable bytes)", d.Length())
	}
	got := readAll(t, d)
	if len(got) != 500 || !bytes.Equal(got, payload) {
		t.Fatalf("truncated read got %d bytes, want 500", len(got))
	}
	if _, err := d.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("want io.EOF, got %v", err)
	}
}

func TestPCMSeekModesAndClamp(t *testing.T) {
	payload := make([]byte, 100)
	for i := range payload {
		payload[i] = byte(i + 1)
	}
	raw := wavBuilder{formatTag: 1, channels: 1, sampleRate: 8000, bitsPerSample: 16, data: payload}.build()
	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	check := func(off int64, whence int, wantPos int64) {
		t.Helper()
		pos, err := d.Seek(off, whence)
		if err != nil {
			t.Fatalf("Seek(%d,%d): %v", off, whence, err)
		}
		if pos != wantPos {
			t.Fatalf("Seek(%d,%d) = %d, want %d", off, whence, pos, wantPos)
		}
		buf := make([]byte, 4)
		n, err := d.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("Read after seek: %v", err)
		}
		if n > 0 && !bytes.Equal(buf[:n], payload[wantPos:wantPos+int64(n)]) {
			t.Fatalf("Read after Seek(%d,%d) returned wrong bytes", off, whence)
		}
	}

	check(10, io.SeekStart, 10)
	check(5, io.SeekCurrent, 19)  // previous check read 4 bytes, so current was 14
	check(-9, io.SeekCurrent, 14) // 19 + 4 read - 9
	check(0, io.SeekEnd, 100)
	check(-20, io.SeekEnd, 80)

	// Out-of-range seeks are clamped, not errored.
	check(999, io.SeekStart, 100)
	check(-50, io.SeekStart, 0)
	pos, err := d.Seek(50, io.SeekEnd)
	if err != nil || pos != 100 {
		t.Fatalf("SeekEnd overshoot = (%d,%v), want 100,nil", pos, err)
	}
	if n, err := d.Read(make([]byte, 4)); n != 0 || err != io.EOF {
		t.Fatalf("Read at end = (%d,%v), want (0,EOF)", n, err)
	}

	// Invalid whence still errors.
	if _, err := d.Seek(0, 42); err == nil {
		t.Fatal("expected error for invalid whence")
	}
}

func TestPCMConcurrentSeekRead(t *testing.T) {
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i)
	}
	raw := wavBuilder{formatTag: 1, channels: 2, sampleRate: 44100, bitsPerSample: 16, data: payload}.build()
	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	// Each individual Seek/Read must remain atomic and never produce an
	// out-of-range position, a non-EOF error or a panic under contention.
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			buf := make([]byte, 32)
			for i := 0; i < 200; i++ {
				off := int64((seed*131 + i*17) % len(payload))
				pos, err := d.Seek(off, io.SeekStart)
				if err != nil {
					t.Errorf("Seek: %v", err)
					return
				}
				if pos < 0 || pos > int64(len(payload)) {
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

	if d.Length() != int64(len(payload)) {
		t.Fatalf("Length changed after concurrency: %d", d.Length())
	}
}

func TestPCMSeekWithExtraChunks(t *testing.T) {
	// A 30-byte LIST chunk between 'fmt ' and 'data' moves the data start off
	// the usual 44-byte header; seeks must still land on the right samples.
	list := make([]byte, 30)
	copy(list, "LIST")
	list[4], list[5], list[6], list[7] = 22, 0, 0, 0 // payload size 22

	payload := make([]byte, 200)
	for i := range payload {
		payload[i] = byte(i * 3)
	}
	raw := wavBuilder{
		formatTag: 1, channels: 1, sampleRate: 8000, bitsPerSample: 16,
		data: payload, extraChunks: [][]byte{list},
	}.build()
	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if d.Length() != 200 {
		t.Fatalf("Length = %d, want 200", d.Length())
	}
	if pos, err := d.Seek(50, io.SeekStart); err != nil || pos != 50 {
		t.Fatalf("Seek = (%d,%v), want 50,nil", pos, err)
	}
	buf := make([]byte, 10)
	if _, err := io.ReadFull(d, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(buf, payload[50:60]) {
		t.Fatal("seek landed on the wrong position with a non-44-byte header")
	}
}

func TestPCMSeekCurrentAfterRead(t *testing.T) {
	payload := make([]byte, 64)
	for i := range payload {
		payload[i] = byte(i)
	}
	raw := wavBuilder{formatTag: 1, channels: 1, sampleRate: 8000, bitsPerSample: 16, data: payload}.build()
	d, err := wav.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	first := make([]byte, 12)
	if _, err := io.ReadFull(d, first); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	pos, err := d.Seek(0, io.SeekCurrent)
	if err != nil || pos != 12 {
		t.Fatalf("SeekCurrent = (%d,%v), want 12,nil", pos, err)
	}
	pos, err = d.Seek(-4, io.SeekCurrent)
	if err != nil || pos != 8 {
		t.Fatalf("relative Seek = (%d,%v), want 8,nil", pos, err)
	}
	rest := make([]byte, 8)
	if _, err := io.ReadFull(d, rest); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(rest, payload[8:16]) {
		t.Fatal("relative seek did not continue from the real current position")
	}
}
