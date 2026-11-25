// Controls the logic of WebRTC senders and relays
// Functionality of inter-node interaction

package main

// Called when a CONNECT message is received
// If the node has a WebRTC source for the specified Stream ID,
// it will create a sender for the node that requested the connection
func (node *WebRTC_CDN_Node) receiveConnectMessage(from string, sid string) {
	node.mutexStatus.Lock()

	source := node.sources[sid]

	if source == nil || source.isClosed() {
		node.mutexStatus.Unlock()
		return // Ignore, no source available
	}

	var oldSender *WRTC_Source_Sender

	if node.senders[sid] != nil {
		oldSender = node.senders[sid][from]
	}

	// Create a sender (replaces any previous one for the same remote node,
	// so duplicate CONNECT messages cannot pile up multiple senders)
	sender := &WRTC_Source_Sender{
		sid:      sid,
		remoteId: from,
		node:     node,
	}

	sender.init()

	if node.senders[sid] == nil {
		node.senders[sid] = make(map[string]*WRTC_Source_Sender)
	}

	node.senders[sid][from] = sender

	ready := source.ready
	localTrackVideo, localTrackAudio := source.currentTracks()

	node.mutexStatus.Unlock()

	// Close the previous sender outside the node lock
	if oldSender != nil {
		oldSender.close()
	}

	if ready {
		// Tracks already available
		sender.onTracksReady(localTrackVideo, localTrackAudio)
	}
}

// Called when a sender is closed (disconnection)
func (node *WebRTC_CDN_Node) onSenderClosed(sender *WRTC_Source_Sender) {
	node.mutexStatus.Lock()
	defer node.mutexStatus.Unlock()

	// Identity check: a stale sender must not remove its replacement
	if node.senders[sender.sid] != nil && node.senders[sender.sid][sender.remoteId] == sender {
		delete(node.senders[sender.sid], sender.remoteId)

		if len(node.senders[sender.sid]) == 0 {
			delete(node.senders, sender.sid)
		}
	}
}

// Called when an ANSWER message is received
// This message is managed by the sender
func (node *WebRTC_CDN_Node) receiveAnswerMessage(from string, sid string, sdp string) {
	node.mutexStatus.Lock()
	defer node.mutexStatus.Unlock()

	if node.senders[sid] != nil && node.senders[sid][from] != nil {
		node.senders[sid][from].onAnswer(sdp)
	}
}

// Called when a CANDIDATE message is received
// It may be forwarded to a sender or a relay
func (node *WebRTC_CDN_Node) receiveCandidateMessage(from string, sid string, candidate string) {
	node.mutexStatus.Lock()
	defer node.mutexStatus.Unlock()

	// Check senders
	if node.senders[sid] != nil && node.senders[sid][from] != nil {
		node.senders[sid][from].onICECandidate(candidate)
	}

	// Check relays
	if node.relays[sid] != nil {
		node.relays[sid].onICECandidate(candidate)
	}
}

// Called when an INFO message is received
func (node *WebRTC_CDN_Node) receiveInfoMessage(from string, sid string) {
	node.mutexStatus.Lock()

	// If we receive an INFO message from another node
	// and we have a local source for that stream, the remote
	// source takes precedence (at most one valid source per stream ID)
	oldSource := node.sources[sid]
	if oldSource != nil {
		delete(node.sources, sid)
	}

	// Senders of the old local source must also be released
	var oldSenders []*WRTC_Source_Sender
	if oldSource != nil && node.senders[sid] != nil {
		for _, sender := range node.senders[sid] {
			oldSenders = append(oldSenders, sender)
		}
		delete(node.senders, sid)
	}

	var relay *WRTC_Relay
	var oldRelay *WRTC_Relay
	var sinksToNotify []*WRTC_Sink

	// If we have any pending sinks for that stream,
	// we (re)create a relay for that source
	if node.sinks[sid] != nil && len(node.sinks[sid]) > 0 {
		// Replace the old relay (if any), so duplicate or repeated
		// discovery messages cannot pile up multiple relays
		oldRelay = node.relays[sid]
		delete(node.relays, sid)

		// Create new relay
		relay = &WRTC_Relay{
			sid:      sid,
			remoteId: from,
			node:     node,
		}

		relay.init()

		node.relays[sid] = relay

		// Collect sinks to notify them that the old tracks are gone
		if oldSource != nil || oldRelay != nil {
			for _, sink := range node.sinks[sid] {
				sinksToNotify = append(sinksToNotify, sink)
			}
		}
	}

	node.mutexStatus.Unlock()

	// Close replaced resources AFTER releasing the node lock
	if oldSource != nil {
		oldTracksVideo, oldTracksAudio := oldSource.currentTracks()
		oldSource.close(true, false)

		// Tell the sinks the old tracks are gone, they will
		// receive the tracks of the new relay when it is ready
		for _, sink := range sinksToNotify {
			sink.onTracksClosed(oldTracksVideo, oldTracksAudio)
		}
	} else if oldRelay != nil {
		oldTracksVideo, oldTracksAudio := oldRelay.currentTracks()
		for _, sink := range sinksToNotify {
			sink.onTracksClosed(oldTracksVideo, oldTracksAudio)
		}
	}

	if oldRelay != nil {
		oldRelay.close()
	}

	for _, sender := range oldSenders {
		sender.close()
	}

	if relay != nil {
		// Send a connect message to the node that announced the source
		node.sendConnectMessage(from, sid)
	}
}

// Called when an OFFER message is received
// This message is managed by the relay
func (node *WebRTC_CDN_Node) receiveOfferMessage(sid string, data string, hasVideo bool, hasAudio bool) {
	node.mutexStatus.Lock()
	defer node.mutexStatus.Unlock()

	if node.relays[sid] != nil {
		go node.relays[sid].onOffer(data, hasVideo, hasAudio)
	}
}

// Called when a relay is ready
// This means the tracks are available
func (node *WebRTC_CDN_Node) onRelayReady(relay *WRTC_Relay) {
	localTrackVideo, localTrackAudio, ok := relay.markReady()

	if !ok {
		return // Relay was closed before becoming ready
	}

	node.mutexStatus.Lock()
	defer node.mutexStatus.Unlock()

	// Ignore the event if the relay was replaced in the meantime
	if node.relays[relay.sid] != relay {
		return
	}

	// Notify sinks
	if node.sinks[relay.sid] != nil {
		for _, sink := range node.sinks[relay.sid] {
			sink.onTracksReady(localTrackVideo, localTrackAudio)
		}
	}
}

// Called when a relay is closed
func (node *WebRTC_CDN_Node) onRelayClosed(relay *WRTC_Relay) {
	localTrackVideo, localTrackAudio := relay.currentTracks()

	node.mutexStatus.Lock()

	// Identity check: a stale relay (already replaced) must not remove
	// the new relay nor disturb the sinks
	if node.relays[relay.sid] != relay {
		node.mutexStatus.Unlock()
		return
	}

	delete(node.relays, relay.sid)

	var sinksToNotify []*WRTC_Sink

	if node.sinks[relay.sid] != nil {
		for _, sink := range node.sinks[relay.sid] {
			sinksToNotify = append(sinksToNotify, sink)
		}
	}

	hasSinks := len(sinksToNotify) > 0

	node.mutexStatus.Unlock()

	// Any sinks waiting, tell them the tracks are closed
	for _, sink := range sinksToNotify {
		sink.onTracksClosed(localTrackVideo, localTrackAudio)
	}

	// If there are sinks for that stream ID
	// and there are no source, try resolving it
	if hasSinks {
		node.sendResolveMessage(relay.sid)
	}
}
