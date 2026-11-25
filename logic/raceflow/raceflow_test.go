package raceflow

import (
	"errors"
	"sync"
	"testing"

	"srserver/conf"
	"srserver/logic/playermanager"
	"srserver/logic/utils"
	pb "srserver/proto/out"
)

// deterministic id generator so test assertions on inventory are stable
func testManager() *Manager {
	counter := int64(0)
	return NewManager(func() int64 {
		counter++
		return counter
	})
}

func testPlayer() *playermanager.Player {
	return &playermanager.Player{
		Id:   1,
		Uid:  "test-uid",
		User: utils.GetDefaultUser(),
	}
}

func totalFuel(p *playermanager.Player) int32 {
	return p.User.Fuel.Fuel + p.User.Fuel.Addition
}

func finishParams(ar *ActiveRace) *pb.FinishParams {
	return &pb.FinishParams{
		Type:      ar.Type,
		EnemyId:   ar.EnemyId,
		UserTime:  10,
		EnemyTime: 12,
	}
}

// ---------- normal finish ----------

func TestFinishFuelRacePaysExactlyOnce(t *testing.T) {
	mgr, player := testManager(), testPlayer()

	ar, err := mgr.Start(player, pb.RaceType_RACE, 42)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if ar.Type != pb.RaceType_RACE || ar.EnemyId != 42 {
		t.Fatalf("active race record mismatch: %+v", ar)
	}
	if got := totalFuel(player); got != 70 {
		t.Fatalf("fuel after start = %d, want 70", got)
	}
	if !mgr.HasActiveRace(player.User.Id) {
		t.Fatal("expected active race record after start")
	}

	moneyBefore := player.User.Money.Money
	result, err := mgr.Finish(player, finishParams(ar))
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if result.Result != pb.RaceResult_WIN {
		t.Fatalf("result = %v, want WIN", result.Result)
	}
	if player.User.Money.Money != moneyBefore+FinishAwardMoney {
		t.Fatalf("money delta = %d, want %d", player.User.Money.Money-moneyBefore, FinishAwardMoney)
	}
	if len(player.User.Inventory.Items) != 1 || player.User.Inventory.Items[0].BaseId != FinishAwardItemBaseId {
		t.Fatalf("inventory after finish = %+v, want one blueprint %d", player.User.Inventory.Items, FinishAwardItemBaseId)
	}
	if mgr.HasActiveRace(player.User.Id) {
		t.Fatal("active race record must be invalidated after finish")
	}
	if mgr.ActiveRaceCount() != 0 {
		t.Fatalf("manager retains %d records, want 0", mgr.ActiveRaceCount())
	}
}

func TestFinishAllRaceCategories(t *testing.T) {
	cases := []struct {
		name      string
		raceType  pb.RaceType
		enemyId   int64
		checkFunc func(*testing.T, *playermanager.Player)
	}{
		{"money race TEST402", pb.RaceType_TEST402, 0, func(t *testing.T, p *playermanager.Player) {
			if p.User.Money.Money != 10_000_000-conf.MONEY_FOR_TEST_RACE.Money {
				t.Fatal("entry fee not withdrawn")
			}
		}},
		{"fuel race RACE", pb.RaceType_RACE, 7, func(t *testing.T, p *playermanager.Player) {
			if totalFuel(p) != 70 {
				t.Fatal("fuel not withdrawn")
			}
		}},
		{"rating counter POINTS", pb.RaceType_POINTS, 7, func(t *testing.T, p *playermanager.Player) {
			if p.User.TimersAndCounters.RatingCount != 1 || p.User.TimersAndCounters.RatingTimer == 0 {
				t.Fatal("rating counter/timer not occupied")
			}
		}},
		{"time counter TIME", pb.RaceType_TIME, 0, func(t *testing.T, p *playermanager.Player) {
			if p.User.TimersAndCounters.TimeCount != 1 || p.User.TimersAndCounters.TimeTimer == 0 {
				t.Fatal("time counter/timer not occupied")
			}
		}},
		{"challenge counter CHALLENGE", pb.RaceType_CHALLENGE, 0, func(t *testing.T, p *playermanager.Player) {
			if p.User.TimersAndCounters.ChallengeCount != 1 || p.User.TimersAndCounters.ChallengeTimer == 0 {
				t.Fatal("challenge counter/timer not occupied")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, player := testManager(), testPlayer()
			ar, err := mgr.Start(player, tc.raceType, tc.enemyId)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			tc.checkFunc(t, player)

			moneyBefore := player.User.Money.Money
			itemsBefore := len(player.User.Inventory.Items)
			if _, err = mgr.Finish(player, finishParams(ar)); err != nil {
				t.Fatalf("finish: %v", err)
			}
			if player.User.Money.Money != moneyBefore+FinishAwardMoney {
				t.Fatal("finish reward not granted")
			}
			if len(player.User.Inventory.Items) != itemsBefore+1 {
				t.Fatal("finish item not granted")
			}
			if mgr.ActiveRaceCount() != 0 {
				t.Fatal("record left behind after finish")
			}
		})
	}
}

func TestRaceResultClassification(t *testing.T) {
	cases := []struct {
		user, enemy float32
		want        pb.RaceResult
	}{
		{10, 12, pb.RaceResult_WIN},
		{12, 10, pb.RaceResult_LOST},
		{10, 10, pb.RaceResult_DRAW},
	}
	for _, tc := range cases {
		mgr, player := testManager(), testPlayer()
		ar, _ := mgr.Start(player, pb.RaceType_RACE, 1)
		res, err := mgr.Finish(player, &pb.FinishParams{Type: ar.Type, EnemyId: ar.EnemyId, UserTime: tc.user, EnemyTime: tc.enemy})
		if err != nil {
			t.Fatalf("finish: %v", err)
		}
		if res.Result != tc.want {
			t.Fatalf("userTime=%v enemyTime=%v: got %v want %v", tc.user, tc.enemy, res.Result, tc.want)
		}
	}
}

// ---------- forged / repeated finishes ----------

func TestForgedFinishWithoutActiveRaceChangesNothing(t *testing.T) {
	mgr, player := testManager(), testPlayer()
	moneyBefore, fuelBefore, itemsBefore := player.User.Money.Money, totalFuel(player), len(player.User.Inventory.Items)

	_, err := mgr.Finish(player, &pb.FinishParams{Type: pb.RaceType_RACE, EnemyId: 999, UserTime: 1, EnemyTime: 2})
	if !errors.Is(err, ErrNoActiveRace) {
		t.Fatalf("err = %v, want ErrNoActiveRace", err)
	}
	if player.User.Money.Money != moneyBefore || totalFuel(player) != fuelBefore || len(player.User.Inventory.Items) != itemsBefore {
		t.Fatal("forged finish mutated balances or inventory")
	}
	if mgr.ActiveRaceCount() != 0 {
		t.Fatal("forged finish created a record")
	}
}

func TestMismatchedFinishIsRejectedAndRaceStaysActive(t *testing.T) {
	mgr, player := testManager(), testPlayer()
	ar, err := mgr.Start(player, pb.RaceType_RACE, 42)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	moneyBefore, fuelBefore := player.User.Money.Money, totalFuel(player)

	// wrong enemy id
	if _, err = mgr.Finish(player, &pb.FinishParams{Type: pb.RaceType_RACE, EnemyId: 999}); !errors.Is(err, ErrRaceMismatch) {
		t.Fatalf("wrong enemy: err = %v, want ErrRaceMismatch", err)
	}
	// wrong race type
	if _, err = mgr.Finish(player, &pb.FinishParams{Type: pb.RaceType_TIME, EnemyId: 42}); !errors.Is(err, ErrRaceMismatch) {
		t.Fatalf("wrong type: err = %v, want ErrRaceMismatch", err)
	}
	if !mgr.HasActiveRace(player.User.Id) {
		t.Fatal("rejected finish must not invalidate the active race")
	}
	if player.User.Money.Money != moneyBefore || totalFuel(player) != fuelBefore {
		t.Fatal("rejected finish mutated balances")
	}

	// the legitimate finish still works exactly once
	if _, err = mgr.Finish(player, finishParams(ar)); err != nil {
		t.Fatalf("legitimate finish after rejected ones: %v", err)
	}
}

func TestRepeatedFinishPaysOnce(t *testing.T) {
	mgr, player := testManager(), testPlayer()
	ar, _ := mgr.Start(player, pb.RaceType_RACE, 42)

	if _, err := mgr.Finish(player, finishParams(ar)); err != nil {
		t.Fatalf("first finish: %v", err)
	}
	moneyAfterFirst, itemsAfterFirst := player.User.Money.Money, len(player.User.Inventory.Items)

	for i := 0; i < 3; i++ {
		if _, err := mgr.Finish(player, finishParams(ar)); !errors.Is(err, ErrNoActiveRace) {
			t.Fatalf("repeat finish #%d: err = %v, want ErrNoActiveRace", i, err)
		}
	}
	if player.User.Money.Money != moneyAfterFirst || len(player.User.Inventory.Items) != itemsAfterFirst {
		t.Fatal("repeated finish granted extra reward")
	}
}

// ---------- start failures ----------

func TestStartFailuresLeaveNoTrace(t *testing.T) {
	t.Run("unknown type", func(t *testing.T) {
		mgr, player := testManager(), testPlayer()
		money, fuel, items := player.User.Money.Money, totalFuel(player), len(player.User.Inventory.Items)
		_, err := mgr.Start(player, pb.RaceType_None, 1)
		if !errors.Is(err, ErrUnknownRaceType) {
			t.Fatalf("err = %v, want ErrUnknownRaceType", err)
		}
		assertUntouched(t, player, money, fuel, items, mgr)
	})

	t.Run("not enough fuel", func(t *testing.T) {
		mgr, player := testManager(), testPlayer()
		player.User.Fuel.Fuel, player.User.Fuel.Addition = 4, 0
		money, fuel, items := player.User.Money.Money, totalFuel(player), len(player.User.Inventory.Items)
		_, err := mgr.Start(player, pb.RaceType_RACE, 1)
		if !errors.Is(err, ErrNotEnoughFuel) {
			t.Fatalf("err = %v, want ErrNotEnoughFuel", err)
		}
		assertUntouched(t, player, money, fuel, items, mgr)
		if totalFuel(player) < 0 {
			t.Fatal("fuel went negative")
		}
	})

	t.Run("not enough money", func(t *testing.T) {
		mgr, player := testManager(), testPlayer()
		player.User.Money.Money = 0
		_, err := mgr.Start(player, pb.RaceType_TEST402, 0)
		if !errors.Is(err, ErrNotEnoughMoney) {
			t.Fatalf("err = %v, want ErrNotEnoughMoney", err)
		}
		if mgr.ActiveRaceCount() != 0 || player.User.Money.Money != 0 {
			t.Fatal("failed money race left a trace")
		}
	})

	t.Run("counter limit reached", func(t *testing.T) {
		mgr, player := testManager(), testPlayer()
		player.User.TimersAndCounters.RatingCount = raceCountLimit
		_, err := mgr.Start(player, pb.RaceType_POINTS, 1)
		if !errors.Is(err, ErrRaceLimitReached) {
			t.Fatalf("err = %v, want ErrRaceLimitReached", err)
		}
		if player.User.TimersAndCounters.RatingCount != raceCountLimit {
			t.Fatal("counter mutated on failed start")
		}
		assertUntouched(t, player, player.User.Money.Money, totalFuel(player), len(player.User.Inventory.Items), mgr)
	})

	t.Run("cooldown timer active", func(t *testing.T) {
		mgr, player := testManager(), testPlayer()
		player.User.TimersAndCounters.TimeTimer = 1 << 62
		_, err := mgr.Start(player, pb.RaceType_TIME, 0)
		if !errors.Is(err, ErrRaceLimitReached) {
			t.Fatalf("err = %v, want ErrRaceLimitReached", err)
		}
		if mgr.ActiveRaceCount() != 0 {
			t.Fatal("timer-blocked start left a record")
		}
	})
}

func assertUntouched(t *testing.T, p *playermanager.Player, money, fuel int32, items int, mgr *Manager) {
	t.Helper()
	if p.User.Money.Money != money || totalFuel(p) != fuel || len(p.User.Inventory.Items) != items {
		t.Fatalf("state changed on failed start: money %d->%d fuel %d->%d items %d->%d",
			money, p.User.Money.Money, fuel, totalFuel(p), items, len(p.User.Inventory.Items))
	}
	if mgr.ActiveRaceCount() != 0 {
		t.Fatal("failed start left an active race record")
	}
}

func TestSecondConcurrentStartIsRejected(t *testing.T) {
	mgr, player := testManager(), testPlayer()
	if _, err := mgr.Start(player, pb.RaceType_RACE, 1); err != nil {
		t.Fatalf("first start: %v", err)
	}
	fuelAfterFirst := totalFuel(player)

	if _, err := mgr.Start(player, pb.RaceType_TIME, 0); !errors.Is(err, ErrAlreadyRacing) {
		t.Fatalf("second start: err = %v, want ErrAlreadyRacing", err)
	}
	if totalFuel(player) != fuelAfterFirst {
		t.Fatal("rejected second start mutated fuel")
	}
}

// ---------- abandon / disconnect restore ----------

func TestAbandonRestoresEachResourceOnce(t *testing.T) {
	t.Run("fuel", func(t *testing.T) {
		mgr, player := testManager(), testPlayer()
		mgr.Start(player, pb.RaceType_RACE, 1)
		if totalFuel(player) != 70 {
			t.Fatal("setup: fuel not occupied")
		}
		if err := mgr.Abandon(player); err != nil {
			t.Fatalf("abandon: %v", err)
		}
		if totalFuel(player) != 75 {
			t.Fatalf("fuel after abandon = %d, want 75", totalFuel(player))
		}
		if err := mgr.Abandon(player); !errors.Is(err, ErrNoActiveRace) {
			t.Fatalf("second abandon: err = %v, want ErrNoActiveRace", err)
		}
		if totalFuel(player) != 75 {
			t.Fatal("second abandon changed fuel: restore must happen once")
		}
	})

	t.Run("money", func(t *testing.T) {
		mgr, player := testManager(), testPlayer()
		mgr.Start(player, pb.RaceType_TEST402, 0)
		if err := mgr.Abandon(player); err != nil {
			t.Fatalf("abandon: %v", err)
		}
		if player.User.Money.Money != 10_000_000 {
			t.Fatalf("money after abandon = %d, want full refund", player.User.Money.Money)
		}
	})

	t.Run("counter", func(t *testing.T) {
		mgr, player := testManager(), testPlayer()
		mgr.Start(player, pb.RaceType_POINTS, 1)
		if err := mgr.Abandon(player); err != nil {
			t.Fatalf("abandon: %v", err)
		}
		tc := player.User.TimersAndCounters
		if tc.RatingCount != 0 || tc.RatingTimer != 0 {
			t.Fatalf("rating slot not restored: count=%d timer=%d", tc.RatingCount, tc.RatingTimer)
		}
		if mgr.ActiveRaceCount() != 0 {
			t.Fatal("record left after abandon")
		}
	})

	t.Run("fuel cannot go negative then refund", func(t *testing.T) {
		mgr, player := testManager(), testPlayer()
		player.User.Fuel.Fuel, player.User.Fuel.Addition = 5, 0
		mgr.Start(player, pb.RaceType_RACE, 1)
		if totalFuel(player) != 0 {
			t.Fatal("fuel should be exactly 0 after start")
		}
		mgr.Abandon(player)
		if totalFuel(player) != 5 {
			t.Fatalf("fuel after refund = %d, want 5", totalFuel(player))
		}
	})
}

func TestDisconnectRestoresInProgressRace(t *testing.T) {
	mgr, player := testManager(), testPlayer()
	mgr.Start(player, pb.RaceType_RACE, 42)

	mgr.HandleDisconnect(player)
	if totalFuel(player) != 75 {
		t.Fatalf("fuel after disconnect = %d, want 75", totalFuel(player))
	}
	if mgr.HasActiveRace(player.User.Id) {
		t.Fatal("record left after disconnect")
	}

	// idempotent: a second disconnect (e.g. duplicated teardown) restores nothing
	player.User.Fuel.Fuel = 60
	mgr.HandleDisconnect(player)
	if totalFuel(player) != 60 {
		t.Fatal("second disconnect restored fuel again")
	}

	// a finish arriving after disconnect must not pay
	_, err := mgr.Finish(player, &pb.FinishParams{Type: pb.RaceType_RACE, EnemyId: 42})
	if !errors.Is(err, ErrNoActiveRace) {
		t.Fatalf("post-disconnect finish: err = %v", err)
	}
}

func TestDisconnectAfterFinishKeepsReward(t *testing.T) {
	mgr, player := testManager(), testPlayer()
	ar, _ := mgr.Start(player, pb.RaceType_RACE, 1)
	moneyBefore := player.User.Money.Money
	if _, err := mgr.Finish(player, finishParams(ar)); err != nil {
		t.Fatalf("finish: %v", err)
	}
	mgr.HandleDisconnect(player)
	if player.User.Money.Money != moneyBefore+FinishAwardMoney {
		t.Fatal("disconnect after finish must not claw back the reward")
	}
}

func TestAbandonThenDisconnectRestoresOnce(t *testing.T) {
	mgr, player := testManager(), testPlayer()
	mgr.Start(player, pb.RaceType_RACE, 1)
	if err := mgr.Abandon(player); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	// tamper: any second restore would be a bug
	player.User.Fuel.Fuel = 33
	mgr.HandleDisconnect(player)
	if totalFuel(player) != 33 {
		t.Fatal("disconnect after abandon restored a second time")
	}
}

func TestDisconnectWithoutRaceIsNoop(t *testing.T) {
	mgr, player := testManager(), testPlayer()
	mgr.HandleDisconnect(player)
	if totalFuel(player) != 75 || mgr.ActiveRaceCount() != 0 {
		t.Fatal("disconnect without active race changed state")
	}
}

// ---------- concurrency ----------

func TestConcurrentFinishesPaysExactlyOnce(t *testing.T) {
	mgr, player := testManager(), testPlayer()
	ar, _ := mgr.Start(player, pb.RaceType_RACE, 42)
	params := finishParams(ar)

	const n = 32
	var wg sync.WaitGroup
	var successes, failures int
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := mgr.Finish(player, params)
			mu.Lock()
			if err == nil {
				successes++
			} else {
				failures++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if successes != 1 || failures != n-1 {
		t.Fatalf("finishes: %d ok, %d rejected, want 1 ok %d rejected", successes, failures, n-1)
	}
	if len(player.User.Inventory.Items) != 1 {
		t.Fatalf("items = %d, want exactly 1", len(player.User.Inventory.Items))
	}
	if mgr.ActiveRaceCount() != 0 {
		t.Fatal("record left behind under concurrent finishes")
	}
}

func TestFinishRacingAbandonNeverBoth(t *testing.T) {
	// Whatever interleaving wins, exactly one outcome occurs: reward (finish)
	// or refund (abandon), never both and never neither.
	for iter := 0; iter < 50; iter++ {
		mgr, player := testManager(), testPlayer()
		ar, _ := mgr.Start(player, pb.RaceType_RACE, 42)
		var wg sync.WaitGroup
		wg.Add(2)
		var finishErr, abandonErr error
		go func() { defer wg.Done(); _, finishErr = mgr.Finish(player, finishParams(ar)) }()
		go func() { defer wg.Done(); abandonErr = mgr.Abandon(player) }()
		wg.Wait()

		finished := finishErr == nil
		abandoned := abandonErr == nil
		if finished == abandoned {
			t.Fatalf("iter %d: finish ok=%v abandon ok=%v, exactly one must succeed", iter, finished, abandoned)
		}
		if finished {
			if totalFuel(player) != 70 || player.User.Money.Money != 10_000_000+FinishAwardMoney {
				t.Fatalf("finish outcome corrupted: fuel=%d money=%d", totalFuel(player), player.User.Money.Money)
			}
		} else {
			if totalFuel(player) != 75 || len(player.User.Inventory.Items) != 0 {
				t.Fatalf("abandon outcome corrupted: fuel=%d items=%d", totalFuel(player), len(player.User.Inventory.Items))
			}
		}
		if mgr.ActiveRaceCount() != 0 {
			t.Fatal("record leaked after concurrent finish/abandon")
		}
	}
}

func TestConcurrentStartsOnlyOneWins(t *testing.T) {
	mgr, player := testManager(), testPlayer()
	const n = 32
	var wg sync.WaitGroup
	var wins int
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := mgr.Start(player, pb.RaceType_RACE, 1)
			if err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("successful starts = %d, want 1", wins)
	}
	if totalFuel(player) != 70 {
		t.Fatalf("fuel = %d, want exactly one deduction (70)", totalFuel(player))
	}
	mgr.HandleDisconnect(player)
}

func TestRapidLifecycleSequenceStaysConsistent(t *testing.T) {
	mgr, player := testManager(), testPlayer()
	for round := 0; round < 10; round++ {
		ar, err := mgr.Start(player, pb.RaceType_RACE, int64(round+1))
		if err != nil {
			t.Fatalf("round %d start: %v", round, err)
		}
		// A finished race legitimately keeps its fuel; each round deducts
		// exactly one more entry cost, never more and never negative.
		wantFuel := int32(75 - 5*(round+1))
		if totalFuel(player) != wantFuel {
			t.Fatalf("round %d: fuel = %d, want %d", round, totalFuel(player), wantFuel)
		}
		if _, err := mgr.Finish(player, finishParams(ar)); err != nil {
			t.Fatalf("round %d finish: %v", round, err)
		}
		if mgr.ActiveRaceCount() != 0 {
			t.Fatalf("round %d: record not cleared", round)
		}
	}
	if totalFuel(player) != 25 {
		t.Fatalf("final fuel = %d, want 25 (10 x 5 consumed)", totalFuel(player))
	}
	if len(player.User.Inventory.Items) != 10 {
		t.Fatalf("items = %d, want 10 (one per finished race)", len(player.User.Inventory.Items))
	}
}
