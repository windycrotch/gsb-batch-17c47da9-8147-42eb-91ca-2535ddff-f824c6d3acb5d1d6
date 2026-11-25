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
	"bytes"
	"context"
	"fmt"
	"math"
	"path"
	"sync"

	"go.uber.org/zap"

	"github.com/nagare-media/ingest/pkg/config/v1alpha1"
	"github.com/nagare-media/ingest/pkg/event"
	"github.com/nagare-media/ingest/pkg/function"
	"github.com/nagare-media/ingest/pkg/media"
	"github.com/nagare-media/ingest/pkg/volume"
)

type manifest struct {
	cfg v1alpha1.Function
	vol volume.Volume
	log *zap.SugaredLogger

	appPathPrefix string

	// mtx serializes all access to the manifest state and all writes of playlist files. Playlists are always rendered
	// in memory and written as a single atomic operation (see writePlaylist), so readers never observe a partially
	// written playlist.
	mtx sync.Mutex
	hls map[string]*hlsManifest
}

type hlsManifest struct {
	pathPrefix string

	// presentation identifies the generation (one ingest run) of the stream this manifest belongs to. Events for a
	// different generation must not leak into this manifest.
	presentation *media.Presentation

	// ended is true once the stream has been stopped. A stopped stream must no longer be advertised as active in the
	// master playlist; its media playlists are finalized with EXT-X-ENDLIST.
	ended bool

	audio hlsGroup
	video hlsGroup
}

type hlsGroup struct {
	tracks map[string]*hlsTrack
	// order keeps the registration order so that the generated playlists are deterministic. Iterating over tracks
	// directly would produce a different order (and a different default audio track) on every regeneration.
	order []string
}

type hlsTrack struct {
	trk *media.Track

	setName   string
	trackName string
	uri       string

	resolution string
	timescale  uint32

	initSegment *hlsSegment
	segments    []*hlsSegment
	// segmentPaths tracks the basenames of already listed segments to avoid duplicates when a fragment is committed
	// more than once (e.g. retransmissions or concurrent uploads of the same track).
	segmentPaths map[string]struct{}

	ended bool
	// stopped is true if the track was terminated prematurely (failed upload) rather than finished normally. A stopped
	// track is not advertised in the master playlist any more.
	stopped bool

	// pendingReset is set when a fresh init segment was committed for a still-live track. It defers dropping the already
	// listed segments until the first fragment of the new round arrives: if that fragment continues the timeline
	// (higher or equal decode time) the playlist is left untouched (an encoder reconnect), but if the timeline regressed
	// the old segments are dropped so the playlist never mixes two rounds.
	pendingReset bool
}

type hlsSegment struct {
	path     string
	duration float64

	// decodeTime is the base media decode time of the fragment. It is used to keep the playlist monotonic even if
	// fragment commits arrive out of order (e.g. overlapping uploads of the same track).
	decodeTime    uint64
	hasDecodeTime bool
}

func New(cfg v1alpha1.Function) (function.Function, error) {
	if err := function.CheckAndSetDefaults(&cfg); err != nil {
		return nil, err
	}

	fn := &manifest{
		cfg: cfg,
		hls: make(map[string]*hlsManifest),
	}

	return fn, nil
}

func (fn *manifest) Config() v1alpha1.Function {
	return fn.cfg
}

func (fn *manifest) Exec(ctx context.Context, execCtx function.ExecCtx) error {
	fn.log = execCtx.Logger()

	vol, ok := execCtx.VolumeRegistry().Get(fn.cfg.Manifest.VolumeRef.Name)
	if !ok {
		return fmt.Errorf("volume '%s' not found", fn.cfg.Manifest.VolumeRef.Name)
	}
	fn.vol = vol

	fn.appPathPrefix = execCtx.PathPrefix()

	eventStream := execCtx.App().EventStream().Sub()
	defer execCtx.App().EventStream().Desub(eventStream)

	for {
		select {
		case <-ctx.Done():
			// TODO: cleanup
			return nil

		case es, ok := <-eventStream:
			if !ok {
				return nil
			}

			switch e := es.(type) {
			case *event.StreamEvent:
				switch e.Type {
				case event.StreamStartEvent:
					fn.handleStreamStarted(e)
				case event.StreamStopEvent:
					fn.handleStreamStopped(e)
				}

			case *event.TrackEvent:
				switch e.Type {
				case event.TrackEndEvent:
					fn.handleTrackEnded(e, false)
				case event.TrackStopEvent:
					fn.handleTrackEnded(e, true)
				}

			case *event.InitSegmentEvent:
				switch e.Type {
				case event.InitSegmentCommittedEvent:
					fn.handleInitSegmentCommitted(e)
				}

			case *event.FragmentEvent:
				switch e.Type {
				case event.FragmentCommittedEvent:
					fn.handleFragmentCommitted(e)
				}
			}
		}
	}
}

// handleStreamStarted resets the manifest state for a new generation of a stream.
//
// A stream is identified by its name. When an encoder restarts, a new generation replaces the old one. Playlists of the
// previous generation are removed so that players can never observe segments from an earlier run.
func (fn *manifest) handleStreamStarted(e *event.StreamEvent) {
	if e.Stream == nil {
		return
	}
	pres := e.Stream

	fn.mtx.Lock()
	defer fn.mtx.Unlock()

	if old, ok := fn.hls[pres.Name]; ok {
		if old.presentation == pres {
			// duplicate start event for the current generation
			return
		}
		fn.removePlaylists(old)
	}

	fn.hls[pres.Name] = newHLSManifest(pres)
}

// handleStreamStopped finalizes all playlists of a stream. The stream is no longer advertised in the master playlist and
// all media playlists are ended with EXT-X-ENDLIST so that the end of the stream is observable.
func (fn *manifest) handleStreamStopped(e *event.StreamEvent) {
	if e.Stream == nil {
		return
	}
	pres := e.Stream

	fn.mtx.Lock()
	defer fn.mtx.Unlock()

	hls, ok := fn.hls[pres.Name]
	if !ok || hls.presentation != pres {
		return
	}
	if hls.ended {
		return
	}

	hls.ended = true
	for _, hlsTrk := range hls.allTracks() {
		hlsTrk.ended = true
		hlsTrk.stopped = true
		if hlsTrk.initSegment != nil {
			fn.writeMediaPlaylist(hls, hlsTrk)
		}
	}

	// A stopped stream must not be advertised as active any more. The master playlist is removed entirely so that a
	// reader polling it observes the stream ending instead of a stale variant list. The media playlists keep the
	// final segment list and are finalized with EXT-X-ENDLIST.
	_ = fn.vol.Delete(fn.masterPlaylistPath(hls))
}

// handleTrackEnded finalizes the media playlist of a single track.
//
// A normally finished track (TrackEndEvent) stays live and advertised: an encoder may reconnect and continue the same
// track, so the playlist must remain append-only. Only a stopped track (TrackStopEvent: failed upload or reclaimed
// stream) is finalized with EXT-X-ENDLIST and removed from the master playlist, so the end is observable.
func (fn *manifest) handleTrackEnded(e *event.TrackEvent, stopped bool) {
	if e.Track == nil || e.Track.SwitchingSet == nil || e.Track.SwitchingSet.Presentation == nil {
		return
	}
	if !stopped {
		// Normal end of a track upload: nothing to finalize.
		return
	}
	pres := e.Track.SwitchingSet.Presentation

	fn.mtx.Lock()
	defer fn.mtx.Unlock()

	hls, ok := fn.hls[pres.Name]
	if !ok || hls.presentation != pres {
		return
	}

	hlsTrk := hls.findTrack(e.Track)
	if hlsTrk == nil {
		// never initialized or already forgotten
		return
	}
	if hlsTrk.ended {
		return
	}

	hlsTrk.ended = true
	hlsTrk.stopped = true

	if hlsTrk.initSegment != nil {
		fn.writeMediaPlaylist(hls, hlsTrk)
	}

	if !hls.ended {
		fn.writeMasterPlaylist(hls)
	}
}

func (fn *manifest) handleInitSegmentCommitted(e *event.InitSegmentEvent) {
	if e.Track == nil || e.Track.SwitchingSet == nil || e.Track.SwitchingSet.Presentation == nil {
		return
	}
	trk := e.Track
	pres := trk.SwitchingSet.Presentation

	// Read all track attributes while holding the presentation lock. Only plain values are copied: the manifest must
	// never keep the presentation locked while writing playlists.
	trk.RLock()
	if trk.CMAFHeader == nil {
		trk.RUnlock()
		return
	}
	handlerType := ""
	if trk.CMAFHeader.Moov != nil &&
		trk.CMAFHeader.Moov.Trak != nil && trk.CMAFHeader.Moov.Trak.Mdia != nil &&
		trk.CMAFHeader.Moov.Trak.Mdia.Hdlr != nil {
		handlerType = trk.CMAFHeader.Moov.Trak.Mdia.Hdlr.HandlerType
	}
	timescale, _ := trk.Timescale()
	resolution, _ := trk.Resolution()
	trk.RUnlock()

	group := groupKindFor(handlerType)
	if group == groupUnknown {
		fn.log.With("handlerType", handlerType).Warn("ignoring track with unsupported handler type")
		return
	}

	var initPath string
	if e.File != nil {
		initPath = path.Base(e.File.Name())
	}

	setName := trk.SwitchingSet.Name
	trackName := trk.Name

	fn.mtx.Lock()
	defer fn.mtx.Unlock()

	hls, ok := fn.hls[pres.Name]
	if ok && hls.presentation != pres {
		// A previous generation of this stream. Its playlists must not survive into the new one.
		fn.removePlaylists(hls)
		ok = false
	}
	if !ok {
		hls = newHLSManifest(pres)
		fn.hls[pres.Name] = hls
	}
	if hls.ended {
		// The stream has already been stopped. Late initialization of an in-flight upload must not bring it back.
		return
	}

	hlsTrk := hls.findTrack(trk)

	firstInit := hlsTrk == nil
	if firstInit {
		hlsTrk = &hlsTrack{trk: trk}
	}

	// Decide how this init affects the track's history.
	//
	//   * fresh track: nothing to keep.
	//   * previously stopped (failed upload): drop the residue of the aborted round.
	//   * still-live track: defer the decision until the first fragment of the new round arrives, because an encoder
	//     reconnect continues the same timeline (history must be kept) while a true restart begins a new timeline
	//     (history must be dropped). See handleFragmentCommitted.
	if firstInit || hlsTrk.ended {
		hlsTrk.segments = nil
		hlsTrk.segmentPaths = make(map[string]struct{})
		hlsTrk.pendingReset = false
	} else {
		hlsTrk.pendingReset = true
	}

	hlsTrk.setName = setName
	hlsTrk.trackName = trackName
	if firstInit {
		// Only derive the URI once: it is part of the published master playlist and recomputing it on a
		// re-initialization would needlessly change the advertised URI.
		hlsTrk.uri = path.Join(setName, trackName, "index.m3u8")
	}
	hlsTrk.resolution = resolution
	hlsTrk.timescale = timescale
	hlsTrk.ended = false
	hlsTrk.stopped = false
	hlsTrk.initSegment = &hlsSegment{path: initPath}

	if firstInit {
		hls.addTrack(group, hlsTrk)
	}

	fn.writeMasterPlaylist(hls)
	fn.writeMediaPlaylist(hls, hlsTrk)
}

func (fn *manifest) handleFragmentCommitted(e *event.FragmentEvent) {
	if e.Track == nil || e.Track.SwitchingSet == nil || e.Track.SwitchingSet.Presentation == nil || e.File == nil {
		return
	}
	trk := e.Track
	pres := trk.SwitchingSet.Presentation

	fn.mtx.Lock()
	defer fn.mtx.Unlock()

	hls, ok := fn.hls[pres.Name]
	if !ok || hls.presentation != pres {
		// Fragment of an unknown or outdated generation: it must not be listed.
		return
	}
	if hls.ended {
		// The stream has already been stopped. Late commits must not bring it back.
		return
	}

	hlsTrk := hls.findTrack(trk)
	if hlsTrk == nil {
		fn.log.Error("failed to find track for new fragment")
		return
	}
	if hlsTrk.ended {
		// The track has already ended. A late commit must not re-open a finalized playlist.
		return
	}

	if hlsTrk.timescale == 0 {
		fn.log.Error("failed to find track timescale")
		return
	}

	seg := &hlsSegment{
		path:     path.Base(e.File.Name()),
		duration: float64(0),
	}
	if e.Fragment != nil {
		seg.duration = float64(e.Fragment.Duration()) / float64(hlsTrk.timescale)
		seg.decodeTime, seg.hasDecodeTime = e.Fragment.BaseMediaDecodeTime()
	}

	// A fresh init segment was committed for this still-live track. Now that the first fragment of the new round has
	// arrived we can tell a reconnect (timeline continues) from a restart (timeline regressed).
	if hlsTrk.pendingReset {
		hlsTrk.pendingReset = false
		if last, ok := hlsTrk.lastDecodeTime(); ok && seg.hasDecodeTime && seg.decodeTime < last {
			// The new round restarts the timeline: drop the previous round's segments so the two are never mixed.
			hlsTrk.segments = nil
			hlsTrk.segmentPaths = make(map[string]struct{})
		}
	}

	if !hlsTrk.addSegment(seg) {
		// already listed
		return
	}

	fn.writeMediaPlaylist(hls, hlsTrk)
}

// lastDecodeTime returns the decode time of the most recently listed segment that carries one.
func (trk *hlsTrack) lastDecodeTime() (uint64, bool) {
	for i := len(trk.segments) - 1; i >= 0; i-- {
		if trk.segments[i].hasDecodeTime {
			return trk.segments[i].decodeTime, true
		}
	}
	return 0, false
}

// hlsGroupKind is the HLS media group a track belongs to.
type hlsGroupKind int

const (
	groupUnknown hlsGroupKind = iota
	groupAudio
	groupVideo
)

func groupKindFor(handlerType string) hlsGroupKind {
	switch handlerType {
	case "vide":
		return groupVideo
	case "soun":
		return groupAudio
	default:
		return groupUnknown
	}
}

func newHLSManifest(pres *media.Presentation) *hlsManifest {
	hls := &hlsManifest{
		pathPrefix:   pres.Name,
		presentation: pres,
	}
	hls.audio.tracks = make(map[string]*hlsTrack)
	hls.video.tracks = make(map[string]*hlsTrack)
	return hls
}

func (hls *hlsManifest) group(kind hlsGroupKind) *hlsGroup {
	switch kind {
	case groupAudio:
		return &hls.audio
	case groupVideo:
		return &hls.video
	default:
		return nil
	}
}

func (hls *hlsManifest) addTrack(kind hlsGroupKind, trk *hlsTrack) {
	g := hls.group(kind)
	if g == nil {
		return
	}
	key := trk.trackKey()
	if _, ok := g.tracks[key]; !ok {
		g.order = append(g.order, key)
	}
	g.tracks[key] = trk
}

// findTrack returns the manifest track for the given media track, or nil. Tracks are identified by pointer identity: a
// re-initialization of the same track keeps the same manifest entry.
func (hls *hlsManifest) findTrack(trk *media.Track) *hlsTrack {
	for _, hlsTrk := range hls.allTracks() {
		if hlsTrk.trk == trk {
			return hlsTrk
		}
	}
	return nil
}

func (hls *hlsManifest) allTracks() []*hlsTrack {
	tracks := make([]*hlsTrack, 0, len(hls.audio.order)+len(hls.video.order))
	for _, key := range hls.audio.order {
		if trk := hls.audio.tracks[key]; trk != nil {
			tracks = append(tracks, trk)
		}
	}
	for _, key := range hls.video.order {
		if trk := hls.video.tracks[key]; trk != nil {
			tracks = append(tracks, trk)
		}
	}
	return tracks
}

func (trk *hlsTrack) trackKey() string {
	return trk.setName + "/" + trk.trackName
}

// active reports whether the track is initialized and can still be advertised in the master playlist.
func (trk *hlsTrack) active() bool {
	return trk != nil && trk.initSegment != nil && !trk.stopped
}

// addSegment adds a segment to the playlist. It returns false if the segment is already listed or is stale.
//
// Playlists are append-only: a segment that is older than the last listed one is dropped. That keeps the playlist
// monotonic even when commit events arrive out of order (e.g. from overlapping uploads of the same track). Segments
// without a decode time are appended in arrival order.
func (trk *hlsTrack) addSegment(seg *hlsSegment) bool {
	if trk.segmentPaths == nil {
		trk.segmentPaths = make(map[string]struct{})
	}
	if _, ok := trk.segmentPaths[seg.path]; ok {
		return false
	}

	if seg.hasDecodeTime && len(trk.segments) > 0 {
		last := trk.segments[len(trk.segments)-1]
		if last.hasDecodeTime && seg.decodeTime < last.decodeTime {
			// stale segment from an older position in the timeline
			return false
		}
	}

	trk.segmentPaths[seg.path] = struct{}{}
	trk.segments = append(trk.segments, seg)
	return true
}

func (fn *manifest) removePlaylists(hls *hlsManifest) {
	// Playlists of an outdated generation must not stay readable. All other modifications only happen while holding
	// fn.mtx, so there is no concurrent writer for these files.
	_ = fn.vol.Delete(fn.masterPlaylistPath(hls))
	for _, hlsTrk := range hls.allTracks() {
		_ = fn.vol.Delete(fn.mediaPlaylistPath(hls, hlsTrk))
	}
}

func (fn *manifest) masterPlaylistPath(hls *hlsManifest) string {
	return path.Join("/", fn.appPathPrefix, hls.pathPrefix, "master.m3u8")
}

func (fn *manifest) mediaPlaylistPath(hls *hlsManifest, hlsTrk *hlsTrack) string {
	return path.Join("/", fn.appPathPrefix, hls.pathPrefix, hlsTrk.setName, hlsTrk.trackName, "index.m3u8")
}

// writeMasterPlaylist writes the master playlist. If no stream is advertised any more (e.g. because every track was
// stopped), the master playlist is removed instead: an empty master playlist would be invalid and, worse, would still
// look like an active stream to a reader.
func (fn *manifest) writeMasterPlaylist(hls *hlsManifest) {
	if !hasActiveTrack(hls) {
		_ = fn.vol.Delete(fn.masterPlaylistPath(hls))
		return
	}
	fn.writePlaylist(fn.masterPlaylistPath(hls), renderMasterPlaylist(hls))
}

func hasActiveTrack(hls *hlsManifest) bool {
	for _, key := range hls.audio.order {
		if trk := hls.audio.tracks[key]; trk != nil && trk.active() {
			return true
		}
	}
	for _, key := range hls.video.order {
		if trk := hls.video.tracks[key]; trk != nil && trk.active() {
			return true
		}
	}
	return false
}

func (fn *manifest) writeMediaPlaylist(hls *hlsManifest, hlsTrk *hlsTrack) {
	fn.writePlaylist(fn.mediaPlaylistPath(hls, hlsTrk), renderMediaPlaylist(hlsTrk, hls.ended || hlsTrk.ended))
}

// writePlaylist writes the complete playlist content in one atomic operation. The content is rendered beforehand so a
// writer error can never leave a truncated or empty playlist behind.
func (fn *manifest) writePlaylist(filePath string, content []byte) {
	file, err := fn.vol.OpenCreate(filePath)
	if err != nil {
		fn.log.With("error", err).Errorf("failed to open HLS playlist: '%s'", filePath)
		return
	}

	fw, err := file.AcquireWriter(false)
	if err != nil {
		fn.log.With("error", err).Errorf("failed to acquire writer for HLS playlist: '%s'", filePath)
		return
	}

	if _, err := fw.Write(content); err != nil {
		if aerr := fw.Abort(); aerr != nil {
			fn.log.With("error", aerr).Warnf("failed to abort HLS playlist: '%s'", filePath)
		}
		fn.log.With("error", err).Errorf("failed to write HLS playlist: '%s'", filePath)
		return
	}

	if err := fw.Commit(); err != nil {
		fn.log.With("error", err).Errorf("failed to commit HLS playlist: '%s'", filePath)
	}
}

func renderMasterPlaylist(hls *hlsManifest) []byte {
	b := &bytes.Buffer{}
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")

	if hls.ended {
		return b.Bytes()
	}

	// Only tracks that are initialized, not stopped and not ended are advertised as active.
	activeAudio := make([]*hlsTrack, 0, len(hls.audio.order))
	for _, key := range hls.audio.order {
		trk := hls.audio.tracks[key]
		if !trk.active() {
			continue
		}
		activeAudio = append(activeAudio, trk)
	}

	for i, trk := range activeAudio {
		def := "NO"
		if i == 0 {
			// The first registered audio track is the default. Registration order is stable across playlist
			// regenerations, so the default track does not change when other tracks are added.
			def = "YES"
		}
		fmt.Fprintf(b, "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"audio_%d\",DEFAULT=%s,URI=\"%s\"\n", i, def, trk.uri)
	}

	audioAttr := ""
	if len(activeAudio) > 0 {
		audioAttr = "AUDIO=\"audio\","
	}

	for _, key := range hls.video.order {
		trk := hls.video.tracks[key]
		if !trk.active() {
			continue
		}

		resStr := ""
		if trk.resolution != "" {
			resStr = fmt.Sprintf("RESOLUTION=%s,", trk.resolution)
		}

		fmt.Fprintf(b, "#EXT-X-STREAM-INF:%s%s\n%s\n", audioAttr, resStr, trk.uri)
	}

	return b.Bytes()
}

func renderMediaPlaylist(hlsTrk *hlsTrack, ended bool) []byte {
	b := &bytes.Buffer{}

	// HLS requires the target duration to be an integer that is larger than or equal to every segment duration.
	targetDur := 1.0
	for _, seg := range hlsTrk.segments {
		if seg.duration > targetDur {
			targetDur = seg.duration
		}
	}

	fmt.Fprintf(b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(targetDur)))

	if hlsTrk.initSegment != nil {
		fmt.Fprintf(b, "#EXT-X-MAP:URI=\"%s\"\n", hlsTrk.initSegment.path)
	}

	for _, seg := range hlsTrk.segments {
		fmt.Fprintf(b, "#EXTINF:%.3f,\n%s\n", seg.duration, seg.path)
	}

	if ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}

	return b.Bytes()
}
