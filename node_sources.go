// WebRTC sources management

package main

// Lock ordering note: node.mutexStatus must never be held while calling
// methods that acquire a connection handler lock (Connection_Handler.statusMutex).
// Source / sink / relay / sender locks may be acquired while holding
// node.mutexStatus, but those objects must never call back into the node
// while holding their own lock.

// Checks if the node has a WebRTC source
// for the specified Stream Id (sid)
func (node *WebRTC_CDN_Node) resolveSource(sid string) bool {
	node.mutexStatus.Lock()
	defer node.mutexStatus.Unlock()

	return node.sources[sid] != nil
}

// Registers a WebRTC source
// If another source exists for the same Stream ID, it is replaced:
// the old source is closed and any relays / senders for the stream
// are released, so at most one valid source exists per stream ID.
func (node *WebRTC_CDN_Node) registerSource(source *WRTC_Source) {
	// Do not register a source that was already closed
	// (its request may have been closed before registration ran)
	if source.isClosed() {
		return
	}

	node.mutexStatus.Lock()

	// Replace the previous source (if any)
	oldSource := node.sources[source.sid]
	node.sources[source.sid] = source

	// Remove any relay for that stream
	oldRelay := node.relays[source.sid]
	delete(node.relays, source.sid)

	// Remove all the senders
	var oldSenders []*WRTC_Source_Sender
	if node.senders[source.sid] != nil {
		for _, sender := range node.senders[source.sid] {
			oldSenders = append(oldSenders, sender)
		}
		delete(node.senders, source.sid)
	}

	node.mutexStatus.Unlock()

	// Close replaced resources AFTER releasing the node lock.
	// Closing a source notifies its connection (sendSourceClose takes the
	// connection lock), so doing it under node.mutexStatus could deadlock.
	if oldSource != nil {
		oldSource.close(true, false)
	}

	if oldRelay != nil {
		oldRelay.close()
	}

	for _, sender := range oldSenders {
		sender.close()
	}

	// Announce to other nodes
	node.sendInfoMessage(REDIS_BROADCAST_CHANNEL, source.sid)
}

// Called when a WebRTC source is ready
// This means the tracks can be played
func (node *WebRTC_CDN_Node) onSourceReady(source *WRTC_Source) {
	// Ignore events from sources that are already closed
	if source.isClosed() {
		return
	}

	localTrackVideo, localTrackAudio := source.currentTracks()

	node.mutexStatus.Lock()
	defer node.mutexStatus.Unlock()

	// Ignore the event if the source was replaced in the meantime.
	// This prevents viewers from receiving tracks (and thus an OFFER)
	// belonging to a stale, already replaced source.
	if node.sources[source.sid] != source {
		return
	}

	source.ready = true

	// Notify sinks
	if node.sinks[source.sid] != nil {
		for _, sink := range node.sinks[source.sid] {
			sink.onTracksReady(localTrackVideo, localTrackAudio)
		}
	}

	// Notify senders
	if node.senders[source.sid] != nil {
		for _, sender := range node.senders[source.sid] {
			sender.onTracksReady(localTrackVideo, localTrackAudio)
		}
	}
}

// Called when the WebRTC source closes by itself (by the client, a disconnection, etc)
// Do not call from another status methods
func (node *WebRTC_CDN_Node) onSourceClosed(source *WRTC_Source) {
	localTrackVideo, localTrackAudio := source.currentTracks()

	node.mutexStatus.Lock()
	defer node.mutexStatus.Unlock()

	// Only the currently registered source may alter the node status.
	// If the source was already replaced by a newer one, this is a stale
	// cleanup event and it must NOT remove the new source nor disturb
	// the sinks (they are about to receive the new tracks).
	if node.sources[source.sid] != source {
		return
	}

	delete(node.sources, source.sid)

	// Close and remove all the senders
	if node.senders[source.sid] != nil {
		for _, sender := range node.senders[source.sid] {
			sender.close()
		}

		delete(node.senders, source.sid)
	}

	// Any sinks waiting, tell them the tracks are closed
	if node.sinks[source.sid] != nil {
		for _, sink := range node.sinks[source.sid] {
			sink.onTracksClosed(localTrackVideo, localTrackAudio)
		}
	}
}
