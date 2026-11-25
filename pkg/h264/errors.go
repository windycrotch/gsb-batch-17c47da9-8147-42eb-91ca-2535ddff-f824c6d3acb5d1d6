package h264

import "errors"

// ErrDecoderUnavailable is returned when the H264 decoder cannot be used in
// the current build (for example a CGO_ENABLED=0 build without FFmpeg).
var ErrDecoderUnavailable = errors.New("h264 decoder unavailable in this build")
