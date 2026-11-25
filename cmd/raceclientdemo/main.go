// Command raceclientdemo shows how to speak the server's binary packet
// protocol without the game client. It is also the reference implementation
// for the packet construction described in docs/RACE_LIFECYCLE.md.
//
// A packet is:
//
//	+-------------------+------------------------------------------------+
//	| length  uint32 BE | payload (length bytes)                         |
//	+-------------------+------------------------------------------------+
//
// and the payload is:
//
//	+-------------+----------+--------------+----------------------------+
//	| method i32  | err i32  | sequence i32 | protobuf body, length-     |
//	|   (BE)      |  (BE)    |    (BE)      | prefixed as int32 BE bytes |
//	+-------------+----------+--------------+----------------------------+
//
// Usage against a locally running server:
//
//	go run ./cmd/raceclientdemo -addr 127.0.0.1:8992
//
// The demo starts a fuel race, finishes it, replays the identical finish
// (which must be rejected) and finally starts another race and disconnects
// (which must trigger a fuel refund server-side).
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"srserver/common"
	pb "srserver/proto/out"

	"google.golang.org/protobuf/proto"
)

const (
	methodCreateRace = 703 // common.CreateRace
	methodGetAward   = 704 // common.GetRaceAward
	methodBrakeRace  = 705 // common.BrakeRace
)

// buildPacket is a dependency-free version of common.Pack construction.
func buildPacket(method int32, msg proto.Message) []byte {
	body, err := proto.Marshal(msg)
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	var payload bytes.Buffer
	binary.Write(&payload, binary.BigEndian, method)   // method
	binary.Write(&payload, binary.BigEndian, int32(0)) // err
	binary.Write(&payload, binary.BigEndian, int32(1)) // sequence
	binary.Write(&payload, binary.BigEndian, int32(len(body)))
	payload.Write(body)

	var frame bytes.Buffer
	binary.Write(&frame, binary.BigEndian, uint32(payload.Len()))
	frame.Write(payload.Bytes())
	return frame.Bytes()
}

func readFrame(conn net.Conn) (*common.Pack, error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return nil, err
	}
	payload := make([]byte, binary.BigEndian.Uint32(lenBuf))
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, err
	}
	return common.NewPackInstance(payload), nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8992", "logic server address")
	flag.Parse()

	conn, err := net.DialTimeout("tcp", *addr, 3*time.Second)
	if err != nil {
		log.Fatalf("connect %s: %v", *addr, err)
	}
	defer conn.Close()

	// 1. start a fuel race against enemy id 42
	conn.Write(buildPacket(methodCreateRace, &pb.StartParams{
		Type:    pb.RaceType_RACE,
		EnemyId: 42,
	}))
	resp, err := readFrame(conn)
	if err != nil {
		log.Fatalf("start response: %v", err)
	}
	fmt.Printf("start: error=%v\n", resp.GetError())

	// 2. matching finish
	finish := buildPacket(methodGetAward, &pb.FinishParams{
		Type:      pb.RaceType_RACE,
		EnemyId:   42,
		UserTime:  10,
		EnemyTime: 12,
	})
	conn.Write(finish)
	if resp, err = readFrame(conn); err != nil {
		log.Fatalf("finish response: %v", err)
	}
	race := &pb.Race{}
	resp.ReadProto(race)
	fmt.Printf("finish: error=%v result=%v money=%+v items=%d\n",
		resp.GetError(), race.Result, race.Money, len(race.Items))

	// 3. replay the exact same bytes -> server must reject without paying
	conn.Write(finish)
	if resp, err = readFrame(conn); err != nil {
		log.Fatalf("replay response: %v", err)
	}
	fmt.Printf("replay: error=%v (must be true)\n", resp.GetError())

	// 4. start again, then just close the connection (simulated disconnect)
	conn.Write(buildPacket(methodCreateRace, &pb.StartParams{
		Type:    pb.RaceType_RACE,
		EnemyId: 7,
	}))
	if resp, err = readFrame(conn); err != nil {
		log.Fatalf("second start response: %v", err)
	}
	fmt.Printf("second start: error=%v; closing connection to trigger refund\n", resp.GetError())

	// BrakeRace (method 705) is an empty-body packet; shown for completeness.
	brake := buildPacket(methodBrakeRace, nil)
	_ = brake
}
