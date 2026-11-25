package player

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
)

const oggMagic = "OggS"

// Ogg page header type flags (RFC 3533, section 6).
const (
	oggFlagContinued  byte = 0x01 // page contains a continued packet from the previous page
	oggFlagBOS        byte = 0x02 // first page of the logical bitstream
	oggFlagEOS        byte = 0x04 // last page of the logical bitstream
	oggPageHeaderSize      = 27
)

var (
	errInvalidOggMagic    = errors.New("ogg: invalid capture pattern")
	errInvalidOggVersion  = errors.New("ogg: unsupported version")
	errInvalidOggChecksum = errors.New("ogg: invalid page checksum")
	errOggSeekFailed      = errors.New("ogg: seek failed, no valid page found before target")
)

// oggCRC32 is the CRC-32 polynomial mandated by RFC 3533 (section A2):
// x^32 + x^28 + x^27 + ... + x^2 + x + 1, computed with zero initial value
// and no final XOR.
var oggCRC32 [256]uint32

func init() {
	for i := range oggCRC32 {
		crc := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
		oggCRC32[i] = crc
	}
}

// oggChecksum computes the Ogg CRC-32 over a complete serialized page
// (header with the checksum field zeroed, followed by the page body).
func oggChecksum(page []byte) uint32 {
	var crc uint32
	for _, b := range page {
		crc = (crc << 8) ^ oggCRC32[byte(crc>>24)^b]
	}
	return crc
}

// oggPageHeader represents the header of an Ogg page.
type oggPageHeader struct {
	HeaderType   byte
	GranulePos   int64
	SerialNumber uint32
	SequenceNum  uint32
	Checksum     uint32
	NumSegments  uint8
	SegmentTable []byte
}

// bodySize returns the total number of body bytes the segment table accounts for.
func (h *oggPageHeader) bodySize() int {
	var totalSize int
	for _, seg := range h.SegmentTable {
		totalSize += int(seg)
	}
	return totalSize
}

// continued reports whether the page starts with a packet continued from the
// previous page.
func (h *oggPageHeader) continued() bool {
	return h.HeaderType&oggFlagContinued != 0
}

// parseOggPageHeader reads and parses an Ogg page header from the reader.
func parseOggPageHeader(r io.Reader) (*oggPageHeader, error) {
	// Read fixed header (27 bytes)
	var buf [oggPageHeaderSize]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, err
	}

	// Check capture pattern "OggS"
	if string(buf[0:4]) != oggMagic {
		return nil, errInvalidOggMagic
	}

	// Check version (must be 0)
	if buf[4] != 0 {
		return nil, errInvalidOggVersion
	}

	hdr := &oggPageHeader{
		HeaderType:   buf[5],
		GranulePos:   int64(binary.LittleEndian.Uint64(buf[6:14])), //nolint:gosec // granule position is semantically signed (-1 valid) but stored as unsigned
		SerialNumber: binary.LittleEndian.Uint32(buf[14:18]),
		SequenceNum:  binary.LittleEndian.Uint32(buf[18:22]),
		Checksum:     binary.LittleEndian.Uint32(buf[22:26]),
		NumSegments:  buf[26],
	}

	// Read segment table
	if hdr.NumSegments > 0 {
		hdr.SegmentTable = make([]byte, hdr.NumSegments)
		if _, err := io.ReadFull(r, hdr.SegmentTable); err != nil {
			return nil, err
		}
	}

	return hdr, nil
}

// readOggPageBody reads the page body and extracts packets.
// Packets are delimited by segment sizes: a segment of 255 bytes continues
// to the next segment, while a segment < 255 terminates the packet.
// Returns complete packets and any partial packet that continues to the next page.
func readOggPageBody(r io.Reader, hdr *oggPageHeader) (packets [][]byte, partial []byte, err error) {
	// Calculate total body size
	totalSize := hdr.bodySize()

	// Read entire body
	body := make([]byte, totalSize)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, nil, err
	}

	return splitOggPageBody(hdr, body), trailingPartial(hdr, body), nil
}

// splitOggPageBody assembles the complete packets contained in a page body.
// Segments of 255 bytes continue the current packet; a segment < 255 closes it.
func splitOggPageBody(hdr *oggPageHeader, body []byte) (packets [][]byte) {
	var currentPacket []byte
	offset := 0

	for _, segSize := range hdr.SegmentTable {
		currentPacket = append(currentPacket, body[offset:offset+int(segSize)]...)
		offset += int(segSize)

		// Segment < 255 terminates the packet
		if segSize < 255 {
			packets = append(packets, currentPacket)
			currentPacket = nil
		}
	}

	return packets
}

// trailingPartial returns the unterminated packet left at the end of a page
// (the last segment was 255, so the packet continues on the next page), or nil.
func trailingPartial(hdr *oggPageHeader, body []byte) []byte {
	if len(hdr.SegmentTable) == 0 || hdr.SegmentTable[len(hdr.SegmentTable)-1] != 255 {
		return nil
	}

	// Walk the segments, remembering where the final run of 255-byte segments
	// begins: every segment < 255 before it closes a complete packet.
	start := 0
	pos := 0
	for _, segSize := range hdr.SegmentTable {
		pos += int(segSize)
		if segSize < 255 {
			start = pos
		}
	}
	if start >= len(body) {
		return nil
	}
	return body[start:]
}

// IsOpusCodec checks if an .ogg file contains Opus codec (not Vorbis).
// Returns true for Opus files, false for Vorbis or invalid files.
func IsOpusCodec(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	// Read first page header
	hdr, err := parseOggPageHeader(f)
	if err != nil {
		return false
	}

	// Read first packet (codec identification)
	packets, _, err := readOggPageBody(f, hdr)
	if err != nil || len(packets) == 0 {
		return false
	}

	// Check if it starts with "OpusHead"
	if len(packets[0]) < 8 {
		return false
	}
	return string(packets[0][:8]) == opusHeadMagic
}

// OggReader reads Ogg streams with seeking support.
// It is codec-agnostic - the caller is responsible for parsing codec headers.
type OggReader struct {
	r           io.ReadSeeker
	fileSize    int64
	dataStart   int64 // byte offset where audio pages begin
	lastGranule int64 // cached from last page
	sampleRate  int   // for duration calculation
	preSkip     int   // samples to skip at start (Opus only, 0 for Vorbis)
	serial      uint32
	hasSerial   bool // the stream may legitimately use serial number 0

	partial []byte // partial packet from previous page (continues on next page)

	// discardContinuation is set by a seek: the first page read may start with
	// the tail of a packet that began before the seek point. That tail must be
	// dropped instead of being joined into the data of the new position.
	discardContinuation bool
}

// NewOggReader creates a new OggReader from a seekable stream.
// sampleRate and preSkip are needed for duration/seeking calculations.
// The caller is responsible for parsing codec headers before calling this.
func NewOggReader(r io.ReadSeeker, sampleRate, preSkip int) (*OggReader, error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	return &OggReader{
		r:          r,
		fileSize:   size,
		sampleRate: sampleRate,
		preSkip:    preSkip,
	}, nil
}

// SetDataStart sets the byte offset where audio data begins.
// Called after header parsing is complete.
func (o *OggReader) SetDataStart(offset int64) {
	o.dataStart = offset
}

// SampleRate returns the sample rate used for duration calculations.
func (o *OggReader) SampleRate() int {
	return o.sampleRate
}

// PreSkip returns the number of samples to skip at the start.
func (o *OggReader) PreSkip() int {
	return o.preSkip
}

// OggPage represents a decoded Ogg page with its audio packets.
type OggPage struct {
	GranulePos int64
	HeaderType byte
	Packets    [][]byte
	ByteOffset int64
}

// ReadPage reads the next Ogg page from the stream.
// Handles packets that span multiple pages by joining partial data.
// Returns io.EOF when no more pages are available. A page with a bad capture
// pattern, unsupported version or wrong checksum returns an error: callers
// must stop instead of silently consuming corrupt data.
func (o *OggReader) ReadPage() (*OggPage, error) {
	offset, err := o.r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}

	hdr, body, err := o.readValidatedPage()
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, io.EOF
		}
		return nil, err
	}

	packets := splitOggPageBody(hdr, body)
	partial := trailingPartial(hdr, body)

	// A seek can land on a page that starts with the tail of a packet from
	// before the seek point. Discard that tail; it must never be joined into
	// a packet of the new position.
	if o.discardContinuation {
		o.discardContinuation = false
		if hdr.continued() {
			packets, partial = o.dropContinuedHead(hdr, body)
		}
	}

	// Join a partial packet carried over from the previous page.
	if len(o.partial) > 0 {
		switch {
		case len(packets) > 0:
			// Join partial with first complete packet
			packets[0] = append(o.partial, packets[0]...)
		case partial != nil:
			// No complete packets, join partial with new partial
			partial = append(o.partial, partial...)
		default:
			// A page that claims no continuation but has no terminator: keep
			// the carried bytes as their own packet rather than dropping them.
			packets = append(packets, o.partial)
		}
		o.partial = nil
	}

	// Store new partial for next page
	o.partial = partial

	return &OggPage{
		GranulePos: hdr.GranulePos,
		HeaderType: hdr.HeaderType,
		Packets:    packets,
		ByteOffset: offset,
	}, nil
}

// dropContinuedHead removes the leading continued-packet fragment from a page
// landed on by a seek. It returns the complete packets and trailing partial
// that remain after the fragment is dropped.
func (o *OggReader) dropContinuedHead(hdr *oggPageHeader, body []byte) (packets [][]byte, partial []byte) {
	pos := 0
	for i, segSize := range hdr.SegmentTable {
		pos += int(segSize)
		if segSize < 255 {
			// This segment closes the stale packet; the rest of the page
			// belongs to packets starting at or after the seek point.
			remaining := &oggPageHeader{SegmentTable: hdr.SegmentTable[i+1:]}
			rest := body[pos:]
			return splitOggPageBody(remaining, rest), trailingPartial(remaining, rest)
		}
	}
	// The whole page was one continued fragment that never terminates here.
	return nil, nil
}

// readValidatedPage reads a full page (header + body) and verifies its
// checksum and logical stream serial number.
func (o *OggReader) readValidatedPage() (*oggPageHeader, []byte, error) {
	hdr, err := parseOggPageHeader(o.r)
	if err != nil {
		return nil, nil, err
	}
	if o.hasSerial && hdr.SerialNumber != o.serial {
		return nil, nil, errInvalidOggMagic
	}

	bodySize := hdr.bodySize()
	body := make([]byte, bodySize)
	if _, err := io.ReadFull(o.r, body); err != nil {
		return nil, nil, err
	}

	// Re-serialize the header with a zeroed checksum field and verify CRC over
	// header + body, exactly as RFC 3533 defines it.
	page := make([]byte, oggPageHeaderSize+len(hdr.SegmentTable)+bodySize)
	copy(page[0:4], oggMagic)
	page[4] = 0
	page[5] = hdr.HeaderType
	binary.LittleEndian.PutUint64(page[6:14], uint64(hdr.GranulePos)) //nolint:gosec // granule is semantically signed
	binary.LittleEndian.PutUint32(page[14:18], hdr.SerialNumber)
	binary.LittleEndian.PutUint32(page[18:22], hdr.SequenceNum)
	// page[22:26] checksum stays zero
	page[26] = hdr.NumSegments
	copy(page[27:], hdr.SegmentTable)
	copy(page[oggPageHeaderSize+len(hdr.SegmentTable):], body)

	if hdr.Checksum != oggChecksum(page) {
		return nil, nil, errInvalidOggChecksum
	}

	return hdr, body, nil
}

// Reset seeks back to the start of audio data.
func (o *OggReader) Reset() error {
	o.partial = nil // Clear any partial packet
	o.discardContinuation = false
	_, err := o.r.Seek(o.dataStart, io.SeekStart)
	return err
}

// ScanLastGranule finds the granule position of the last page.
// This should be called after header parsing to enable duration calculation.
func (o *OggReader) ScanLastGranule() error {
	// Seek near end of file and scan for last page
	searchSize := min(int64(65536), o.fileSize) // Search last 64KB

	if _, err := o.r.Seek(o.fileSize-searchSize, io.SeekStart); err != nil {
		return err
	}

	// Read and scan for "OggS" magic
	buf := make([]byte, searchSize)
	n, err := io.ReadFull(o.r, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	buf = buf[:n]

	// Find last "OggS" occurrence
	lastOggS := -1
	for i := len(buf) - 4; i >= 0; i-- {
		if string(buf[i:i+4]) == oggMagic {
			lastOggS = i
			break
		}
	}

	if lastOggS == -1 {
		return errors.New("ogg: no page found at end of file")
	}

	// Parse granule position from that header
	if lastOggS+14 > len(buf) {
		return errors.New("ogg: incomplete last page header")
	}
	o.lastGranule = int64(binary.LittleEndian.Uint64(buf[lastOggS+6 : lastOggS+14])) //nolint:gosec // granule position is defined as unsigned but used as signed for duration calculations

	// Remember the logical stream's serial number so seeks can reject pages
	// from other (e.g. chained) bitstreams.
	if lastOggS+18 <= len(buf) {
		o.serial = binary.LittleEndian.Uint32(buf[lastOggS+14 : lastOggS+18])
		o.hasSerial = true
	}

	return nil
}

// Duration returns the total number of audio samples (excluding pre-skip).
func (o *OggReader) Duration() int64 {
	return o.lastGranule - int64(o.preSkip)
}

// LastGranule returns the absolute granule position of the stream's last page.
func (o *OggReader) LastGranule() int64 {
	return o.lastGranule
}

// SeekToGranule seeks to the latest page whose granule position is not later
// than the target granule position, and prepares the reader so the first
// ReadPage after it returns complete packets only: any packet fragment a
// continued page inherited from before the seek point is dropped.
//
// A target at or past the end converges to end-of-stream (the next ReadPage
// returns io.EOF); a non-positive target converges to the first audio page.
// A target for which no valid page can be found returns an error.
func (o *OggReader) SeekToGranule(target int64) error {
	// Every seek starts from clean packet state: a half packet from the page
	// the seek abandoned must never survive into the new position.
	o.partial = nil
	o.discardContinuation = false

	// Handle seek to start
	if target <= 0 {
		return o.Reset()
	}

	// Target beyond the last page: converge to the end instead of replaying
	// the final pages.
	if o.lastGranule > 0 && target >= o.lastGranule {
		_, err := o.r.Seek(o.fileSize, io.SeekStart)
		return err
	}

	// Bisection search
	low := o.dataStart
	high := o.fileSize

	bestOffset := o.dataStart
	found := false

	for high-low > 4096 { // Stop when range is small enough
		mid := (low + high) / 2

		offset, granule, err := o.findPageNear(mid)
		if err != nil {
			// No valid page found in upper half, search lower
			high = mid
			continue
		}

		if granule <= target {
			// This page is valid, remember it and search higher
			bestOffset = offset
			found = true
			low = offset + 1
		} else {
			// This page is past target, search lower
			high = mid
		}
	}

	// Linear scan from best known position to find exact page
	scanStart := bestOffset
	if !found {
		// Small files never enter the bisection loop, or bisection found
		// nothing: scan from the start of the audio data.
		scanStart = o.dataStart
	}
	if _, err := o.r.Seek(scanStart, io.SeekStart); err != nil {
		return err
	}

	bestOffset = scanStart
	found = false
	validPageSeen := false
	for {
		offset, hdr, err := o.readPageAtCurrent()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			// A corrupt page where the seek wanted to land is fatal.
			return err
		}
		validPageSeen = true

		if hdr.GranulePos > target {
			break
		}

		if hdr.GranulePos >= 0 { // -1 means no granule
			bestOffset = offset
			found = true
		} else if !found {
			// A granule-less page before any anchored one is still a valid
			// landing spot: its packets decode and the decoder trims by
			// granule once an anchored page follows.
			bestOffset = offset
		}
	}

	if !found {
		if !validPageSeen {
			// The whole landing zone is unreadable: do not guess a position.
			return errOggSeekFailed
		}
		// Every valid page starts after the target (typical for tiny
		// single-page files): land on the first audio page and trim inside
		// the decoder.
		if _, err := o.r.Seek(scanStart, io.SeekStart); err != nil {
			return err
		}
		o.discardContinuation = true
		return nil
	}

	// Seek to best page found and arm continuation dropping for the first read.
	if _, err := o.r.Seek(bestOffset, io.SeekStart); err != nil {
		return err
	}
	o.discardContinuation = true
	return nil
}

// readPageAtCurrent parses and fully validates (serial + checksum) the page at
// the current read position and skips over its body, returning the page's byte
// offset and header.
func (o *OggReader) readPageAtCurrent() (int64, *oggPageHeader, error) {
	offset, err := o.r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, nil, err
	}

	hdr, _, err := o.readValidatedPage()
	if err != nil {
		return 0, nil, err
	}
	return offset, hdr, nil
}

// findPageNear finds a valid Ogg page starting at or after the given offset.
// Returns the page's byte offset and granule position.
func (o *OggReader) findPageNear(offset int64) (pageOffset, granule int64, err error) {
	if _, err := o.r.Seek(offset, io.SeekStart); err != nil {
		return 0, 0, err
	}

	// Read a chunk and scan for "OggS"
	buf := make([]byte, 4096)
	n, readErr := o.r.Read(buf)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return 0, 0, readErr
	}
	buf = buf[:n]

	for i := range len(buf) - oggPageHeaderSize {
		if string(buf[i:i+4]) != oggMagic || buf[i+4] != 0 {
			continue
		}
		if o.hasSerial && binary.LittleEndian.Uint32(buf[i+14:i+18]) != o.serial {
			continue
		}
		pageOffset = offset + int64(i)
		granule = int64(binary.LittleEndian.Uint64(buf[i+6 : i+14])) //nolint:gosec // granule position is semantically signed
		return pageOffset, granule, nil
	}

	return 0, 0, errors.New("ogg: no page found")
}
