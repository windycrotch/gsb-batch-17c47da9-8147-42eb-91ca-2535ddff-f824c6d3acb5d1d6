//go:build !ios && !android && (amd64 || arm64)

package ffgo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestPlaylist writes content to dir/playlist.m3u8 and returns its path.
func writeTestPlaylist(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "playlist.m3u8")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write playlist: %v", err)
	}
	return p
}

// writeTestSegment creates a non-empty segment file next to the playlist.
func writeTestSegment(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("segment-data"), 0o644); err != nil {
		t.Fatalf("write segment %s: %v", name, err)
	}
}

func TestParseHLSPlaylist(t *testing.T) {
	content := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-TARGETDURATION:2",
		"#EXT-X-MEDIA-SEQUENCE:7",
		"#EXTINF:2.000000,",
		"seg_000.ts",
		"#EXT-X-DISCONTINUITY",
		"#EXTINF:2.000000,",
		"seg_001.ts",
		"#EXT-X-ENDLIST",
		"",
	}, "\n")

	pl := parseHLSPlaylist(content)
	if !pl.hasEndList {
		t.Error("expected ENDLIST")
	}
	if !pl.hasMediaSeq || pl.mediaSequence != 7 {
		t.Errorf("media sequence = %d (present=%v), want 7", pl.mediaSequence, pl.hasMediaSeq)
	}
	if len(pl.entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(pl.entries))
	}
	if pl.entries[0].uri != "seg_000.ts" || pl.entries[1].uri != "seg_001.ts" {
		t.Errorf("unexpected URIs: %q, %q", pl.entries[0].uri, pl.entries[1].uri)
	}
	if len(pl.entries[1].tags) != 1 || pl.entries[1].tags[0] != "#EXT-X-DISCONTINUITY" {
		t.Errorf("second entry tags = %v, want the discontinuity tag", pl.entries[1].tags)
	}
	// Header lines must be captured in order, before the first entry.
	if len(pl.header) < 3 || pl.header[0] != "#EXTM3U" {
		t.Errorf("header = %v", pl.header)
	}
}

func TestParseHLSPlaylistCRLF(t *testing.T) {
	content := "#EXTM3U\r\n#EXTINF:1.0,\r\nseg.ts\r\n#EXT-X-ENDLIST\r\n"
	pl := parseHLSPlaylist(content)
	if len(pl.entries) != 1 || pl.entries[0].uri != "seg.ts" {
		t.Fatalf("entries = %+v", pl.entries)
	}
	if pl.entries[0].uri != "seg.ts" {
		t.Errorf("URI retained a stray \\r: %q", pl.entries[0].uri)
	}
}

// TestVerifyPlaylistRemovesMissingSegments is a pure-filesystem test of the
// repair path: entries pointing at files that do not exist are dropped, the
// media sequence is advanced accordingly, and ErrIncompletePlaylist is
// reported.
func TestVerifyPlaylistRemovesMissingSegments(t *testing.T) {
	dir := t.TempDir()
	writeTestSegment(t, dir, "seg_000.ts")
	// seg_001.ts is intentionally missing.
	writeTestSegment(t, dir, "seg_002.ts")

	playlist := writeTestPlaylist(t, dir, strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-MEDIA-SEQUENCE:10",
		"#EXTINF:2.0,",
		"seg_000.ts",
		"#EXTINF:2.0,",
		"seg_001.ts",
		"#EXTINF:2.0,",
		"seg_002.ts",
		"#EXT-X-ENDLIST",
		"",
	}, "\n"))

	h := &hlsOutputState{playlistPath: playlist, segmentDir: dir, listSize: 0}
	err := h.verifyPlaylist()
	if !errors.Is(err, ErrIncompletePlaylist) {
		t.Fatalf("verifyPlaylist error = %v, want ErrIncompletePlaylist", err)
	}

	entries := readPlaylistEntries(t, playlist)
	want := []string{"seg_000.ts", "seg_002.ts"}
	if strings.Join(entries, ",") != strings.Join(want, ",") {
		t.Errorf("entries = %v, want %v", entries, want)
	}

	data, _ := os.ReadFile(playlist)
	pl := parseHLSPlaylist(string(data))
	// seg_002 was the third original entry, so the surviving first entry is at
	// offset 0; the media sequence must remain 10.
	if pl.mediaSequence != 10 {
		t.Errorf("media sequence = %d, want 10", pl.mediaSequence)
	}
	if !pl.hasEndList {
		t.Error("ENDLIST was lost during repair")
	}
}

// TestVerifyPlaylistAllMissing removes the playlist entirely when nothing it
// advertises was committed.
func TestVerifyPlaylistAllMissing(t *testing.T) {
	dir := t.TempDir()
	playlist := writeTestPlaylist(t, dir, strings.Join([]string{
		"#EXTM3U",
		"#EXTINF:2.0,",
		"gone_000.ts",
		"#EXTINF:2.0,",
		"gone_001.ts",
		"#EXT-X-ENDLIST",
		"",
	}, "\n"))

	h := &hlsOutputState{playlistPath: playlist, segmentDir: dir, listSize: 0}
	err := h.verifyPlaylist()
	if !errors.Is(err, ErrIncompletePlaylist) {
		t.Fatalf("verifyPlaylist error = %v, want ErrIncompletePlaylist", err)
	}
	if _, statErr := os.Stat(playlist); !os.IsNotExist(statErr) {
		t.Errorf("playlist should have been removed, stat err = %v", statErr)
	}
}

// TestVerifyPlaylistTruncatesToLimit checks the list-size cap is enforced even
// if FFmpeg wrote more entries than configured.
func TestVerifyPlaylistTruncatesToLimit(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"seg_000.ts", "seg_001.ts", "seg_002.ts", "seg_003.ts"} {
		writeTestSegment(t, dir, name)
	}
	playlist := writeTestPlaylist(t, dir, strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-MEDIA-SEQUENCE:0",
		"#EXTINF:2.0,",
		"seg_000.ts",
		"#EXTINF:2.0,",
		"seg_001.ts",
		"#EXTINF:2.0,",
		"seg_002.ts",
		"#EXTINF:2.0,",
		"seg_003.ts",
		"#EXT-X-ENDLIST",
		"",
	}, "\n"))

	h := &hlsOutputState{playlistPath: playlist, segmentDir: dir, listSize: 2}
	if err := h.verifyPlaylist(); err != nil {
		t.Fatalf("verifyPlaylist: %v", err)
	}
	entries := readPlaylistEntries(t, playlist)
	want := []string{"seg_002.ts", "seg_003.ts"}
	if strings.Join(entries, ",") != strings.Join(want, ",") {
		t.Errorf("entries = %v, want the newest %v", entries, want)
	}
	data, _ := os.ReadFile(playlist)
	if got := parseHLSPlaylist(string(data)).mediaSequence; got != 2 {
		t.Errorf("media sequence = %d, want 2 after truncation", got)
	}
}

// TestVerifyPlaylistRewritesRelativePaths ensures entries that name a file in
// the segment directory are rewritten to be valid relative to the playlist.
func TestVerifyPlaylistRewritesRelativePaths(t *testing.T) {
	root := t.TempDir()
	playlistDir := filepath.Join(root, "play")
	segDir := filepath.Join(root, "segments")
	for _, d := range []string{playlistDir, segDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(segDir, "seg_000.ts"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	playlist := filepath.Join(playlistDir, "playlist.m3u8")
	if err := os.WriteFile(playlist, []byte("#EXTM3U\n#EXTINF:2.0,\nseg_000.ts\n#EXT-X-ENDLIST\n"), 0o644); err != nil {
		t.Fatalf("write playlist: %v", err)
	}

	h := &hlsOutputState{playlistPath: playlist, segmentDir: segDir, listSize: 0}
	if err := h.verifyPlaylist(); err != nil {
		t.Fatalf("verifyPlaylist: %v", err)
	}
	entries := readPlaylistEntries(t, playlist)
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	if !strings.HasPrefix(entries[0], "../segments/") {
		t.Errorf("entry %q was not rewritten relative to the playlist", entries[0])
	}
	// The rewritten entry must resolve to the real file.
	if _, err := os.Stat(filepath.Join(playlistDir, filepath.FromSlash(entries[0]))); err != nil {
		t.Errorf("rewritten entry does not resolve: %v", err)
	}
}

// TestVerifyPlaylistLeavesBaseURLAlone checks that hls_base_url playlists keep
// their URLs untouched (they are not local files).
func TestVerifyPlaylistLeavesBaseURLAlone(t *testing.T) {
	dir := t.TempDir()
	writeTestSegment(t, dir, "seg_000.ts")
	playlist := writeTestPlaylist(t, dir, "#EXTM3U\n#EXTINF:2.0,\nhttps://cdn.example.com/seg_000.ts\n#EXT-X-ENDLIST\n")

	h := &hlsOutputState{
		playlistPath: playlist,
		segmentDir:   dir,
		listSize:     0,
		baseURL:      "https://cdn.example.com/",
	}
	if err := h.verifyPlaylist(); err != nil {
		t.Fatalf("verifyPlaylist: %v", err)
	}
	entries := readPlaylistEntries(t, playlist)
	if len(entries) != 1 || entries[0] != "https://cdn.example.com/seg_000.ts" {
		t.Errorf("entries = %v, external URL should be untouched", entries)
	}
}

// TestVerifyPlaylistNoOpWhenConsistent ensures a correct playlist is not
// rewritten.
func TestVerifyPlaylistNoOpWhenConsistent(t *testing.T) {
	dir := t.TempDir()
	writeTestSegment(t, dir, "seg_000.ts")
	original := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:2.0,\nseg_000.ts\n#EXT-X-ENDLIST\n"
	playlist := writeTestPlaylist(t, dir, original)

	before, _ := os.Stat(playlist)
	h := &hlsOutputState{playlistPath: playlist, segmentDir: dir, listSize: 0}
	if err := h.verifyPlaylist(); err != nil {
		t.Fatalf("verifyPlaylist: %v", err)
	}
	after, _ := os.Stat(playlist)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("consistent playlist was rewritten unnecessarily")
	}
	data, _ := os.ReadFile(playlist)
	if string(data) != original {
		t.Errorf("playlist content changed:\n%s", data)
	}
}
