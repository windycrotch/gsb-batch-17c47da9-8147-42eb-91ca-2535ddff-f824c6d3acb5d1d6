package raceflow

import (
	"errors"
	"testing"

	"srserver/logic/playermanager"
	pb "srserver/proto/out"
)

func TestTestAndShadowRaceFeeLifecycle(t *testing.T) {
	cases := []pb.RaceType{pb.RaceType_TEST804, pb.RaceType_SHADOW}
	for _, raceType := range cases {
		mgr, player := testManager(), testPlayer()
		ar, err := mgr.Start(player, raceType, 0)
		if err != nil {
			t.Fatalf("%v start: %v", raceType, err)
		}
		if ar.Distance != 804 {
			t.Fatalf("%v distance = %v, want 804", raceType, ar.Distance)
		}
		if player.User.Money.Money != 10_000_000-400 {
			t.Fatalf("%v fee not withdrawn", raceType)
		}
		if err = mgr.Abandon(player); err != nil {
			t.Fatalf("abandon: %v", err)
		}
		if player.User.Money.Money != 10_000_000 {
			t.Fatalf("%v fee not refunded", raceType)
		}

		// insufficient money on every fee-based type leaves no trace
		broke := testPlayer()
		broke.User.Money.Money = 10
		if _, err := mgr.Start(broke, raceType, 0); !errors.Is(err, ErrNotEnoughMoney) {
			t.Fatalf("%v: err = %v, want ErrNotEnoughMoney", raceType, err)
		}
		if broke.User.Money.Money != 10 || mgr.ActiveRaceCount() != 0 {
			t.Fatalf("%v failed start left a trace", raceType)
		}
	}
}

func TestAllCounterTypesRefundOnDisconnect(t *testing.T) {
	cases := []struct {
		raceType pb.RaceType
		read     func(*playermanager.Player) (int32, int64)
	}{
		{pb.RaceType_TIME, func(p *playermanager.Player) (int32, int64) {
			return p.User.TimersAndCounters.TimeCount, p.User.TimersAndCounters.TimeTimer
		}},
		{pb.RaceType_CHALLENGE, func(p *playermanager.Player) (int32, int64) {
			return p.User.TimersAndCounters.ChallengeCount, p.User.TimersAndCounters.ChallengeTimer
		}},
	}
	for _, tc := range cases {
		mgr, player := testManager(), testPlayer()
		if _, err := mgr.Start(player, tc.raceType, 0); err != nil {
			t.Fatalf("start %v: %v", tc.raceType, err)
		}
		mgr.HandleDisconnect(player)
		count, timer := tc.read(player)
		if count != 0 || timer != 0 {
			t.Fatalf("%v slot not restored after disconnect: count=%d timer=%d", tc.raceType, count, timer)
		}
	}
}

func TestNilPlayerAndParamsGuards(t *testing.T) {
	mgr := testManager()
	if _, err := mgr.Start(nil, pb.RaceType_RACE, 1); !errors.Is(err, ErrUnknownRaceType) {
		t.Fatalf("nil player start: %v", err)
	}
	if _, err := mgr.Finish(nil, &pb.FinishParams{}); !errors.Is(err, ErrNoActiveRace) {
		t.Fatalf("nil player finish: %v", err)
	}
	player := testPlayer()
	if _, err := mgr.Finish(player, nil); !errors.Is(err, ErrNoActiveRace) {
		t.Fatalf("nil params finish: %v", err)
	}
	if err := mgr.Abandon(nil); !errors.Is(err, ErrNoActiveRace) {
		t.Fatalf("nil player abandon: %v", err)
	}
	mgr.HandleDisconnect(nil) // must not panic
	if mgr.ActiveRace(1) != nil {
		t.Fatal("ActiveRace on empty manager must be nil")
	}
}

func TestAbandonOnEmptyManager(t *testing.T) {
	mgr, player := testManager(), testPlayer()
	if err := mgr.Abandon(player); !errors.Is(err, ErrNoActiveRace) {
		t.Fatalf("abandon without race: %v", err)
	}
}

func TestCooldownTimerBlocksEveryCounterRace(t *testing.T) {
	cases := []struct {
		raceType pb.RaceType
		block    func(*playermanager.Player)
	}{
		{pb.RaceType_POINTS, func(p *playermanager.Player) { p.User.TimersAndCounters.RatingTimer = 1 << 62 }},
		{pb.RaceType_CHALLENGE, func(p *playermanager.Player) { p.User.TimersAndCounters.ChallengeTimer = 1 << 62 }},
	}
	for _, tc := range cases {
		mgr, player := testManager(), testPlayer()
		tc.block(player)
		if _, err := mgr.Start(player, tc.raceType, 1); !errors.Is(err, ErrRaceLimitReached) {
			t.Fatalf("%v timer block: err = %v, want ErrRaceLimitReached", tc.raceType, err)
		}
		if mgr.ActiveRaceCount() != 0 {
			t.Fatalf("%v timer block left a record", tc.raceType)
		}
	}
}

func TestFuelAdditionRefundRoundTrip(t *testing.T) {
	mgr, player := testManager(), testPlayer()
	// Fuel above the cap lives in Addition; WithdrawFuel spends it first.
	player.User.Fuel.Fuel, player.User.Fuel.Addition = 75, 20
	mgr.Start(player, pb.RaceType_RACE, 1)
	if player.User.Fuel.Fuel != 75 || player.User.Fuel.Addition != 15 {
		t.Fatalf("after start: fuel=%d addition=%d, want 75/15", player.User.Fuel.Fuel, player.User.Fuel.Addition)
	}
	if err := mgr.Abandon(player); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if player.User.Fuel.Fuel != 75 || player.User.Fuel.Addition != 20 {
		t.Fatalf("after refund: fuel=%d addition=%d, want 75/20", player.User.Fuel.Fuel, player.User.Fuel.Addition)
	}
}

func TestFinishWithoutRewardKeepsOccupiedResource(t *testing.T) {
	// A finish pays the reward but does not refund the entry resource:
	// the race was actually run.
	mgr, player := testManager(), testPlayer()
	ar, _ := mgr.Start(player, pb.RaceType_RACE, 1)
	if _, err := mgr.Finish(player, finishParams(ar)); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if totalFuel(player) != 70 {
		t.Fatalf("fuel after finish = %d, want 70 (fuel is not refunded on finish)", totalFuel(player))
	}

	// fee-based race: fee stays consumed, reward is added on top
	mgr2, player2 := testManager(), testPlayer()
	ar2, _ := mgr2.Start(player2, pb.RaceType_TEST402, 0)
	moneyAfterStart := player2.User.Money.Money
	if _, err := mgr2.Finish(player2, finishParams(ar2)); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if player2.User.Money.Money != moneyAfterStart+FinishAwardMoney {
		t.Fatalf("money after finish = %d, want fee kept + reward", player2.User.Money.Money)
	}
}
