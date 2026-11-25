package player

import (
	"errors"
	"io"

	"github.com/gopxl/beep/v2"
)

// decodeOgg decodes an Ogg stream (Opus or Vorbis) into a beep streamer.
func decodeOgg(rc io.ReadSeekCloser) (beep.StreamSeekCloser, beep.Format, error) {
	// Read first page to get the identification packet
	hdr, err := parseOggPageHeader(rc)
	if err != nil {
		return nil, beep.Format{}, err
	}
	packets, partial, err := readOggPageBody(rc, hdr)
	if err != nil {
		return nil, beep.Format{}, err
	}
	if len(packets) == 0 {
		return nil, beep.Format{}, errors.New("ogg: no packets in first page")
	}

	// Detect codec from first packet
	codec, err := detectOggCodec(packets[0])
	if err != nil {
		return nil, beep.Format{}, err
	}

	// Feed header packets until codec is ready
	// Track partial packets that span pages
	for {
		complete, err := codec.AddHeaderPacket(nil) // Check if already complete
		if err != nil {
			return nil, beep.Format{}, err
		}
		if complete {
			break
		}

		// Read more pages for headers
		hdr, err := parseOggPageHeader(rc)
		if err != nil {
			return nil, beep.Format{}, err
		}
		pagePackets, newPartial, err := readOggPageBody(rc, hdr)
		if err != nil {
			return nil, beep.Format{}, err
		}

		// Join partial from previous page with first packet/partial of this page
		if len(partial) > 0 {
			if len(pagePackets) > 0 {
				// Previous partial + first complete packet = one header
				pagePackets[0] = append(partial, pagePackets[0]...)
			} else if newPartial != nil {
				// Previous partial + new partial (still spanning)
				newPartial = append(partial, newPartial...)
			}
		}

		// Feed complete packets to codec
		for _, pkt := range pagePackets {
			complete, err = codec.AddHeaderPacket(pkt)
			if err != nil {
				return nil, beep.Format{}, err
			}
			if complete {
				break
			}
		}

		// Track new partial for next iteration
		partial = newPartial
	}

	// Record where audio data starts (after headers)
	dataStart, err := rc.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, beep.Format{}, err
	}

	// Create OggReader
	ogg, err := NewOggReader(rc, codec.SampleRate(), codec.PreSkip())
	if err != nil {
		return nil, beep.Format{}, err
	}
	ogg.SetDataStart(dataStart)
	if err := ogg.ScanLastGranule(); err != nil {
		return nil, beep.Format{}, err
	}
	// Seek back to audio start
	if _, err := rc.Seek(dataStart, io.SeekStart); err != nil {
		return nil, beep.Format{}, err
	}

	format := beep.Format{
		SampleRate:  beep.SampleRate(codec.SampleRate()),
		NumChannels: codec.Channels(),
		Precision:   2,
	}

	decoder := &oggDecoder{
		ogg:        ogg,
		codec:      codec,
		closer:     rc,
		scratch:    make([]float32, 8192*codec.Channels()),
		pcmBuffer:  make([]float32, 0),
		totalLen:   ogg.Duration(),
		nextSample: -1, // unknown until the first granule-anchored page is read
	}
	decoder.pcmPos = 0 // empty buffer triggers a page load

	return decoder, format, nil
}

// oggDecoder implements beep.StreamSeekCloser for Ogg streams.
//
// Playback position, decoder state and page-reading state are kept in sync.
// After a Seek, packets belonging to samples before the requested position are
// decoded (codecs need the lead-in) but discarded instead of emitted, and any
// packet fragment the landing page inherited from before the seek point is
// dropped by the OggReader before assembly. A decode or page-read error stops
// the streamer rather than being papered over with silence or stale buffered
// samples.
type oggDecoder struct {
	ogg    *OggReader
	codec  OggCodec
	closer io.Closer

	// scratch is the per-packet decode buffer pages are assembled into.
	// pcmBuffer holds one decoded page's worth of PCM, consumed frame by
	// frame; pcmPos is its interleaved read position.
	scratch   []float32
	pcmBuffer []float32
	pcmPos    int

	// pageStart is the public-sample-domain position of the first frame held
	// in pcmBuffer.
	pageStart int64

	// Seek recovery: while skipping is true, decoded frames at or before
	// targetSample are dropped.
	skipping     bool
	targetSample int64
	// nextSample is the public-sample-domain position of the first frame of
	// the next page loaded; -1 while no granule anchor has been seen yet.
	nextSample int64

	granulePos int64
	totalLen   int64
	err        error
}

// Stream reads audio samples into the provided buffer.
func (d *oggDecoder) Stream(samples [][2]float64) (n int, ok bool) {
	if d.err != nil {
		return 0, false
	}

	channels := d.codec.Channels()

	for n < len(samples) {
		// Consume already decoded PCM first.
		if d.pcmPos < len(d.pcmBuffer) {
			n = d.emitBuffered(samples, n, channels)
			continue
		}

		// The buffered page is exhausted: read and decode the next page,
		// which also performs seek-recovery trimming when skipping.
		if !d.loadNextPage() {
			return n, n > 0
		}
	}

	return n, true
}

// emitBuffered copies available PCM frames into the output buffer, advancing
// the public playback position for every frame actually emitted.
func (d *oggDecoder) emitBuffered(samples [][2]float64, n, channels int) int {
	for n < len(samples) && d.pcmPos < len(d.pcmBuffer) {
		if channels == 2 {
			samples[n][0] = float64(d.pcmBuffer[d.pcmPos])
			samples[n][1] = float64(d.pcmBuffer[d.pcmPos+1])
			d.pcmPos += 2
		} else {
			samples[n][0] = float64(d.pcmBuffer[d.pcmPos])
			samples[n][1] = float64(d.pcmBuffer[d.pcmPos])
			d.pcmPos++
		}
		n++
		d.granulePos++
	}
	return n
}

// loadNextPage reads the next Ogg page, decodes every packet of it into one
// PCM buffer so the page's sample range can be anchored to its granule
// position, and stages the PCM for consumption. It returns false when
// streaming must stop (EOF or a recorded error).
func (d *oggDecoder) loadNextPage() bool {
	page, err := d.ogg.ReadPage()
	if err != nil {
		if errors.Is(err, io.EOF) {
			if d.skipping {
				// Reached the end without reaching the target: converge on the
				// target and stop cleanly.
				d.granulePos = d.targetSample
				d.skipping = false
			}
			return false
		}
		d.fail(err)
		return false
	}

	// Decode the full page up front: a page's granule position marks the end
	// of its last complete packet, so the page's first frame can only be
	// located once its total frame count is known.
	channels := d.codec.Channels()
	decoded := make([]float32, 0, len(page.Packets)*1024*channels)
	totalFrames := 0
	for _, pkt := range page.Packets {
		frames, err := d.codec.Decode(pkt, d.scratch)
		if err != nil {
			d.fail(err)
			return false
		}
		totalFrames += frames
		decoded = append(decoded, d.scratch[:frames*channels]...)
	}

	// Anchor the page on the public sample timeline and strip codec
	// artifacts. Two kinds of decoded frame are not part of the stream:
	//
	//   - leading priming: a codec (Vorbis) can emit setup frames before
	//     stream sample 0 on the first page after a reset; they map to
	//     negative sample indices and are dropped;
	//   - trailing padding: a codec can emit frames past the last granule,
	//     most visibly on the EOS page; the stream's valid length is fixed.
	var primingFrames, trailingFrames int

	if page.GranulePos >= 0 {
		validEnd := d.codec.GranuleToSamples(page.GranulePos)
		if page.HeaderType&oggFlagEOS != 0 && d.totalLen > 0 {
			validEnd = d.totalLen
		}

		if d.nextSample >= 0 {
			// We already know where this page must begin from the previous
			// page's valid end. This is immune to priming/padding because it
			// never derives position from this page's decoded frame count.
			d.pageStart = d.nextSample
			decodedEnd := d.pageStart + int64(totalFrames)
			if decodedEnd < 0 {
				primingFrames = int(-decodedEnd)
			}
			if decodedEnd > validEnd {
				trailingFrames = int(decodedEnd - validEnd)
			}
		} else {
			// First anchored page: derive the start from the granule. A
			// negative result means the leading decoded frames are priming.
			rawStart := validEnd - int64(totalFrames)
			if rawStart < 0 {
				primingFrames = int(-rawStart)
				d.pageStart = 0
			} else {
				d.pageStart = rawStart
			}
			decodedEnd := d.pageStart + int64(totalFrames-primingFrames)
			if decodedEnd > validEnd {
				trailingFrames = int(decodedEnd - validEnd)
			}
		}
		d.nextSample = validEnd
	} else {
		// Granule-less continuation page: continue from the running anchor.
		if d.nextSample < 0 {
			d.pageStart = 0
		} else {
			d.pageStart = d.nextSample
		}
		d.nextSample = d.pageStart + int64(totalFrames)
	}

	if primingFrames > 0 {
		decoded = decoded[primingFrames*channels:]
		totalFrames -= primingFrames
	}
	if trailingFrames > 0 {
		decoded = decoded[:(totalFrames-trailingFrames)*channels]
		totalFrames -= trailingFrames
	}

	// Stage the page's decoded PCM for consumption.
	d.pcmBuffer = decoded
	d.pcmPos = 0

	if d.skipping {
		// Drop frames while the whole page precedes the target; otherwise trim
		// into the page at the target.
		pageEnd := d.pageStart + int64(totalFrames)
		if pageEnd <= d.targetSample {
			d.pcmPos = len(d.pcmBuffer)
			return true
		}
		if d.pageStart < d.targetSample {
			skipFrames := int(d.targetSample - d.pageStart)
			d.pcmPos = skipFrames * channels
			d.granulePos = d.targetSample
		} else {
			d.granulePos = d.targetSample
		}
		d.skipping = false
	}
	return true
}

// fail records a decode/page error and empties the PCM buffer so no stale
// samples are emitted afterwards.
func (d *oggDecoder) fail(err error) {
	d.err = err
	d.skipping = false
	d.pcmBuffer = d.pcmBuffer[:0]
	d.pcmPos = 0
}

// Err returns any error that occurred during streaming.
func (d *oggDecoder) Err() error { return d.err }

// Len returns the total number of samples.
func (d *oggDecoder) Len() int { return int(d.totalLen) }

// Position returns the current sample position.
func (d *oggDecoder) Position() int { return int(d.granulePos) }

// Seek seeks to the given sample position. The position is clamped to
// [0, Len]: a negative target converges to the start, a target past the end
// converges to the end of the stream.
func (d *oggDecoder) Seek(p int) error {
	if p < 0 {
		p = 0
	}
	if int64(p) > d.totalLen {
		p = int(d.totalLen)
	}

	// Ogg granule positions include the codec pre-skip (Opus); the public
	// sample position does not. Translate back before bisecting pages.
	targetGranule := int64(p) + int64(d.ogg.preSkip)
	if err := d.ogg.SeekToGranule(targetGranule); err != nil {
		// On failure stop the streamer and surface the error rather than
		// continuing from an arbitrary position with stale state.
		d.fail(err)
		return err
	}

	// Reset every piece of decoder and reader state so the next emitted frame
	// comes only from data at or after the requested position.
	d.pageStart = 0
	d.nextSample = -1
	d.pcmBuffer = d.pcmBuffer[:0]
	d.pcmPos = 0
	d.granulePos = int64(p)
	d.err = nil

	if p >= int(d.totalLen) {
		// Converged on the end: nothing should be decoded or emitted.
		d.skipping = false
	} else {
		d.skipping = true
		d.targetSample = int64(p)
	}

	return d.codec.Reset()
}

// Close closes the decoder and underlying file.
func (d *oggDecoder) Close() error {
	return d.closer.Close()
}
