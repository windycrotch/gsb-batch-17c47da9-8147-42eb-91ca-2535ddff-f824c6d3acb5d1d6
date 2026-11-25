// Reliability tests: publisher re-publish, multi-node relay rebuild,
// subscription / forwarding resource cleanup and late signaling.
//
// Run with:  go test -race -v ./...

package main

import (
	"testing"
	"time"
)

// Single node: the publisher disconnects and re-publishes the same stream.
// The waiting viewer must leave STANDBY, receive a new OFFER and get
// connected again. The node must hold exactly one source for the stream.
func TestSingleNodeRepublish(t *testing.T) {
	t.Setenv("STAND_ALONE", "YES")
	t.Setenv("STUN_SERVER", "NONE")

	node := newTestNode(t, "node-single")
	srv := startNodeServer(t, node)

	pub := publishStream(t, srv, "stream-a")

	viewer := playStream(t, srv, "stream-a")
	viewer.handleNextOffer(t, 15*time.Second)
	viewer.waitTrack(t, 15*time.Second)

	// Publisher disconnects (websocket closed)
	pub.close()
	pub.client.close()

	// The source must be fully removed
	waitForCondition(t, 10*time.Second, "source removed after publisher disconnect", func() bool {
		return getNodeState(node).sources == 0
	})

	// The viewer must be told to stand by
	if _, ok := viewer.client.waitFor("STANDBY", 10*time.Second); !ok {
		t.Fatal("viewer did not receive STANDBY after the source closed")
	}

	// The viewer stays subscribed
	waitForCondition(t, 10*time.Second, "sink kept while waiting", func() bool {
		return getNodeState(node).sinks == 1
	})

	// Publisher re-publishes the same stream on a new connection
	pub2 := publishStream(t, srv, "stream-a")
	defer pub2.close()
	defer pub2.client.close()

	// The waiting viewer must receive a new OFFER and get connected again
	viewer.handleNextOffer(t, 15*time.Second)
	viewer.waitTrack(t, 15*time.Second)

	// Node state: exactly one source, one sink, no relays
	waitForCondition(t, 10*time.Second, "single source after re-publish", func() bool {
		st := getNodeState(node)
		return st.sources == 1 && st.sinks == 1 && st.relays == 0 && st.senders == 0
	})

	viewer.close()
}

// Single node: the publisher re-publishes the same stream-id on the SAME
// websocket connection (new request-id). The old source must be replaced
// (not coexist), the old request must be closed, and waiting viewers must
// receive a new OFFER. This used to deadlock the connection.
func TestRepublishSameConnection(t *testing.T) {
	t.Setenv("STAND_ALONE", "YES")
	t.Setenv("STUN_SERVER", "NONE")

	node := newTestNode(t, "node-single-2")
	srv := startNodeServer(t, node)

	client := dialWS(t, srv)
	defer client.close()

	pub1 := publishOnClient(t, client, "stream-b", "pub1")

	viewer := playStream(t, srv, "stream-b")
	defer viewer.close()
	viewer.handleNextOffer(t, 15*time.Second)
	viewer.waitTrack(t, 15*time.Second)

	// Re-publish the same stream-id on the same connection
	pub2 := publishOnClient(t, client, "stream-b", "pub2")
	defer pub2.close()

	// The old publish request must be closed by the node
	closeMsg, ok := client.waitFor("CLOSE", 10*time.Second)
	if !ok {
		t.Fatal("old publish request was not closed after re-publish")
	}
	if closeMsg.params["request-id"] != "pub1" {
		t.Fatalf("expected CLOSE for request pub1, got: %s", closeMsg.params["request-id"])
	}

	// The viewer must receive a new OFFER and recover
	viewer.handleNextOffer(t, 15*time.Second)
	viewer.waitTrack(t, 15*time.Second)

	// At most one valid source for the stream
	waitForCondition(t, 10*time.Second, "old source replaced", func() bool {
		st := getNodeState(node)
		return st.sources == 1 && st.sinks == 1
	})

	pub1.close()
}

// A stale source cleanup (old source closing after being replaced) must
// not remove the newly registered source.
func TestStaleSourceCleanupKeepsNewSource(t *testing.T) {
	t.Setenv("STAND_ALONE", "YES")

	node := newTestNode(t, "node-unit-1")

	srcOld := &WRTC_Source{sid: "s", node: node}
	srcOld.init()

	srcNew := &WRTC_Source{sid: "s", node: node}
	srcNew.init()

	node.mutexStatus.Lock()
	node.sources["s"] = srcNew
	node.mutexStatus.Unlock()

	// The old source closes late: must not remove the new one
	node.onSourceClosed(srcOld)

	node.mutexStatus.Lock()
	if node.sources["s"] != srcNew {
		t.Fatal("stale source cleanup removed the new source")
	}
	node.mutexStatus.Unlock()

	// The new source closes: must be removed
	node.onSourceClosed(srcNew)

	node.mutexStatus.Lock()
	if node.sources["s"] != nil {
		t.Fatal("current source was not removed on close")
	}
	node.mutexStatus.Unlock()
}

// A stale relay cleanup must not remove the rebuilt relay.
func TestStaleRelayCleanupKeepsNewRelay(t *testing.T) {
	t.Setenv("STAND_ALONE", "YES")

	node := newTestNode(t, "node-unit-2")

	relayOld := &WRTC_Relay{sid: "s", remoteId: "remote", node: node}
	relayOld.init()
	defer relayOld.close()

	relayNew := &WRTC_Relay{sid: "s", remoteId: "remote", node: node}
	relayNew.init()
	defer relayNew.close()

	node.mutexStatus.Lock()
	node.relays["s"] = relayNew
	node.mutexStatus.Unlock()

	// The old relay closes late: must not remove the new one
	node.onRelayClosed(relayOld)

	node.mutexStatus.Lock()
	if node.relays["s"] != relayNew {
		t.Fatal("stale relay cleanup removed the new relay")
	}
	node.mutexStatus.Unlock()

	// The current relay closes: must be removed
	node.onRelayClosed(relayNew)

	node.mutexStatus.Lock()
	if node.relays["s"] != nil {
		t.Fatal("current relay was not removed on close")
	}
	node.mutexStatus.Unlock()
}

// Multi node: a viewer on node B watches a stream published on node A.
// When the publisher re-publishes on node A, node B must rebuild its
// relay and the viewer must receive a new OFFER. At the end there must
// be exactly one relay on B and one sender on A.
func TestMultiNodeRemoteRepublish(t *testing.T) {
	t.Setenv("STAND_ALONE", "NO")
	t.Setenv("STUN_SERVER", "NONE")

	nodeA := newTestNode(t, "node-a")
	nodeB := newTestNode(t, "node-b")

	linkNodes(nodeA, nodeB)

	srvA := startNodeServer(t, nodeA)
	srvB := startNodeServer(t, nodeB)

	// The viewer subscribes before the stream is published anywhere
	viewer := playStream(t, srvB, "stream-c")
	defer viewer.close()

	// Publish on node A: discovery (RESOLVE/INFO) must create the relay on B
	pub := publishStream(t, srvA, "stream-c")

	viewer.handleNextOffer(t, 20*time.Second)
	viewer.waitTrack(t, 20*time.Second)

	waitForCondition(t, 10*time.Second, "relay and sender established", func() bool {
		return getNodeState(nodeB).relays == 1 && getNodeState(nodeA).senders == 1
	})

	relayBefore := getRelay(nodeB, "stream-c")
	if relayBefore == nil {
		t.Fatal("expected a relay on node B")
	}

	// Publisher disconnects and re-publishes the same stream on node A
	pub.close()
	pub.client.close()

	waitForCondition(t, 10*time.Second, "old source removed on A", func() bool {
		return getNodeState(nodeA).sources == 0
	})

	pub2 := publishStream(t, srvA, "stream-c")
	defer pub2.close()
	defer pub2.client.close()

	// The viewer on node B must receive a new OFFER and recover
	viewer.handleNextOffer(t, 20*time.Second)
	viewer.waitTrack(t, 20*time.Second)

	// Convergence: exactly one source, one sender, one relay
	waitForCondition(t, 15*time.Second, "converged state after remote re-publish", func() bool {
		stA := getNodeState(nodeA)
		stB := getNodeState(nodeB)
		return stA.sources == 1 && stA.senders == 1 && stB.relays == 1 && stB.sinks == 1
	})

	// The relay must have been rebuilt (not the stale one)
	relayAfter := getRelay(nodeB, "stream-c")
	if relayAfter == nil {
		t.Fatal("expected a relay on node B after re-publish")
	}
	if relayAfter == relayBefore {
		t.Fatal("relay was not rebuilt after remote re-publish")
	}
}

// Multi node: when the last viewer of a stream leaves (CLOSE), the relay
// on the viewer's node and the sender on the publisher's node must be
// released. Duplicate CLOSE and late ANSWER/CANDIDATE must be harmless.
func TestLastViewerLeavesReleasesRelay(t *testing.T) {
	t.Setenv("STAND_ALONE", "NO")
	t.Setenv("STUN_SERVER", "NONE")

	nodeA := newTestNode(t, "node-a2")
	nodeB := newTestNode(t, "node-b2")

	linkNodes(nodeA, nodeB)

	srvA := startNodeServer(t, nodeA)
	srvB := startNodeServer(t, nodeB)

	pub := publishStream(t, srvA, "stream-d")
	defer pub.close()
	defer pub.client.close()

	viewer := playStream(t, srvB, "stream-d")
	viewer.handleNextOffer(t, 20*time.Second)
	viewer.waitTrack(t, 20*time.Second)

	waitForCondition(t, 10*time.Second, "relay and sender established", func() bool {
		return getNodeState(nodeB).relays == 1 && getNodeState(nodeA).senders == 1
	})

	// The viewer closes the subscription
	viewer.client.send("CLOSE", map[string]string{"Request-ID": "play"}, "")

	// Duplicate CLOSE and late signaling must be ignored without panics
	viewer.client.send("CLOSE", map[string]string{"Request-ID": "play"}, "")
	viewer.client.send("ANSWER", map[string]string{"Request-ID": "play"}, "{}")
	viewer.client.send("CANDIDATE", map[string]string{"Request-ID": "play"}, "{}")
	viewer.client.send("CLOSE", map[string]string{"Request-ID": "play"}, "")

	// The sink must be removed and the relay released (last viewer left)
	waitForCondition(t, 10*time.Second, "sink and relay released", func() bool {
		st := getNodeState(nodeB)
		return st.sinks == 0 && st.relays == 0
	})

	// The sender on the remote node must be released as well
	waitForCondition(t, 15*time.Second, "sender released on remote node", func() bool {
		return getNodeState(nodeA).senders == 0
	})

	viewer.close()
}

// Multi node: the viewer's websocket disconnects without a CLOSE message.
// All subscription and forwarding resources must be released.
func TestViewerDisconnectReleasesResources(t *testing.T) {
	t.Setenv("STAND_ALONE", "NO")
	t.Setenv("STUN_SERVER", "NONE")

	nodeA := newTestNode(t, "node-a3")
	nodeB := newTestNode(t, "node-b3")

	linkNodes(nodeA, nodeB)

	srvA := startNodeServer(t, nodeA)
	srvB := startNodeServer(t, nodeB)

	pub := publishStream(t, srvA, "stream-e")
	defer pub.close()
	defer pub.client.close()

	viewer := playStream(t, srvB, "stream-e")
	viewer.handleNextOffer(t, 20*time.Second)
	viewer.waitTrack(t, 20*time.Second)

	waitForCondition(t, 10*time.Second, "relay and sender established", func() bool {
		return getNodeState(nodeB).relays == 1 && getNodeState(nodeA).senders == 1
	})

	// Abrupt websocket disconnection (no CLOSE message)
	viewer.close()

	waitForCondition(t, 10*time.Second, "sink and relay released after disconnect", func() bool {
		st := getNodeState(nodeB)
		return st.sinks == 0 && st.relays == 0
	})

	waitForCondition(t, 15*time.Second, "sender released after disconnect", func() bool {
		return getNodeState(nodeA).senders == 0
	})
}

// Multi node: a discovery message points to a node that never answers
// (stale INFO). The relay watchdog must fire, the stream must be
// re-resolved, and the viewer must not wait forever.
func TestViewerRecoversFromStaleDiscovery(t *testing.T) {
	t.Setenv("STAND_ALONE", "NO")
	t.Setenv("STUN_SERVER", "NONE")

	// Shorten the relay watchdog for the test
	prevTimeout := relayReadyTimeout
	relayReadyTimeout = 500 * time.Millisecond
	t.Cleanup(func() {
		relayReadyTimeout = prevTimeout
	})

	nodeA := newTestNode(t, "node-a4")
	nodeB := newTestNode(t, "node-b4")

	link := linkNodes(nodeA, nodeB)

	srvA := startNodeServer(t, nodeA)
	srvB := startNodeServer(t, nodeB)

	// Drop inter-node traffic while we set up the stale state
	link.setDrop(true)

	pub := publishStream(t, srvA, "stream-f")
	defer pub.close()
	defer pub.client.close()

	waitForCondition(t, 10*time.Second, "source ready on A", func() bool {
		return getNodeState(nodeA).sources == 1
	})

	viewer := playStream(t, srvB, "stream-f")
	defer viewer.close()

	// Wait until the sink is registered on node B
	waitForCondition(t, 5*time.Second, "sink registered on B", func() bool {
		return getNodeState(nodeB).sinks == 1
	})

	// A stale discovery message arrives from a node that does not exist
	nodeB.receiveInfoMessage("ghost-node", "stream-f")

	// Node B now has a relay to a node that will never answer
	waitForCondition(t, 5*time.Second, "relay to ghost node created", func() bool {
		return getNodeState(nodeB).relays == 1
	})

	// Restore connectivity: the re-resolve after the watchdog must succeed
	link.setDrop(false)

	// The viewer must eventually receive an OFFER and get connected
	viewer.handleNextOffer(t, 20*time.Second)
	viewer.waitTrack(t, 20*time.Second)

	// Exactly one relay, pointing to the real node
	waitForCondition(t, 10*time.Second, "single relay to the real node", func() bool {
		relay := getRelay(nodeB, "stream-f")
		return relay != nil && relay.remoteId == "node-a4" && getNodeState(nodeB).relays == 1
	})
}

// Single node: concurrent CLOSE, websocket disconnection and late
// ANSWER/CANDIDATE messages must not corrupt counters or panic.
// Run with -race to also catch data races.
func TestConcurrentCloseAndLateSignaling(t *testing.T) {
	t.Setenv("STAND_ALONE", "YES")
	t.Setenv("STUN_SERVER", "NONE")

	node := newTestNode(t, "node-race")
	srv := startNodeServer(t, node)

	pub := publishStream(t, srv, "stream-g")

	viewer := playStream(t, srv, "stream-g")
	viewer.handleNextOffer(t, 15*time.Second)
	viewer.waitTrack(t, 15*time.Second)

	// Fire everything at once: duplicate closes, late signaling,
	// websocket disconnects on both publisher and viewer.
	done := make(chan struct{})

	go func() {
		viewer.client.send("CLOSE", map[string]string{"Request-ID": "play"}, "")
		viewer.client.send("CLOSE", map[string]string{"Request-ID": "play"}, "")
		done <- struct{}{}
	}()

	go func() {
		viewer.client.send("ANSWER", map[string]string{"Request-ID": "play"}, "{}")
		viewer.client.send("CANDIDATE", map[string]string{"Request-ID": "play"}, "{}")
		done <- struct{}{}
	}()

	go func() {
		pub.client.send("CLOSE", map[string]string{"Request-ID": "pub"}, "")
		pub.client.send("CLOSE", map[string]string{"Request-ID": "pub"}, "")
		pub.client.send("ANSWER", map[string]string{"Request-ID": "pub"}, "{}")
		pub.client.send("CANDIDATE", map[string]string{"Request-ID": "pub"}, "{}")
		done <- struct{}{}
	}()

	go func() {
		viewer.client.close()
		done <- struct{}{}
	}()

	go func() {
		pub.client.close()
		done <- struct{}{}
	}()

	for i := 0; i < 5; i++ {
		<-done
	}

	pub.close()

	// Everything must be released
	waitForCondition(t, 15*time.Second, "all resources released", func() bool {
		st := getNodeState(node)
		return st.sources == 0 && st.sinks == 0 && st.relays == 0 && st.senders == 0
	})

	// Connections must be deregistered
	waitForCondition(t, 10*time.Second, "connections deregistered", func() bool {
		node.mutexConnections.Lock()
		defer node.mutexConnections.Unlock()
		return len(node.connections) == 0
	})

	viewer.close()
}
