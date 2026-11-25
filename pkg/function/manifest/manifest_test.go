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

package manifest

import (
	"strings"
	"testing"

	"github.com/nagare-media/ingest/pkg/media"
)

func TestRenderMasterPlaylistKeepsDefaultAudioStable(t *testing.T) {
	hls := newHLSManifest(&media.Presentation{Name: "/host/example.str"})

	// Add a video track and two audio tracks.
	hls.addTrack(groupVideo, &hlsTrack{
		setName: "video", trackName: "v720", uri: "video/v720/index.m3u8",
		resolution: "1280x720", initSegment: &hlsSegment{path: "init.mp4"},
	})
	hls.addTrack(groupAudio, &hlsTrack{
		setName: "audio", trackName: "a64", uri: "audio/a64/index.m3u8",
		initSegment: &hlsSegment{path: "init.mp4"},
	})

	first := string(renderMasterPlaylist(hls))
	if !strings.Contains(first, `NAME="audio_0",DEFAULT=YES`) {
		t.Fatalf("first audio track must be default:\n%s", first)
	}

	// Adding a second audio track must not change the default.
	hls.addTrack(groupAudio, &hlsTrack{
		setName: "audio", trackName: "a128", uri: "audio/a128/index.m3u8",
		initSegment: &hlsSegment{path: "init.mp4"},
	})
	second := string(renderMasterPlaylist(hls))

	lines := strings.Split(second, "\n")
	var defaults []string
	for _, line := range lines {
		if strings.HasPrefix(line, "#EXT-X-MEDIA:") && strings.Contains(line, "DEFAULT=YES") {
			defaults = append(defaults, line)
		}
	}
	if len(defaults) != 1 {
		t.Fatalf("expected exactly one default audio track:\n%s", second)
	}
	if !strings.Contains(defaults[0], "audio_0") {
		t.Fatalf("default audio track changed after adding a track:\n%s", second)
	}

	// Rendering twice must be byte-for-byte identical (deterministic order).
	if third := string(renderMasterPlaylist(hls)); third != second {
		t.Fatalf("master playlist rendering is not deterministic:\n--- first ---\n%s\n--- second ---\n%s", second, third)
	}
}

func TestRenderMasterPlaylistExcludesStoppedTracks(t *testing.T) {
	hls := newHLSManifest(&media.Presentation{Name: "/host/example.str"})

	trk := &hlsTrack{
		setName: "video", trackName: "v720", uri: "video/v720/index.m3u8",
		resolution: "1280x720", initSegment: &hlsSegment{path: "init.mp4"},
	}
	hls.addTrack(groupVideo, trk)

	if !strings.Contains(string(renderMasterPlaylist(hls)), "video/v720/index.m3u8") {
		t.Fatal("expected track to be advertised")
	}

	trk.stopped = true
	if hasActiveTrack(hls) {
		t.Fatal("a stopped track must not count as active")
	}
	if strings.Contains(string(renderMasterPlaylist(hls)), "video/v720/index.m3u8") {
		t.Fatal("a stopped track must not be advertised")
	}
}

func TestRenderMediaPlaylistTargetDuration(t *testing.T) {
	trk := &hlsTrack{
		initSegment: &hlsSegment{path: "init.mp4"},
		segments: []*hlsSegment{
			{path: "a.m4s", duration: 0.5},
			{path: "b.m4s", duration: 1.9},
			{path: "c.m4s", duration: 1.0},
		},
	}

	raw := string(renderMediaPlaylist(trk, false))
	if !strings.Contains(raw, "#EXT-X-TARGETDURATION:2\n") {
		t.Fatalf("target duration must round up to the largest segment duration:\n%s", raw)
	}
	if !strings.Contains(raw, "#EXT-X-MAP:URI=\"init.mp4\"\n") {
		t.Fatalf("missing init segment:\n%s", raw)
	}
	if strings.Contains(raw, "#EXT-X-ENDLIST") {
		t.Fatalf("a live playlist must not be finalized:\n%s", raw)
	}

	raw = string(renderMediaPlaylist(trk, true))
	if !strings.HasSuffix(raw, "#EXT-X-ENDLIST\n") {
		t.Fatalf("a finalized playlist must end with EXT-X-ENDLIST:\n%s", raw)
	}
}

func TestAddSegmentOrderingAndDedup(t *testing.T) {
	trk := &hlsTrack{}

	if !trk.addSegment(&hlsSegment{path: "f0", decodeTime: 0, hasDecodeTime: true}) {
		t.Fatal("first segment must be added")
	}
	if !trk.addSegment(&hlsSegment{path: "f1", decodeTime: 1, hasDecodeTime: true}) {
		t.Fatal("in-order segment must be added")
	}
	// duplicate path
	if trk.addSegment(&hlsSegment{path: "f1", decodeTime: 1, hasDecodeTime: true}) {
		t.Fatal("duplicate segment must be dropped")
	}
	// stale segment (older decode time)
	if trk.addSegment(&hlsSegment{path: "f-old", decodeTime: 0, hasDecodeTime: true}) {
		t.Fatal("stale segment must be dropped")
	}

	if len(trk.segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(trk.segments))
	}
	if trk.segments[0].path != "f0" || trk.segments[1].path != "f1" {
		t.Fatalf("segments out of order: %v", []string{trk.segments[0].path, trk.segments[1].path})
	}
}

func TestLastDecodeTime(t *testing.T) {
	trk := &hlsTrack{}
	if _, ok := trk.lastDecodeTime(); ok {
		t.Fatal("empty track must not report a decode time")
	}
	trk.segments = []*hlsSegment{
		{path: "a", decodeTime: 5, hasDecodeTime: true},
		{path: "b"}, // no decode time
	}
	if dt, ok := trk.lastDecodeTime(); !ok || dt != 5 {
		t.Fatalf("lastDecodeTime = %d, %v; want 5, true", dt, ok)
	}
}
