//go:build !ios && !android && (amd64 || arm64)

package ffgo

import (
	"errors"

	"github.com/obinnaokechukwu/ffgo/avutil"
)

// FFmpegError is an error from FFmpeg operations.
// It contains the raw FFmpeg error code and a human-readable message.
type FFmpegError = avutil.Error

// Common errors
var (
	// ErrOutOfMemory indicates memory allocation failed.
	ErrOutOfMemory = errors.New("ffgo: out of memory")

	// ErrNotLoaded indicates FFmpeg libraries are not loaded.
	ErrNotLoaded = errors.New("ffgo: FFmpeg libraries not loaded")

	// ErrClosed indicates the resource has been closed.
	ErrClosed = errors.New("ffgo: resource is closed")

	// ErrNoVideoStream indicates no video stream is present.
	ErrNoVideoStream = errors.New("ffgo: no video stream")

	// ErrNoAudioStream indicates no audio stream is present.
	ErrNoAudioStream = errors.New("ffgo: no audio stream")

	// ErrDecoderNotOpened indicates the decoder has not been opened.
	ErrDecoderNotOpened = errors.New("ffgo: decoder not opened")

	// ErrAVDeviceUnavailable indicates FFmpeg's libavdevice could not be loaded.
	ErrAVDeviceUnavailable = errors.New("ffgo: libavdevice not available")

	// ErrDeviceEnumerationUnavailable indicates device enumeration is not available
	// (e.g. missing shim wrappers, unsupported FFmpeg build, or platform constraints).
	ErrDeviceEnumerationUnavailable = errors.New("ffgo: device enumeration not available")

	// ErrIncompletePlaylist indicates that a generated HLS playlist referenced
	// segments that were never committed to disk (for example because a write
	// failed mid-stream). ffgo removes such stale entries before returning this
	// error so downstream consumers never see a playlist pointing at missing
	// files.
	ErrIncompletePlaylist = errors.New("ffgo: HLS playlist is inconsistent with the segments on disk")

	// ErrMuxerFinalized indicates that the muxer's trailer has already been
	// written, so no further frames or packets can be accepted.
	ErrMuxerFinalized = errors.New("ffgo: muxer is finalized")
)

// Error code constants re-exported from avutil
const (
	AVERROR_EOF               = avutil.AVERROR_EOF
	AVERROR_EAGAIN            = avutil.AVERROR_EAGAIN
	AVERROR_EINVAL            = avutil.AVERROR_EINVAL
	AVERROR_ENOMEM            = avutil.AVERROR_ENOMEM
	AVERROR_DECODER_NOT_FOUND = avutil.AVERROR_DECODER_NOT_FOUND
	AVERROR_ENCODER_NOT_FOUND = avutil.AVERROR_ENCODER_NOT_FOUND
)

// NewError creates an FFmpegError from an error code.
// Returns nil if code >= 0.
func NewError(code int32, op string) error {
	return avutil.NewError(code, op)
}

// ErrorCode returns the FFmpeg error code from an error, or 0 if not an FFmpeg error.
func ErrorCode(err error) int32 {
	return avutil.Code(err)
}
