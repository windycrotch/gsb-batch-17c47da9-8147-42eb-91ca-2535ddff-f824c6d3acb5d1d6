package adpcm

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/qiniu/audio"
	"github.com/qiniu/audio/wav"
	"github.com/qiniu/x/bufiox"
)

// -------------------------------------------------------------------------------------

type decoded struct {
	src *bufiox.Reader

	// samples holds the samples decoded from the current block.
	samples []byte
	// blockBytes is the number of decoded sample bytes produced by a
	// complete block (samplesPerBlock * channels * 2).
	blockBytes int
	// off is the read position inside samples for the current block.
	off int
	// remaining is the number of decoded bytes still buffered in samples.
	remaining int
	// eof reports that the last block (complete or truncated) has been
	// consumed. No further block is loaded once it is set.
	eof bool

	// pos is the current logical offset of the decoded stream in bytes.
	pos int64
	// length is the number of decoded bytes backed by complete blocks.
	// A trailing partial block does not contribute to it.
	length int64
	// blocks is the number of complete blocks available.
	blocks int64

	headerSize int64
	blockAlign int
	sampleRate int
	channelNum int

	mu sync.Mutex
}

func newDecoded(src *bufiox.Reader, cfg *wav.Config) *decoded {
	samplesPerBlock := cfg.SamplesPerBlock()
	// loadBlock emits samplesPerBlock 16-bit values per block (its layout is
	// identical for mono and stereo), so the decoded block occupies twice as
	// many bytes as it contains samples.
	blockBytes := samplesPerBlock * 2

	// The declared data size may exceed what is really readable; clamp it to
	// the bytes actually present after the header.
	avail := cfg.DataSize
	if end, err := src.Seek(0, io.SeekEnd); err == nil {
		real := end - cfg.HeaderSize
		if real < 0 {
			real = 0
		}
		if real < avail {
			avail = real
		}
		if _, err := src.Seek(cfg.HeaderSize, io.SeekStart); err != nil {
			// Leave the source where it was if the reposition failed.
			avail = cfg.DataSize
		}
	}

	blocks := avail / int64(cfg.BlockAlign)
	return &decoded{
		// The buffer must hold a whole block, otherwise Peek can never
		// return a complete block and every block would look truncated.
		src:        bufiox.NewReaderSize(src, cfg.BlockAlign*2),
		sampleRate: cfg.SampleRate,
		channelNum: cfg.Channels,
		blockAlign: cfg.BlockAlign,
		blockBytes: blockBytes,
		samples:    make([]byte, blockBytes),
		blocks:     blocks,
		length:     blocks * int64(blockBytes),
		headerSize: cfg.HeaderSize,
		eof:        blocks == 0,
	}
}

// nextBlock loads the block following the current one. A complete block is
// decoded fully; a block truncated by a damaged/short file yields as many
// whole samples as can be reconstructed. It never panics on short input.
func (p *decoded) nextBlock() error {
	block, peekErr := p.src.Peek(p.blockAlign)
	n := len(block)
	if n == 0 {
		return peekErr
	}
	got := loadBlock(p.channelNum, block, p.samples)
	// Discard exactly the bytes that were available and decoded.
	if _, err := p.src.Discard(n); err != nil && peekErr == nil {
		peekErr = err
	}
	p.off = 0
	p.remaining = got
	// A short block means the stream ends inside this block.
	if n < p.blockAlign {
		p.eof = true
	}
	if peekErr == io.EOF || peekErr == io.ErrUnexpectedEOF {
		p.eof = true
		return nil
	}
	return peekErr
}

func (p *decoded) Read(b []byte) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for n == 0 && err == nil {
		if p.remaining > 0 {
			nn := copy(b[n:], p.samples[p.off:p.off+p.remaining])
			p.off += nn
			p.remaining -= nn
			p.pos += int64(nn)
			n += nn
			if nn > 0 || len(b) == 0 {
				// Deliver data first; io.EOF (if any) is reported on the
				// following call, so the last samples are never swallowed.
				return n, nil
			}
		}
		if p.eof {
			return n, io.EOF
		}
		if err = p.nextBlock(); err != nil {
			return n, err
		}
	}
	return n, err
}

// Seek positions the decoded stream. ADPCM compression only allows random
// access at block boundaries: the requested offset is resolved against the
// complete-block region and clamped to [0, Length]; the source is moved to
// the corresponding block and the first Read starts at the exact requested
// sample inside the freshly decoded block.
func (p *decoded) Seek(offset int64, whence int) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = p.pos + offset
	case io.SeekEnd:
		target = p.length + offset
	default:
		return 0, errors.New("adpcm: invalid whence")
	}
	if target < 0 {
		target = 0
	}
	if target > p.length {
		target = p.length
	}

	frame := int64(p.channelNum * 2)
	blockIndex := target / int64(p.blockBytes)
	inside := int(target % int64(p.blockBytes))
	// Keep the offset sample/frame aligned.
	if inside%int(frame) != 0 {
		inside -= inside % int(frame)
	}

	if blockIndex >= p.blocks {
		// Position at the very end: no block is loaded, the next Read ends.
		if _, err := p.src.Seek(p.headerSize+p.blocks*int64(p.blockAlign), io.SeekStart); err != nil {
			return 0, err
		}
		p.off = 0
		p.remaining = 0
		p.eof = true
		p.pos = p.length
		return p.length, nil
	}

	if _, err := p.src.Seek(p.headerSize+blockIndex*int64(p.blockAlign), io.SeekStart); err != nil {
		return 0, err
	}
	p.eof = false
	p.off = 0
	p.remaining = 0
	if err := p.nextBlock(); err != nil {
		return 0, err
	}
	p.off = inside
	p.remaining -= inside
	p.pos = blockIndex*int64(p.blockBytes) + int64(inside)
	return p.pos, nil
}

// Length returns the size of decoded stream in bytes. Only complete blocks
// are accounted for; a trailing chunk shorter than one block is excluded.
func (p *decoded) Length() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.length
}

// SampleRate returns the sample rate like 44100.
func (p *decoded) SampleRate() int {
	return p.sampleRate
}

// Channels returns the number of channels. One channel is mono playback.
// Two channels are stereo playback. No other values are supported.
func (p *decoded) Channels() int {
	return p.channelNum
}

// BytesPerSample returns the number of bytes per sample per channel.
// The usual value is 2. Only values 1 and 2 are supported.
func (p *decoded) BytesPerSample() int {
	return 2
}

// -------------------------------------------------------------------------------------

func decode(src *bufiox.Reader, cfg *wav.Config) (dec audio.Decoded, err error) {
	if cfg.BitsPerSample != 4 {
		return nil, fmt.Errorf("adpcm wav: bits per sample must be 4 but was %d", cfg.BitsPerSample)
	}
	if cfg.BlockAlign <= 0 {
		return nil, fmt.Errorf("adpcm wav: invalid block align %d", cfg.BlockAlign)
	}
	d := newDecoded(src, cfg)
	return d, nil
}

const (
	adpcmFormat = 0x11
)

func init() {
	wav.RegisterFormat(adpcmFormat, decode)
}

// -------------------------------------------------------------------------------------
