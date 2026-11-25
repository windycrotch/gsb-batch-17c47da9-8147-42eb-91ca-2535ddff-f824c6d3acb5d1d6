# Race lifecycle: start → finish / abandon

This document describes the server-side race state machine introduced to
close three classes of abuse:

1. **Forged finish** — a finish packet with no matching start used to grant
   the fixed reward to anyone.
2. **Repeated finish** — replaying the same finish packet used to pay the
   reward multiple times.
3. **Leaked occupation** — disconnecting or braking after a start never
   returned the fuel, entry fee or race-count slot taken at start.

## Components

| Piece | Location | Role |
|---|---|---|
| `raceflow.Manager` | `logic/raceflow/raceflow.go` | Thread-safe lifecycle authority. Holds at most one `ActiveRace` per player, snapshots every occupied resource at start, and restores it exactly once. |
| Packet handlers | `logic/handlers/race.go` | `CreateRace` (703), `GetRaceAward` (704), `BrakeRace` (705). They only translate packets into manager calls and never touch balances directly. |
| Disconnect hook | `logic/LogicServer.go` | `OnClientDisconnect` runs in the connection's teardown `defer`, so a dropped TCP connection refunds an in-progress race. |

## State machine

```
                    Start (valid)
   no record ───────────────────────► ActiveRace {type, enemyId, snapshots}
      ▲                                   │            │             │
      │                                   │ finish     │ abandon     │ TCP close
      │            Start fails validation │ (matching) │ (705)       │
      └────── nothing mutated ◄───────────┤            ▼             ▼
                                          ▼         refund once   refund once
                                     record deleted                record deleted
                                     reward paid                   no reward
```

### Start (`CreateRace`, method 703)

- All validation happens **before** any mutation: unknown race type,
  insufficient fuel/money, count limit (`>= 5`) or an active cooldown timer
  reject the request, return an error packet and leave no record, no
  balance change and no counter change.
- On success the manager stores an `ActiveRace` containing the race type,
  the enemy id and snapshots of the occupied resources:

  | Race type | Occupied resource | Restored on abandon/disconnect |
  |---|---|---|
  | `TEST402`, `TEST804`, `SHADOW` | entry fee (`Money.Money`, 400) | full fee |
  | `RACE` | 5 fuel (checked with `IsFullEnought` first, never negative) | fuel + addition + fuelTime |
  | `POINTS` | rating counter slot + timer | count and timer |
  | `TIME` | time counter slot + timer | count and timer |
  | `CHALLENGE` | challenge counter slot + timer | count and timer |

- A second start while a race is active is rejected
  (`ErrAlreadyRacing`) without touching anything.

### Finish (`GetRaceAward`, method 704)

- The request's `(type, enemyId)` must equal the stored `ActiveRace`;
  anything else returns `ErrRaceMismatch` and leaves the active race
  untouched (so a rejected forged packet cannot block the real finish).
- The record is **deleted before the reward is granted**. Deletion is the
  single invalidation step, so every replay afterwards fails with
  `ErrNoActiveRace` and cannot change money or inventory.
- Reward remains the legacy fixed one: 1000 money and one blueprint
  (`baseId 101`) added to `User.Inventory.Items`.

### Abandon (`BrakeRace`, method 705) and disconnect

- Both paths call the same `restore` logic: money/fuel snapshots are
  written back and the occupied counter slot (count and timer) is reset.
- The record is deleted as part of the restore, and `refunded` is set first,
  so restoration can happen only once even if a brake packet and the
  disconnect teardown race each other.
- A brake packet without an active race is now an error instead of silently
  consuming 5 fuel (the legacy behaviour).

### Concurrency

Every manager method takes one mutex, and the connection loop processes one
player's packets sequentially on its own goroutine. The critical
check-and-mutate sequences (lookup → validate → record; lookup → delete →
pay) are therefore atomic. The tests in
`logic/raceflow/raceflow_test.go` include 32-way concurrent finishes and
concurrent finish/abandon runs under `-race`; exactly one outcome can win
and no balance ever goes inconsistent.

## Building packets without the game client

The wire format is a plain length-prefixed binary frame
(see `common/Pack.go`, `logic/LogicServer.go`):

```
frame   = uint32 BE payloadLength, then payload
payload = int32 BE method | int32 BE err | int32 BE sequence | body
body    = int32 BE protobufLength | marshalled protobuf bytes
```

Method ids live in `common/Methods.go`; message shapes are in
`proto/src/race.proto` (`StartParams`, `FinishParams`). Minimal Go
construction with no client:

```go
body, _ := proto.Marshal(&pb.StartParams{Type: pb.RaceType_RACE, EnemyId: 42})
buf := new(bytes.Buffer)
binary.Write(buf, binary.BigEndian, int32(703))       // CreateRace
binary.Write(buf, binary.BigEndian, int32(0))         // err
binary.Write(buf, binary.BigEndian, int32(1))         // sequence
binary.Write(buf, binary.BigEndian, int32(len(body))) // body length
buf.Write(body)
frame := append(binary.BigEndian.AppendUint32(nil, uint32(buf.Len())), buf.Bytes()...)
conn.Write(frame)
```

Any language with a protobuf runtime can do the same: marshal
`StartParams`/`FinishParams`, prefix, send over a raw TCP socket.

### Runnable demo

`cmd/raceclientdemo` contains a complete dependency-light client:

```sh
go run ./cmd/raceclientdemo -addr 127.0.0.1:8992
```

It starts a race, finishes it, replays the identical finish bytes (the
server rejects it) and then starts another race and disconnects to
demonstrate the refund.

## Running the tests

Tests need neither MySQL nor the `resources` submodule: `Player.Save`
becomes a no-op when no database is open, and content lookups are stubbed
at the handler boundary.

```sh
# generate protobuf code first (only once per clean checkout)
mkdir -p proto/out
protoc --proto_path=proto/src --go_out=./proto/out \
  --go_opt=paths=source_relative proto/src/*.proto

go test ./...
go test -race ./logic/raceflow/ ./logic/handlers/
```

### Coverage map

| Required scenario | Test |
|---|---|
| Normal finish (all race categories, result classification) | `TestFinishFuelRacePaysExactlyOnce`, `TestFinishAllRaceCategories`, `TestRaceResultClassification`, `TestPacketNormalStartAndFinish`, `TestRealTCPConnectionLifecycle` |
| Forged finish (no start / wrong type / wrong enemy) | `TestForgedFinishWithoutActiveRaceChangesNothing`, `TestMismatchedFinishIsRejectedAndRaceStaysActive`, `TestPacketForgedFinishWithoutStart`, `TestPacketMismatchedFinishRejected` |
| Repeated finish | `TestRepeatedFinishPaysOnce`, `TestPacketRepeatedFinishPaysOnce`, `TestConcurrentFinishesPaysExactlyOnce` |
| Failed start (fuel / money / limit / timer / unknown type) | `TestStartFailuresLeaveNoTrace`, `TestCooldownTimerBlocksEveryCounterRace`, `TestPacketStartFailureLeavesNoTrace` |
| Abandon restore, once | `TestAbandonRestoresEachResourceOnce`, `TestAbandonThenDisconnectRestoresOnce`, `TestPacketBrakeRestoresFuelOnce`, `TestPacketCounterRaceRefundOnBrake` |
| Disconnect restore, once | `TestDisconnectRestoresInProgressRace`, `TestDisconnectAfterFinishKeepsReward`, `TestDisconnectWithoutRaceIsNoop`, `TestPacketDisconnectRestoresAndClearsRecord`, `TestRealTCPConnectionLifecycle` |
| Rapid / concurrent consistency | `TestConcurrentStartsOnlyOneWins`, `TestFinishRacingAbandonNeverBoth`, `TestRapidLifecycleSequenceStaysConsistent` |
