// Package raceflow implements the server-side race lifecycle:
// start -> finish | abandon/disconnect.
//
// Invariants enforced by this package:
//   - a player can have at most one active race; starting a second one
//     while one is in progress is rejected without touching any balance;
//   - a start that fails validation (unknown type, insufficient fuel/money,
//     race-count limit) creates no record and changes nothing;
//   - a finish is accepted only when its (type, enemyId) matches the active
//     race recorded at start, and the record is invalidated before the
//     reward is granted, so forged or repeated finishes cannot pay out;
//   - abandon/disconnect restores exactly the fuel, money or race-count
//     slot occupied at start, and restoration can happen only once because
//     the record is removed as part of the restore.
package raceflow

import (
	"errors"
	"sync"
	"time"

	"srserver/conf"
	"srserver/logic/playermanager"
	pb "srserver/proto/out"
)

// Fixed finish reward, kept identical to the legacy server behaviour.
const (
	FinishAwardMoney      int32 = 1000
	FinishAwardItemBaseId int32 = 101
	FinishAwardItemCount  int32 = 1
	raceFuelCost          int32 = 5
	raceCountLimit        int32 = 5
	raceResultEpsilon           = 0.001
)

// Validation / lifecycle errors. Handlers map these to error response packets.
var (
	ErrAlreadyRacing    = errors.New("player already has an active race")
	ErrUnknownRaceType  = errors.New("unknown race type")
	ErrNotEnoughFuel    = errors.New("not enough fuel for race")
	ErrNotEnoughMoney   = errors.New("not enough money for race")
	ErrRaceLimitReached = errors.New("race count limit reached or cooldown timer is active")
	ErrNoActiveRace     = errors.New("player has no active race")
	ErrRaceMismatch     = errors.New("finish request does not match the active race")
)

// resourceKind tells what was occupied by a race and therefore what must
// be restored on abandon/disconnect.
type resourceKind int

const (
	resourceNone      resourceKind = iota // test/shadow races occupy money only
	resourceFuel                          // RACE
	resourceRating                        // POINTS
	resourceTime                          // TIME
	resourceChallenge                     // CHALLENGE
)

type fuelSnapshot struct {
	fuel     int32
	addition int32
	fuelTime int32
}

type moneySnapshot struct {
	money            int32
	gold             int32
	topPoints        int32
	upgradePoints    int32
	tournamentPoints int32
}

type counterSnapshot struct {
	kind  resourceKind
	count int32
	timer int64
}

// ActiveRace is the record created on a successful start and destroyed on
// finish, abandon or disconnect.
type ActiveRace struct {
	PlayerId  int64
	Type      pb.RaceType
	EnemyId   int64
	Distance  float32
	StartedAt int64

	resource resourceKind
	fuel     fuelSnapshot
	money    moneySnapshot
	counter  counterSnapshot
	refunded bool
}

// FinishResult is everything the handler needs to build the race response
// after a successful finish.
type FinishResult struct {
	Type    pb.RaceType
	EnemyId int64
	Result  pb.RaceResult
	Money   *pb.Money
	Items   []*pb.Item
}

// Manager owns all active races. Every mutation is serialized by its mutex,
// which makes rapid sequential starts and a disconnect arriving while a
// finish is being processed safe.
type Manager struct {
	mu    sync.Mutex
	races map[int64]*ActiveRace
	newID func() int64
}

// NewManager creates a Manager. idGen supplies unique inventory item ids
// (wired to utils.MakeId in production; a deterministic counter in tests).
func NewManager(idGen func() int64) *Manager {
	return &Manager{
		races: make(map[int64]*ActiveRace),
		newID: idGen,
	}
}

// ActiveRace returns a copy of the player's current race record, or nil.
func (m *Manager) ActiveRace(playerId int64) *ActiveRace {
	m.mu.Lock()
	defer m.mu.Unlock()
	ar := m.races[playerId]
	if ar == nil {
		return nil
	}
	snapshot := *ar
	return &snapshot
}

// HasActiveRace reports whether the player currently has a race in progress.
func (m *Manager) HasActiveRace(playerId int64) bool {
	return m.ActiveRace(playerId) != nil
}

// Start validates the request, occupies the resource required by the race
// type and records the active race. On any validation failure nothing is
// mutated and no record is left behind.
func (m *Manager) Start(player *playermanager.Player, raceType pb.RaceType, enemyId int64) (*ActiveRace, error) {
	if player == nil {
		return nil, ErrUnknownRaceType
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	playerId := player.User.GetId()
	if _, exists := m.races[playerId]; exists {
		return nil, ErrAlreadyRacing
	}

	// Validate before mutating anything.
	resource, distance, err := m.validate(player, raceType)
	if err != nil {
		return nil, err
	}

	// Snapshot the state that has to be restored on abandon/disconnect.
	ar := &ActiveRace{
		PlayerId:  playerId,
		Type:      raceType,
		EnemyId:   enemyId,
		Distance:  distance,
		StartedAt: time.Now().UnixMilli(),
		resource:  resource,
		fuel: fuelSnapshot{
			fuel:     player.User.Fuel.Fuel,
			addition: player.User.Fuel.Addition,
			fuelTime: player.User.Fuel.FuelTime,
		},
		money: moneySnapshot{
			money:            player.User.Money.Money,
			gold:             player.User.Money.Gold,
			topPoints:        player.User.Money.TopPoints,
			upgradePoints:    player.User.Money.UpgradePoints,
			tournamentPoints: player.User.Money.TournamentPoints,
		},
	}

	// Occupy the resource. Every branch below mirrors one of the snapshots.
	switch resource {
	case resourceNone:
		// TEST402 / TEST804 / SHADOW: entry fee.
		player.Withdraw(conf.MONEY_FOR_TEST_RACE)
	case resourceFuel:
		// RACE: fuel is consumed only after the IsFullEnought check passed.
		player.WithdrawFuel(raceFuelCost)
	case resourceRating:
		tc := player.User.TimersAndCounters
		ar.counter = counterSnapshot{kind: resourceRating, count: tc.RatingCount, timer: tc.RatingTimer}
		tc.RatingCount++
		tc.RatingTimer = time.Now().UnixMilli() + conf.RATING_RACE_DELAY
		player.Save()
	case resourceTime:
		tc := player.User.TimersAndCounters
		ar.counter = counterSnapshot{kind: resourceTime, count: tc.TimeCount, timer: tc.TimeTimer}
		tc.TimeCount++
		tc.TimeTimer = time.Now().UnixMilli() + conf.TIME_RACE_DELAY
		player.Save()
	case resourceChallenge:
		tc := player.User.TimersAndCounters
		ar.counter = counterSnapshot{kind: resourceChallenge, count: tc.ChallengeCount, timer: tc.ChallengeTimer}
		tc.ChallengeCount++
		tc.ChallengeTimer = time.Now().UnixMilli() + conf.CHALLENGE_RACE_DELAY
		player.Save()
	}

	m.races[playerId] = ar
	stored := *ar
	return &stored, nil
}

// validate checks race type and resource availability without mutating state.
func (m *Manager) validate(player *playermanager.Player, raceType pb.RaceType) (resourceKind, float32, error) {
	now := time.Now().UnixMilli()
	tc := player.User.TimersAndCounters

	switch raceType {
	case pb.RaceType_TEST402:
		if !player.CheckMoney(conf.MONEY_FOR_TEST_RACE) {
			return resourceNone, 0, ErrNotEnoughMoney
		}
		return resourceNone, 402, nil
	case pb.RaceType_TEST804:
		if !player.CheckMoney(conf.MONEY_FOR_TEST_RACE) {
			return resourceNone, 0, ErrNotEnoughMoney
		}
		return resourceNone, 804, nil
	case pb.RaceType_SHADOW:
		if !player.CheckMoney(conf.MONEY_FOR_TEST_RACE) {
			return resourceNone, 0, ErrNotEnoughMoney
		}
		return resourceNone, 804, nil
	case pb.RaceType_RACE:
		if !player.IsFullEnought(raceFuelCost) {
			return resourceNone, 0, ErrNotEnoughFuel
		}
		return resourceFuel, 402, nil
	case pb.RaceType_POINTS:
		if tc.RatingCount >= raceCountLimit || tc.RatingTimer > now {
			return resourceNone, 0, ErrRaceLimitReached
		}
		return resourceRating, 804, nil
	case pb.RaceType_TIME:
		if tc.TimeCount >= raceCountLimit || tc.TimeTimer > now {
			return resourceNone, 0, ErrRaceLimitReached
		}
		return resourceTime, 804, nil
	case pb.RaceType_CHALLENGE:
		if tc.ChallengeCount >= raceCountLimit || tc.ChallengeTimer > now {
			return resourceNone, 0, ErrRaceLimitReached
		}
		return resourceChallenge, 402, nil
	default:
		return resourceNone, 0, ErrUnknownRaceType
	}
}

// Finish accepts only a request matching the player's active race. The
// record is removed (and therefore cannot be finished twice) before the
// reward is granted. Forged or repeated requests return an error and change
// neither balances nor inventory.
func (m *Manager) Finish(player *playermanager.Player, params *pb.FinishParams) (*FinishResult, error) {
	if player == nil || params == nil {
		return nil, ErrNoActiveRace
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	playerId := player.User.GetId()
	ar, exists := m.races[playerId]
	if !exists {
		return nil, ErrNoActiveRace
	}
	if ar.Type != params.Type || ar.EnemyId != params.EnemyId {
		return nil, ErrRaceMismatch
	}

	// Invalidate the record before granting anything: deleting it from the
	// map is the single state transition that makes any replay fail lookup.
	delete(m.races, playerId)

	award := &pb.Money{Money: FinishAwardMoney}
	item := &pb.Item{
		Id:     m.newID(),
		Count:  FinishAwardItemCount,
		Type:   pb.ItemType_BLUEPRINT,
		BaseId: FinishAwardItemBaseId,
	}

	// Grant reward exactly once.
	player.Deposit(award)
	player.User.Inventory.Items = append(player.User.Inventory.Items, item)
	player.Save()

	return &FinishResult{
		Type:    ar.Type,
		EnemyId: ar.EnemyId,
		Result:  raceResult(params.UserTime, params.EnemyTime),
		Money:   award,
		Items:   []*pb.Item{item},
	}, nil
}

// Abandon ends the active race without a reward and restores whatever its
// start occupied. It is a no-op-error when no race is active (so a stray
// brake packet can never consume fuel, unlike the legacy handler).
func (m *Manager) Abandon(player *playermanager.Player) error {
	if player == nil {
		return ErrNoActiveRace
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	ar, exists := m.races[player.User.GetId()]
	if !exists || ar.refunded {
		return ErrNoActiveRace
	}
	m.restore(player, ar)
	delete(m.races, player.User.GetId())
	return nil
}

// HandleDisconnect is invoked from connection teardown. Any in-progress
// race is restored exactly once; without an active race it is a no-op.
func (m *Manager) HandleDisconnect(player *playermanager.Player) {
	if player == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	ar, exists := m.races[player.User.GetId()]
	if !exists || ar.refunded {
		return
	}
	m.restore(player, ar)
	delete(m.races, player.User.GetId())
}

// restore writes back the snapshots taken at Start. Restore happens at most
// once because the caller deletes the record immediately afterwards.
func (m *Manager) restore(player *playermanager.Player, ar *ActiveRace) {
	ar.refunded = true

	// Money is only occupied by fee-based races; restoring the snapshot is
	// harmless for fuel/counter races since their money was never touched.
	player.User.Money.Money = ar.money.money
	player.User.Money.Gold = ar.money.gold
	player.User.Money.TopPoints = ar.money.topPoints
	player.User.Money.UpgradePoints = ar.money.upgradePoints
	player.User.Money.TournamentPoints = ar.money.tournamentPoints

	if ar.resource == resourceFuel {
		player.User.Fuel.Fuel = ar.fuel.fuel
		player.User.Fuel.Addition = ar.fuel.addition
		player.User.Fuel.FuelTime = ar.fuel.fuelTime
	}

	tc := player.User.TimersAndCounters
	switch ar.counter.kind {
	case resourceRating:
		tc.RatingCount = ar.counter.count
		tc.RatingTimer = ar.counter.timer
	case resourceTime:
		tc.TimeCount = ar.counter.count
		tc.TimeTimer = ar.counter.timer
	case resourceChallenge:
		tc.ChallengeCount = ar.counter.count
		tc.ChallengeTimer = ar.counter.timer
	}
	player.Save()
}

// ActiveRaceCount exposes the number of tracked races (diagnostics/tests).
func (m *Manager) ActiveRaceCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.races)
}

func raceResult(userTime, enemyTime float32) pb.RaceResult {
	if userTime < enemyTime {
		return pb.RaceResult_WIN
	}
	if (userTime - enemyTime) < raceResultEpsilon {
		return pb.RaceResult_DRAW
	}
	return pb.RaceResult_LOST
}
