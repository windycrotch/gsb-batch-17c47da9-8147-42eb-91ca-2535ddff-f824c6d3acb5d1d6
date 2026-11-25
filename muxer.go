//go:build !ios && !android && (amd64 || arm64)

package ffgo

import (
	"errors"
	"sync"

	"github.com/obinnaokechukwu/ffgo/avcodec"
	"github.com/obinnaokechukwu/ffgo/avformat"
	"github.com/obinnaokechukwu/ffgo/avutil"
	"github.com/obinnaokechukwu/ffgo/internal/bindings"
)

// Muxer combines multiple streams into a container.
// It provides low-level control over muxing, allowing stream copy mode
// or encoding with multiple audio/subtitle tracks.
//
// A Muxer is safe for concurrent use. The first unrecoverable write error is
// remembered and returned by every subsequent write; finalization (trailer +
// output verification) happens exactly once, whether triggered by
// WriteTrailer, Close, or both.
type Muxer struct {
	mu            sync.Mutex
	formatCtx     avformat.FormatContext
	ioCtx         avformat.IOContext
	streams       []*MuxerStream
	headerWritten bool
	path          string
	format        string
	closed        bool

	// headerOpts are muxer-specific options (e.g. HLS segmentation settings)
	// applied when the header is written.
	headerOpts map[string]string

	// hls is non-nil for HLS outputs; it verifies/repairs the playlist during
	// finalization.
	hls *hlsOutputState

	// writeErr is the first unrecoverable write error. Once set, all further
	// writes fail with the same error so callers cannot keep producing
	// segments from a broken pipeline.
	writeErr error

	// trailerWritten/finalized make finalization single-shot. finalErr is the
	// result of that one finalization and is returned by every subsequent
	// WriteTrailer/Close call.
	trailerWritten bool
	finalized      bool
	finalErr       error
}

// MuxerStream represents a stream being muxed.
type MuxerStream struct {
	muxer     *Muxer
	stream    avformat.Stream
	codecCtx  avcodec.Context
	index     int
	timeBase  Rational
	mediaType MediaType
	encoder   *streamEncoder // nil for copy mode
	copyMode  bool
}

// streamEncoder handles encoding for a muxer stream.
type streamEncoder struct {
	codecCtx avcodec.Context
	packet   avcodec.Packet
	frame    Frame // reusable frame for format conversion if needed
}

// NewMuxer creates a muxer for the given output path and format.
// The format parameter is the FFmpeg mux format name (e.g., "matroska", "mp4", "avi").
// If format is empty, it will be guessed from the file extension.
func NewMuxer(path string, format string) (*Muxer, error) {
	if err := bindings.Load(); err != nil {
		return nil, err
	}

	if path == "" {
		return nil, errors.New("ffgo: output path cannot be empty")
	}

	// If format not specified, guess from extension
	if format == "" {
		format = guessFormatFromPath(path)
	}
	if format == "" {
		return nil, errors.New("ffgo: cannot determine output format")
	}

	m := &Muxer{
		path:    path,
		format:  format,
		streams: make([]*MuxerStream, 0),
	}

	// Create output format context
	if err := avformat.AllocOutputContext2(&m.formatCtx, nil, format, path); err != nil {
		return nil, err
	}

	if format == "hls" {
		m.hls = &hlsOutputState{}
	}

	return m, nil
}

// VideoStreamConfig configures a video stream for the muxer.
type VideoStreamConfig struct {
	Codec       CodecID     // Video codec (e.g., CodecIDH264)
	Width       int         // Video width
	Height      int         // Video height
	PixelFormat PixelFormat // Pixel format (default: YUV420P)
	FrameRate   int         // Frame rate in fps
	BitRate     int64       // Bitrate in bits/second
	GOPSize     int         // GOP size (keyframe interval)
}

// AddVideoStream adds a video stream to the muxer with encoding.
func (m *Muxer) AddVideoStream(config *VideoStreamConfig) (*MuxerStream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, errors.New("ffgo: muxer is closed")
	}
	if m.headerWritten {
		return nil, errors.New("ffgo: cannot add streams after header is written")
	}
	if config == nil {
		return nil, errors.New("ffgo: video config is required")
	}

	// Apply defaults
	if config.Codec == CodecIDNone {
		config.Codec = CodecIDH264
	}
	if config.PixelFormat == PixelFormatNone {
		config.PixelFormat = PixelFormatYUV420P
	}
	if config.FrameRate <= 0 {
		config.FrameRate = 30
	}
	if config.BitRate <= 0 {
		config.BitRate = 2000000
	}
	if config.GOPSize <= 0 {
		config.GOPSize = 12
	}

	// Find encoder
	codec := avcodec.FindEncoder(config.Codec)
	if codec == nil {
		return nil, errors.New("ffgo: video encoder not found")
	}

	// Create stream
	stream := avformat.NewStream(m.formatCtx, codec)
	if stream == nil {
		return nil, errors.New("ffgo: failed to create video stream")
	}

	// Create codec context
	codecCtx := avcodec.AllocContext3(codec)
	if codecCtx == nil {
		return nil, errors.New("ffgo: failed to allocate video codec context")
	}

	// Configure codec
	avcodec.SetCtxWidth(codecCtx, int32(config.Width))
	avcodec.SetCtxHeight(codecCtx, int32(config.Height))
	avcodec.SetCtxPixFmt(codecCtx, int32(config.PixelFormat))
	avcodec.SetCtxTimeBase(codecCtx, 1, int32(config.FrameRate))
	avcodec.SetCtxFramerate(codecCtx, int32(config.FrameRate), 1)
	avcodec.SetCtxBitRate(codecCtx, config.BitRate)
	avcodec.SetCtxGopSize(codecCtx, int32(config.GOPSize))

	// Set global header flag (commonly needed for containers like MP4)
	flags := avcodec.GetCtxFlags(codecCtx)
	avcodec.SetCtxFlags(codecCtx, flags|avcodec.CodecFlagGlobalHeader)

	// Open codec
	if err := avcodec.Open2(codecCtx, codec, nil); err != nil {
		avcodec.FreeContext(&codecCtx)
		return nil, err
	}

	// Copy parameters to stream
	codecPar := avformat.GetStreamCodecPar(stream)
	if err := avcodec.ParametersFromContext(codecPar, codecCtx); err != nil {
		avcodec.FreeContext(&codecCtx)
		return nil, err
	}

	ms := &MuxerStream{
		muxer:     m,
		stream:    stream,
		codecCtx:  codecCtx,
		index:     len(m.streams),
		timeBase:  NewRational(1, int32(config.FrameRate)),
		mediaType: MediaTypeVideo,
		encoder: &streamEncoder{
			codecCtx: codecCtx,
			packet:   avcodec.PacketAlloc(),
		},
	}

	m.streams = append(m.streams, ms)
	return ms, nil
}

// AudioStreamConfig configures an audio stream for the muxer.
type AudioStreamConfig struct {
	Codec        CodecID      // Audio codec (e.g., CodecIDAAC)
	SampleRate   int          // Sample rate in Hz
	Channels     int          // Number of channels
	SampleFormat SampleFormat // Sample format
	BitRate      int64        // Bitrate in bits/second
}

// AddAudioStream adds an audio stream to the muxer with encoding.
func (m *Muxer) AddAudioStream(config *AudioStreamConfig) (*MuxerStream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, errors.New("ffgo: muxer is closed")
	}
	if m.headerWritten {
		return nil, errors.New("ffgo: cannot add streams after header is written")
	}
	if config == nil {
		return nil, errors.New("ffgo: audio config is required")
	}

	// Apply defaults
	if config.Codec == CodecIDNone {
		config.Codec = CodecIDAAC
	}
	if config.SampleRate <= 0 {
		config.SampleRate = 48000
	}
	if config.Channels <= 0 {
		config.Channels = 2
	}
	if config.SampleFormat == SampleFormatNone {
		config.SampleFormat = SampleFormatFltP
	}
	if config.BitRate <= 0 {
		config.BitRate = 128000
	}

	// Find encoder
	codec := avcodec.FindEncoder(config.Codec)
	if codec == nil {
		return nil, errors.New("ffgo: audio encoder not found")
	}

	// Create stream
	stream := avformat.NewStream(m.formatCtx, codec)
	if stream == nil {
		return nil, errors.New("ffgo: failed to create audio stream")
	}

	// Create codec context
	codecCtx := avcodec.AllocContext3(codec)
	if codecCtx == nil {
		return nil, errors.New("ffgo: failed to allocate audio codec context")
	}

	// Configure codec
	avcodec.SetCtxSampleRate(codecCtx, int32(config.SampleRate))
	avcodec.SetCtxSampleFmt(codecCtx, int32(config.SampleFormat))
	avcodec.SetCtxBitRate(codecCtx, config.BitRate)
	avcodec.SetCtxTimeBase(codecCtx, 1, int32(config.SampleRate))

	// Set channel layout based on channel count
	avcodec.SetCtxChannelLayout(codecCtx, int32(config.Channels))

	// Set global header flag (commonly needed for containers like MP4)
	flags := avcodec.GetCtxFlags(codecCtx)
	avcodec.SetCtxFlags(codecCtx, flags|avcodec.CodecFlagGlobalHeader)

	// Open codec
	if err := avcodec.Open2(codecCtx, codec, nil); err != nil {
		avcodec.FreeContext(&codecCtx)
		return nil, err
	}

	// Copy parameters to stream
	codecPar := avformat.GetStreamCodecPar(stream)
	if err := avcodec.ParametersFromContext(codecPar, codecCtx); err != nil {
		avcodec.FreeContext(&codecCtx)
		return nil, err
	}

	ms := &MuxerStream{
		muxer:     m,
		stream:    stream,
		codecCtx:  codecCtx,
		index:     len(m.streams),
		timeBase:  NewRational(1, int32(config.SampleRate)),
		mediaType: MediaTypeAudio,
		encoder: &streamEncoder{
			codecCtx: codecCtx,
			packet:   avcodec.PacketAlloc(),
		},
	}

	m.streams = append(m.streams, ms)
	return ms, nil
}

// CopyStreamConfig configures a stream for copy mode (no re-encoding).
type CopyStreamConfig struct {
	CodecParameters avcodec.Parameters // Source stream codec parameters
	TimeBase        Rational           // Source stream time base
}

// AddCopyStream adds a stream in copy mode (no re-encoding).
// The codec parameters are copied from the source stream.
func (m *Muxer) AddCopyStream(config *CopyStreamConfig) (*MuxerStream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, errors.New("ffgo: muxer is closed")
	}
	if m.headerWritten {
		return nil, errors.New("ffgo: cannot add streams after header is written")
	}
	if config == nil || config.CodecParameters == nil {
		return nil, errors.New("ffgo: codec parameters are required for copy stream")
	}

	// Create stream
	stream := avformat.NewStream(m.formatCtx, nil)
	if stream == nil {
		return nil, errors.New("ffgo: failed to create copy stream")
	}

	// Copy codec parameters
	codecPar := avformat.GetStreamCodecPar(stream)
	if err := avcodec.ParametersCopy(codecPar, config.CodecParameters); err != nil {
		return nil, err
	}

	// Set time base
	avformat.SetStreamTimeBase(stream, config.TimeBase.Num, config.TimeBase.Den)

	ms := &MuxerStream{
		muxer:     m,
		stream:    stream,
		index:     len(m.streams),
		timeBase:  config.TimeBase,
		mediaType: avformat.GetCodecParType(codecPar),
		copyMode:  true,
	}

	m.streams = append(m.streams, ms)
	return ms, nil
}

// WriteHeader writes the container header.
// Must be called after all streams are added and before writing any frames/packets.
// Any muxer options configured on the Muxer (for example by NewHLSSegmenter)
// are applied.
func (m *Muxer) WriteHeader() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrClosed
	}
	if m.headerWritten {
		return errors.New("ffgo: header already written")
	}
	if m.writeErr != nil {
		return m.writeErr
	}
	if len(m.streams) == 0 {
		return errors.New("ffgo: no streams added")
	}

	return m.writeHeaderLocked(m.headerOpts)
}

// WriteHeaderWithOptions writes the container header with muxer-specific options.
// Options are passed to FFmpeg's avformat_write_header and are merged over any
// options configured on the Muxer itself (caller-supplied keys win).
func (m *Muxer) WriteHeaderWithOptions(opts map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrClosed
	}
	if m.headerWritten {
		return errors.New("ffgo: header already written")
	}
	if m.writeErr != nil {
		return m.writeErr
	}
	if len(m.streams) == 0 {
		return errors.New("ffgo: no streams added")
	}

	merged := make(map[string]string, len(m.headerOpts)+len(opts))
	for k, v := range m.headerOpts {
		merged[k] = v
	}
	for k, v := range opts {
		merged[k] = v
	}

	return m.writeHeaderLocked(merged)
}

// SetHeaderOptions configures muxer-specific options that WriteHeader applies.
// It must be called before the header is written.
func (m *Muxer) SetHeaderOptions(opts map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrClosed
	}
	if m.headerWritten {
		return errors.New("ffgo: cannot set header options after header is written")
	}
	if m.headerOpts == nil {
		m.headerOpts = make(map[string]string, len(opts))
	}
	for k, v := range opts {
		m.headerOpts[k] = v
	}
	return nil
}

func (m *Muxer) writeHeaderLocked(opts map[string]string) error {
	// For HLS outputs, normalize the segment filename pattern and remember the
	// list size so finalization can verify the playlist against the files that
	// actually exist.
	if m.hls != nil {
		if opts == nil {
			opts = map[string]string{}
		}
		if err := m.hls.configure(m.path, opts); err != nil {
			return err
		}
	}

	// Open output file
	if err := avformat.IOOpen(&m.ioCtx, m.path, avformat.IOFlagWrite); err != nil {
		return err
	}
	avformat.SetIOContext(m.formatCtx, m.ioCtx)

	// Write header
	var dict avutil.Dictionary
	for k, v := range opts {
		if v == "" {
			continue
		}
		if err := avutil.DictSet(&dict, k, v, 0); err != nil {
			if dict != nil {
				avutil.DictFree(&dict)
			}
			return err
		}
	}
	defer func() {
		if dict != nil {
			avutil.DictFree(&dict)
		}
	}()

	if err := avformat.WriteHeader(m.formatCtx, &dict); err != nil {
		return err
	}

	m.headerWritten = true
	return nil
}

// WriteFrame encodes and writes a frame to a stream.
// Only valid for streams created with AddVideoStream or AddAudioStream.
//
// If a write fails, the error is remembered: every subsequent WriteFrame (and
// WritePacket) returns that same error, and no further segments are produced.
func (m *Muxer) WriteFrame(ms *MuxerStream, frame Frame) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrClosed
	}
	// A pending write error is more informative than "finalized", so report it
	// first; either way no further output is produced.
	if m.writeErr != nil {
		return m.writeErr
	}
	if m.finalized {
		return ErrMuxerFinalized
	}
	if !m.headerWritten {
		return errors.New("ffgo: header not written")
	}
	if ms == nil || ms.muxer != m {
		return errors.New("ffgo: invalid stream")
	}
	if ms.copyMode {
		return errors.New("ffgo: cannot write frames to copy-mode stream, use WritePacket")
	}
	if ms.encoder == nil {
		return errors.New("ffgo: stream has no encoder")
	}

	err := m.writeFrameLocked(ms, frame.ptr)
	if err != nil {
		m.writeErr = err
	}
	return err
}

// writeFrameLocked performs the encode/mux cycle for a single frame.
func (m *Muxer) writeFrameLocked(ms *MuxerStream, framePtr avutil.Frame) error {
	// Send frame to encoder
	if err := avcodec.SendFrame(ms.codecCtx, framePtr); err != nil {
		return err
	}

	// Receive and write packets
	for {
		avcodec.PacketUnref(ms.encoder.packet)
		err := avcodec.ReceivePacket(ms.codecCtx, ms.encoder.packet)
		if err != nil {
			if avutil.IsAgain(err) || avutil.IsEOF(err) {
				return nil
			}
			return err
		}

		if err := m.writePacketLocked(ms, ms.encoder.packet); err != nil {
			return err
		}
	}
}

// writePacketLocked stamps the stream index, rescales timestamps and muxes a
// single encoded packet.
func (m *Muxer) writePacketLocked(ms *MuxerStream, packet avcodec.Packet) error {
	avcodec.SetPacketStreamIndex(packet, int32(ms.index))
	streamTbNum, streamTbDen := avformat.GetStreamTimeBase(ms.stream)
	streamTb := NewRational(streamTbNum, streamTbDen)
	avcodec.RescalePacketTS(packet, ms.timeBase, streamTb)
	return avformat.InterleavedWriteFrame(m.formatCtx, packet)
}

// WritePacket writes a packet directly to a stream.
// For copy-mode streams, timestamps should already be in the source time base.
func (m *Muxer) WritePacket(ms *MuxerStream, packet *Packet) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrClosed
	}
	if m.finalized {
		return ErrMuxerFinalized
	}
	if m.writeErr != nil {
		return m.writeErr
	}
	if !m.headerWritten {
		return errors.New("ffgo: header not written")
	}
	if ms == nil || ms.muxer != m {
		return errors.New("ffgo: invalid stream")
	}
	if packet == nil || packet.ptr == nil {
		return errors.New("ffgo: packet cannot be nil")
	}

	// Set stream index
	avcodec.SetPacketStreamIndex(packet.ptr, int32(ms.index))

	// Rescale timestamps for copy mode
	if ms.copyMode {
		streamTbNum, streamTbDen := avformat.GetStreamTimeBase(ms.stream)
		streamTb := NewRational(streamTbNum, streamTbDen)
		avcodec.RescalePacketTS(packet.ptr, ms.timeBase, streamTb)
	}

	// Write packet
	err := avformat.InterleavedWriteFrame(m.formatCtx, packet.ptr)
	if err != nil {
		m.writeErr = err
	}
	return err
}

// WriteTrailer finalizes the container.
// Must be called after all frames/packets are written. It is idempotent: the
// trailer is written at most once, the encoder buffers are flushed and, for
// HLS outputs, the playlist is verified against the segments on disk. The
// result of that single finalization is returned to every caller.
func (m *Muxer) WriteTrailer() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.finalized {
		return m.finalErr
	}
	if m.closed {
		return ErrClosed
	}
	if !m.headerWritten {
		return errors.New("ffgo: header not written")
	}

	m.finalizeLocked()
	return m.finalErr
}

// finalizeLocked flushes encoders, writes the trailer and verifies the HLS
// playlist exactly once. All errors observed during finalization are joined
// into m.finalErr so callers see the write failure and any output
// inconsistency together.
func (m *Muxer) finalizeLocked() {
	if m.finalized {
		return
	}
	m.finalized = true

	var errs []error
	if m.writeErr != nil {
		errs = append(errs, m.writeErr)
	}

	if !m.headerWritten || m.formatCtx == nil {
		m.finalErr = errors.Join(errs...)
		return
	}

	// Flush encoders (errors are non-fatal at this point; the stream is already
	// degraded and the write error, if any, is reported).
	for _, ms := range m.streams {
		if ms.encoder != nil && ms.codecCtx != nil {
			m.flushEncoder(ms)
		}
	}

	if err := avformat.WriteTrailer(m.formatCtx); err != nil {
		errs = append(errs, err)
	}

	// Verify that the playlist references only committed segments. This runs
	// even after a trailer error because FFmpeg may still have written a
	// partial playlist while failing on the I/O close.
	if m.hls != nil {
		if err := m.hls.verifyPlaylist(); err != nil {
			errs = append(errs, err)
		}
	}

	m.finalErr = errors.Join(errs...)
}

// flushEncoder flushes remaining packets from an encoder.
func (m *Muxer) flushEncoder(ms *MuxerStream) {
	// Send flush signal (errors during flush are non-fatal)
	_ = avcodec.SendFrame(ms.codecCtx, nil)

	// Receive and write remaining packets
	for {
		avcodec.PacketUnref(ms.encoder.packet)
		err := avcodec.ReceivePacket(ms.codecCtx, ms.encoder.packet)
		if err != nil {
			break
		}
		_ = m.writePacketLocked(ms, ms.encoder.packet)
	}
}

// Close releases all resources. It finalizes the output first (see
// WriteTrailer); if finalization fails the error is returned and the resources
// are still released. Close is idempotent and safe to call concurrently with
// writes: exactly one finalization happens, and later calls return the same
// result without writing the trailer again or double-freeing resources.
func (m *Muxer) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return m.finalErr
	}
	m.closed = true

	if m.headerWritten {
		m.finalizeLocked()
	}

	// Free encoder resources
	for _, ms := range m.streams {
		if ms.encoder != nil {
			if ms.encoder.packet != nil {
				avcodec.PacketFree(&ms.encoder.packet)
			}
			if !ms.encoder.frame.IsNil() {
				_ = ms.encoder.frame.Free()
			}
		}
		if ms.codecCtx != nil && !ms.copyMode {
			avcodec.FreeContext(&ms.codecCtx)
		}
	}

	// Close I/O context. The trailer has already been written; an error here
	// means buffered output could not be flushed, so surface it.
	if m.ioCtx != nil {
		if err := avformat.IOCloseP(&m.ioCtx); err != nil && m.finalErr == nil {
			m.finalErr = err
		}
	}

	// Free format context
	if m.formatCtx != nil {
		avformat.FreeContext(m.formatCtx)
		m.formatCtx = nil
	}

	return m.finalErr
}

// Streams returns all streams in the muxer.
func (m *Muxer) Streams() []*MuxerStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams
}

// Index returns the stream index.
func (ms *MuxerStream) Index() int {
	return ms.index
}

// MediaType returns the stream's media type.
func (ms *MuxerStream) MediaType() MediaType {
	return ms.mediaType
}

// TimeBase returns the stream's time base.
func (ms *MuxerStream) TimeBase() Rational {
	return ms.timeBase
}

// IsCopyMode returns true if the stream is in copy mode (no encoding).
func (ms *MuxerStream) IsCopyMode() bool {
	return ms.copyMode
}
