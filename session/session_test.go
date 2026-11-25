package session

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// playerData 是测试用的玩家数据替身。
type playerData struct {
	account string
	coin    int
}

// 场景 1：新连接接管在线身份。
// 登录成功后新连接成为唯一有效会话；旧连接先收到明确的下线通知，
// 再被关闭，并且不再被视为该账号的有效连接。
func TestTakeoverKicksOldSession(t *testing.T) {
	store := newFakeStore()
	mgr := NewManager(store)

	oldConn := newFakeConn("old")
	newConn := newFakeConn("new")
	const acc = "acc-1"

	// 第一次登录并完成数据加载。
	p1 := mgr.BeginLogin(acc, oldConn)
	d1 := &playerData{account: acc, coin: 100}
	s1, err := mgr.FinishLogin(p1, d1, nil)
	if err != nil || s1 == nil {
		t.Fatalf("first login should succeed, got session=%v err=%v", s1, err)
	}
	if mgr.Count() != 1 {
		t.Fatalf("expect 1 online session, got %d", mgr.Count())
	}
	if !mgr.IsCurrent(acc, oldConn) {
		t.Fatal("old conn should be current before takeover")
	}

	// 第二次登录：立即接管，旧连接被通知并关闭。
	p2 := mgr.BeginLogin(acc, newConn)
	if got := oldConn.closeCount(); got != 1 {
		t.Fatalf("old conn should be closed once, got %d", got)
	}
	kick, ok := oldConn.lastMsg().(*KickMsg)
	if !ok {
		t.Fatalf("old conn should receive *KickMsg, got %#v", oldConn.lastMsg())
	}
	if kick.Reason != KickReasonReplaced || kick.Account != acc {
		t.Fatalf("unexpected kick notice: %+v", kick)
	}
	if mgr.IsCurrent(acc, oldConn) {
		t.Fatal("old conn must no longer be current after takeover")
	}
	if !mgr.Current(acc).Loading() {
		t.Fatal("new session should be in loading state until data loads")
	}
	if mgr.Count() != 1 {
		t.Fatalf("takeover must keep exactly one session, got %d", mgr.Count())
	}

	// 新会话数据加载完成。
	d2 := &playerData{account: acc, coin: 200}
	s2, err := mgr.FinishLogin(p2, d2, nil)
	if err != nil || s2 == nil {
		t.Fatalf("second login should finish, got err=%v", err)
	}
	if !mgr.IsCurrent(acc, newConn) {
		t.Fatal("new conn should be current after finish")
	}
	if s2.Data != d2 {
		t.Fatal("new session should hold the freshly loaded data")
	}
	if s1.Gen() == s2.Gen() {
		t.Fatal("session generations must differ")
	}
}

// 场景 2：被顶替的旧连接随后关闭，不得影响新会话。
// 旧连接关闭事件到达时，管理器应直接忽略，新会话保持在线、
// 定时器不被停掉、数据不被额外保存。
func TestStaleConnCloseDoesNotTouchNewSession(t *testing.T) {
	store := newFakeStore()
	mgr := NewManager(store)

	oldConn := newFakeConn("old")
	newConn := newFakeConn("new")
	const acc = "acc-2"

	p1 := mgr.BeginLogin(acc, oldConn)
	s1, _ := mgr.FinishLogin(p1, &playerData{account: acc, coin: 1}, nil)

	var timersStopped int32
	p2 := mgr.BeginLogin(acc, newConn)
	s2, err := mgr.FinishLogin(p2, &playerData{account: acc, coin: 2}, nil)
	if err != nil {
		t.Fatalf("second login finish failed: %v", err)
	}
	_ = s1

	// 旧连接此时才报告关闭（模拟网络抖动导致旧连接迟迟不断）。
	closed := mgr.OnConnClosed(acc, oldConn, func(s *Session) {
		atomic.StoreInt32(&timersStopped, 1)
	})
	if closed != nil {
		t.Fatalf("stale conn close must be ignored, got session %+v", closed)
	}
	if atomic.LoadInt32(&timersStopped) != 0 {
		t.Fatal("stale conn close must not stop the new session timers")
	}
	if store.asyncSaveCount() != 0 {
		t.Fatalf("stale conn close must not trigger any save, got %d", store.asyncSaveCount())
	}
	if !mgr.IsCurrent(acc, newConn) {
		t.Fatal("new session must remain the current online session")
	}
	if mgr.Current(acc) != s2 {
		t.Fatal("new session object must be untouched")
	}
	if oldConn.closeCount() != 1 {
		t.Fatal("old conn had already been closed by takeover exactly once")
	}

	// 新会话自己的连接关闭应当正常下线。
	closed2 := mgr.OnConnClosed(acc, newConn, func(s *Session) {
		atomic.AddInt32(&timersStopped, 1)
	})
	if closed2 == nil {
		t.Fatal("current conn close should end the session")
	}
	if atomic.LoadInt32(&timersStopped) != 1 {
		t.Fatal("current conn close should stop timers before saving")
	}
	if mgr.Count() != 0 {
		t.Fatalf("account should be offline, remaining sessions=%d", mgr.Count())
	}
	if store.asyncSaveCount() != 1 {
		t.Fatalf("exactly one async save expected on real logout, got %d", store.asyncSaveCount())
	}
}

// 场景 3：数据加载完成前重复登录，最终只能留下一个在线会话，
// 较早的登录结果到达时必须作废，不能出现两个会话同时持有同一玩家。
func TestDuplicateLoginBeforeDataLoaded(t *testing.T) {
	store := newFakeStore()
	mgr := NewManager(store)
	const acc = "acc-3"

	connA := newFakeConn("A")
	connB := newFakeConn("B")
	connC := newFakeConn("C")

	pA := mgr.BeginLogin(acc, connA)
	store.Load(acc, func(data interface{}, err error) {
		_, _ = mgr.FinishLogin(pA, data, err)
	})
	if store.pendingLoadCount(acc) == 0 {
		t.Fatal("login A should have started a load")
	}

	// A 的数据还没回来，B 又登录：A 被顶替。
	pB := mgr.BeginLogin(acc, connB)
	store.Load(acc, func(data interface{}, err error) {
		_, _ = mgr.FinishLogin(pB, data, err)
	})
	if pA.Gen() == pB.Gen() {
		t.Fatal("each login must get a distinct generation")
	}
	if connA.closeCount() != 1 {
		t.Fatal("conn A should be closed when B takes over")
	}
	if mgr.Count() != 1 {
		t.Fatalf("only one placeholder session allowed, got %d", mgr.Count())
	}

	// C 再来一次，B 也被顶替。
	pC := mgr.BeginLogin(acc, connC)
	store.Load(acc, func(data interface{}, err error) {
		// C 的加载回调由测试主体在下方显式完成，这里挂起即可。
	})
	if connB.closeCount() != 1 || connC.closeCount() != 0 {
		t.Fatalf("unexpected close counts A=%d B=%d C=%d",
			connA.closeCount(), connB.closeCount(), connC.closeCount())
	}

	// A、B 的加载回调先后到达，都必须被判过期。
	if _, err := mgr.FinishLogin(pA, &playerData{account: acc, coin: 1}, nil); err != ErrStaleLogin {
		t.Fatalf("stale login A must return ErrStaleLogin, got %v", err)
	}
	if _, err := mgr.FinishLogin(pB, &playerData{account: acc, coin: 2}, nil); err != ErrStaleLogin {
		t.Fatalf("stale login B must return ErrStaleLogin, got %v", err)
	}
	// 过期回调不得改动当前会话。
	if !mgr.Current(acc).Loading() {
		t.Fatal("session C should still be loading after stale callbacks")
	}
	if mgr.Count() != 1 {
		t.Fatalf("still exactly one session, got %d", mgr.Count())
	}

	// C 的数据到达，登录完成。
	final := &playerData{account: acc, coin: 3}
	sC, err := mgr.FinishLogin(pC, final, nil)
	if err != nil {
		t.Fatalf("login C should finish, got %v", err)
	}
	if !mgr.IsCurrent(acc, connC) {
		t.Fatal("conn C must be the sole current connection")
	}
	if sC.Data != final {
		t.Fatal("session C should hold its own loaded data")
	}
	if mgr.IsCurrent(acc, connA) || mgr.IsCurrent(acc, connB) {
		t.Fatal("neither A nor B may remain a valid connection")
	}
}

// 场景 3 补充：加载失败时会话被清理、连接被关闭、错误上抛。
func TestLoginLoadFailureCleansUp(t *testing.T) {
	store := newFakeStore()
	mgr := NewManager(store)
	const acc = "acc-fail"
	store.failLoad[acc] = errBoom

	conn := newFakeConn("x")
	p := mgr.BeginLogin(acc, conn)
	_, err := mgr.FinishLogin(p, nil, errBoom)
	if !errors.Is(err, errBoom) {
		t.Fatalf("load error should propagate, got %v", err)
	}
	if conn.closeCount() != 1 {
		t.Fatalf("failed login should close the conn, got %d", conn.closeCount())
	}
	if mgr.Count() != 0 {
		t.Fatalf("failed login must leave no session, got %d", mgr.Count())
	}
}

// 场景 4：服务器关停时所有在线玩家同步落库，且写入失败可观察。
func TestShutdownSavesAllSynchronously(t *testing.T) {
	store := newFakeStore()
	mgr := NewManager(store)

	d1 := &playerData{account: "u1", coin: 10}
	d2 := &playerData{account: "u2", coin: 20}

	p1 := mgr.BeginLogin("u1", newFakeConn("c1"))
	if _, err := mgr.FinishLogin(p1, d1, nil); err != nil {
		t.Fatal(err)
	}
	p2 := mgr.BeginLogin("u2", newFakeConn("c2"))
	if _, err := mgr.FinishLogin(p2, d2, nil); err != nil {
		t.Fatal(err)
	}
	p3 := mgr.BeginLogin("u3", newFakeConn("c3"))
	// u3 故意停留在加载中状态：关停时没有可信快照，应被记录为错误而非覆盖存档。
	_ = p3

	store.failSync["u2"] = errBoom

	errs := mgr.Shutdown()
	if !errs.HasErrors() {
		t.Fatal("injected failure must be observable")
	}
	if _, ok := errs["u2"]; !ok {
		t.Fatalf("u2 save failure must be reported, got %v", errs)
	}
	if _, ok := errs["u3"]; !ok {
		t.Fatalf("u3 not-ready state must be reported, got %v", errs)
	}
	if _, ok := errs["u1"]; ok {
		t.Fatalf("u1 saved fine, must not appear in errors, got %v", errs)
	}
	if store.syncSaveCount() != 2 {
		t.Fatalf("only ready players are sync-saved (u1,u2), got %d", store.syncSaveCount())
	}
	if store.data["u1"] != d1 {
		t.Fatal("u1 data should have been persisted synchronously")
	}
	if mgr.Count() != 0 {
		t.Fatal("no session remains online after shutdown")
	}
	if !mgr.IsShutdown() {
		t.Fatal("manager should be marked shutdown")
	}
}

// 下线顺序：先停定时器，后保存数据。
// 用回调记录顺序，确保 OnConnClosed 的 stopTimers 严格先于 Store.Save。
func TestLogoutStopsTimersBeforeSave(t *testing.T) {
	store := newFakeStore()
	mgr := NewManager(store)
	release := make(chan struct{})
	store.saveBlock = release

	const acc = "acc-order"
	conn := newFakeConn("c")
	p := mgr.BeginLogin(acc, conn)
	data := &playerData{account: acc}
	if _, err := mgr.FinishLogin(p, data, nil); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var order []string
	var mu sync.Mutex

	go func() {
		mgr.OnConnClosed(acc, conn, func(s *Session) {
			mu.Lock()
			order = append(order, "stop-timers")
			mu.Unlock()
		})
		close(done)
	}()

	// Save 被阻塞，给 goroutine 时间证明“先停定时器”已经发生。
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if len(order) != 1 || order[0] != "stop-timers" {
		mu.Unlock()
		t.Fatalf("timers must be stopped before save, order=%v", order)
	}
	mu.Unlock()

	close(release)
	<-done
	if store.asyncSaveCount() != 1 {
		t.Fatalf("exactly one save expected, got %d", store.asyncSaveCount())
	}
	if store.saveCalls[0].data != data {
		t.Fatal("saved data must be the session player data")
	}
}

// 加载未完成时断线：不能用半份数据覆盖存档。
func TestCloseWhileLoadingSkipsSave(t *testing.T) {
	store := newFakeStore()
	mgr := NewManager(store)
	const acc = "acc-loading"
	conn := newFakeConn("c")
	p := mgr.BeginLogin(acc, conn)

	closed := mgr.OnConnClosed(acc, conn, nil)
	if closed == nil {
		t.Fatal("loading session should still be removed on close")
	}
	if store.asyncSaveCount() != 0 {
		t.Fatalf("no save allowed before data is loaded, got %d", store.asyncSaveCount())
	}
	if _, err := mgr.FinishLogin(p, &playerData{account: acc}, nil); err != ErrStaleLogin {
		t.Fatalf("late load callback after disconnect must be stale, got %v", err)
	}
}

// 并发压力：同一账号大量登录与关闭交错进行，任一时刻在线会话数不超过 1，
// 且最终状态自洽。
func TestConcurrentLoginsSingleSessionInvariant(t *testing.T) {
	store := newFakeStore()
	mgr := NewManager(store)
	const acc = "acc-race"
	const n = 50

	var wg sync.WaitGroup
	conns := make([]*fakeConn, n)
	pendings := make([]*PendingLogin, n)
	for i := 0; i < n; i++ {
		conns[i] = newFakeConn("conn")
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pendings[i] = mgr.BeginLogin(acc, conns[i])
		}(i)
	}
	wg.Wait()

	if mgr.Count() != 1 {
		t.Fatalf("invariant violated: %d sessions for one account", mgr.Count())
	}

	// 所有加载回调到达：只有最后一次登录能成功，其余全部过期。
	var success int
	for i, p := range pendings {
		_, err := mgr.FinishLogin(p, &playerData{account: acc, coin: i}, nil)
		if err == nil {
			success++
		} else if err != ErrStaleLogin {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("exactly one login may survive, got %d", success)
	}
	if mgr.Count() != 1 {
		t.Fatalf("one session remains, got %d", mgr.Count())
	}

	// 除最终连接外，其余连接都应被关闭过。
	finalConn := mgr.Current(acc).Conn
	var closedOthers int
	for _, c := range conns {
		if c == finalConn {
			if c.closeCount() != 0 {
				t.Fatal("final conn must not have been closed")
			}
			continue
		}
		if c.closeCount() != 1 {
			t.Fatalf("superseded conn closed exactly once, got %d", c.closeCount())
		}
		closedOthers++
	}
	if closedOthers != n-1 {
		t.Fatalf("expected %d superseded conns, got %d", n-1, closedOthers)
	}

	// 每条被顶替连接都应先收到下线通知。
	for _, c := range conns {
		if c == finalConn {
			continue
		}
		if c.msgCount() == 0 {
			t.Fatal("every superseded conn must receive a kick notice")
		}
		if _, ok := c.lastMsg().(*KickMsg); !ok {
			t.Fatalf("last message must be *KickMsg, got %#v", c.lastMsg())
		}
	}
}

// 模拟真实时序：假连接的 Close 回调里直接回灌 OnConnClosed，
// 验证“顶替时关闭旧连接 -> 关闭事件到达”这一链路不会误删新会话。
func TestCloseCallbackDuringTakeover(t *testing.T) {
	store := newFakeStore()
	mgr := NewManager(store)
	const acc = "acc-cb"

	oldConn := newFakeConn("old")
	newConn := newFakeConn("new")

	p1 := mgr.BeginLogin(acc, oldConn)
	if _, err := mgr.FinishLogin(p1, &playerData{account: acc, coin: 1}, nil); err != nil {
		t.Fatal(err)
	}

	// 旧连接一旦被 Manager 关闭，立刻产生关闭事件回灌。
	oldConn.onClose = func() {
		if s := mgr.OnConnClosed(acc, oldConn, nil); s != nil {
			t.Error("close event of a replaced conn must not end any session")
		}
	}

	p2 := mgr.BeginLogin(acc, newConn)
	if _, err := mgr.FinishLogin(p2, &playerData{account: acc, coin: 2}, nil); err != nil {
		t.Fatal(err)
	}

	if !mgr.IsCurrent(acc, newConn) {
		t.Fatal("new session must survive the old conn's close callback")
	}
	if mgr.Count() != 1 || store.asyncSaveCount() != 0 {
		t.Fatalf("unexpected state count=%d saves=%d", mgr.Count(), store.asyncSaveCount())
	}
}

// 关停幂等：重复 Shutdown 不会重复保存，也不会 panic。
func TestShutdownIdempotent(t *testing.T) {
	store := newFakeStore()
	mgr := NewManager(store)
	p := mgr.BeginLogin("u", newFakeConn("c"))
	if _, err := mgr.FinishLogin(p, &playerData{account: "u"}, nil); err != nil {
		t.Fatal(err)
	}

	errs1 := mgr.Shutdown()
	if errs1.HasErrors() {
		t.Fatalf("first shutdown should be clean, got %v", errs1)
	}
	errs2 := mgr.Shutdown()
	if errs2.HasErrors() {
		t.Fatalf("second shutdown should be a no-op, got %v", errs2)
	}
	if store.syncSaveCount() != 1 {
		t.Fatalf("data saved exactly once, got %d", store.syncSaveCount())
	}
}
