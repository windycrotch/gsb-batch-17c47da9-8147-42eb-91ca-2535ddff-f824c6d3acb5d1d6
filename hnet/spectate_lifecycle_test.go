package hnet

import (
	"sync"
	"testing"
	"time"
)

// assertInRegistry checks the online-player set through the production
// collection accessors.
func assertInRegistry(t *testing.T, h *testHarness, id uint32, want bool) {
	t.Helper()

	h.server.mutex.Lock()
	got := h.server.Players.ByID(id) != nil
	h.server.mutex.Unlock()

	if got != want {
		t.Fatalf("player %d registry presence = %v, want %v", id, got, want)
	}
}

// assertRelation verifies both sides of a spectating relation agree.
func assertRelation(t *testing.T, spectator, host *Player, want bool) {
	t.Helper()

	spectator.Server.mutex.Lock()
	defer spectator.Server.mutex.Unlock()

	hostHasSpectator := host.Spectators.ByID(spectator.Info.Id) != nil
	spectatorPointsAtHost := spectator.Host == host

	if hostHasSpectator != want {
		t.Fatalf("host spectator list contains spectator = %v, want %v", hostHasSpectator, want)
	}

	if spectatorPointsAtHost != want {
		t.Fatalf("spectator.Host points at host = %v, want %v", spectatorPointsAtHost, want)
	}
}

// startSpectatingViaEntry drives a start-spectate request through the real
// packet handling entry point (HandleConnection -> servePlayer -> handler).
func startSpectatingViaEntry(spectatorConn *fakeConn, hostId uint32) {
	spectatorConn.queuePacket(CLIENT_START_SPECTATING, spectateRequestPayload(hostId))
}

func stopSpectatingViaEntry(spectatorConn *fakeConn, hostId uint32) {
	spectatorConn.queuePacket(CLIENT_STOP_SPECTATING, spectateRequestPayload(hostId))
}

// TestSpectatorDisconnectRemovesRelation: a spectator disconnecting must be
// removed from the host's spectator list, and the host must not keep sending
// data onto the closed connection.
func TestSpectatorDisconnectRemovesRelation(t *testing.T) {
	h := newTestHarness()
	host, hostConn := h.connectPlayer(1, "host")
	spectator, spectatorConn := h.connectPlayer(2, "spectator")

	startSpectatingViaEntry(spectatorConn, host.Info.Id)
	h.waitForHandler()
	assertRelation(t, spectator, host, true)

	// The host received the start notification.
	if !hostConn.containsPacket(SERVER_START_SPECTATING) {
		t.Fatal("host did not receive SERVER_START_SPECTATING")
	}

	h.disconnect(spectator.Info.Id)

	assertInRegistry(t, h, spectator.Info.Id, false)
	assertRelation(t, spectator, host, false)

	h.server.mutex.Lock()
	hostSpectatorCount := host.Spectators.Count()
	h.server.mutex.Unlock()

	if hostSpectatorCount != 0 {
		t.Fatalf("host still has %d spectators after spectator disconnect", hostSpectatorCount)
	}

	if spectatorConn.containsPacket(SERVER_SPECTATE_FRAMES) {
		t.Fatal("disconnected spectator unexpectedly received frame data")
	}
}

// TestHostDisconnectRemovesRelations: the host going away clears the
// relation on every spectator without closing their connections.
func TestHostDisconnectRemovesRelations(t *testing.T) {
	h := newTestHarness()
	host, _ := h.connectPlayer(1, "host")
	spectatorA, connA := h.connectPlayer(2, "a")
	spectatorB, connB := h.connectPlayer(3, "b")

	startSpectatingViaEntry(connA, host.Info.Id)
	startSpectatingViaEntry(connB, host.Info.Id)
	h.waitForHandler()
	assertRelation(t, spectatorA, host, true)
	assertRelation(t, spectatorB, host, true)

	h.disconnect(host.Info.Id)

	assertInRegistry(t, h, host.Info.Id, false)

	h.server.mutex.Lock()
	if spectatorA.Host != nil || spectatorB.Host != nil {
		t.Fatal("spectators still reference the offline host")
	}
	if host.Spectators.Count() != 0 {
		t.Fatal("offline host still keeps a spectator collection")
	}
	h.server.mutex.Unlock()

	// Spectators stay online.
	assertInRegistry(t, h, spectatorA.Info.Id, true)
	assertInRegistry(t, h, spectatorB.Info.Id, true)

	if connA.isClosed() || connB.isClosed() {
		t.Fatal("spectator connections must remain open when the host leaves")
	}

	if !connA.containsPacket(SERVER_USER_QUIT) || !connB.containsPacket(SERVER_USER_QUIT) {
		t.Fatal("remaining players did not receive the quit broadcast")
	}
}

// TestReloginDoesNotInheritRelations: when an account is replaced by a new
// connection, the old connection's relations are torn down and the new
// connection starts with a clean spectating identity.
func TestReloginDoesNotInheritRelations(t *testing.T) {
	h := newTestHarness()
	host, _ := h.connectPlayer(1, "host")
	oldPlayer, oldConn := h.connectPlayer(2, "account")
	fan, fanConn := h.connectPlayer(3, "fan")

	// Old connection both spectates a host and has a spectator.
	startSpectatingViaEntry(oldConn, host.Info.Id)
	startSpectatingViaEntry(fanConn, oldPlayer.Info.Id)
	h.waitForHandler()
	assertRelation(t, oldPlayer, host, true)
	assertRelation(t, fan, oldPlayer, true)

	// Simulate the same account logging in on a new connection: the
	// production login path replaces the old one.
	h.server.mutex.Lock()
	oldDone := h.connections[oldPlayer.Info.Id].done
	newPlayer, newConn := h.makeReplacement(oldPlayer)
	h.server.mutex.Unlock()

	h.waitForHandler()

	// Old connection was closed and fully detached.
	if !oldConn.isClosed() {
		t.Fatal("old connection was not closed after replacement")
	}
	if !oldConn.containsPacket(SERVER_LOGIN_REVOKED) {
		t.Fatal("old connection did not receive the login-revoked packet")
	}

	h.server.mutex.Lock()
	oldRelationsClean := oldPlayer.Host == nil && oldPlayer.Spectators.Count() == 0
	newIdentityClean := newPlayer.Host == nil && newPlayer.Spectators.Count() == 0
	registryPointsToNew := h.server.Players.ByID(oldPlayer.Info.Id) == newPlayer
	fanDetached := fan.Host == nil
	h.server.mutex.Unlock()

	if !oldRelationsClean {
		t.Fatal("old connection retained spectating relations after replacement")
	}
	if !newIdentityClean {
		t.Fatal("new connection inherited spectating relations from the old one")
	}
	if !registryPointsToNew {
		t.Fatal("registry does not point at the replacement connection")
	}
	if !fanDetached {
		t.Fatal("a spectator of the old connection was not detached")
	}

	// The old connection's read loop must observe the teardown close and
	// run its deferred, idempotent cleanup without affecting the new one.
	select {
	case <-oldDone:
	case <-time.After(5 * time.Second):
		t.Fatal("old connection serving goroutine did not exit")
	}
	h.server.mutex.Lock()
	if h.server.Players.ByID(oldPlayer.Info.Id) != newPlayer {
		t.Fatal("idempotent teardown of the old connection removed the new one")
	}
	h.server.mutex.Unlock()

	// The new connection never received spectate traffic addressed to the
	// old identity.
	if newConn.containsPacket(SERVER_SPECTATE_FRAMES) {
		t.Fatal("new connection inherited replay frames from the old identity")
	}
}

// makeReplacement builds a fresh connection for an existing account and runs
// the same replacement steps OnLoginSuccess performs (revoke + teardown +
// register), while the caller holds the server lock.
func (h *testHarness) makeReplacement(old *Player) (*Player, *fakeConn) {
	conn := newFakeConn()
	newPlayer := h.server.newConnectionPlayer(conn)
	done := make(chan struct{})

	go func() {
		h.server.runPlayer(newPlayer)
		close(done)
	}()

	newPlayer.Info.Id = old.Info.Id
	newPlayer.Info.Name = old.Info.Name
	newPlayer.Stats.UserId = old.Info.Id

	_ = old.SendPacketData(SERVER_LOGIN_REVOKED, []byte{})
	h.server.teardown(old)
	h.server.Players.Add(newPlayer)

	h.connections[old.Info.Id] = &testConnection{player: newPlayer, conn: conn, done: done}
	return newPlayer, conn
}

// TestSwitchSpectatingTargetClearsOldRelation: switching hosts removes the
// spectator from the old host and notifies it, while the new host gains the
// relation.
func TestSwitchSpectatingTargetClearsOldRelation(t *testing.T) {
	h := newTestHarness()
	hostA, connA := h.connectPlayer(1, "a")
	hostB, connB := h.connectPlayer(2, "b")
	spectator, spectatorConn := h.connectPlayer(3, "spec")

	startSpectatingViaEntry(spectatorConn, hostA.Info.Id)
	h.waitForHandler()
	assertRelation(t, spectator, hostA, true)

	startSpectatingViaEntry(spectatorConn, hostB.Info.Id)
	h.waitForHandler()

	assertRelation(t, spectator, hostA, false)
	assertRelation(t, spectator, hostB, true)

	if !connA.containsPacket(SERVER_STOP_SPECTATING) {
		t.Fatal("old host was not notified that spectating stopped")
	}
	if !connB.containsPacket(SERVER_START_SPECTATING) {
		t.Fatal("new host was not notified that spectating started")
	}
}

// TestStopSpectatingIsIdempotent: explicit stop, repeated stop and stopping
// against an already-offline host all end cleanly with no residual state.
func TestStopSpectatingIsIdempotent(t *testing.T) {
	h := newTestHarness()
	host, hostConn := h.connectPlayer(1, "host")
	spectator, spectatorConn := h.connectPlayer(2, "spec")

	// Stopping while never spectating is a safe no-op.
	stopSpectatingViaEntry(spectatorConn, host.Info.Id)
	h.waitForHandler()
	assertRelation(t, spectator, host, false)

	startSpectatingViaEntry(spectatorConn, host.Info.Id)
	h.waitForHandler()
	assertRelation(t, spectator, host, true)

	stopSpectatingViaEntry(spectatorConn, host.Info.Id)
	h.waitForHandler()
	assertRelation(t, spectator, host, false)
	if !hostConn.containsPacket(SERVER_STOP_SPECTATING) {
		t.Fatal("host did not receive the stop notification")
	}

	stopCountBefore := countPackets(hostConn, SERVER_STOP_SPECTATING)

	// Repeated stop must not error or send another notification.
	stopSpectatingViaEntry(spectatorConn, host.Info.Id)
	h.waitForHandler()
	if countPackets(hostConn, SERVER_STOP_SPECTATING) != stopCountBefore {
		t.Fatal("repeated stop produced an extra notification")
	}
	assertRelation(t, spectator, host, false)

	// Stop after the host went away (relation was cleared on disconnect) is
	// also a safe no-op.
	startSpectatingViaEntry(spectatorConn, host.Info.Id)
	h.waitForHandler()
	h.disconnect(host.Info.Id)
	stopSpectatingViaEntry(spectatorConn, 999)
	h.waitForHandler()

	h.server.mutex.Lock()
	if spectator.Host != nil {
		t.Fatal("spectator kept a host reference after the host went offline")
	}
	h.server.mutex.Unlock()
}

func countPackets(conn *fakeConn, packetId uint32) int {
	count := 0
	for _, id := range conn.packetIds() {
		if id == packetId {
			count++
		}
	}
	return count
}

// TestStartSpectatingOfflineTargetFails: a request for a player that is not
// online must never create a relation.
func TestStartSpectatingOfflineTargetFails(t *testing.T) {
	h := newTestHarness()
	spectator, spectatorConn := h.connectPlayer(2, "spec")

	startSpectatingViaEntry(spectatorConn, 4242)
	h.waitForHandler()

	h.server.mutex.Lock()
	defer h.server.mutex.Unlock()

	if spectator.Host != nil {
		t.Fatal("spectating relation created for an offline target")
	}
}

// TestCannotSpectateSelf: a player cannot attach to their own connection.
func TestCannotSpectateSelf(t *testing.T) {
	h := newTestHarness()
	player, conn := h.connectPlayer(7, "self")

	startSpectatingViaEntry(conn, player.Info.Id)
	h.waitForHandler()

	h.server.mutex.Lock()
	defer h.server.mutex.Unlock()

	if player.Host != nil || player.Spectators.Count() != 0 {
		t.Fatal("self-spectating created a relation")
	}
}

// TestConcurrentStartVsHostDisconnect: hammer start-spectate against a host
// that disconnects at the same time. No relation pointing at the offline
// host may ever survive.
func TestConcurrentStartVsHostDisconnect(t *testing.T) {
	for iteration := 0; iteration < 200; iteration++ {
		h := newTestHarness()
		host, _ := h.connectPlayer(1, "host")
		spectator, spectatorConn := h.connectPlayer(2, "spec")

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			startSpectatingViaEntry(spectatorConn, host.Info.Id)
		}()

		go func() {
			defer wg.Done()
			h.disconnect(host.Info.Id)
		}()

		wg.Wait()
		h.waitForHandler()

		h.server.mutex.Lock()
		hostOffline := h.server.Players.ByID(host.Info.Id) == nil
		dangling := spectator.Host == host
		hostListClean := host.Spectators.Count() == 0
		h.server.mutex.Unlock()

		if hostOffline && dangling {
			t.Fatalf("iteration %d: dangling spectating relation to offline host", iteration)
		}
		if !hostListClean {
			t.Fatalf("iteration %d: offline host retained a spectator entry", iteration)
		}
	}
}

// TestConcurrentStatusBroadcastVsStop: status changes racing with a stop
// request must never deliver a status update after the stop completed. The
// race is hammered for many iterations; every iteration ends by issuing one
// more status change *after* the stop, which must not reach the spectator.
func TestConcurrentStatusBroadcastVsStop(t *testing.T) {
	for iteration := 0; iteration < 200; iteration++ {
		h := newTestHarness()
		host, _ := h.connectPlayer(1, "host")
		spectator, spectatorConn := h.connectPlayer(2, "spec")

		startSpectatingViaEntry(spectatorConn, host.Info.Id)
		h.waitForHandler()
		assertRelation(t, spectator, host, true)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			// Drive a status change directly through the production handler.
			_ = handleStatusChange(statusChangeStream(ACTION_AWAY), host)
		}()

		go func() {
			defer wg.Done()
			stopSpectatingViaEntry(spectatorConn, host.Info.Id)
		}()

		wg.Wait()
		h.waitForHandler()

		h.server.mutex.Lock()
		stopped := spectator.Host == nil
		h.server.mutex.Unlock()

		if !stopped {
			t.Fatalf("iteration %d: stop did not complete", iteration)
		}

		// Deterministic post-stop check: a status change issued after the
		// stop has fully completed must never reach the spectator.
		statusUpdatesBefore := countPackets(spectatorConn, SERVER_SPECTATE_STATUS_UPDATE)
		if err := handleStatusChange(statusChangeStream(ACTION_AWAY), host); err != nil {
			t.Fatalf("post-stop status change failed: %s", err)
		}
		h.waitForHandler()

		if got := countPackets(spectatorConn, SERVER_SPECTATE_STATUS_UPDATE); got != statusUpdatesBefore {
			t.Fatalf(
				"iteration %d: spectator received a status update after stopping (%d -> %d)",
				iteration, statusUpdatesBefore, got,
			)
		}
	}
}

// TestSpectateDataFlowAndStopOverWire: drives the full packet entry path and
// checks the emitted data packets - start/status/frames while spectating,
// then a stop after which no further data reaches the spectator.
func TestSpectateDataFlowAndStopOverWire(t *testing.T) {
	h := newTestHarness()
	host, hostConn := h.connectPlayer(1, "host")
	spectator, spectatorConn := h.connectPlayer(2, "spec")

	startSpectatingViaEntry(spectatorConn, host.Info.Id)
	h.waitForHandler()
	assertRelation(t, spectator, host, true)

	// Host gets the start notification; spectator gets the current status.
	if !hostConn.containsPacket(SERVER_START_SPECTATING) {
		t.Fatal("host did not receive SERVER_START_SPECTATING over the wire")
	}
	if !spectatorConn.containsPacket(SERVER_SPECTATE_STATUS_UPDATE) {
		t.Fatal("spectator did not receive the initial status update")
	}

	// A host status change reaches the spectator.
	statusBefore := countPackets(spectatorConn, SERVER_SPECTATE_STATUS_UPDATE)
	if err := handleStatusChange(statusChangeStream(ACTION_AWAY), host); err != nil {
		t.Fatalf("status change failed: %s", err)
	}
	h.waitForHandler()
	if countPackets(spectatorConn, SERVER_SPECTATE_STATUS_UPDATE) != statusBefore+1 {
		t.Fatal("status change was not broadcast to the spectator")
	}

	// Host frames reach the spectator.
	if err := handleSpectateFrames(scorePackStream(), host); err != nil {
		t.Fatalf("frame broadcast failed: %s", err)
	}
	h.waitForHandler()
	if !spectatorConn.containsPacket(SERVER_SPECTATE_FRAMES) {
		t.Fatal("spectator did not receive replay frames")
	}

	// Stop through the real packet entry path.
	stopSpectatingViaEntry(spectatorConn, host.Info.Id)
	h.waitForHandler()
	assertRelation(t, spectator, host, false)
	if !hostConn.containsPacket(SERVER_STOP_SPECTATING) {
		t.Fatal("host did not receive SERVER_STOP_SPECTATING over the wire")
	}

	// Nothing spectate-related may reach the spectator afterwards.
	statusUpdatesBefore := countPackets(spectatorConn, SERVER_SPECTATE_STATUS_UPDATE)
	framesBefore := countPackets(spectatorConn, SERVER_SPECTATE_FRAMES)

	if err := handleStatusChange(statusChangeStream(ACTION_AWAY), host); err != nil {
		t.Fatalf("post-stop status change failed: %s", err)
	}
	if err := handleSpectateFrames(scorePackStream(), host); err != nil {
		t.Fatalf("post-stop frame broadcast failed: %s", err)
	}
	h.waitForHandler()

	if countPackets(spectatorConn, SERVER_SPECTATE_STATUS_UPDATE) != statusUpdatesBefore {
		t.Fatal("spectator received a status update after stopping")
	}
	if countPackets(spectatorConn, SERVER_SPECTATE_FRAMES) != framesBefore {
		t.Fatal("spectator received replay frames after stopping")
	}
}

// TestFramesNotSentToClosedConnection: replay frames broadcast by a host
// only reach currently-attached, open spectator connections.
func TestFramesNotSentToClosedConnection(t *testing.T) {
	h := newTestHarness()
	host, _ := h.connectPlayer(1, "host")
	spectatorA, connA := h.connectPlayer(2, "a")
	spectatorB, connB := h.connectPlayer(3, "b")

	startSpectatingViaEntry(connA, host.Info.Id)
	startSpectatingViaEntry(connB, host.Info.Id)
	h.waitForHandler()

	h.disconnect(spectatorA.Info.Id)

	// Host broadcasts frames through the production handler path.
	stream := scorePackStream()
	if err := handleSpectateFrames(stream, host); err != nil {
		t.Fatalf("handleSpectateFrames returned error: %s", err)
	}

	if connA.containsPacket(SERVER_SPECTATE_FRAMES) {
		t.Fatal("frames were sent to the closed spectator connection")
	}
	if !connB.containsPacket(SERVER_SPECTATE_FRAMES) {
		t.Fatal("remaining spectator did not receive the frame broadcast")
	}

	// Sanity: spectator A is gone and B is still attached.
	assertInRegistry(t, h, spectatorA.Info.Id, false)
	assertRelation(t, spectatorB, host, true)
}

// TestUnauthenticatedConnectionTeardown: a connection that drops before ever
// logging in is removed without panics or bogus broadcasts.
func TestUnauthenticatedConnectionTeardown(t *testing.T) {
	h := newTestHarness()
	conn := newFakeConn()
	done := make(chan struct{})

	go func() {
		h.server.HandleConnection(conn)
		close(done)
	}()

	conn.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HandleConnection did not return after disconnect")
	}

	h.server.mutex.Lock()
	if h.server.Players.Count() != 0 {
		t.Fatal("unauthenticated connection leaked into the player registry")
	}
	h.server.mutex.Unlock()
}

// TestDoubleTeardownIsIdempotent: calling the disconnect path twice (as
// happens when a replaced connection's read loop later errors) performs the
// quit broadcast exactly once and never double-closes the registry.
func TestDoubleTeardownIsIdempotent(t *testing.T) {
	h := newTestHarness()
	player, conn := h.connectPlayer(1, "p")
	other, otherConn := h.connectPlayer(2, "other")

	// First teardown via the explicit path.
	player.CloseConnection()
	h.waitForHandler()

	quitBefore := countPackets(otherConn, SERVER_USER_QUIT)

	// Second teardown (e.g. deferred cleanup noticing the dead socket).
	player.CloseConnection()
	h.waitForHandler()

	if got := countPackets(otherConn, SERVER_USER_QUIT); got != quitBefore {
		t.Fatalf("quit broadcast sent %d times, want %d", got, quitBefore)
	}
	if !conn.isClosed() {
		t.Fatal("connection was not closed by teardown")
	}
	assertInRegistry(t, h, player.Info.Id, false)
	assertInRegistry(t, h, other.Info.Id, true)
}
