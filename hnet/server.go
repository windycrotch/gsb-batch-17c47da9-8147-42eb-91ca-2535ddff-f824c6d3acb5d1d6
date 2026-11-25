package hnet

import (
	"encoding/binary"
	"fmt"
	"net"
	"runtime/debug"
	"sync"

	"github.com/hexis-revival/go-raknet"
	"github.com/hexis-revival/hexagon/common"
)

const HNET_PACKET_SIZE = 9

type HNetServer struct {
	// mutex guards every piece of connection-scoped shared state: the
	// Players registry, each player's Host pointer and every Spectators
	// collection. All reads and mutations of that state, including the
	// packet writes that follow them, happen while holding it. This keeps
	// spectating relations convergent with the connection lifecycle and
	// guarantees we never write to a connection that is already closed.
	mutex    sync.Mutex
	Players  PlayerCollection
	State    *common.State
	Listener *raknet.Listener
	Logger   *common.Logger
	Host     string
	Port     int
}

func NewServer(host string, port int, logger *common.Logger, state *common.State) *HNetServer {
	return &HNetServer{
		Players: NewPlayerCollection(),
		Logger:  logger,
		State:   state,
		Host:    host,
		Port:    port,
	}
}

func (server *HNetServer) Serve() {
	// Set hexis protocol version
	raknet.SetProtocolVersion(6)

	bind := fmt.Sprintf("%s:%d", server.Host, server.Port)
	listener, err := raknet.Listen(bind)

	if err != nil {
		server.Logger.Errorf("Failed to listen on %s: '%s'", bind, err)
		return
	}

	defer listener.Close()

	server.Logger.Infof("Listening on %s", listener.Addr())
	server.Listener = listener

	for {
		conn, _ := listener.Accept()
		go server.HandleConnection(conn)
	}
}

// HandleConnection is the real entry point for an accepted connection.
func (server *HNetServer) HandleConnection(conn net.Conn) {
	server.runPlayer(server.newConnectionPlayer(conn))
}

// newConnectionPlayer builds the Player state attached to a fresh
// connection.
func (server *HNetServer) newConnectionPlayer(conn net.Conn) *Player {
	logger := common.CreateLogger(
		conn.RemoteAddr().String(),
		server.Logger.GetLevel(),
	)

	return &Player{
		Conn:       conn,
		Logger:     logger,
		Server:     server,
		Info:       NewUserInfo(),
		Stats:      NewUserStats(),
		Spectators: NewPlayerCollection(),
	}
}

// runPlayer drives a connection's full lifecycle: connect notification,
// the packet read/dispatch loop, and the deferred teardown. It is shared by
// the production entry point and tests driving an in-memory connection.
func (server *HNetServer) runPlayer(player *Player) {
	player.OnConnect()
	defer server.CloseConnection(player)

	server.servePlayer(player)
}

// servePlayer runs the packet read/dispatch loop for a connection. It is
// separated from HandleConnection so the exact production dispatch path can
// be driven from tests with an in-memory connection.
func (server *HNetServer) servePlayer(player *Player) {
	for {
		buffer, err := player.Receive()

		if err != nil {
			player.Logger.Debugf("Error receiving data: %s", err)
			break
		}

		if len(buffer) < HNET_PACKET_SIZE {
			player.Logger.Warningf("Invalid packet size: %d", len(buffer))
			break
		}

		magicByte := buffer[0]
		packetId := common.ReadU32BE(buffer[1:5])
		packetSize := common.ReadU32BE(buffer[5:9])

		if magicByte != 0x87 {
			player.Logger.Warningf("Invalid magic byte: %d", magicByte)
			break
		}

		if packetSize > uint32(len(buffer)-HNET_PACKET_SIZE) {
			player.Logger.Warningf("Invalid packet size: %d", packetSize)
			break
		}

		packetData := buffer[HNET_PACKET_SIZE : HNET_PACKET_SIZE+packetSize]
		handler, ok := Handlers[packetId]

		if !ok {
			player.Logger.Warningf("Unknown packetId: %d -> '%s'", packetId, common.FormatBytes(packetData))
			continue
		}

		stream := common.NewIOStream(packetData, binary.BigEndian)
		err = handler(stream, player)

		if err != nil {
			player.Logger.Errorf("Error handling packet '%d': %s", packetId, err)
			continue
		}
	}
}

// CloseConnection is the deferred teardown for a connection's goroutine. It
// must never panic the serving loop, even if a handler already tore the
// player down explicitly.
func (server *HNetServer) CloseConnection(player *Player) {
	if r := recover(); r != nil {
		server.Logger.Errorf("Panic: '%s'", r)
		server.Logger.Debug(string(debug.Stack()))
	}

	server.disconnect(player)
}

// disconnect removes a player and every spectating relation that references
// them. It is idempotent: a connection that was replaced (kicked) and then
// noticed its own socket closing runs through here twice without side
// effects. All state changes and the resulting notifications happen under
// the server lock, so concurrent handlers can never observe or recreate a
// relation pointing at a disconnected player.
func (server *HNetServer) disconnect(player *Player) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if player.closed {
		return
	}

	server.teardown(player)
}

// teardown performs the actual relation cleanup, registry removal, quit
// broadcast and socket close. The caller must hold the server lock and is
// responsible for the idempotency check.
func (server *HNetServer) teardown(player *Player) {
	player.closed = true
	player.Logger.Infof("Disconnected -> <%s>", player.Conn.RemoteAddr())

	// If this player was spectating somebody, detach them from that host's
	// spectator list. The host is going away too when it is the player
	// themselves, so guard against that.
	if host := player.Host; host != nil && host != player {
		host.Spectators.Remove(player)
	}
	player.Host = nil

	// Everybody spectating this player loses their target. Their connections
	// stay open, but they must no longer hold a relation to an offline host.
	for _, spectator := range player.Spectators.Ptrs() {
		if spectator.Host == player {
			spectator.Host = nil
		}
	}
	player.Spectators = NewPlayerCollection()

	// Drop the player from the online registry, then notify the remaining
	// players. The snapshot is taken while still holding the lock, and the
	// writes happen before the connection is closed, so no data reaches a
	// closed socket.
	server.Players.Remove(player)
	server.Players.Broadcast(SERVER_USER_QUIT, &QuitResponse{player.Info.Id})

	player.Conn.Close()
}
