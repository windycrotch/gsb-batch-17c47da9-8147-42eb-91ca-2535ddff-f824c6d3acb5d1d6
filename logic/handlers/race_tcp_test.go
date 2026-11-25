package handlers

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"srserver/common"
	"srserver/logic/clients"
	pb "srserver/proto/out"

	"google.golang.org/protobuf/proto"
)

// readFrame decodes one length-prefixed frame from the real server loop's
// framing: uint32 BE length, then method|err|sequence|data.
func readFrame(t *testing.T, r *bufio.Reader) *common.Pack {
	t.Helper()
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		t.Fatalf("read length: %v", err)
	}
	payload := make([]byte, binary.BigEndian.Uint32(lenBuf))
	if _, err := io.ReadFull(r, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return common.NewPackInstance(payload)
}

// writeFrame mirrors logic/LogicServer.go's expected wire format exactly.
func writeFrame(t *testing.T, conn net.Conn, pack *common.Pack) {
	t.Helper()
	if _, err := conn.Write(pack.ToByteBuff()); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// TestRealTCPConnectionLifecycle drives the production accept/read loop
// (handleConnection's framing + ProcessPacket) over a real loopback TCP
// connection, with the client registered directly in the clients registry
// (the auth Init handler is MySQL-backed and is unrelated to race rules).
func TestRealTCPConnectionLifecycle(t *testing.T) {
	stubContentDB()
	resetRaceManager()

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	player := newTestClientPlayer()
	registryClient := &clients.Client{Conn: serverConn, Player: player}
	clients.Clients[serverConn] = registryClient
	defer delete(clients.Clients, serverConn)

	// production connection loop, identical wiring to LogicServer.Start
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		reader := bufio.NewReader(serverConn)
		lenBuf := make([]byte, 4)
		for {
			if _, err := io.ReadFull(reader, lenBuf); err != nil {
				OnClientDisconnect(clients.Clients[serverConn])
				delete(clients.Clients, serverConn)
				return
			}
			payload := make([]byte, binary.BigEndian.Uint32(lenBuf))
			if _, err := io.ReadFull(reader, payload); err != nil {
				OnClientDisconnect(clients.Clients[serverConn])
				delete(clients.Clients, serverConn)
				return
			}
			ProcessPacket(common.NewPackInstance(payload), clients.Clients[serverConn])
		}
	}()

	r := bufio.NewReader(clientConn)
	playerID := player.User.Id

	// 1) forged finish over the wire before any start -> error, no payout
	forged := common.NewPack()
	forged.SetMethod(common.GetRaceAward)
	forged.WriteProto(&pb.FinishParams{Type: pb.RaceType_RACE, EnemyId: 999})
	writeFrame(t, clientConn, forged)
	resp := readFrame(t, r)
	if !resp.GetError() {
		t.Fatal("forged finish over TCP must error")
	}

	// 2) start -> fuel occupied, record exists
	moneyBefore := player.User.Money.Money
	start := common.NewPack()
	start.SetMethod(common.CreateRace)
	start.WriteProto(&pb.StartParams{Type: pb.RaceType_RACE, EnemyId: 42})
	writeFrame(t, clientConn, start)
	resp = readFrame(t, r)
	if resp.GetError() {
		t.Fatal("start over TCP must succeed")
	}
	if totalFuelOf(registryClient) != 70 {
		t.Fatalf("fuel = %d, want 70", totalFuelOf(registryClient))
	}
	if !RaceManager.HasActiveRace(playerID) {
		t.Fatal("no record after TCP start")
	}

	// 3) replay the forged bytes while a real race is active -> mismatch, still no payout
	writeFrame(t, clientConn, forged)
	resp = readFrame(t, r)
	if !resp.GetError() {
		t.Fatal("mismatched finish over TCP must error")
	}
	if player.User.Money.Money != moneyBefore {
		t.Fatal("mismatched finish changed money")
	}

	// 4) matching finish -> reward once, record gone
	finish := common.NewPack()
	finish.SetMethod(common.GetRaceAward)
	finish.WriteProto(&pb.FinishParams{Type: pb.RaceType_RACE, EnemyId: 42, UserTime: 8, EnemyTime: 9})
	writeFrame(t, clientConn, finish)
	resp = readFrame(t, r)
	if resp.GetError() {
		t.Fatal("matching finish over TCP must succeed")
	}
	race := &pb.Race{}
	resp.ReadProto(race)
	if race.Result != pb.RaceResult_WIN {
		t.Fatalf("result = %v", race.Result)
	}
	if player.User.Money.Money != moneyBefore+raceflowAward {
		t.Fatalf("money = %d, want +%d", player.User.Money.Money, raceflowAward)
	}
	writeFrame(t, clientConn, finish) // replay identical bytes
	resp = readFrame(t, r)
	if !resp.GetError() || player.User.Money.Money != moneyBefore+raceflowAward {
		t.Fatal("replayed finish over TCP paid twice")
	}
	if len(player.User.Inventory.Items) != 1 {
		t.Fatal("item granted more than once")
	}

	// 5) disconnect during a second race -> fuel restored, record removed
	start2 := common.NewPack()
	start2.SetMethod(common.CreateRace)
	start2.WriteProto(&pb.StartParams{Type: pb.RaceType_RACE, EnemyId: 7})
	writeFrame(t, clientConn, start2)
	readFrame(t, r)
	if totalFuelOf(registryClient) != 65 {
		t.Fatalf("fuel = %d, want 65", totalFuelOf(registryClient))
	}
	clientConn.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("server loop did not notice disconnect")
	}
	if totalFuelOf(registryClient) != 70 {
		t.Fatalf("fuel after disconnect = %d, want 70 (refund of the 5 from race 2)", totalFuelOf(registryClient))
	}
	if RaceManager.HasActiveRace(playerID) {
		t.Fatal("record survived disconnect")
	}
}

// rawFinishBytes demonstrates how to hand-build any client packet without
// the game client: marshal the protobuf body, length-prefix it, prepend the
// 12-byte header. Returns bytes that could be sent with a plain net.Dial.
func rawFinishBytes(t *testing.T, method int32, msg proto.Message) []byte {
	t.Helper()
	pack := common.NewPack()
	pack.SetMethod(method)
	pack.WriteProto(msg)
	return pack.ToByteBuff()
}

func TestRawPacketBytesAreSelfDescribing(t *testing.T) {
	raw := rawFinishBytes(t, common.GetRaceAward, &pb.FinishParams{Type: pb.RaceType_TIME, EnemyId: 123})
	parsed := common.NewPackInstance(raw[4:])
	if parsed.GetMethod() != common.GetRaceAward {
		t.Fatal("method round-trip failed")
	}
	params := &pb.FinishParams{}
	parsed.ReadProto(params)
	if params.Type != pb.RaceType_TIME || params.EnemyId != 123 {
		t.Fatalf("payload round-trip failed: %+v", params)
	}
}
