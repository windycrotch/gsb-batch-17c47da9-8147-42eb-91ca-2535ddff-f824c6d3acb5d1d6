// Test helpers: in-process signaling clients and node factories.
//
// These helpers allow running end-to-end scenarios (publish / play /
// re-publish / multi-node relay) locally, without a redis server and
// without external STUN servers.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func init() {
	// Keep test output readable
	LOG_REQUESTS_ENABLED = false
}

// Creates a node for testing. Env vars (STAND_ALONE, STUN_SERVER, ...)
// must be set with t.Setenv BEFORE calling this function.
func newTestNode(t *testing.T, id string) *WebRTC_CDN_Node {
	t.Helper()

	node := &WebRTC_CDN_Node{id: id}
	node.init()
	node.upgrader = &websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	return node
}

// Starts an HTTP test server serving the node's /ws endpoint
func startNodeServer(t *testing.T, node *WebRTC_CDN_Node) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(node)

	t.Cleanup(func() {
		srv.Close()
	})

	return srv
}

// Links two nodes as if they were connected through redis.
// Returns a controller that can drop all inter-node traffic
// (to simulate network partitions / lost messages).
type nodeLink struct {
	mu   sync.Mutex
	drop bool
}

func linkNodes(a *WebRTC_CDN_Node, b *WebRTC_CDN_Node) *nodeLink {
	link := &nodeLink{}

	deliver := func(from *WebRTC_CDN_Node, to *WebRTC_CDN_Node) {
		from.redisSendHook = func(channel string, msg map[string]string) {
			if channel != REDIS_BROADCAST_CHANNEL && channel != to.id {
				return // Unicast message for another node
			}

			link.mu.Lock()
			dropped := link.drop
			link.mu.Unlock()

			if dropped {
				return
			}

			raw, err := json.Marshal(msg)
			if err != nil {
				return
			}

			// Deliver asynchronously, like redis pub/sub would
			go to.receiveRedisMessage(string(raw))
		}
	}

	deliver(a, b)
	deliver(b, a)

	return link
}

func (link *nodeLink) setDrop(drop bool) {
	link.mu.Lock()
	defer link.mu.Unlock()
	link.drop = drop
}

// ---------- Node state assertions ----------

type nodeState struct {
	sources int
	relays  int
	sinks   int
	senders int
}

func getNodeState(node *WebRTC_CDN_Node) nodeState {
	node.mutexStatus.Lock()
	defer node.mutexStatus.Unlock()

	st := nodeState{
		sources: len(node.sources),
		relays:  len(node.relays),
	}

	for _, m := range node.sinks {
		st.sinks += len(m)
	}

	for _, m := range node.senders {
		st.senders += len(m)
	}

	return st
}

func getRelay(node *WebRTC_CDN_Node, sid string) *WRTC_Relay {
	node.mutexStatus.Lock()
	defer node.mutexStatus.Unlock()

	return node.relays[sid]
}

func waitForCondition(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("condition not met within %v: %s", timeout, what)
}

// ---------- WebSocket test client ----------

type testWSClient struct {
	t    *testing.T
	conn *websocket.Conn

	wmu sync.Mutex // Serializes writes (gorilla allows only one concurrent writer)

	mu     sync.Mutex
	inbox  []SignalingMessage
	notify chan struct{}

	candMu       sync.Mutex
	candHandlers map[string]func(body string) // By request-id
}

func dialWS(t *testing.T, srv *httptest.Server) *testWSClient {
	t.Helper()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("could not connect to %s: %v", wsURL, err)
	}

	c := &testWSClient{
		t:            t,
		conn:         conn,
		notify:       make(chan struct{}, 1),
		candHandlers: make(map[string]func(body string)),
	}

	go c.readLoop()

	return c
}

func (c *testWSClient) readLoop() {
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return // Closed
		}

		msg := parseSignalingMessage(string(data))

		if msg.method == "CANDIDATE" {
			c.candMu.Lock()
			handler := c.candHandlers[msg.params["request-id"]]
			c.candMu.Unlock()

			if handler != nil {
				handler(msg.body)
			}
		}

		c.mu.Lock()
		c.inbox = append(c.inbox, msg)
		c.mu.Unlock()

		select {
		case c.notify <- struct{}{}:
		default:
		}
	}
}

func (c *testWSClient) send(method string, params map[string]string, body string) {
	msg := SignalingMessage{
		method: method,
		params: params,
		body:   body,
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()

	c.conn.WriteMessage(websocket.TextMessage, []byte(msg.serialize()))
}

// Waits for a message of the given method. Non-matching messages
// are kept buffered for later waits.
func (c *testWSClient) waitFor(method string, timeout time.Duration) (SignalingMessage, bool) {
	deadline := time.Now().Add(timeout)

	for {
		c.mu.Lock()
		for i := range c.inbox {
			if c.inbox[i].method == method {
				m := c.inbox[i]
				c.inbox = append(c.inbox[:i], c.inbox[i+1:]...)
				c.mu.Unlock()
				return m, true
			}
		}
		c.mu.Unlock()

		if time.Now().After(deadline) {
			return SignalingMessage{}, false
		}

		select {
		case <-c.notify:
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (c *testWSClient) setCandidateHandler(requestId string, handler func(body string)) {
	c.candMu.Lock()
	defer c.candMu.Unlock()
	c.candHandlers[requestId] = handler
}

func (c *testWSClient) close() {
	c.conn.Close()
}

// ---------- Client-side WebRTC peer connection ----------

type clientPC struct {
	pc *webrtc.PeerConnection

	mu        sync.Mutex
	remoteSet bool
	pending   []webrtc.ICECandidateInit
}

func newClientPC(t *testing.T) *clientPC {
	t.Helper()

	pc, err := webrtc.NewPeerConnection(loadWebRTCConfig())
	if err != nil {
		t.Fatalf("could not create peer connection: %v", err)
	}

	return &clientPC{pc: pc}
}

func (s *clientPC) addCandidate(body string) {
	if body == "" {
		return
	}

	cand := webrtc.ICECandidateInit{}
	if err := json.Unmarshal([]byte(body), &cand); err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.remoteSet {
		s.pending = append(s.pending, cand)
		return
	}

	s.pc.AddICECandidate(cand)
}

func (s *clientPC) setRemoteDescription(sd webrtc.SessionDescription) error {
	if err := s.pc.SetRemoteDescription(sd); err != nil {
		return err
	}

	s.mu.Lock()
	s.remoteSet = true
	for _, c := range s.pending {
		s.pc.AddICECandidate(c)
	}
	s.pending = nil
	s.mu.Unlock()

	return nil
}

// ---------- Test publisher ----------

type testPublisher struct {
	client *testWSClient
	state  *clientPC
	track  *webrtc.TrackLocalStaticSample

	done      chan struct{}
	closeOnce sync.Once
}

// Publishes a video-only stream on an existing connection.
// Waits for the OK and the OFFER from the node, and answers it.
func publishOnClient(t *testing.T, c *testWSClient, sid string, requestId string) *testPublisher {
	t.Helper()

	state := newClientPC(t)

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", "pion")
	if err != nil {
		t.Fatalf("could not create track: %v", err)
	}

	if _, err := state.pc.AddTrack(track); err != nil {
		t.Fatalf("could not add track: %v", err)
	}

	p := &testPublisher{
		client: c,
		state:  state,
		track:  track,
		done:   make(chan struct{}),
	}

	state.pc.OnICECandidate(func(i *webrtc.ICECandidate) {
		body := ""
		if i != nil {
			b, _ := json.Marshal(i.ToJSON())
			body = string(b)
		}
		c.send("CANDIDATE", map[string]string{"Request-ID": requestId, "Stream-ID": sid}, body)
	})

	c.setCandidateHandler(requestId, state.addCandidate)

	c.send("PUBLISH", map[string]string{
		"Request-ID":  requestId,
		"Stream-ID":   sid,
		"Stream-Type": "VIDEO",
	}, "")

	if _, ok := c.waitFor("OK", 10*time.Second); !ok {
		t.Fatal("PUBLISH: did not receive OK")
	}

	offerMsg, ok := c.waitFor("OFFER", 10*time.Second)
	if !ok {
		t.Fatal("PUBLISH: did not receive OFFER from the node")
	}

	sd := webrtc.SessionDescription{}
	if err := json.Unmarshal([]byte(offerMsg.body), &sd); err != nil {
		t.Fatalf("PUBLISH: could not parse offer: %v", err)
	}

	if err := state.setRemoteDescription(sd); err != nil {
		t.Fatalf("PUBLISH: could not set remote description: %v", err)
	}

	answer, err := state.pc.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("PUBLISH: could not create answer: %v", err)
	}

	if err := state.pc.SetLocalDescription(answer); err != nil {
		t.Fatalf("PUBLISH: could not set local description: %v", err)
	}

	answerJSON, _ := json.Marshal(answer)
	c.send("ANSWER", map[string]string{"Request-ID": requestId, "Stream-ID": sid}, string(answerJSON))

	go p.writeSamples()

	return p
}

// Publishes a video-only stream on a new connection
func publishStream(t *testing.T, srv *httptest.Server, sid string) *testPublisher {
	t.Helper()
	return publishOnClient(t, dialWS(t, srv), sid, "pub")
}

func (p *testPublisher) writeSamples() {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.track.WriteSample(media.Sample{Data: []byte{0x01, 0x02, 0x03}, Duration: time.Second / 50})
		}
	}
}

// Stops publishing media and closes the peer connection (the websocket
// stays open). Simulates the publisher peer connection dying.
func (p *testPublisher) close() {
	p.closeOnce.Do(func() {
		close(p.done)
		p.state.pc.Close()
	})
}

// ---------- Test viewer ----------

type testViewer struct {
	client *testWSClient

	mu      sync.Mutex
	state   *clientPC
	pending []string

	trackCh chan string
}

// Sends a PLAY request and waits for OK + STANDBY
func playStream(t *testing.T, srv *httptest.Server, sid string) *testViewer {
	t.Helper()

	c := dialWS(t, srv)

	v := &testViewer{
		client:  c,
		trackCh: make(chan string, 16),
	}

	c.setCandidateHandler("play", func(body string) {
		v.mu.Lock()
		st := v.state
		if st == nil {
			v.pending = append(v.pending, body)
			v.mu.Unlock()
			return
		}
		v.mu.Unlock()

		st.addCandidate(body)
	})

	c.send("PLAY", map[string]string{"Request-ID": "play", "Stream-ID": sid}, "")

	if _, ok := c.waitFor("OK", 10*time.Second); !ok {
		t.Fatal("PLAY: did not receive OK")
	}

	if _, ok := c.waitFor("STANDBY", 10*time.Second); !ok {
		t.Fatal("PLAY: did not receive initial STANDBY")
	}

	return v
}

// Waits for the next OFFER from the node and answers it.
// Each OFFER is handled with a fresh peer connection (like a browser
// client renegotiating after the source changed).
func (v *testViewer) handleNextOffer(t *testing.T, timeout time.Duration) {
	t.Helper()

	offerMsg, ok := v.client.waitFor("OFFER", timeout)
	if !ok {
		t.Fatal("viewer: did not receive OFFER")
	}

	state := newClientPC(t)

	state.pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		select {
		case v.trackCh <- tr.Kind().String():
		default:
		}
	})

	state.pc.OnICECandidate(func(i *webrtc.ICECandidate) {
		body := ""
		if i != nil {
			b, _ := json.Marshal(i.ToJSON())
			body = string(b)
		}
		v.client.send("CANDIDATE", map[string]string{"Request-ID": "play"}, body)
	})

	v.mu.Lock()
	old := v.state
	v.state = state
	pending := v.pending
	v.pending = nil
	v.mu.Unlock()

	if old != nil {
		old.pc.Close()
	}

	for _, body := range pending {
		state.addCandidate(body)
	}

	sd := webrtc.SessionDescription{}
	if err := json.Unmarshal([]byte(offerMsg.body), &sd); err != nil {
		t.Fatalf("viewer: could not parse offer: %v", err)
	}

	if err := state.setRemoteDescription(sd); err != nil {
		t.Fatalf("viewer: could not set remote description: %v", err)
	}

	answer, err := state.pc.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("viewer: could not create answer: %v", err)
	}

	if err := state.pc.SetLocalDescription(answer); err != nil {
		t.Fatalf("viewer: could not set local description: %v", err)
	}

	answerJSON, _ := json.Marshal(answer)
	v.client.send("ANSWER", map[string]string{"Request-ID": "play"}, string(answerJSON))
}

// Waits until a media track is received (the viewer is effectively connected)
func (v *testViewer) waitTrack(t *testing.T, timeout time.Duration) {
	t.Helper()

	select {
	case <-v.trackCh:
	case <-time.After(timeout):
		t.Fatal("viewer: did not receive any media track")
	}
}

func (v *testViewer) close() {
	v.client.close()

	v.mu.Lock()
	st := v.state
	v.mu.Unlock()

	if st != nil {
		st.pc.Close()
	}
}
