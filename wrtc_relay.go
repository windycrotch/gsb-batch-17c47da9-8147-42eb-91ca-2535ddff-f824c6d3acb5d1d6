// WebRTC relay connection. Used to receive WebRTC data from another node

package main

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// Max time a relay may stay not-ready before it is closed and re-resolved.
// This prevents viewers from waiting forever when the remote node does
// not answer the CONNECT request (for example, a stale discovery message).
var relayReadyTimeout = 15 * time.Second

// WRTC_Relay - This data structure contains the status data
// of an inter-node INPUT connection
// Receives the tracks of a remote WRTC_Source
type WRTC_Relay struct {
	sid      string // WebRTC stream ID
	remoteId string // ID of the remote node sending it

	node *WebRTC_CDN_Node // Reference to the node

	ready bool

	closed bool // True when the relay is no longer active

	readyTimer *time.Timer // Watchdog to close the relay if it never becomes ready

	peerConnection *webrtc.PeerConnection
	statusMutex    *sync.Mutex

	hasAudio bool
	hasVideo bool

	localTrackVideo *webrtc.TrackLocalStaticRTP
	localTrackAudio *webrtc.TrackLocalStaticRTP
}

// Initialize
func (relay *WRTC_Relay) init() {
	relay.ready = false
	relay.closed = false
	relay.statusMutex = &sync.Mutex{}

	// Watchdog: if the relay does not become ready in time, close it so
	// the stream is re-resolved and viewers do not wait forever
	relay.readyTimer = time.AfterFunc(relayReadyTimeout, func() {
		relay.statusMutex.Lock()
		mustClose := !relay.closed && !relay.ready
		relay.statusMutex.Unlock()

		if mustClose {
			LogDebug("Source relay timed out waiting for tracks | RemoteNode: " + relay.remoteId + " | SreamID: " + relay.sid)
			relay.onClose()
		}
	})
}

// Returns true if the relay is ready (tracks available)
func (relay *WRTC_Relay) isReady() bool {
	relay.statusMutex.Lock()
	defer relay.statusMutex.Unlock()

	return relay.ready
}

// Marks the relay as ready and returns its tracks.
// Returns ok = false if the relay was already closed.
func (relay *WRTC_Relay) markReady() (localTrackVideo *webrtc.TrackLocalStaticRTP, localTrackAudio *webrtc.TrackLocalStaticRTP, ok bool) {
	relay.statusMutex.Lock()
	defer relay.statusMutex.Unlock()

	if relay.closed {
		return nil, nil, false
	}

	relay.ready = true

	if relay.readyTimer != nil {
		relay.readyTimer.Stop()
	}

	return relay.localTrackVideo, relay.localTrackAudio, true
}

// Returns the current local tracks (thread safe)
func (relay *WRTC_Relay) currentTracks() (localTrackVideo *webrtc.TrackLocalStaticRTP, localTrackAudio *webrtc.TrackLocalStaticRTP) {
	relay.statusMutex.Lock()
	defer relay.statusMutex.Unlock()

	return relay.localTrackVideo, relay.localTrackAudio
}

// Called when an offer SDP message is received
func (relay *WRTC_Relay) onOffer(offerJSON string, hasVideo bool, hasAudio bool) {
	relay.statusMutex.Lock()
	defer relay.statusMutex.Unlock()

	if relay.closed {
		return // Ignore offers for a closed relay
	}

	relay.hasVideo = hasVideo
	relay.hasAudio = hasAudio

	// Clear old peer connection
	if relay.peerConnection != nil {
		relay.peerConnection.OnICECandidate(nil)
		relay.peerConnection.OnConnectionStateChange(nil)
		relay.peerConnection.OnTrack(nil)
		relay.peerConnection.Close()
	}

	peerConnectionConfig := loadWebRTCConfig() // Load WebRTC configuration

	// Create a new PeerConnection
	peerConnection, err := webrtc.NewPeerConnection(peerConnectionConfig)
	if err != nil {
		LogError(err)
		go relay.onClose()
		return
	}

	relay.peerConnection = peerConnection

	peerConnection.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		relay.statusMutex.Lock()

		if remoteTrack.Kind() == webrtc.RTPCodecTypeVideo {
			if relay.localTrackVideo != nil {
				relay.statusMutex.Unlock()
				return
			}

			localTrack, newTrackErr := webrtc.NewTrackLocalStaticRTP(remoteTrack.Codec().RTPCodecCapability, "video", "pion")
			if newTrackErr != nil {
				relay.statusMutex.Unlock()
				LogError(newTrackErr)
				return
			}

			relay.localTrackVideo = localTrack

			go pipeTrack(remoteTrack, localTrack)
		} else if remoteTrack.Kind() == webrtc.RTPCodecTypeAudio {
			if relay.localTrackAudio != nil {
				relay.statusMutex.Unlock()
				return
			}

			localTrack, newTrackErr := webrtc.NewTrackLocalStaticRTP(remoteTrack.Codec().RTPCodecCapability, "audio", "pion")
			if newTrackErr != nil {
				relay.statusMutex.Unlock()
				LogError(newTrackErr)
				return
			}

			relay.localTrackAudio = localTrack

			go pipeTrack(remoteTrack, localTrack)
		} else {
			relay.statusMutex.Unlock()
			return
		}

		ready := (!relay.hasAudio || relay.localTrackAudio != nil) && (!relay.hasVideo || relay.localTrackVideo != nil)

		relay.statusMutex.Unlock()

		if ready {
			// Received all the tracks, the relay is now ready
			// Note: the node is notified AFTER releasing the relay lock,
			// since the node may call back into the relay.
			relay.node.onRelayReady(relay)
		}
	})

	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		relay.statusMutex.Lock()
		defer relay.statusMutex.Unlock()

		if i != nil {
			b, e := json.Marshal(i.ToJSON())
			if e != nil {
				LogError(e)
			} else {
				relay.sendICECandidate(string(b))
			}
		} else {
			relay.sendICECandidate("")
		}
	})

	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			LogDebug("Source relay Disconnected | RemoteNode: " + relay.remoteId + " | SreamID: " + relay.sid)
			relay.onClose()
		} else if state == webrtc.PeerConnectionStateConnected {
			LogDebug("Source relay Connected | RemoteNode: " + relay.remoteId + " | SreamID: " + relay.sid)
		}
	})

	// Set the remote SessionDescription
	sd := webrtc.SessionDescription{}
	err = json.Unmarshal([]byte(offerJSON), &sd)
	if err != nil {
		LogError(err)
		return
	}
	err = relay.peerConnection.SetRemoteDescription(sd)
	if err != nil {
		LogError(err)
		return
	}

	// Create SDP answer
	answer, err := relay.peerConnection.CreateAnswer(nil)
	if err != nil {
		LogError(err)
		return
	}

	// Sets the LocalDescription, and starts our UDP listeners
	err = relay.peerConnection.SetLocalDescription(answer)
	if err != nil {
		LogError(err)
		return
	}

	// Send ANSWER to the signaling system
	answerJSON, e := json.Marshal(answer)

	if e != nil {
		LogError(e)
		return
	}
	relay.sendAnswer(string(answerJSON))
}

// Call when an ICE Candidate message is received from the remote node
func (relay *WRTC_Relay) onICECandidate(candidateJSON string) {
	relay.statusMutex.Lock()
	defer relay.statusMutex.Unlock()

	if relay.peerConnection == nil {
		return
	}

	if candidateJSON == "" {
		return
	}

	candidate := webrtc.ICECandidateInit{}

	err := json.Unmarshal([]byte(candidateJSON), &candidate)

	if err != nil {
		LogError(err)
	}

	err = relay.peerConnection.AddICECandidate(candidate)

	if err != nil {
		LogError(err)
	}
}

// SEND

// Send candidate message to the remote node
func (relay *WRTC_Relay) sendICECandidate(candidateJSON string) {
	mp := make(map[string]string)

	mp["type"] = "CANDIDATE"
	mp["src"] = relay.node.id
	mp["dst"] = relay.remoteId
	mp["sid"] = relay.sid
	mp["data"] = candidateJSON

	relay.node.sendRedisMessage(relay.remoteId, &mp)
}

// Send answer SDP message to the remote node
func (relay *WRTC_Relay) sendAnswer(answerJSON string) {
	mp := make(map[string]string)

	mp["type"] = "ANSWER"
	mp["src"] = relay.node.id
	mp["dst"] = relay.remoteId
	mp["sid"] = relay.sid
	mp["data"] = answerJSON

	relay.node.sendRedisMessage(relay.remoteId, &mp)
}

// Called if the peer connection is closed
func (relay *WRTC_Relay) onClose() {
	relay.statusMutex.Lock()

	if relay.closed {
		relay.statusMutex.Unlock()
		return // Already closed
	}

	relay.closed = true

	if relay.readyTimer != nil {
		relay.readyTimer.Stop()
	}

	peerConnection := relay.peerConnection
	relay.peerConnection = nil

	relay.statusMutex.Unlock()

	if peerConnection != nil {
		peerConnection.OnICECandidate(nil)
		peerConnection.OnConnectionStateChange(nil)
		peerConnection.OnTrack(nil)
		peerConnection.Close()
	}

	// Notify the node outside the relay lock to prevent lock-order inversions
	relay.node.onRelayClosed(relay)
}

// Close the relay
// It is safe to call this method multiple times
func (relay *WRTC_Relay) close() {
	relay.statusMutex.Lock()

	if relay.closed {
		relay.statusMutex.Unlock()
		return
	}

	relay.closed = true

	if relay.readyTimer != nil {
		relay.readyTimer.Stop()
	}

	peerConnection := relay.peerConnection
	relay.peerConnection = nil

	relay.statusMutex.Unlock()

	if peerConnection != nil {
		peerConnection.OnICECandidate(nil)
		peerConnection.OnConnectionStateChange(nil)
		peerConnection.OnTrack(nil)
		peerConnection.Close()
	}
}
