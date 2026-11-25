//go:build !ios && !android && (amd64 || arm64)

package ffgo

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HLSSegmenterConfig configures HLS (m3u8 + MPEG-TS/fMP4 segments) output.
//
// SegmentTime controls the target segment duration, ListSize caps how many
// segments the playlist advertises (0 means unlimited) and
// SegmentFilenamePattern is the printf-style pattern used for segment files
// (e.g. "seg_%03d.ts"). All three are applied to the muxer automatically;
// callers do not need to call HeaderOptions themselves.
type HLSSegmenterConfig struct {
	SegmentTime            time.Duration
	ListSize               int
	SegmentFilenamePattern string
	Flags                  []string
	AVOptions              map[string]string
}

// NewHLSSegmenter creates an HLS muxer for playlistPath.
//
// The configuration is applied when the header is written:
//   - SegmentTime -> hls_time
//   - ListSize    -> hls_list_size (0 keeps every segment)
//   - SegmentFilenamePattern -> hls_segment_filename (relative patterns are
//     resolved against the playlist's directory so the playlist references
//     files that actually sit next to it; if empty, FFmpeg's default
//     "<playlist stem>%d.ts" beside the playlist is used)
//   - Flags       -> hls_flags
//   - AVOptions   -> passed through as-is
//
// On Close/WriteTrailer the playlist is verified: any entry that does not
// resolve to a committed segment is removed, the entry count is capped to
// ListSize, and an error wrapping ErrIncompletePlaylist is returned when
// repairs were necessary.
func NewHLSSegmenter(playlistPath string, cfg *HLSSegmenterConfig) (*Muxer, error) {
	if cfg == nil {
		return nil, fmt.Errorf("ffgo: HLSSegmenterConfig is required")
	}
	if strings.TrimSpace(playlistPath) == "" {
		return nil, fmt.Errorf("ffgo: HLS playlist path is required")
	}

	opts, err := cfg.headerOptions(false)
	if err != nil {
		return nil, err
	}

	m, err := NewMuxer(playlistPath, "hls")
	if err != nil {
		return nil, err
	}
	if err := m.SetHeaderOptions(opts); err != nil {
		_ = m.Close()
		return nil, err
	}
	return m, nil
}

// HeaderOptions returns FFmpeg muxer options suitable to pass to Muxer.WriteHeaderWithOptions.
// NewHLSSegmenter applies these automatically; the method remains available for
// callers that build a Muxer themselves.
//
// ListSize is applied verbatim: 0 means "keep every segment" (the playlist is
// not truncated), which matches FFmpeg's hls_list_size semantics.
func (cfg *HLSSegmenterConfig) HeaderOptions() (map[string]string, error) {
	return cfg.headerOptions(true)
}

func (cfg *HLSSegmenterConfig) headerOptions(requirePattern bool) (map[string]string, error) {
	if cfg == nil {
		return nil, fmt.Errorf("ffgo: HLSSegmenterConfig is nil")
	}
	if requirePattern && cfg.SegmentFilenamePattern == "" {
		return nil, fmt.Errorf("ffgo: HLSSegmenterConfig.SegmentFilenamePattern is required")
	}
	if cfg.ListSize < 0 {
		return nil, fmt.Errorf("ffgo: HLSSegmenterConfig.ListSize must not be negative")
	}
	if cfg.SegmentTime < 0 {
		return nil, fmt.Errorf("ffgo: HLSSegmenterConfig.SegmentTime must not be negative")
	}

	opts := make(map[string]string, len(cfg.AVOptions)+8)
	for k, v := range cfg.AVOptions {
		opts[k] = v
	}
	if cfg.SegmentTime > 0 {
		opts["hls_time"] = strconv.FormatFloat(cfg.SegmentTime.Seconds(), 'f', -1, 64)
	}
	// Always set the list size so the configured value (including 0, meaning
	// unlimited) cannot silently fall back to FFmpeg's default of 5.
	opts["hls_list_size"] = strconv.Itoa(cfg.ListSize)
	if cfg.SegmentFilenamePattern != "" {
		opts["hls_segment_filename"] = cfg.SegmentFilenamePattern
	}
	if len(cfg.Flags) > 0 {
		opts["hls_flags"] = strings.Join(cfg.Flags, ",")
	}
	return opts, nil
}

// DASHSegmenterConfig configures DASH (mpd + media segments) output.
type DASHSegmenterConfig struct {
	SegmentTime time.Duration
	InitName    string
	MediaName   string
	AVOptions   map[string]string
}

// NewDASHSegmenter creates a DASH muxer for mpdPath. The configuration is
// applied when the header is written; callers do not need to call
// HeaderOptions themselves.
func NewDASHSegmenter(mpdPath string, cfg *DASHSegmenterConfig) (*Muxer, error) {
	if cfg == nil {
		return nil, fmt.Errorf("ffgo: DASHSegmenterConfig is required")
	}
	if strings.TrimSpace(mpdPath) == "" {
		return nil, fmt.Errorf("ffgo: DASH manifest path is required")
	}

	m, err := NewMuxer(mpdPath, "dash")
	if err != nil {
		return nil, err
	}
	if err := m.SetHeaderOptions(cfg.HeaderOptions()); err != nil {
		_ = m.Close()
		return nil, err
	}
	return m, nil
}

// HeaderOptions returns FFmpeg muxer options suitable to pass to Muxer.WriteHeaderWithOptions.
// NewDASHSegmenter applies these automatically; the method remains available for
// callers that build a Muxer themselves.
func (cfg *DASHSegmenterConfig) HeaderOptions() map[string]string {
	if cfg == nil {
		return nil
	}
	opts := make(map[string]string, len(cfg.AVOptions)+8)
	for k, v := range cfg.AVOptions {
		opts[k] = v
	}
	if cfg.SegmentTime > 0 {
		opts["seg_duration"] = strconv.FormatFloat(cfg.SegmentTime.Seconds(), 'f', -1, 64)
	}
	if cfg.InitName != "" {
		opts["init_seg_name"] = cfg.InitName
	}
	if cfg.MediaName != "" {
		opts["media_seg_name"] = cfg.MediaName
	}
	return opts
}
