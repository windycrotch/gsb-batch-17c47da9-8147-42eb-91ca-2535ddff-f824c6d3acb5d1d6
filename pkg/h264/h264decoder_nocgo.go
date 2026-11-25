//go:build !cgo

package h264

import (
	"image"
)

// H264Decoder is a no-op placeholder in !cgo builds.
type H264Decoder struct{}

// NewH264Decoder returns ErrDecoderUnavailable in !cgo builds.
func NewH264Decoder() (*H264Decoder, error) {
	return nil, ErrDecoderUnavailable
}

// Decode always returns ErrDecoderUnavailable in !cgo builds.
func (d *H264Decoder) Decode(nalu []byte) (image.Image, error) {
	return nil, ErrDecoderUnavailable
}

// Close is a no-op in !cgo builds.
func (d *H264Decoder) Close() {}
