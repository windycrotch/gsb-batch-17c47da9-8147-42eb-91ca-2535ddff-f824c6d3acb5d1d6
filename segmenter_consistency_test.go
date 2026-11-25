//go:build !ios && !android && (amd64 || arm64)

package ffgo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obinnaokechukwu/ffgo/avutil"
)

// hlsTestFrame allocates and fills a small YUV420P frame with a uniform value.
// It returns a raw Frame so callers can hand it to Muxer.WriteFrame.
func hlsTestFrame(t *testing.T, width, height, index int) Frame {
	t.Helper()
	frame, ok := hlsTestFrameNoAssert(width, height, index)
	if !ok {
		t.Fatalf("failed to allocate/fill frame %d", index)
	}
	return frame
}

// hlsTestFrameNoAssert is hlsTestFrame without test assertions, so it is safe
// to call from goroutines other than the test goroutine.
func hlsTestFrameNoAssert(width, height, index int) (Frame, bool) {
	frame := FrameAlloc()
	if frame.IsNil() {
		return Frame{}, false
	}
	AVUtil.SetFrameWidth(frame, int32(width))
	AVUtil.SetFrameHeight(frame, int32(height))
	AVUtil.SetFrameFormat(frame, int32(PixelFormatYUV420P))
	if err := AVUtil.FrameGetBuffer(frame, 32); err != nil {
		_ = FrameFree(&frame)
		return Frame{}, false
	}
	fillTestFrameYUV420(frame, uint8(index*7))
	avutil.SetFramePTS(frame.ptr, int64(index))
	return frame, true
}

// newTestHLSSegmenter builds an HLS muxer writing a 64x64 / 10 fps stream into
// dir. The playlist lives at dir/playlist.m3u8 and segments follow segPattern.
func newTestHLSSegmenter(t *testing.T, dir, segPattern string, cfg *HLSSegmenterConfig) (*Muxer, *MuxerStream) {
	t.Helper()
	playlist := filepath.Join(dir, "playlist.m3u8")
	cfg.SegmentFilenamePattern = filepath.Join(dir, segPattern)
	m, err := NewHLSSegmenter(playlist, cfg)
	if err != nil {
		t.Fatalf("NewHLSSegmenter: %v", err)
	}
	vs, err := m.AddVideoStream(&VideoStreamConfig{
		Codec:       CodecIDH264,
		Width:       64,
		Height:      64,
		PixelFormat: PixelFormatYUV420P,
		FrameRate:   10,
		BitRate:     200000,
		GOPSize:     5,
	})
	if err != nil {
		_ = m.Close()
		t.Fatalf("AddVideoStream: %v", err)
	}
	if err := m.WriteHeader(); err != nil {
		_ = m.Close()
		t.Fatalf("WriteHeader: %v", err)
	}
	return m, vs
}

// writeHLSFrames writes n frames starting at start, failing the test on any
// write error.
func writeHLSFrames(t *testing.T, m *Muxer, vs *MuxerStream, start, n int) {
	t.Helper()
	for i := start; i < start+n; i++ {
		frame := hlsTestFrame(t, 64, 64, i)
		err := m.WriteFrame(vs, frame)
		_ = FrameFree(&frame)
		if err != nil {
			t.Fatalf("WriteFrame(%d): %v", i, err)
		}
	}
}

// readPlaylistEntries returns the segment URIs listed in a media playlist.
func readPlaylistEntries(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read playlist: %v", err)
	}
	pl := parseHLSPlaylist(string(data))
	uris := make([]string, 0, len(pl.entries))
	for _, e := range pl.entries {
		uris = append(uris, e.uri)
	}
	return uris
}

// assertPlaylistConsistent verifies that every segment a playlist advertises
// exists as a non-empty file next to it, and that the entry count respects the
// configured limit (0 means unlimited).
func assertPlaylistConsistent(t *testing.T, playlistPath string, maxEntries int) []string {
	t.Helper()
	entries := readPlaylistEntries(t, playlistPath)
	dir := filepath.Dir(playlistPath)
	for _, uri := range entries {
		if strings.Contains(uri, "://") {
			continue
		}
		p := uri
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, filepath.FromSlash(uri))
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("playlist references missing segment %q: %v", uri, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("playlist references empty segment %q", uri)
		}
	}
	if maxEntries > 0 && len(entries) > maxEntries {
		t.Errorf("playlist has %d entries, exceeds limit %d", len(entries), maxEntries)
	}
	return entries
}

// segmentFiles returns the regular .ts segment files in dir.
func segmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.ts"))
	if err != nil {
		t.Fatalf("glob segments: %v", err)
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.Mode().IsRegular() {
			names = append(names, filepath.Base(m))
		}
	}
	return names
}

// TestHLSSegmenterNormalTranscode covers the happy path: a short synthetic clip
// is encoded into HLS, Close succeeds, and the playlist matches the segments.
func TestHLSSegmenterNormalTranscode(t *testing.T) {
	if !requireFFmpeg(t) {
		return
	}
	dir := t.TempDir()
	cfg := &HLSSegmenterConfig{SegmentTime: 200 * time.Millisecond, ListSize: 0}
	m, vs := newTestHLSSegmenter(t, dir, "seg_%03d.ts", cfg)

	writeHLSFrames(t, m, vs, 0, 40)

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	playlist := filepath.Join(dir, "playlist.m3u8")
	if _, err := os.Stat(playlist); err != nil {
		t.Fatalf("playlist not created: %v", err)
	}
	entries := assertPlaylistConsistent(t, playlist, 0)
	if len(entries) == 0 {
		t.Fatal("playlist is empty")
	}
	if got := len(segmentFiles(t, dir)); got < len(entries) {
		t.Errorf("only %d segment files on disk but playlist lists %d", got, len(entries))
	}
}

// TestHLSSegmenterConfigApplied verifies that SegmentTime, ListSize and
// SegmentFilenamePattern actually reach the output.
func TestHLSSegmenterConfigApplied(t *testing.T) {
	if !requireFFmpeg(t) {
		return
	}
	dir := t.TempDir()
	const listSize = 2
	cfg := &HLSSegmenterConfig{SegmentTime: 100 * time.Millisecond, ListSize: listSize}
	m, vs := newTestHLSSegmenter(t, dir, "chunk-%02d.ts", cfg)

	// 80 frames @10fps = 8s, far more than listSize segments at 100ms each.
	writeHLSFrames(t, m, vs, 0, 80)

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The custom filename pattern must have been used.
	matches, _ := filepath.Glob(filepath.Join(dir, "chunk-*.ts"))
	if len(matches) == 0 {
		t.Fatalf("segment filename pattern was ignored; no chunk-*.ts files in %s", dir)
	}

	playlist := filepath.Join(dir, "playlist.m3u8")
	entries := assertPlaylistConsistent(t, playlist, listSize)
	if len(entries) > listSize {
		t.Fatalf("playlist lists %d entries, want <= %d", len(entries), listSize)
	}
	if len(entries) == 0 {
		t.Fatal("playlist is empty")
	}
}

// TestHLSSegmenterWriteFailureIsSticky covers a mid-stream write failure: once
// one frame fails to mux, later writes must keep failing, and closing must
// report the failure. The resulting playlist must not reference the segment
// that was never written.
func TestHLSSegmenterWriteFailureIsSticky(t *testing.T) {
	if !requireFFmpeg(t) {
		return
	}
	dir := t.TempDir()

	// Make the second segment unwritable by pre-creating a directory with that
	// exact name. FFmpeg cannot open it for writing, so muxing fails.
	if err := os.Mkdir(filepath.Join(dir, "seg_001.ts"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfg := &HLSSegmenterConfig{SegmentTime: 200 * time.Millisecond, ListSize: 0}
	m, vs := newTestHLSSegmenter(t, dir, "seg_%03d.ts", cfg)

	var firstErr error
	var errFrame int
	for i := 0; i < 60; i++ {
		frame := hlsTestFrame(t, 64, 64, i)
		err := m.WriteFrame(vs, frame)
		_ = FrameFree(&frame)
		if err != nil {
			firstErr = err
			errFrame = i
			break
		}
	}
	if firstErr == nil {
		_ = m.Close()
		t.Fatal("expected a write error when a segment cannot be created")
	}

	// Every subsequent write must return an error (the same one).
	for i := errFrame + 1; i < errFrame+5; i++ {
		frame := hlsTestFrame(t, 64, 64, i)
		err := m.WriteFrame(vs, frame)
		_ = FrameFree(&frame)
		if err == nil {
			t.Fatalf("WriteFrame(%d) succeeded after an earlier failure; errors must be sticky", i)
		}
		if !errors.Is(err, firstErr) && !strings.Contains(err.Error(), firstErr.Error()) {
			t.Errorf("subsequent write error %q does not match the first error %q", err, firstErr)
		}
	}

	closeErr := m.Close()
	if closeErr == nil {
		t.Fatal("Close reported success despite a write failure")
	}
	if !errors.Is(closeErr, firstErr) && !strings.Contains(closeErr.Error(), firstErr.Error()) {
		t.Errorf("Close error %q does not surface the original write error %q", closeErr, firstErr)
	}

	// Closing again must not do additional finalization work but must keep
	// reporting the failure.
	if secondErr := m.Close(); secondErr == nil {
		t.Error("second Close reported success; finalization failure must remain visible")
	}

	// The playlist, if it exists, must not reference missing segments.
	playlist := filepath.Join(dir, "playlist.m3u8")
	if _, err := os.Stat(playlist); err == nil {
		assertPlaylistConsistent(t, playlist, 0)
		for _, uri := range readPlaylistEntries(t, playlist) {
			if strings.Contains(uri, "seg_001") {
				t.Errorf("playlist still references the failed segment %q", uri)
			}
		}
	}
}

// TestHLSSegmenterWriteCloseConcurrent exercises Close racing with in-flight
// writes. Exactly one finalization must happen; Close must never deadlock or
// panic, and every closer must observe the same result.
func TestHLSSegmenterWriteCloseConcurrent(t *testing.T) {
	if !requireFFmpeg(t) {
		return
	}
	dir := t.TempDir()
	cfg := &HLSSegmenterConfig{SegmentTime: 200 * time.Millisecond, ListSize: 0}
	m, vs := newTestHLSSegmenter(t, dir, "seg_%03d.ts", cfg)

	var wg sync.WaitGroup
	start := make(chan struct{})

	// Writer goroutine: keeps writing until the muxer is closed. Frames are
	// built without test assertions (which are unsafe off the test goroutine);
	// allocation failures simply stop the writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 200; i++ {
			frame, ok := hlsTestFrameNoAssert(64, 64, i)
			if !ok {
				return
			}
			err := m.WriteFrame(vs, frame)
			_ = FrameFree(&frame)
			if err != nil {
				// Either the muxer was closed out from under us (ErrClosed /
				// ErrMuxerFinalized) or the pipeline reported a real error.
				// Both are valid reasons to stop writing.
				return
			}
		}
	}()

	// Two concurrent closers.
	const closers = 2
	results := make([]error, closers)
	for i := 0; i < closers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			// Stagger so one closer is mid-finalization while the other arrives.
			time.Sleep(time.Duration(idx) * time.Millisecond)
			results[idx] = m.Close()
		}(i)
	}

	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Close/write deadlocked")
	}

	if (results[0] == nil) != (results[1] == nil) {
		t.Errorf("inconsistent Close results: %v vs %v", results[0], results[1])
	}
	if results[0] != nil && results[1] != nil && results[0].Error() != results[1].Error() {
		t.Errorf("Close results differ: %q vs %q", results[0], results[1])
	}

	// A post-close write must fail deterministically.
	frame := hlsTestFrame(t, 64, 64, 999)
	err := m.WriteFrame(vs, frame)
	_ = FrameFree(&frame)
	if err == nil {
		t.Error("WriteFrame after Close succeeded")
	}

	playlist := filepath.Join(dir, "playlist.m3u8")
	if _, err := os.Stat(playlist); err == nil {
		assertPlaylistConsistent(t, playlist, 0)
	}
}

// TestHLSSegmenterRelativePatternResolvesBesidePlaylist ensures a relative
// segment pattern is interpreted relative to the playlist, not the process
// working directory (which would scatter segments outside the output dir).
func TestHLSSegmenterRelativePatternResolvesBesidePlaylist(t *testing.T) {
	if !requireFFmpeg(t) {
		return
	}
	dir := t.TempDir()
	playlist := filepath.Join(dir, "playlist.m3u8")

	// Deliberately relative pattern.
	m, err := NewHLSSegmenter(playlist, &HLSSegmenterConfig{
		SegmentTime:            200 * time.Millisecond,
		ListSize:               0,
		SegmentFilenamePattern: "relseg_%03d.ts",
	})
	if err != nil {
		t.Fatalf("NewHLSSegmenter: %v", err)
	}
	vs, err := m.AddVideoStream(&VideoStreamConfig{
		Codec: CodecIDH264, Width: 64, Height: 64, PixelFormat: PixelFormatYUV420P,
		FrameRate: 10, BitRate: 200000, GOPSize: 5,
	})
	if err != nil {
		_ = m.Close()
		t.Fatalf("AddVideoStream: %v", err)
	}
	if err := m.WriteHeader(); err != nil {
		_ = m.Close()
		t.Fatalf("WriteHeader: %v", err)
	}
	writeHLSFrames(t, m, vs, 0, 40)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "relseg_*.ts"))
	if len(matches) == 0 {
		t.Fatalf("relative segment pattern did not resolve beside the playlist; dir contents: %v", segmentFiles(t, dir))
	}
	assertPlaylistConsistent(t, playlist, 0)
}

// TestHLSSegmenterKeepAllSegmentsWithListSizeZero verifies the documented
// semantics of ListSize=0: unlike FFmpeg's default of 5, every segment stays in
// the playlist.
func TestHLSSegmenterKeepAllSegmentsWithListSizeZero(t *testing.T) {
	if !requireFFmpeg(t) {
		return
	}
	dir := t.TempDir()
	cfg := &HLSSegmenterConfig{SegmentTime: 100 * time.Millisecond, ListSize: 0}
	m, vs := newTestHLSSegmenter(t, dir, "seg_%03d.ts", cfg)
	writeHLSFrames(t, m, vs, 0, 80)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	playlist := filepath.Join(dir, "playlist.m3u8")
	entries := assertPlaylistConsistent(t, playlist, 0)
	if len(entries) < 3 {
		t.Fatalf("expected all segments retained, got %d entries", len(entries))
	}
}
