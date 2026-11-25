// WebRTC sinks management

package main

import (
	"github.com/pion/webrtc/v4"
)

// Generates an unique ID for each sink
func (node *WebRTC_CDN_Node) getSinkID() uint64 {
	node.mutexSinkCount.Lock()
	defer node.mutexSinkCount.Unlock()

	node.sinkCount++

	return node.sinkCount
}

// Registers a sink
func (node *WebRTC_CDN_Node) registerSink(sink *WRTC_Sink) {
	// Do not register a sink that was already closed
	// (its request may have been closed before registration ran)
	if sink.isClosed() {
		return
	}

	node.mutexStatus.Lock()

	if node.sinks[sink.sid] == nil {
		node.sinks[sink.sid] = make(map[uint64]*WRTC_Sink)
	}

	node.sinks[sink.sid][sink.sinkId] = sink

	var localTrackVideo, localTrackAudio *webrtc.TrackLocalStaticRTP
	hasTracks := false
	needResolve := false

	// Is there a ready source for it?
	if node.sources[sink.sid] != nil && node.sources[sink.sid].ready {
		localTrackVideo, localTrackAudio = node.sources[sink.sid].currentTracks()
		hasTracks = true
	} else if node.relays[sink.sid] != nil && node.relays[sink.sid].isReady() {
		// Is there a ready relay for it?
		localTrackVideo, localTrackAudio = node.relays[sink.sid].currentTracks()
		hasTracks = true
	} else if node.sources[sink.sid] == nil && node.relays[sink.sid] == nil {
		// No source and no relay in progress: ask other nodes.
		// If a source or a relay exists but is not ready yet, the sink
		// will be notified when it becomes ready (or re-resolved if it
		// fails), so there is no need to send another RESOLVE here.
		needResolve = true
	}

	node.mutexStatus.Unlock()

	if hasTracks {
		sink.onTracksReady(localTrackVideo, localTrackAudio)
		return
	}

	if needResolve {
		// Can't find any source, maybe other node has it?
		// Announce to other nodes to create the relay
		node.sendResolveMessage(sink.sid)
	}
}

// Removes a sink
func (node *WebRTC_CDN_Node) removeSink(sink *WRTC_Sink) {
	node.mutexStatus.Lock()

	if node.sinks[sink.sid] == nil {
		node.mutexStatus.Unlock()
		return
	}

	if node.sinks[sink.sid][sink.sinkId] != sink {
		node.mutexStatus.Unlock()
		return
	}

	delete(node.sinks[sink.sid], sink.sinkId)

	var relay *WRTC_Relay

	if len(node.sinks[sink.sid]) == 0 {
		delete(node.sinks, sink.sid)

		// No more sinks for that stream means there is no need for relays
		// Remove it
		relay = node.relays[sink.sid]
		delete(node.relays, sink.sid)
	}

	node.mutexStatus.Unlock()

	// Close the relay after releasing the node lock
	if relay != nil {
		relay.close()
	}
}
