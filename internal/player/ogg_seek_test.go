package player

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

// This file builds self-contained Ogg test fixtures: no ffmpeg, no real
// codec. A deterministic fake codec (fakeOggCodec) turns packets whose first
// bytes encode a packet index into recognizable PCM, so tests can assert on
// the exact decoded output and playback position. The Ogg pages themselves
// are real RFC 3533 pages with correct checksums.

// fakePacketPrefix marks synthetic audio packets for fakeOggCodec.
const fakePacketPrefix = "FAKEPKT"

// errFakeCorruptPacket is returned by the fake codec for packets explicitly
// marked corrupt.
var errFakeCorruptPacket = errors.New("fake codec: corrupt packet marker")

// fakeOggCodec is a deterministic OggCodec stand-in. Every non-corrupt packet
// decodes to fixedFrames frames, one frame per packet index so the decoded
// sample values identify which packet produced them. Packet wire format:
//
//	"FAKEPKT" | uint32 LE packetIndex | uint32 LE frames | optional 0xEE tail
//
// A packet starting with "CORRUPT" fails to decode, modelling a bad frame.
type fakeOggCodec struct {
	channels    int
	sampleRate  int
	preSkip     int
	priming     int // extra leading frames the first decode after a Reset emits
	resetCalled int
	headerDone  bool
	fixedFrames int
	primed      bool
}

func newFakeOggCodec(preSkip int) *fakeOggCodec {
	return &fakeOggCodec{channels: 1, sampleRate: 48000, preSkip: preSkip, fixedFrames: 100}
}

func (c *fakeOggCodec) SampleRate() int { return c.sampleRate }
func (c *fakeOggCodec) Channels() int   { return c.channels }
func (c *fakeOggCodec) PreSkip() int    { return c.preSkip }

func (c *fakeOggCodec) GranuleToSamples(granule int64) int64 {
	return granule - int64(c.preSkip)
}

func (c *fakeOggCodec) AddHeaderPacket(packet []byte) (bool, error) {
	if packet == nil {
		return c.headerDone, nil
	}
	c.headerDone = true
	return true, nil
}

func (c *fakeOggCodec) Decode(packet []byte, pcm []float32) (int, error) {
	if bytes.HasPrefix(packet, []byte("CORRUPT")) {
		return 0, errFakeCorruptPacket
	}
	if len(packet) < len(fakePacketPrefix)+8 {
		return 0, fmt.Errorf("fake codec: short packet %d bytes", len(packet))
	}
	idx := binary.LittleEndian.Uint32(packet[len(fakePacketPrefix):])
	frames := int(binary.LittleEndian.Uint32(packet[len(fakePacketPrefix)+4:]))
	if frames == 0 {
		frames = c.fixedFrames
	}

	// Model decoder priming (e.g. Vorbis): the first decode after Reset emits
	// c.priming extra leading frames at negative sample indices.
	priming := 0
	if c.priming > 0 && !c.primed {
		priming = c.priming
		c.primed = true
	}
	total := frames + priming
	if total*c.channels > len(pcm) {
		return 0, fmt.Errorf("fake codec: pcm buffer too small: %d > %d", total*c.channels, len(pcm))
	}
	for f := range total {
		// Negative values mark priming frames; real frames keep their identity.
		if f < priming {
			pcm[f] = float32(-(f + 1))
			continue
		}
		pcm[f] = float32(int(idx)*10000 + (f - priming))
	}
	return total, nil
}

func (c *fakeOggCodec) Reset() error {
	c.resetCalled++
	c.primed = false
	return nil
}

// fakePacket builds a synthetic audio packet for packet index idx.
func fakePacket(idx uint32) []byte {
	pkt := make([]byte, len(fakePacketPrefix)+8)
	copy(pkt, fakePacketPrefix)
	binary.LittleEndian.PutUint32(pkt[len(fakePacketPrefix):], idx)
	binary.LittleEndian.PutUint32(pkt[len(fakePacketPrefix)+4:], 100)
	return pkt
}

// corruptPacket builds a packet the fake codec refuses to decode.
func corruptPacket() []byte { return []byte("CORRUPT-packet") }

// oggBuilder assembles Ogg pages, honoring packet splitting across pages and
// continuation flags. Every emitted page has a valid RFC 3533 CRC.
type oggBuilder struct {
	buf     bytes.Buffer
	serial  uint32
	nextSeq uint32
}

func newOggBuilder(serial uint32) *oggBuilder {
	return &oggBuilder{serial: serial}
}

// writeHeaderPage writes a BOS page carrying an identification packet the
// caller's codec will accept as its header.
func (b *oggBuilder) writeHeaderPage(headerPacket []byte) {
	b.writePage(0, oggFlagBOS, [][]byte{headerPacket}, nil)
}

// writePage serializes one page. continuation is the tail of a packet split
// on the previous page; it becomes the page's first segment with the
// continued flag set.
func (b *oggBuilder) writePage(granule int64, flags byte, packets [][]byte, continuation []byte) {
	var segments []byte
	var body []byte

	if len(continuation) > 0 {
		flags |= oggFlagContinued
		segments = appendSegments(segments, continuation)
		body = append(body, continuation...)
	}
	for _, pkt := range packets {
		segments = appendSegments(segments, pkt)
		body = append(body, pkt...)
	}

	header := make([]byte, 27+len(segments))
	copy(header[0:4], oggMagic)
	header[4] = 0
	header[5] = flags
	binary.LittleEndian.PutUint64(header[6:14], uint64(granule)) //nolint:gosec // test granule
	binary.LittleEndian.PutUint32(header[14:18], b.serial)
	binary.LittleEndian.PutUint32(header[18:22], b.nextSeq)
	header[26] = byte(len(segments)) //nolint:gosec // test pages have few segments
	copy(header[27:], segments)
	page := append(header, body...)
	binary.LittleEndian.PutUint32(page[22:26], oggChecksum(page))
	b.buf.Write(page)
	b.nextSeq++
}

// appendSegments appends the lacing values for one packet.
func appendSegments(segments []byte, pkt []byte) []byte {
	remaining := len(pkt)
	for remaining >= 255 {
		segments = append(segments, 255)
		remaining -= 255
	}
	return append(segments, byte(remaining))
}

// writeContinuationPage serializes a page made of one packet fragment: when
// terminated is false every lacing segment is 255 (the packet continues on
// the next page); when true the final segment closes the packet.
func (b *oggBuilder) writeContinuationPage(granule int64, fragment []byte, terminated bool) {
	var segments []byte
	if terminated {
		segments = appendSegments(segments, fragment)
	} else {
		// Force an unterminated packet: 255-byte segments covering the whole
		// fragment, no closing segment.
		remaining := fragment
		for len(remaining) >= 255 {
			segments = append(segments, 255)
			remaining = remaining[255:]
		}
		if len(remaining) > 0 {
			segments = append(segments, 255)
			// Pad the fragment so the last segment really holds 255 bytes.
			pad := 255 - len(remaining)
			fragment = append(append([]byte(nil), fragment...), make([]byte, pad)...)
		}
	}
	flags := oggFlagContinued
	header := make([]byte, 27+len(segments))
	copy(header[0:4], oggMagic)
	header[5] = flags
	binary.LittleEndian.PutUint64(header[6:14], uint64(granule)) //nolint:gosec // test granule
	binary.LittleEndian.PutUint32(header[14:18], b.serial)
	binary.LittleEndian.PutUint32(header[18:22], b.nextSeq)
	header[26] = byte(len(segments)) //nolint:gosec // test pages have few segments
	copy(header[27:], segments)
	page := append(header, fragment...)
	binary.LittleEndian.PutUint32(page[22:26], oggChecksum(page))
	b.buf.Write(page)
	b.nextSeq++
}

// bytes returns the serialized stream.
func (b *oggBuilder) bytes() []byte { return b.buf.Bytes() }

// buildFakeStream writes a stream of nPackets audio packets, one packet per
// page, each packet decoding to frameLen frames. Granules are set so page i
// ends at public sample (i+1)*frameLen (+ preSkip in granule space). Returns
// the raw bytes and the data-start offset (after the single header page).
func buildFakeStream(t *testing.T, codec *fakeOggCodec, nPackets, frameLen int) ([]byte, int64) {
	t.Helper()
	b := newOggBuilder(0x4242)
	b.writeHeaderPage([]byte("FAKE-HEADER"))
	dataStart := int64(b.buf.Len())

	for i := range nPackets {
		pkt := make([]byte, len(fakePacketPrefix)+8)
		copy(pkt, fakePacketPrefix)
		binary.LittleEndian.PutUint32(pkt[len(fakePacketPrefix):], uint32(i))
		binary.LittleEndian.PutUint32(pkt[len(fakePacketPrefix)+4:], uint32(frameLen))
		granule := int64((i+1)*frameLen + codec.preSkip)
		flags := byte(0)
		if i == nPackets-1 {
			flags = oggFlagEOS
		}
		b.writePage(granule, flags, [][]byte{pkt}, nil)
	}
	return b.bytes(), dataStart
}

// newFakeDecoder wires an oggDecoder over an in-memory Ogg stream using the
// fake codec, mirroring decodeOgg's state without header parsing.
func newFakeDecoder(t *testing.T, data []byte, dataStart int64, codec *fakeOggCodec) *oggDecoder {
	t.Helper()
	r := bytes.NewReader(data)
	ogg, err := NewOggReader(r, codec.sampleRate, codec.preSkip)
	if err != nil {
		t.Fatalf("NewOggReader: %v", err)
	}
	ogg.SetDataStart(dataStart)
	if err := ogg.ScanLastGranule(); err != nil {
		t.Fatalf("ScanLastGranule: %v", err)
	}
	if _, err := r.Seek(dataStart, 0); err != nil {
		t.Fatalf("seek dataStart: %v", err)
	}
	d := &oggDecoder{
		ogg:        ogg,
		codec:      codec,
		closer:     ioNopCloser{},
		scratch:    make([]float32, 8192*codec.channels),
		pcmBuffer:  make([]float32, 0),
		totalLen:   ogg.Duration(),
		nextSample: -1,
	}
	return d
}

type ioNopCloser struct{}

func (ioNopCloser) Close() error { return nil }

// readAllFrames drains the decoder and returns every emitted frame value.
func readAllFrames(d *oggDecoder) []float64 {
	var out []float64
	buf := make([][2]float64, 64)
	for {
		n, ok := d.Stream(buf)
		for i := range n {
			out = append(out, buf[i][0])
		}
		if !ok {
			break
		}
	}
	return out
}

// expectedFrames returns the fake codec's sample values for packet idx.
func expectedFrames(idx, frameLen int) []float64 {
	out := make([]float64, frameLen)
	for f := range frameLen {
		out[f] = float64(idx*10000 + f)
	}
	return out
}
