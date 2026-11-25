//go:build !ios && !android && (amd64 || arm64)

package ffgo

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// hlsOutputState tracks the HLS segmenter configuration that ffgo needs in
// order to verify (and, if necessary, repair) the playlist that FFmpeg's hls
// muxer writes on finalization.
//
// FFmpeg has no way to report which segments were committed successfully, so
// the playlist itself is the source of truth: every URI it advertises must
// resolve to a non-empty regular file next to the playlist (or in the segment
// directory when hls_segment_filename points elsewhere), and the number of
// entries must respect hls_list_size.
type hlsOutputState struct {
	playlistPath string
	segmentDir   string

	// listSize is the maximum number of entries the playlist may advertise.
	// Following FFmpeg semantics, 0 means unlimited.
	listSize int

	// baseURL, when non-empty, is the hls_base_url option. Its presence means
	// playlist URIs are intentionally prefixed and must not be rewritten to
	// point at local files.
	baseURL string
}

// configure records the muxer options that affect the playlist layout and
// normalizes hls_segment_filename so that relative patterns are resolved
// against the playlist's directory (FFmpeg would otherwise resolve them
// against the process working directory, which makes the playlist point at
// files that do not live beside it).
func (h *hlsOutputState) configure(playlistPath string, opts map[string]string) error {
	h.playlistPath = playlistPath
	h.baseURL = opts["hls_base_url"]

	// FFmpeg's default for hls_list_size is 5; an explicitly supplied value
	// (including 0, which means unlimited) is honored as-is.
	h.listSize = 5
	if v, ok := opts["hls_list_size"]; ok && strings.TrimSpace(v) != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("ffgo: invalid hls_list_size %q: %w", v, err)
		}
		if n < 0 {
			n = 0
		}
		h.listSize = n
	}

	pattern := opts["hls_segment_filename"]
	if pattern == "" {
		// FFmpeg's default naming scheme is "<playlist stem>%d.ts" in the
		// playlist's directory.
		h.segmentDir = filepath.Dir(playlistPath)
		return nil
	}

	resolved := pattern
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(playlistPath), resolved)
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	opts["hls_segment_filename"] = resolved
	h.segmentDir = filepath.Dir(resolved)
	return nil
}

// verifyPlaylist checks that the playlist is internally consistent: every
// non-external URI must resolve to a committed segment and the entry count
// must not exceed the configured list size. When the playlist advertises
// segments that were never written (for example because encoding failed
// mid-stream), the stale entries are removed and an error wrapping
// ErrIncompletePlaylist is returned. If no valid entry remains the playlist is
// removed altogether.
//
// Segment URIs are normalized to paths relative to the playlist so a playlist
// whose entries were written into a different directory still resolves to real
// files. Playlists that use hls_base_url are left with their URLs untouched.
func (h *hlsOutputState) verifyPlaylist() error {
	if h == nil || h.playlistPath == "" {
		return nil
	}

	data, err := os.ReadFile(h.playlistPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing was committed (e.g. finalization happened before the
			// header was written). There is no playlist to be inconsistent.
			return nil
		}
		return fmt.Errorf("ffgo: reading HLS playlist %q: %w", h.playlistPath, err)
	}

	pl := parseHLSPlaylist(string(data))
	playlistDir := filepath.Dir(h.playlistPath)

	kept := make([]hlsPlaylistEntry, 0, len(pl.entries))
	origIndex := make([]int, 0, len(pl.entries))
	missing := 0
	rewritten := 0
	for i, e := range pl.entries {
		if strings.Contains(e.uri, "://") {
			// External URI: cannot verify, trust as-is.
			kept = append(kept, e)
			origIndex = append(origIndex, i)
			continue
		}
		path, ok := h.resolveExistingSegment(e.uri)
		if !ok {
			missing++
			continue
		}
		if h.baseURL == "" {
			if rel, err := filepath.Rel(playlistDir, path); err == nil {
				relSlash := filepath.ToSlash(rel)
				if relSlash != e.uri {
					e.uri = relSlash
					rewritten++
				}
			}
		}
		kept = append(kept, e)
		origIndex = append(origIndex, i)
	}

	truncated := 0
	if h.listSize > 0 && len(kept) > h.listSize {
		truncated = len(kept) - h.listSize
		kept = kept[truncated:]
		origIndex = origIndex[truncated:]
	}

	if missing == 0 && truncated == 0 && rewritten == 0 {
		return nil
	}

	if len(kept) == 0 {
		if err := os.Remove(h.playlistPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("%w: %d referenced segment(s) missing and playlist could not be removed: %v",
				ErrIncompletePlaylist, missing, err)
		}
		if missing == 0 {
			// Truncation cannot empty the list, but be defensive.
			return nil
		}
		return fmt.Errorf("%w: %d referenced segment(s) missing; inconsistent playlist removed",
			ErrIncompletePlaylist, missing)
	}

	// Recompute the media sequence so the first surviving entry keeps its
	// original sequence number.
	newSequence := pl.mediaSequence + int64(origIndex[0])

	rendered := renderHLSPlaylist(pl, kept, newSequence)
	tmpPath := h.playlistPath + ".ffgo-tmp"
	if err := os.WriteFile(tmpPath, rendered, 0o644); err != nil {
		return fmt.Errorf("ffgo: rewriting HLS playlist %q: %w", h.playlistPath, err)
	}
	if err := os.Rename(tmpPath, h.playlistPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("ffgo: replacing HLS playlist %q: %w", h.playlistPath, err)
	}

	if missing > 0 {
		return fmt.Errorf("%w: %d referenced segment(s) missing; inconsistent playlist repaired",
			ErrIncompletePlaylist, missing)
	}
	return nil
}

// resolveExistingSegment maps a playlist URI to an existing committed segment
// file, trying the playlist directory first and then the configured segment
// directory.
func (h *hlsOutputState) resolveExistingSegment(uri string) (string, bool) {
	if uri == "" {
		return "", false
	}
	clean := filepath.FromSlash(uri)
	if filepath.IsAbs(clean) {
		if isCommittedSegment(clean) {
			return clean, true
		}
		return "", false
	}
	primary := filepath.Join(filepath.Dir(h.playlistPath), clean)
	if isCommittedSegment(primary) {
		return primary, true
	}
	if h.segmentDir != "" {
		secondary := filepath.Join(h.segmentDir, clean)
		if isCommittedSegment(secondary) {
			return secondary, true
		}
	}
	return "", false
}

func isCommittedSegment(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() && info.Size() > 0
}

// hlsPlaylistEntry is a single media segment entry (its #EXTINF line plus any
// tags such as #EXT-X-DISCONTINUITY that belong to it).
type hlsPlaylistEntry struct {
	tags   []string
	extInf string
	uri    string
}

// hlsPlaylist is a parsed HLS media playlist.
type hlsPlaylist struct {
	header        []string
	hasMediaSeq   bool
	mediaSequence int64
	entries       []hlsPlaylistEntry
	hasEndList    bool
}

func parseHLSPlaylist(content string) hlsPlaylist {
	var pl hlsPlaylist
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")

	var pendingTags []string
	inEntries := false
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "#EXT-X-MEDIA-SEQUENCE:"):
			pl.hasMediaSeq = true
			if n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(trimmed, "#EXT-X-MEDIA-SEQUENCE:")), 10, 64); err == nil {
				pl.mediaSequence = n
			}
		case strings.HasPrefix(trimmed, "#EXT-X-ENDLIST"):
			pl.hasEndList = true
		case strings.HasPrefix(trimmed, "#EXTINF"):
			pendingTags = append(pendingTags, trimmed)
			inEntries = true
		case strings.HasPrefix(trimmed, "#"):
			if inEntries {
				pendingTags = append(pendingTags, trimmed)
			} else {
				pl.header = append(pl.header, trimmed)
			}
		default:
			// A URI line completes the entry.
			var entry hlsPlaylistEntry
			for _, tag := range pendingTags {
				if strings.HasPrefix(tag, "#EXTINF") {
					entry.extInf = tag
				} else {
					entry.tags = append(entry.tags, tag)
				}
			}
			pendingTags = nil
			entry.uri = trimmed
			pl.entries = append(pl.entries, entry)
			inEntries = true
		}
	}
	return pl
}

func renderHLSPlaylist(pl hlsPlaylist, entries []hlsPlaylistEntry, mediaSequence int64) []byte {
	var b strings.Builder
	if len(pl.header) == 0 {
		b.WriteString("#EXTM3U\n")
	} else {
		for _, line := range pl.header {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if pl.hasMediaSeq {
		fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", mediaSequence)
	}
	for _, e := range entries {
		for _, tag := range e.tags {
			b.WriteString(tag)
			b.WriteByte('\n')
		}
		if e.extInf != "" {
			b.WriteString(e.extInf)
			b.WriteByte('\n')
		}
		b.WriteString(e.uri)
		b.WriteByte('\n')
	}
	if pl.hasEndList {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return []byte(b.String())
}
