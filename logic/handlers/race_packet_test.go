package handlers

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"srserver/common"
	"srserver/logic/clients"
	"srserver/logic/playermanager"
	"srserver/logic/raceflow"
	"srserver/logic/utils"
	pb "srserver/proto/out"

	"google.golang.org/protobuf/proto"
)

// fakeConn is an in-memory net.Conn: everything the server writes is kept
// in a buffer so tests can parse the response frames without any socket or
// real game client.
type fakeConn struct {
	net.Conn
	writeBuf bytes.Buffer
}

func (c *fakeConn) Write(b []byte) (int, error) { return c.writeBuf.Write(b) }
func (c *fakeConn) Close() error                { return nil }

func newTestClient() (*clients.Client, *fakeConn) {
	conn := &fakeConn{}
	player := newTestClientPlayer()
	return &clients.Client{Conn: conn, Player: player}, conn
}

func newTestClientPlayer() *playermanager.Player {
	// The player gets the same fuel/money/inventory shape as a freshly
	// registered account, without MySQL (Player.Save is a no-op when no
	// database is open).
	return &playermanager.Player{Id: 1, Uid: "packet-test", User: utils.GetDefaultUser()}
}

const raceflowAward = raceflow.FinishAwardMoney

// deterministic item ids for stable assertions
func resetRaceManager() {
	counter := int64(0)
	RaceManager = raceflow.NewManager(func() int64 {
		counter++
		return counter
	})
}

// buildRequest constructs a packet exactly the way a client would:
// [method int32 BE][err int32 BE][sequence int32 BE][length-prefixed proto].
func buildRequest(t *testing.T, method int32, msg proto.Message) *common.Pack {
	t.Helper()
	pack := common.NewPack()
	pack.SetMethod(method)
	pack.SetSequence(7)
	if msg != nil {
		pack.WriteProto(msg)
	}
	return pack
}

// overTheWire serializes a packet to its on-wire frame and parses it back,
// proving the request can be produced without the real client and that the
// server-side frame decoder accepts it.
//
// Frame layout (see common/Pack.go and logic/LogicServer.go):
//
//	+-------------------+----------------------------------------------+
//	| length  uint32 BE | payload (= method|err|sequence|data, 12+B)   |
//	+-------------------+----------------------------------------------+
func overTheWire(t *testing.T, pack *common.Pack) *common.Pack {
	t.Helper()
	frame := pack.ToByteBuff()
	if len(frame) < 4 {
		t.Fatal("frame shorter than length prefix")
	}
	declared := binary.BigEndian.Uint32(frame[:4])
	if int(declared) != len(frame)-4 {
		t.Fatalf("frame length prefix = %d, actual payload = %d", declared, len(frame)-4)
	}
	return common.NewPackInstance(frame[4:])
}

func readResponse(t *testing.T, conn *fakeConn) *common.Pack {
	t.Helper()
	frame := conn.writeBuf.Bytes()
	if len(frame) < 4 {
		t.Fatal("no response frame written")
	}
	declared := binary.BigEndian.Uint32(frame[:4])
	if int(declared) != len(frame)-4 {
		t.Fatalf("response frame length prefix = %d, actual = %d", declared, len(frame)-4)
	}
	return common.NewPackInstance(frame[4:])
}

func stubContentDB() {
	findTrack = func(int32) *pb.BaseTrack { return &pb.BaseTrack{BaseId: 2018} }
	randomTrack = func() *pb.BaseTrack { return &pb.BaseTrack{BaseId: 9001} }
	getEnemyByID = func(id int64) *pb.Enemy {
		return &pb.Enemy{Id: id, EnemyType: &pb.EnemyType{Races: []pb.RaceResult{}}}
	}
	createShadowEnemy = func(p *playermanager.Player) *pb.Enemy {
		return &pb.Enemy{Id: 555, EnemyType: &pb.EnemyType{}}
	}
}

// handlersDispatch routes a parsed packet exactly like the connection loop.
func handlersDispatch(t *testing.T, client *clients.Client, pack *common.Pack) {
	t.Helper()
	ProcessPacket(pack, client)
}

func totalFuelOf(c *clients.Client) int32 {
	return c.Player.User.Fuel.Fuel + c.Player.User.Fuel.Addition
}

// ---------- packet-level lifecycle tests ----------

func TestPacketNormalStartAndFinish(t *testing.T) {
	resetRaceManager()
	stubContentDB()
	client, conn := newTestClient()

	// 1) start a fuel race against enemy 42
	startReq := buildRequest(t, common.CreateRace, &pb.StartParams{Type: pb.RaceType_RACE, EnemyId: 42})
	handlersDispatch(t, client, overTheWire(t, startReq))
	resp := readResponse(t, conn)
	if resp.GetError() {
		t.Fatal("start response flagged as error")
	}
	track := &pb.Track{}
	resp.ReadProto(track)
	if track.BaseId != 9001 { // RACE draws a random track
		t.Fatalf("track baseId = %d, want 9001", track.BaseId)
	}
	if resp.IsReadable() {
		enemy := &pb.Enemy{}
		resp.ReadProto(enemy)
		if enemy.Id != 42 {
			t.Fatalf("enemy id = %d, want 42", enemy.Id)
		}
	} else {
		t.Fatal("RACE response must carry the enemy")
	}
	if totalFuelOf(client) != 70 {
		t.Fatalf("fuel after start = %d, want 70", totalFuelOf(client))
	}
	if !RaceManager.HasActiveRace(client.Player.User.Id) {
		t.Fatal("no active race recorded after start")
	}

	// 2) matching finish packet
	conn.writeBuf.Reset()
	moneyBefore := client.Player.User.Money.Money
	finishReq := buildRequest(t, common.GetRaceAward, &pb.FinishParams{
		Type: pb.RaceType_RACE, EnemyId: 42, UserTime: 10, EnemyTime: 12,
	})
	handlersDispatch(t, client, overTheWire(t, finishReq))
	resp = readResponse(t, conn)
	if resp.GetError() {
		t.Fatal("finish response flagged as error")
	}
	race := &pb.Race{}
	resp.ReadProto(race)
	if race.Result != pb.RaceResult_WIN || race.Money.GetMoney() != raceflow.FinishAwardMoney {
		t.Fatalf("race response = %+v", race)
	}
	if client.Player.User.Money.Money != moneyBefore+raceflow.FinishAwardMoney {
		t.Fatal("finish reward not credited")
	}
	if len(client.Player.User.Inventory.Items) != 1 {
		t.Fatal("finish item not added to inventory")
	}
	if RaceManager.HasActiveRace(client.Player.User.Id) {
		t.Fatal("record not invalidated after finish")
	}
}

func TestPacketForgedFinishWithoutStart(t *testing.T) {
	resetRaceManager()
	stubContentDB()
	client, conn := newTestClient()
	moneyBefore, fuelBefore, itemsBefore :=
		client.Player.User.Money.Money, totalFuelOf(client), len(client.Player.User.Inventory.Items)

	forged := buildRequest(t, common.GetRaceAward, &pb.FinishParams{
		Type: pb.RaceType_RACE, EnemyId: 999, UserTime: 1, EnemyTime: 2,
	})
	handlersDispatch(t, client, overTheWire(t, forged))
	resp := readResponse(t, conn)
	if !resp.GetError() {
		t.Fatal("forged finish must return an error response")
	}
	if client.Player.User.Money.Money != moneyBefore || totalFuelOf(client) != fuelBefore ||
		len(client.Player.User.Inventory.Items) != itemsBefore {
		t.Fatal("forged finish changed balance or inventory")
	}
	if RaceManager.ActiveRaceCount() != 0 {
		t.Fatal("forged finish created a race record")
	}
}

func TestPacketRepeatedFinishPaysOnce(t *testing.T) {
	resetRaceManager()
	stubContentDB()
	client, conn := newTestClient()

	start := buildRequest(t, common.CreateRace, &pb.StartParams{Type: pb.RaceType_RACE, EnemyId: 42})
	handlersDispatch(t, client, overTheWire(t, start))
	readResponse(t, conn)

	finish := buildRequest(t, common.GetRaceAward, &pb.FinishParams{Type: pb.RaceType_RACE, EnemyId: 42})
	conn.writeBuf.Reset()
	handlersDispatch(t, client, overTheWire(t, finish))
	if readResponse(t, conn).GetError() {
		t.Fatal("first finish must succeed")
	}
	moneyOnce, itemsOnce := client.Player.User.Money.Money, len(client.Player.User.Inventory.Items)

	// replay the exact same bytes, twice
	frame := finish.ToByteBuff()
	for i := 0; i < 2; i++ {
		conn.writeBuf.Reset()
		handlersDispatch(t, client, common.NewPackInstance(frame[4:]))
		if !readResponse(t, conn).GetError() {
			t.Fatalf("replayed finish #%d must be rejected", i)
		}
	}
	if client.Player.User.Money.Money != moneyOnce || len(client.Player.User.Inventory.Items) != itemsOnce {
		t.Fatal("replayed finish granted extra reward")
	}
}

func TestPacketMismatchedFinishRejected(t *testing.T) {
	resetRaceManager()
	stubContentDB()
	client, conn := newTestClient()

	start := buildRequest(t, common.CreateRace, &pb.StartParams{Type: pb.RaceType_RACE, EnemyId: 42})
	handlersDispatch(t, client, overTheWire(t, start))
	readResponse(t, conn)

	conn.writeBuf.Reset()
	wrongEnemy := buildRequest(t, common.GetRaceAward, &pb.FinishParams{Type: pb.RaceType_RACE, EnemyId: 7})
	handlersDispatch(t, client, overTheWire(t, wrongEnemy))
	if !readResponse(t, conn).GetError() {
		t.Fatal("finish with wrong enemy id must be rejected")
	}
	if !RaceManager.HasActiveRace(client.Player.User.Id) {
		t.Fatal("rejected finish must leave the active race intact")
	}

	conn.writeBuf.Reset()
	wrongType := buildRequest(t, common.GetRaceAward, &pb.FinishParams{Type: pb.RaceType_TIME, EnemyId: 42})
	handlersDispatch(t, client, overTheWire(t, wrongType))
	if !readResponse(t, conn).GetError() {
		t.Fatal("finish with wrong race type must be rejected")
	}

	// the genuine finish still works
	conn.writeBuf.Reset()
	good := buildRequest(t, common.GetRaceAward, &pb.FinishParams{Type: pb.RaceType_RACE, EnemyId: 42})
	handlersDispatch(t, client, overTheWire(t, good))
	if readResponse(t, conn).GetError() {
		t.Fatal("matching finish must still succeed after rejected ones")
	}
}

func TestPacketStartFailureLeavesNoTrace(t *testing.T) {
	t.Run("unknown type", func(t *testing.T) {
		resetRaceManager()
		stubContentDB()
		client, conn := newTestClient()
		req := buildRequest(t, common.CreateRace, &pb.StartParams{Type: pb.RaceType_None})
		handlersDispatch(t, client, overTheWire(t, req))
		if !readResponse(t, conn).GetError() {
			t.Fatal("unknown type must return error")
		}
		if RaceManager.ActiveRaceCount() != 0 {
			t.Fatal("unknown type left a race record")
		}
	})

	t.Run("out of fuel", func(t *testing.T) {
		resetRaceManager()
		stubContentDB()
		client, conn := newTestClient()
		client.Player.User.Fuel.Fuel, client.Player.User.Fuel.Addition = 3, 0
		req := buildRequest(t, common.CreateRace, &pb.StartParams{Type: pb.RaceType_RACE, EnemyId: 1})
		handlersDispatch(t, client, overTheWire(t, req))
		if !readResponse(t, conn).GetError() {
			t.Fatal("insufficient fuel must return error")
		}
		if totalFuelOf(client) != 3 {
			t.Fatal("failed start changed fuel")
		}
		if RaceManager.ActiveRaceCount() != 0 {
			t.Fatal("failed start left a race record")
		}

		// nothing to finish either
		conn.writeBuf.Reset()
		finish := buildRequest(t, common.GetRaceAward, &pb.FinishParams{Type: pb.RaceType_RACE, EnemyId: 1})
		handlersDispatch(t, client, overTheWire(t, finish))
		if !readResponse(t, conn).GetError() {
			t.Fatal("finish after failed start must be rejected")
		}
		if totalFuelOf(client) != 3 {
			t.Fatal("post-failure finish changed fuel")
		}
	})

	t.Run("counter limit", func(t *testing.T) {
		resetRaceManager()
		stubContentDB()
		client, conn := newTestClient()
		client.Player.User.TimersAndCounters.RatingCount = 5
		req := buildRequest(t, common.CreateRace, &pb.StartParams{Type: pb.RaceType_POINTS, EnemyId: 1})
		handlersDispatch(t, client, overTheWire(t, req))
		if !readResponse(t, conn).GetError() {
			t.Fatal("limit-reached start must return error")
		}
		if client.Player.User.TimersAndCounters.RatingCount != 5 || RaceManager.ActiveRaceCount() != 0 {
			t.Fatal("limit-reached start mutated counters or left a record")
		}
	})
}

func TestPacketBrakeRestoresFuelOnce(t *testing.T) {
	resetRaceManager()
	stubContentDB()
	client, conn := newTestClient()

	start := buildRequest(t, common.CreateRace, &pb.StartParams{Type: pb.RaceType_RACE, EnemyId: 42})
	handlersDispatch(t, client, overTheWire(t, start))
	readResponse(t, conn)
	if totalFuelOf(client) != 70 {
		t.Fatalf("fuel after start = %d, want 70", totalFuelOf(client))
	}

	conn.writeBuf.Reset()
	brake := common.NewPack()
	brake.SetMethod(common.BrakeRace)
	handlersDispatch(t, client, overTheWire(t, brake))
	if readResponse(t, conn).GetError() {
		t.Fatal("brake on active race must succeed")
	}
	if totalFuelOf(client) != 75 {
		t.Fatalf("fuel after brake = %d, want 75", totalFuelOf(client))
	}
	if RaceManager.HasActiveRace(client.Player.User.Id) {
		t.Fatal("brake did not remove the race record")
	}

	// a stray brake with no active race must not consume fuel (legacy bug)
	conn.writeBuf.Reset()
	handlersDispatch(t, client, overTheWire(t, brake))
	if !readResponse(t, conn).GetError() {
		t.Fatal("brake without active race must return error")
	}
	if totalFuelOf(client) != 75 {
		t.Fatal("stray brake consumed fuel")
	}
}

func TestPacketCounterRaceRefundOnBrake(t *testing.T) {
	resetRaceManager()
	stubContentDB()
	client, conn := newTestClient()

	start := buildRequest(t, common.CreateRace, &pb.StartParams{Type: pb.RaceType_POINTS, EnemyId: 9})
	handlersDispatch(t, client, overTheWire(t, start))
	readResponse(t, conn)
	tc := client.Player.User.TimersAndCounters
	if tc.RatingCount != 1 || tc.RatingTimer == 0 {
		t.Fatal("rating slot not occupied by start")
	}

	conn.writeBuf.Reset()
	brake := common.NewPack()
	brake.SetMethod(common.BrakeRace)
	handlersDispatch(t, client, overTheWire(t, brake))
	readResponse(t, conn)
	if tc.RatingCount != 0 || tc.RatingTimer != 0 {
		t.Fatalf("rating slot not restored: count=%d timer=%d", tc.RatingCount, tc.RatingTimer)
	}
}

func TestPacketDisconnectRestoresAndClearsRecord(t *testing.T) {
	resetRaceManager()
	stubContentDB()
	client, _ := newTestClient()

	start := buildRequest(t, common.CreateRace, &pb.StartParams{Type: pb.RaceType_RACE, EnemyId: 42})
	handlersDispatch(t, client, overTheWire(t, start))
	if totalFuelOf(client) != 70 {
		t.Fatal("setup: fuel not occupied")
	}

	OnClientDisconnect(client)
	if totalFuelOf(client) != 75 {
		t.Fatalf("fuel after disconnect = %d, want 75", totalFuelOf(client))
	}
	if RaceManager.HasActiveRace(client.Player.User.Id) {
		t.Fatal("disconnect left a race record")
	}

	// finish packet landing after disconnect pays nothing
	conn := client.Conn.(*fakeConn)
	conn.writeBuf.Reset()
	finish := buildRequest(t, common.GetRaceAward, &pb.FinishParams{Type: pb.RaceType_RACE, EnemyId: 42})
	handlersDispatch(t, client, overTheWire(t, finish))
	if !readResponse(t, conn).GetError() {
		t.Fatal("finish after disconnect must be rejected")
	}
	if totalFuelOf(client) != 75 || len(client.Player.User.Inventory.Items) != 0 {
		t.Fatal("post-disconnect finish changed state")
	}
}

func TestPacketFinishUpdatesCurrentCarDistance(t *testing.T) {
	resetRaceManager()
	stubContentDB()
	client, conn := newTestClient()
	car := &pb.UserCar{Id: 77, BaseId: 3}
	client.Player.User.Garage.Cars = []*pb.UserCar{car}
	client.Player.User.Garage.CurrentId = 77

	start := buildRequest(t, common.CreateRace, &pb.StartParams{Type: pb.RaceType_RACE, EnemyId: 42})
	handlersDispatch(t, client, overTheWire(t, start))
	readResponse(t, conn)

	conn.writeBuf.Reset()
	finish := buildRequest(t, common.GetRaceAward, &pb.FinishParams{
		Type: pb.RaceType_RACE, EnemyId: 42, UserDistance: 405, UserTime: 9, EnemyTime: 10,
	})
	handlersDispatch(t, client, overTheWire(t, finish))
	if readResponse(t, conn).GetError() {
		t.Fatal("finish must succeed")
	}
	if car.AccumulatorDistance != 405 {
		t.Fatalf("car distance = %d, want 405", car.AccumulatorDistance)
	}
}

func TestPacketUnauthorizedClientRejected(t *testing.T) {
	resetRaceManager()
	conn := &fakeConn{}
	anonymous := &clients.Client{Conn: conn} // Player == nil
	req := buildRequest(t, common.GetRaceAward, &pb.FinishParams{Type: pb.RaceType_RACE})
	ProcessPacket(overTheWire(t, req), anonymous)
	if conn.writeBuf.Len() != 0 {
		t.Fatal("unauthorized packet must be dropped without a response")
	}
	if RaceManager.ActiveRaceCount() != 0 {
		t.Fatal("unauthorized packet created state")
	}
}

func TestOnClientDisconnectGuards(t *testing.T) {
	OnClientDisconnect(nil)               // must not panic
	OnClientDisconnect(&clients.Client{}) // nil player
}

func TestPacketShadowRaceCarriesEnemy(t *testing.T) {
	resetRaceManager()
	stubContentDB()
	client, conn := newTestClient()

	req := buildRequest(t, common.CreateRace, &pb.StartParams{Type: pb.RaceType_SHADOW})
	handlersDispatch(t, client, overTheWire(t, req))
	resp := readResponse(t, conn)
	if resp.GetError() {
		t.Fatal("shadow start must succeed")
	}
	resp.ReadProto(&pb.Track{})
	enemy := &pb.Enemy{}
	resp.ReadProto(enemy)
	if enemy.Id != 555 || enemy.Type != pb.RaceType_SHADOW {
		t.Fatalf("shadow enemy = %+v", enemy)
	}

	// finish without enemy id: exercises the no-enemy response branch
	conn.writeBuf.Reset()
	finish := buildRequest(t, common.GetRaceAward, &pb.FinishParams{Type: pb.RaceType_SHADOW})
	handlersDispatch(t, client, overTheWire(t, finish))
	resp = readResponse(t, conn)
	if resp.GetError() {
		t.Fatal("shadow finish must succeed")
	}
	race := &pb.Race{}
	resp.ReadProto(race)
	if race.IsCanRepeat {
		t.Fatal("races without an enemy cannot be repeatable")
	}
}

func TestPacketEnemyRepeatFlag(t *testing.T) {
	resetRaceManager()
	stubContentDB()
	client, conn := newTestClient()

	start := buildRequest(t, common.CreateRace, &pb.StartParams{Type: pb.RaceType_RACE, EnemyId: 42})
	handlersDispatch(t, client, overTheWire(t, start))
	readResponse(t, conn)

	conn.writeBuf.Reset()
	finish := buildRequest(t, common.GetRaceAward, &pb.FinishParams{Type: pb.RaceType_RACE, EnemyId: 42})
	handlersDispatch(t, client, overTheWire(t, finish))
	race := &pb.Race{}
	readResponse(t, conn).ReadProto(race)
	if !race.IsCanRepeat {
		t.Fatal("first race against an enemy must be repeatable (count < 3)")
	}
}
