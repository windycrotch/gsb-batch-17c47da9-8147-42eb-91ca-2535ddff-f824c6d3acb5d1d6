package hnet

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hexis-revival/hexagon/common"
)

// fakeConn is an in-memory net.Conn. Writes are captured for inspection and
// never block (the outgoing buffer grows). Reads consume queued incoming
// packets and return an error once the connection is closed.
type fakeConn struct {
	addr *net.TCPAddr

	mutex        sync.Mutex
	incoming     chan []byte
	outgoing     [][]byte
	closed       bool
	closedAt     time.Time
	readDeadline time.Time
}

func newFakeConn() *fakeConn {
	return &fakeConn{
		addr:     &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0},
		incoming: make(chan []byte, 256),
	}
}

func (conn *fakeConn) Read(buffer []byte) (int, error) {
	conn.mutex.Lock()
	if conn.closed {
		conn.mutex.Unlock()
		return 0, io.EOF
	}
	conn.mutex.Unlock()

	if !conn.readDeadline.IsZero() && time.Now().After(conn.readDeadline) {
		return 0, errors.New("read deadline exceeded")
	}

	data, ok := <-conn.incoming
	if !ok {
		return 0, io.EOF
	}

	return copy(buffer, data), nil
}

func (conn *fakeConn) Write(data []byte) (int, error) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()

	if conn.closed {
		return 0, io.ErrClosedPipe
	}

	copied := make([]byte, len(data))
	copy(copied, data)
	conn.outgoing = append(conn.outgoing, copied)
	return len(data), nil
}

func (conn *fakeConn) Close() error {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()

	if !conn.closed {
		conn.closed = true
		conn.closedAt = time.Now()
		close(conn.incoming)
	}

	return nil
}

func (conn *fakeConn) isClosed() bool {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	return conn.closed
}

func (conn *fakeConn) LocalAddr() net.Addr  { return conn.addr }
func (conn *fakeConn) RemoteAddr() net.Addr { return conn.addr }
func (conn *fakeConn) SetDeadline(time.Time) error {
	return nil
}
func (conn *fakeConn) SetReadDeadline(deadline time.Time) error {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.readDeadline = deadline
	return nil
}
func (conn *fakeConn) SetWriteDeadline(time.Time) error { return nil }

// queuePacket frames a payload the same way the production client does and
// hands it to the connection's read loop. Queuing onto a closed connection
// is a no-op, mirroring a packet that arrives after the socket is gone.
func (conn *fakeConn) queuePacket(packetId uint32, payload []byte) {
	frame := make([]byte, 0, HNET_PACKET_SIZE+len(payload))
	frame = append(frame, 0x87)

	idBytes := make([]byte, 4)
	sizeBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(idBytes, packetId)
	binary.BigEndian.PutUint32(sizeBytes, uint32(len(payload)))
	frame = append(frame, idBytes...)
	frame = append(frame, sizeBytes...)
	frame = append(frame, payload...)

	conn.mutex.Lock()
	defer conn.mutex.Unlock()

	if conn.closed {
		return
	}

	conn.incoming <- frame
}

// packetRecord is one captured outgoing packet.
type packetRecord struct {
	Id      uint32
	Payload []byte
}

// outgoingPackets snapshots every packet written to the connection.
func (conn *fakeConn) outgoingPackets() []packetRecord {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()

	records := make([]packetRecord, 0, len(conn.outgoing))

	for _, raw := range conn.outgoing {
		if len(raw) < HNET_PACKET_SIZE || raw[0] != 0x87 {
			continue
		}

		id := binary.BigEndian.Uint32(raw[1:5])
		size := binary.BigEndian.Uint32(raw[5:9])
		payload := raw[HNET_PACKET_SIZE : HNET_PACKET_SIZE+size]
		records = append(records, packetRecord{Id: id, Payload: payload})
	}

	return records
}

func (conn *fakeConn) packetIds() []uint32 {
	records := conn.outgoingPackets()
	ids := make([]uint32, 0, len(records))

	for _, record := range records {
		ids = append(ids, record.Id)
	}

	return ids
}

// containsPacket reports whether a packet with the given id was sent.
func (conn *fakeConn) containsPacket(packetId uint32) bool {
	for _, id := range conn.packetIds() {
		if id == packetId {
			return true
		}
	}

	return false
}

// testConnection pairs a player with the in-memory connection serving it and
// the channel that closes when runPlayer returns.
type testConnection struct {
	player *Player
	conn   *fakeConn
	done   chan struct{}
}

// testHarness bundles a server with the connections created for a test.
type testHarness struct {
	server      *HNetServer
	connections map[uint32]*testConnection
}

func newTestHarness() *testHarness {
	server := NewServer("127.0.0.1", 0, common.CreateLogger("test", common.QUIET), nil)

	return &testHarness{
		server:      server,
		connections: make(map[uint32]*testConnection),
	}
}

// connectPlayer runs the real connection lifecycle (HandleConnection's
// player construction plus the runPlayer packet loop) on a fresh in-memory
// connection, then gives that exact player an authenticated identity and
// registers it - the same state a successful login would leave behind.
// Because the serving goroutine and the registry share one Player object,
// packets mutate the same state the assertions inspect.
func (h *testHarness) connectPlayer(id uint32, name string) (*Player, *fakeConn) {
	conn := newFakeConn()
	player := h.server.newConnectionPlayer(conn)
	done := make(chan struct{})

	go func() {
		h.server.runPlayer(player)
		close(done)
	}()

	player.Info.Id = id
	player.Info.Name = name
	player.Stats.UserId = id

	h.server.mutex.Lock()
	h.server.Players.Add(player)
	h.server.mutex.Unlock()

	h.connections[id] = &testConnection{player: player, conn: conn, done: done}
	return player, conn
}

// disconnect closes the in-memory connection and waits for the production
// teardown path (the deferred CloseConnection inside HandleConnection) to
// finish running.
func (h *testHarness) disconnect(id uint32) {
	connection := h.connections[id]
	connection.conn.Close()

	select {
	case <-connection.done:
	case <-time.After(5 * time.Second):
	}
}

// waitForHandler gives an in-flight packet handler a chance to complete. It
// only yields; correctness assertions rely on the server lock, not timing.
func (h *testHarness) waitForHandler() {
	time.Sleep(20 * time.Millisecond)
}

// spectateRequestPayload builds the payload the client sends for start/stop
// spectating: one bool byte followed by a big-endian user id.
func spectateRequestPayload(userId uint32) []byte {
	stream := common.NewIOStream([]byte{}, binary.BigEndian)
	stream.WriteBool(true)
	stream.WriteU32(userId)
	return stream.Get()
}
