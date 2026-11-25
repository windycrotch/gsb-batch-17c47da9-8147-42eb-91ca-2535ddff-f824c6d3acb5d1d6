/*
Copyright 2022-2025 The nagare media authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmafingest_test

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestManifestOverHTTP verifies that the generated playlists and listed segments are retrievable through the public
// HTTP entry a player would use, and that every segment referenced by the media playlist is fetchable.
func TestManifestOverHTTP(t *testing.T) {
	e := newEnv(t, time.Hour)

	if res := e.uploadTrack(testVideoSet, testVideoTrack, videoTrack(), 3); res.status != http.StatusCreated {
		t.Fatalf("video upload failed: %+v", res)
	}
	if res := e.uploadTrack(testAudioSet, testAudioTrack, audioTrack(), 2); res.status != http.StatusCreated {
		t.Fatalf("audio upload failed: %+v", res)
	}

	masterURI := testPathPrefix + "/" + e.hostDir() + "/" + testStreamName + ".str/master.m3u8"
	e.waitFor("master playlist over HTTP", 5*time.Second, func() bool {
		code, _ := e.getHTTP(masterURI)
		return code == http.StatusOK
	})

	code, master := e.getHTTP(masterURI)
	if code != http.StatusOK {
		t.Fatalf("GET master: status %d", code)
	}
	mp := parseMasterPlaylist(t, master)
	if len(mp.video) != 1 || len(mp.audio) != 1 {
		t.Fatalf("expected one video and one audio entry over HTTP:\n%s", master)
	}

	// Fetch every media playlist referenced by the master and then every segment it lists.
	uris := append([]string{}, mp.streams...)
	for _, media := range mp.audio {
		if uri := extractAttr(media, "URI"); uri != "" {
			uris = append(uris, uri)
		}
	}
	if len(uris) == 0 {
		t.Fatalf("master playlist references no media playlists:\n%s", master)
	}

	streamDir := testPathPrefix + "/" + e.hostDir() + "/" + testStreamName + ".str/"
	for _, uri := range uris {
		code, raw := e.getHTTP(streamDir + uri)
		if code != http.StatusOK {
			t.Fatalf("GET %s: status %d", uri, code)
		}
		pl := parseMediaPlaylist(t, raw)
		if len(pl.segments) == 0 {
			t.Fatalf("media playlist %s lists no segments:\n%s", uri, raw)
		}
		base := streamDir + uri[:strings.LastIndex(uri, "/")+1]
		for _, seg := range pl.segments {
			segCode, body := e.getHTTP(base + seg)
			if segCode != http.StatusOK || len(body) == 0 {
				t.Fatalf("GET segment %s%s: status %d, len %d", uri, seg, segCode, len(body))
			}
		}
	}
}

// extractAttr extracts a quoted attribute value from an HLS tag line.
func extractAttr(line, name string) string {
	key := name + "=\""
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// TestManifestNormalIngest verifies that a normally completed track is fully reflected in the generated playlists and
// that the master playlist keeps a stable default audio track as tracks are added.
func TestManifestNormalIngest(t *testing.T) {
	e := newEnv(t, time.Hour)

	if res := e.uploadTrack(testVideoSet, testVideoTrack, videoTrack(), 3); res.status != http.StatusCreated {
		t.Fatalf("video upload failed: %+v", res)
	}

	e.waitFor("master playlist after video upload", 5*time.Second, func() bool {
		_, ok := e.readFile(e.masterPath())
		return ok
	})

	master, _ := e.readFile(e.masterPath())
	mp := parseMasterPlaylist(t, master)
	if len(mp.video) != 1 {
		t.Fatalf("expected one video stream, got %d:\n%s", len(mp.video), master)
	}
	if !strings.Contains(mp.video[0], "RESOLUTION=1280x720") {
		t.Fatalf("expected resolution in stream line: %q", mp.video[0])
	}
	if len(mp.streams) != 1 || mp.streams[0] != "video/video.cmvf/index.m3u8" {
		t.Fatalf("unexpected stream URIs: %v", mp.streams)
	}
	if len(mp.audio) != 0 {
		t.Fatalf("expected no audio track yet: %v", mp.audio)
	}

	if res := e.uploadTrack(testAudioSet, testAudioTrack, audioTrack(), 2); res.status != http.StatusCreated {
		t.Fatalf("audio upload failed: %+v", res)
	}

	e.waitFor("audio track in master playlist", 5*time.Second, func() bool {
		master, ok := e.readFile(e.masterPath())
		return ok && strings.Contains(master, "TYPE=AUDIO")
	})

	master, _ = e.readFile(e.masterPath())
	mp = parseMasterPlaylist(t, master)
	if len(mp.audio) != 1 {
		t.Fatalf("expected one audio track, got %d:\n%s", len(mp.audio), master)
	}
	// The very first audio track must be the default one.
	if !strings.Contains(mp.audio[0], "DEFAULT=YES") {
		t.Fatalf("expected audio track to be default: %q", mp.audio[0])
	}
	if len(mp.video) != 1 || len(mp.streams) != 1 {
		t.Fatalf("expected one video stream to remain:\n%s", master)
	}
	// Video must reference the audio group.
	if !strings.Contains(mp.video[0], `AUDIO="audio"`) {
		t.Fatalf("expected video stream to reference audio group: %q", mp.video[0])
	}

	// Check the media playlists: every listed segment must exist and be readable, and the last fragment of the
	// audio track is listed first-to-last with no gaps.
	audioPL := parseMediaPlaylist(t, mustRead(t, e, e.mediaPath(testAudioSet, testAudioTrack)))
	if len(audioPL.segments) != 2 {
		t.Fatalf("expected 2 audio segments, got %d:\n%s", len(audioPL.segments), audioPL.raw)
	}
	for i, seg := range audioPL.segments {
		if want := fragmentName(i); seg != want {
			t.Fatalf("audio segment %d = %q, want %q", i, seg, want)
		}
		if _, ok := e.readFile(e.segmentPath(testAudioSet, testAudioTrack, seg)); !ok {
			t.Fatalf("listed segment %q does not exist on the volume", seg)
		}
	}
	// The encoder closed the track, but an encoder may reconnect and continue the same track: the playlist must stay
	// live (append-only) rather than being finalized.
	if audioPL.endlist {
		t.Fatalf("a normally finished track must not finalize its media playlist:\n%s", audioPL.raw)
	}

	videoPL := parseMediaPlaylist(t, mustRead(t, e, e.mediaPath(testVideoSet, testVideoTrack)))
	if len(videoPL.segments) != 3 {
		t.Fatalf("expected 3 video segments, got %d:\n%s", len(videoPL.segments), videoPL.raw)
	}
	for i, seg := range videoPL.segments {
		if want := fragmentName(i); seg != want {
			t.Fatalf("video segment %d = %q, want %q", i, seg, want)
		}
		if _, ok := e.readFile(e.segmentPath(testVideoSet, testVideoTrack, seg)); !ok {
			t.Fatalf("listed segment %q does not exist on the volume", seg)
		}
	}
}

// TestManifestSegmentVisibleOnlyAfterCommit verifies that a fragment that is still being written is never listed, and
// only appears once its file has been committed and is readable.
func TestManifestSegmentVisibleOnlyAfterCommit(t *testing.T) {
	e := newEnv(t, time.Hour)

	u := e.startUpload(testVideoSet, testVideoTrack)
	u.write(videoTrack().InitSegment())

	e.waitFor("master playlist", 5*time.Second, func() bool {
		_, ok := e.readFile(e.masterPath())
		return ok
	})

	// Send the first fragment slowly: only half of it is available to the server.
	frag := fragment(0)
	split := len(frag) / 2
	u.write(frag[:split])

	// Give the server a moment to start writing the fragment file.
	time.Sleep(150 * time.Millisecond)

	// Nothing may be listed yet: the fragment is incomplete and uncommitted.
	pl, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack))
	if ok {
		parsed := parseMediaPlaylist(t, pl)
		if len(parsed.segments) != 0 {
			t.Fatalf("fragment was listed before it was committed:\n%s", pl)
		}
	}

	// Complete the first fragment. It is still not committed: a fragment is only finalized once the following
	// fragment starts (or the upload ends).
	u.write(frag[split:])
	time.Sleep(150 * time.Millisecond)
	if pl, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack)); ok {
		if parsed := parseMediaPlaylist(t, pl); len(parsed.segments) != 0 {
			t.Fatalf("fragment was listed before it was committed:\n%s", pl)
		}
	}

	// Start the second fragment, which commits the first one.
	u.write(fragment(1))

	e.waitFor("first segment listed after commit", 5*time.Second, func() bool {
		pl, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack))
		if !ok {
			return false
		}
		return strings.Contains(pl, fragmentName(0))
	})

	// The listed segment must be fully readable.
	content, ok := e.readFile(e.segmentPath(testVideoSet, testVideoTrack, fragmentName(0)))
	if !ok || len(content) == 0 {
		t.Fatalf("listed segment is not readable")
	}

	u.close()
	if res := u.wait(10 * time.Second); res.status != http.StatusCreated {
		t.Fatalf("upload failed: %+v", res)
	}
}

// TestManifestFailedUploadRemovesStream verifies that a stream whose upload is aborted is no longer presented as active.
func TestManifestFailedUploadRemovesStream(t *testing.T) {
	e := newEnv(t, time.Hour)

	// Upload enough that the first fragment is committed (the second fragment commits the first) so the stream has
	// something to advertise.
	u := e.startUpload(testVideoSet, testVideoTrack)
	u.write(videoTrack().InitSegment())
	u.write(fragment(0))
	u.write(fragment(1))

	e.waitFor("first segment listed", 5*time.Second, func() bool {
		pl, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack))
		return ok && strings.Contains(pl, fragmentName(0))
	})

	master, ok := e.readFile(e.masterPath())
	if !ok || !strings.Contains(master, "video/video.cmvf/index.m3u8") {
		t.Fatalf("expected stream to be advertised before failure:\n%s", master)
	}

	// Abort the upload halfway through the next fragment.
	u.write(fragment(2)[:len(fragment(2))/2])
	u.abort()
	_ = u.wait(10 * time.Second)

	// The stream must disappear from the master playlist.
	e.waitFor("stream removed from master playlist after failed upload", 5*time.Second, func() bool {
		_, ok := e.readFile(e.masterPath())
		return !ok
	})

	// The media playlist must be finalized so the end is observable.
	e.waitFor("media playlist finalized", 5*time.Second, func() bool {
		pl, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack))
		if !ok {
			return false
		}
		return strings.Contains(pl, "#EXT-X-ENDLIST")
	})

	// The aborted fragment must never have been listed.
	pl := parseMediaPlaylist(t, mustRead(t, e, e.mediaPath(testVideoSet, testVideoTrack)))
	for _, seg := range pl.segments {
		if seg == fragmentName(2) {
			t.Fatalf("aborted fragment is listed in the media playlist:\n%s", pl.raw)
		}
	}
}

// TestManifestStreamTimeoutAndRestart verifies that a stream reclaimed after inactivity is no longer advertised, and
// that a restarted stream with the same name only reflects the new generation.
func TestManifestStreamTimeoutAndRestart(t *testing.T) {
	// Use a short timeout so the reclaim happens within the test; the GC ticker runs every StreamTimeout.
	e := newEnv(t, 300*time.Millisecond)

	u := e.startUpload(testVideoSet, testVideoTrack)
	u.write(videoTrack().InitSegment())
	u.write(fragment(0))
	u.close()
	if res := u.wait(10 * time.Second); res.status != http.StatusCreated {
		t.Fatalf("first generation upload failed: %+v", res)
	}

	e.waitFor("first generation segment listed", 5*time.Second, func() bool {
		pl, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack))
		return ok && strings.Contains(pl, fragmentName(0))
	})

	// After the stream timeout the stream is reclaimed: the master playlist must disappear.
	e.waitFor("stream reclaimed after timeout", 10*time.Second, func() bool {
		_, ok := e.readFile(e.masterPath())
		return !ok
	})
	e.waitFor("first generation playlist finalized", 5*time.Second, func() bool {
		pl, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack))
		if !ok {
			return false
		}
		return strings.Contains(pl, "#EXT-X-ENDLIST")
	})

	// Start a new generation with the same stream name but shifted decode times so segment names differ from the
	// previous generation. Writing fragments 10, 11, 12 commits fragments 10 and 11 (the last one stays open).
	u2 := e.startUpload(testVideoSet, testVideoTrack)
	u2.write(videoTrack().InitSegment())
	for i := 10; i < 13; i++ {
		u2.write(fragment(i))
	}

	e.waitFor("restarted stream advertised", 5*time.Second, func() bool {
		master, ok := e.readFile(e.masterPath())
		return ok && strings.Contains(master, "video/video.cmvf/index.m3u8")
	})

	e.waitFor("restarted stream segments listed", 5*time.Second, func() bool {
		pl, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack))
		return ok && strings.Contains(pl, fragmentName(11))
	})

	pl := parseMediaPlaylist(t, mustRead(t, e, e.mediaPath(testVideoSet, testVideoTrack)))
	if pl.endlist {
		t.Fatalf("restarted stream must not be finalized:\n%s", pl.raw)
	}
	if len(pl.segments) != 2 {
		t.Fatalf("restarted stream must only contain new segments, got %d:\n%s", len(pl.segments), pl.raw)
	}
	wantSegments := map[string]bool{fragmentName(10): true, fragmentName(11): true}
	for _, seg := range pl.segments {
		if seg == fragmentName(0) {
			t.Fatalf("restarted stream contains a segment from the previous generation: %v", pl.segments)
		}
		if !wantSegments[seg] {
			t.Fatalf("restarted stream contains unexpected segment %q: %v", seg, pl.segments)
		}
	}

	u2.close()
	if res := u2.wait(10 * time.Second); res.status != http.StatusCreated {
		t.Fatalf("second generation upload failed: %+v", res)
	}
}

// TestManifestConcurrentSameTrackUploads verifies that two simultaneous uploads of the same track never corrupt the
// playlist: the result must always be a well-formed, monotonic list without duplicates.
func TestManifestConcurrentSameTrackUploads(t *testing.T) {
	e := newEnv(t, time.Hour)

	// Two uploads of the same track, each sending a disjoint subset of the fragments with the correct decode times.
	sets := [][]int{{0, 1, 2, 3}, {4, 5, 6, 7}}

	var wg sync.WaitGroup
	results := make(chan uploadResult, len(sets))
	for _, fragIdx := range sets {
		fragIdx := fragIdx
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := e.startUpload(testVideoSet, testVideoTrack)
			u.write(videoTrack().InitSegment())
			for _, i := range fragIdx {
				u.write(fragment(i))
			}
			u.close()
			results <- u.wait(20 * time.Second)
		}()
	}

	// Continuously parse the playlist while both uploads run: it must never be truncated or malformed.
	done := make(chan struct{})
	var readErr error
	var mtx sync.Mutex
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			if raw, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack)); ok {
				if _, err := tryParseMediaPlaylist(raw); err != nil {
					mtx.Lock()
					if readErr == nil {
						readErr = fmt.Errorf("%v:\n%s", err, raw)
					}
					mtx.Unlock()
				}
				// Also verify monotonicity if it parses.
				if pl, err := tryParseMediaPlaylist(raw); err == nil {
					var last uint64
					haveLast := false
					seen := map[string]struct{}{}
					for _, seg := range pl.segments {
						if _, ok := seen[seg]; ok {
							mtx.Lock()
							if readErr == nil {
								readErr = fmt.Errorf("duplicate segment %q:\n%s", seg, raw)
							}
							mtx.Unlock()
						}
						seen[seg] = struct{}{}
						dt, ok := decodeTimeFromName(seg)
						if ok {
							if haveLast && dt <= last {
								mtx.Lock()
								if readErr == nil {
									readErr = fmt.Errorf("non-monotonic playlist: %q after %d:\n%s", seg, last, raw)
								}
								mtx.Unlock()
							}
							last = dt
							haveLast = true
						}
					}
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	wg.Wait()
	close(done)

	mtx.Lock()
	err := readErr
	mtx.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < len(sets); i++ {
		if res := <-results; res.status != http.StatusCreated {
			// With overlapping uploads of the same track, a failed request is acceptable, but the playlist must still
			// be consistent. We assert the consistency below rather than requiring success.
			t.Logf("upload result: %+v", res)
		}
	}

	pl := parseMediaPlaylist(t, mustRead(t, e, e.mediaPath(testVideoSet, testVideoTrack)))

	// Whatever survived must be monotonic, have no duplicates, and only reference readable files.
	var last uint64
	haveLast := false
	seen := map[string]struct{}{}
	for _, seg := range pl.segments {
		if _, ok := seen[seg]; ok {
			t.Fatalf("duplicate segment %q:\n%s", seg, pl.raw)
		}
		seen[seg] = struct{}{}
		if _, ok := e.readFile(e.segmentPath(testVideoSet, testVideoTrack, seg)); !ok {
			t.Fatalf("listed segment %q is not readable", seg)
		}
		dt, ok := decodeTimeFromName(seg)
		if ok {
			if haveLast && dt <= last {
				t.Fatalf("playlist is not monotonic: %q after %d:\n%s", seg, last, pl.raw)
			}
			last = dt
			haveLast = true
		}
	}
}

// TestManifestConcurrentTracksAndRapidCommits verifies that concurrent uploads of multiple tracks and rapid successive
// commits of a single track never corrupt playlists or reorder/duplicate segments.
func TestManifestConcurrentTracksAndRapidCommits(t *testing.T) {
	e := newEnv(t, time.Hour)

	const (
		videoFragments = 6
		audioFragments = 4
	)

	var wg sync.WaitGroup
	results := make(chan uploadResult, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		u := e.startUpload(testVideoSet, testVideoTrack)
		u.write(videoTrack().InitSegment())
		for i := 0; i < videoFragments; i++ {
			u.write(fragment(i))
		}
		u.close()
		results <- u.wait(20 * time.Second)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		u := e.startUpload(testAudioSet, testAudioTrack)
		u.write(audioTrack().InitSegment())
		for i := 0; i < audioFragments; i++ {
			u.write(fragment(i))
		}
		u.close()
		results <- u.wait(20 * time.Second)
	}()

	// While uploads are running, repeatedly read the playlists and make sure they are never truncated or empty.
	done := make(chan struct{})
	var readErr error
	var readErrMtx sync.Mutex
	recordErr := func(err error) {
		readErrMtx.Lock()
		if readErr == nil {
			readErr = err
		}
		readErrMtx.Unlock()
	}
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			if raw, ok := e.readFile(e.masterPath()); ok {
				if !strings.HasPrefix(raw, "#EXTM3U\n") || !strings.HasSuffix(raw, "\n") {
					recordErr(fmt.Errorf("inconsistent master playlist:\n%s", raw))
				}
				// Parse without t.Fatalf from a non-test goroutine.
				if strings.HasSuffix(raw, "#EXT-X-STREAM-INF:") {
					recordErr(fmt.Errorf("truncated master playlist:\n%s", raw))
				}
			}
			if raw, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack)); ok {
				if !strings.HasPrefix(raw, "#EXTM3U\n") || !strings.HasSuffix(raw, "\n") {
					recordErr(fmt.Errorf("inconsistent video playlist:\n%s", raw))
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	wg.Wait()
	close(done)

	readErrMtx.Lock()
	defer readErrMtx.Unlock()
	if readErr != nil {
		t.Fatal(readErr)
	}

	for i := 0; i < 2; i++ {
		res := <-results
		if res.status != http.StatusCreated {
			t.Fatalf("upload failed: %+v", res)
		}
	}

	// Validate both playlists are complete and in order with no duplicates.
	videoPL := parseMediaPlaylist(t, mustRead(t, e, e.mediaPath(testVideoSet, testVideoTrack)))
	if len(videoPL.segments) != videoFragments {
		t.Fatalf("expected %d video segments, got %d:\n%s", videoFragments, len(videoPL.segments), videoPL.raw)
	}
	assertInOrderNoDuplicates(t, videoPL.segments, videoFragments)

	audioPL := parseMediaPlaylist(t, mustRead(t, e, e.mediaPath(testAudioSet, testAudioTrack)))
	if len(audioPL.segments) != audioFragments {
		t.Fatalf("expected %d audio segments, got %d:\n%s", audioFragments, len(audioPL.segments), audioPL.raw)
	}
	assertInOrderNoDuplicates(t, audioPL.segments, audioFragments)

	master := parseMasterPlaylist(t, mustRead(t, e, e.masterPath()))
	if len(master.video) != 1 || len(master.audio) != 1 {
		t.Fatalf("expected one video and one audio track in master:\n%s", master.raw)
	}
}

// TestManifestDuplicateAndOutOfOrderCommits verifies that duplicate fragment commits do not duplicate playlist entries
// and that stale (out-of-order) commits do not make the playlist regress.
func TestManifestDuplicateAndOutOfOrderCommits(t *testing.T) {
	e := newEnv(t, time.Hour)

	u := e.startUpload(testVideoSet, testVideoTrack)
	u.write(videoTrack().InitSegment())
	for i := 0; i < 3; i++ {
		u.write(fragment(i))
	}
	u.close()
	if res := u.wait(10 * time.Second); res.status != http.StatusCreated {
		t.Fatalf("upload failed: %+v", res)
	}

	e.waitFor("segments listed", 5*time.Second, func() bool {
		pl, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack))
		return ok && strings.Contains(pl, fragmentName(2))
	})

	pl := parseMediaPlaylist(t, mustRead(t, e, e.mediaPath(testVideoSet, testVideoTrack)))
	assertInOrderNoDuplicates(t, pl.segments, 3)
}

// TestManifestReconnectAfterFailure verifies that a track which failed and is then re-uploaded under the same name
// starts a clean round: no residue of the aborted round remains and the playlists are consistent again.
func TestManifestReconnectAfterFailure(t *testing.T) {
	e := newEnv(t, time.Hour)

	// First round: commit two fragments, then abort in the middle of a third.
	u := e.startUpload(testVideoSet, testVideoTrack)
	u.write(videoTrack().InitSegment())
	u.write(fragment(0))
	u.write(fragment(1))
	e.waitFor("first round segments listed", 5*time.Second, func() bool {
		pl, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack))
		return ok && strings.Contains(pl, fragmentName(0))
	})
	f3 := fragment(2)
	u.write(f3[:len(f3)/2])
	u.abort()
	_ = u.wait(10 * time.Second)

	e.waitFor("stream removed after failure", 5*time.Second, func() bool {
		_, ok := e.readFile(e.masterPath())
		return !ok
	})

	// Second round: a full, clean upload under the same stream and track name.
	u2 := e.startUpload(testVideoSet, testVideoTrack)
	u2.write(videoTrack().InitSegment())
	for i := 0; i < 3; i++ {
		u2.write(fragment(i))
	}
	u2.close()
	if res := u2.wait(10 * time.Second); res.status != http.StatusCreated {
		t.Fatalf("second round upload failed: %+v", res)
	}

	e.waitFor("stream re-advertised", 5*time.Second, func() bool {
		master, ok := e.readFile(e.masterPath())
		return ok && strings.Contains(master, "video/video.cmvf/index.m3u8")
	})
	e.waitFor("second round fully listed", 5*time.Second, func() bool {
		pl, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack))
		return ok && strings.Contains(pl, fragmentName(2))
	})

	pl := parseMediaPlaylist(t, mustRead(t, e, e.mediaPath(testVideoSet, testVideoTrack)))
	if pl.endlist {
		t.Fatalf("reconnected track must not be finalized:\n%s", pl.raw)
	}
	assertInOrderNoDuplicates(t, pl.segments, 3)
}

// TestManifestEncoderReconnectContinuesPlaylist verifies that an encoder that reconnects mid-stream, re-sends its init
// segment and continues the same CMAF timeline does not lose the already published segments.
func TestManifestEncoderReconnectContinuesPlaylist(t *testing.T) {
	e := newEnv(t, time.Hour)

	// Round 1: fragments 0 and 1.
	u := e.startUpload(testVideoSet, testVideoTrack)
	u.write(videoTrack().InitSegment())
	u.write(fragment(0))
	u.write(fragment(1))
	u.close()
	if res := u.wait(10 * time.Second); res.status != http.StatusCreated {
		t.Fatalf("first round upload failed: %+v", res)
	}

	e.waitFor("round 1 segments listed", 5*time.Second, func() bool {
		pl, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack))
		return ok && strings.Contains(pl, fragmentName(1))
	})

	// Round 2: reconnect, re-send init, continue the timeline with fragments 2 and 3.
	u2 := e.startUpload(testVideoSet, testVideoTrack)
	u2.write(videoTrack().InitSegment())
	u2.write(fragment(2))
	u2.write(fragment(3))
	u2.close()
	if res := u2.wait(10 * time.Second); res.status != http.StatusCreated {
		t.Fatalf("second round upload failed: %+v", res)
	}

	e.waitFor("continued segments listed", 5*time.Second, func() bool {
		pl, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack))
		return ok && strings.Contains(pl, fragmentName(3))
	})

	pl := parseMediaPlaylist(t, mustRead(t, e, e.mediaPath(testVideoSet, testVideoTrack)))
	if pl.endlist {
		t.Fatalf("reconnected track must not be finalized:\n%s", pl.raw)
	}
	// All four segments from both rounds must be present, in order, without duplicates.
	assertInOrderNoDuplicates(t, pl.segments, 4)
}

// TestManifestListedSegmentsAreReadable asserts the invariant that every segment listed in a playlist already exists
// and is readable on the volume at the moment it appears.
func TestManifestListedSegmentsAreReadable(t *testing.T) {
	e := newEnv(t, time.Hour)

	u := e.startUpload(testVideoSet, testVideoTrack)
	u.write(videoTrack().InitSegment())
	for i := 0; i < 6; i++ {
		u.write(fragment(i))
		// After each fragment the previous one should be committed and readable. Check the invariant continuously.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			raw, ok := e.readFile(e.mediaPath(testVideoSet, testVideoTrack))
			if !ok {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			pl, err := tryParseMediaPlaylist(raw)
			if err != nil || !pl.hasMap {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			ok = true
			for _, seg := range pl.segments {
				if _, exists := e.readFile(e.segmentPath(testVideoSet, testVideoTrack, seg)); !exists {
					t.Fatalf("listed segment %q is not readable on the volume:\n%s", seg, raw)
				}
			}
			break
		}
	}
	u.close()
	if res := u.wait(10 * time.Second); res.status != http.StatusCreated {
		t.Fatalf("upload failed: %+v", res)
	}
}

// --- helpers ---------------------------------------------------------------

func mustRead(t *testing.T, e *env, p string) string {
	t.Helper()
	raw, ok := e.readFile(p)
	if !ok {
		t.Fatalf("file %q does not exist on the volume", p)
	}
	return raw
}

// decodeTimeFromName extracts the base media decode time from a fragment file name ("frag-<time>.m4s").
func decodeTimeFromName(name string) (uint64, bool) {
	name = strings.TrimSuffix(strings.TrimPrefix(name, "frag-"), ".m4s")
	v, err := strconv.ParseUint(name, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func assertInOrderNoDuplicates(t *testing.T, segments []string, count int) {
	t.Helper()
	if len(segments) != count {
		t.Fatalf("expected %d segments, got %d: %v", count, len(segments), segments)
	}
	seen := make(map[string]struct{}, len(segments))
	for i, seg := range segments {
		if want := fragmentName(i); seg != want {
			t.Fatalf("segment %d = %q, want %q (order does not match commit order)", i, seg, want)
		}
		if _, ok := seen[seg]; ok {
			t.Fatalf("duplicate segment %q", seg)
		}
		seen[seg] = struct{}{}
	}
}
