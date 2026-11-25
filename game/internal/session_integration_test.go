package internal

import (
	"errors"
	"net"
	"sync"
	"testing"

	"leaf_server/base"
	"leaf_server/msg"
	"leaf_server/session"

	"github.com/name5566/leaf/gate"
)

var errInjected = errors.New("injected storage failure")

// fakeAgent 实现 gate.Agent，记录下发消息与关闭次数，供集成测试使用。
type fakeAgent struct {
	mu       sync.Mutex
	userData interface{}
	msgs     []interface{}
	closed   int
}

func (a *fakeAgent) WriteMsg(m interface{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.msgs = append(a.msgs, m)
}

func (a *fakeAgent) LocalAddr() net.Addr  { return nil }
func (a *fakeAgent) RemoteAddr() net.Addr { return nil }
func (a *fakeAgent) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed++
}
func (a *fakeAgent) Destroy()                  {}
func (a *fakeAgent) UserData() interface{}     { return a.userData }
func (a *fakeAgent) SetUserData(d interface{}) { a.userData = d }

func (a *fakeAgent) msgCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.msgs)
}

func (a *fakeAgent) lastMsg() interface{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.msgs) == 0 {
		return nil
	}
	return a.msgs[len(a.msgs)-1]
}

func (a *fakeAgent) closeCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed
}

var _ gate.Agent = (*fakeAgent)(nil)

// gameStore 是集成测试用的假存储：加载时返回带 Account 的已注册玩家，
// 记录保存行为，可注入同步保存失败。
// 同时实现 playerWriter，接管 Player.Save/SaveSync，避免触碰真实 mongodb。
type gameStore struct {
	mu          sync.Mutex
	asyncSaves  []string
	writes      []string // Player.Write（含新玩家首次落库）
	writeSyncs  []string // Player.WriteSync
	syncFail    map[string]error
	loadedCount int
	// newPlayer 为 true 时 Load 返回零值玩家（模拟库中不存在的新玩家）。
	newPlayer bool
}

func newGameStore() *gameStore {
	return &gameStore{syncFail: make(map[string]error)}
}

func (st *gameStore) Load(account string, cb func(interface{}, error)) {
	st.mu.Lock()
	st.loadedCount++
	fresh := st.newPlayer
	st.mu.Unlock()
	p := CreatePlayer()
	if !fresh {
		// 模拟已注册玩家：Account 非空，登录路径不会触发注册分支，
		// 从而避开对 mgodb / uidbuilder 的依赖。
		p.Account = "acct-" + account
	}
	cb(p, nil)
}

func (st *gameStore) Save(account string, data interface{}) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.asyncSaves = append(st.asyncSaves, account)
}

func (st *gameStore) SaveSync(account string, data interface{}) error {
	// 与生产 dbStore 保持同一调用链：Store.SaveSync -> Player.SaveSync -> WriteSync。
	return st.WriteSync(data.(*Player))
}

// Write / WriteSync 实现 playerWriter。
func (st *gameStore) Write(p *Player) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.writes = append(st.writes, p.objid)
}

func (st *gameStore) WriteSync(p *Player) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.writeSyncs = append(st.writeSyncs, p.objid)
	if err, ok := st.syncFail[p.objid]; ok {
		return err
	}
	return nil
}

func (st *gameStore) asyncCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.asyncSaves)
}

func (st *gameStore) writeCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.writes)
}

func (st *gameStore) writeSyncCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.writeSyncs)
}

var (
	_ session.Store = (*gameStore)(nil)
	_ playerWriter  = (*gameStore)(nil)
)

// resetEnv 为每个测试重建干净的会话/玩家/定时器管理器与假存储，
// 避免包级单例在用例间串状态。
func resetEnv(t *testing.T) (*gameStore, *session.Manager) {
	t.Helper()
	store := newGameStore()
	mgr := session.NewManager(store)
	SessionMgr = mgr
	SessionStore = store
	playerStore = store // 接管 Player 的保存动作
	PlayerManager = NewPlayerManager()
	TimerManager = NewTimerManager()
	// 用进程内自增计数器替换依赖 mongodb 的 UID 生成器。
	var testUID int64
	uidGenerator = func() int64 {
		testUID += 10000
		return testUID
	}
	return store, mgr
}

func newInfo(objID string) *base.AccountInfo {
	return &base.AccountInfo{Account: "user-" + objID, Password: "pw", ObjID: objID}
}

// 完整登录：rpcLoginSuccess 走加载回调后，玩家进入在线表，
// 连接收到登录成功应答。
func TestRPCLoginSuccessOnline(t *testing.T) {
	_, mgr := resetEnv(t)
	const objID = "obj-login"
	a := &fakeAgent{userData: newInfo(objID)}

	rpcLoginSuccess([]interface{}{a})

	if mgr.Count() != 1 {
		t.Fatalf("expect 1 online session, got %d", mgr.Count())
	}
	player := PlayerManager.Get(objID)
	if player == nil || !player.IsActive() {
		t.Fatal("player should be online and active")
	}
	if !mgr.IsCurrent(objID, &agentConn{agent: a}) {
		t.Fatal("agent should be the current connection")
	}
	ret, ok := a.lastMsg().(*msg.RetMsg)
	if !ok || ret.Cmd != "login" || ret.Code != 0 {
		t.Fatalf("expect login success RetMsg, got %#v", a.lastMsg())
	}
}

// 接管：第二次登录顶替旧连接。旧连接先收到 KickNotify 再被关闭；
// 旧玩家内存中的最新进度必须先落库；旧连接的 CloseAgent 事件不得影响新会话。
func TestRPCLoginTakeoverAndStaleClose(t *testing.T) {
	store, mgr := resetEnv(t)
	const objID = "obj-takeover"
	oldAgent := &fakeAgent{userData: newInfo(objID)}
	newAgent := &fakeAgent{userData: newInfo(objID)}

	rpcLoginSuccess([]interface{}{oldAgent})
	oldPlayer := PlayerManager.Get(objID)
	TimerManager.AddTimer(oldPlayer, "loop-timer", 60000, true, func() {})
	// 模拟旧会话运行期间产生、尚未落库的进度变更。
	oldPlayer.SetIntAttr(INTATTR_COIN, 777)
	writesBefore := store.writeCount()

	rpcLoginSuccess([]interface{}{newAgent})

	// 旧玩家的定时器必须在接管时被立即清理。
	if _, ok := TimerManager.timerMap[oldPlayer]; ok {
		t.Fatal("old player timers must be removed on takeover")
	}
	if oldPlayer.IsActive() {
		t.Fatal("old player must be deactivated on takeover")
	}
	// 旧玩家最新进度必须落库一次，避免新会话加载到旧存档。
	if store.writeCount() != writesBefore+1 {
		t.Fatalf("old player progress must be saved on takeover, writes before=%d after=%d",
			writesBefore, store.writeCount())
	}

	// 旧连接收到明确的下线通知并被关闭。
	if oldAgent.closeCount() != 1 {
		t.Fatalf("old agent closed once, got %d", oldAgent.closeCount())
	}
	kick, ok := oldAgent.lastMsg().(*msg.KickNotify)
	if !ok || kick.Reason != session.KickReasonReplaced {
		t.Fatalf("old agent should receive KickNotify, got %#v", oldAgent.lastMsg())
	}
	if mgr.Count() != 1 {
		t.Fatalf("only one session, got %d", mgr.Count())
	}
	if mgr.IsCurrent(objID, &agentConn{agent: oldAgent}) {
		t.Fatal("old agent must no longer be current")
	}
	if !mgr.IsCurrent(objID, &agentConn{agent: newAgent}) {
		t.Fatal("new agent must be current")
	}

	// 旧连接随后关闭：必须被忽略，新会话完好。
	rpcCloseAgent([]interface{}{oldAgent})
	if mgr.Count() != 1 {
		t.Fatalf("stale close must not remove the new session, count=%d", mgr.Count())
	}
	if PlayerManager.Get(objID) == nil {
		t.Fatal("new session player must remain in player map")
	}

	// 新连接关闭：正常下线，玩家移出在线表。
	rpcCloseAgent([]interface{}{newAgent})
	if mgr.Count() != 0 {
		t.Fatalf("account should be offline, count=%d", mgr.Count())
	}
	if PlayerManager.Get(objID) != nil {
		t.Fatal("player should be removed from player map on logout")
	}
}

// 业务消息入口：被顶替的旧连接上的迟到消息必须被拒绝处理。
func TestBusinessMessageRejectedFromStaleAgent(t *testing.T) {
	_, mgr := resetEnv(t)
	const objID = "obj-msg"
	oldAgent := &fakeAgent{userData: newInfo(objID)}
	newAgent := &fakeAgent{userData: newInfo(objID)}

	rpcLoginSuccess([]interface{}{oldAgent})
	rpcLoginSuccess([]interface{}{newAgent})

	if mgr.IsCurrent(objID, &agentConn{agent: oldAgent}) {
		t.Fatal("old agent must be rejected as non-current")
	}
	if !mgr.IsCurrent(objID, &agentConn{agent: newAgent}) {
		t.Fatal("new agent must be current")
	}
}

// 下线顺序与落库：当前连接关闭时先停定时器，再异步保存数据。
func TestRPCCloseStopsTimersAndSaves(t *testing.T) {
	store, _ := resetEnv(t)
	const objID = "obj-save"
	a := &fakeAgent{userData: newInfo(objID)}
	rpcLoginSuccess([]interface{}{a})

	player := PlayerManager.Get(objID)
	TimerManager.AddTimer(player, "t1", 60000, false, func() {})
	if len(TimerManager.timerMap[player]) != 1 {
		t.Fatal("timer should be registered")
	}

	rpcCloseAgent([]interface{}{a})

	if _, ok := TimerManager.timerMap[player]; ok {
		t.Fatal("all timers must be removed on logout")
	}
	if store.asyncCount() != 1 {
		t.Fatalf("exactly one async save on logout, got %d", store.asyncCount())
	}
	if player.IsActive() {
		t.Fatal("player must be deactivated after logout")
	}
}

// 未登录连接的关闭事件应当直接忽略，不产生任何保存。
func TestRPCCloseWithoutLoginIgnored(t *testing.T) {
	store, mgr := resetEnv(t)
	a := &fakeAgent{} // UserData == nil

	rpcCloseAgent([]interface{}{a})
	if mgr.Count() != 0 || store.asyncCount() != 0 {
		t.Fatalf("anonymous close must be a no-op, count=%d saves=%d",
			mgr.Count(), store.asyncCount())
	}
}

// 关停：所有在线玩家走同步保存，失败通过返回的 SaveErrors 可观察。
func TestRPCShutdownSavesAndReportsErrors(t *testing.T) {
	store, mgr := resetEnv(t)
	a1 := &fakeAgent{userData: newInfo("o1")}
	a2 := &fakeAgent{userData: newInfo("o2")}
	rpcLoginSuccess([]interface{}{a1})
	rpcLoginSuccess([]interface{}{a2})
	store.syncFail["o2"] = errInjected

	errs := mgr.Shutdown()
	if !errs.HasErrors() {
		t.Fatal("injected sync save failure must be observable")
	}
	if _, ok := errs["o2"]; !ok {
		t.Fatalf("o2 failure expected in %v", errs)
	}
	// Shutdown 走 session.Store.SaveSync -> Player.SaveSync -> WriteSync。
	if store.writeSyncCount() != 2 {
		t.Fatalf("both players sync-saved once on shutdown, got %d", store.writeSyncCount())
	}
	if mgr.Count() != 0 {
		t.Fatalf("all sessions gone after shutdown, got %d", mgr.Count())
	}
}

// 业务消息入口 onClientMsg：当前有效连接的消息会反射调用到 Player 方法，
// 被顶替旧连接的迟到消息必须在调用前被拦截，绝不改动玩家数据。
func TestOnClientMsgRejectsStaleAndAcceptsCurrent(t *testing.T) {
	_, mgr := resetEnv(t)
	const objID = "obj-entry"
	oldAgent := &fakeAgent{userData: newInfo(objID)}
	newAgent := &fakeAgent{userData: newInfo(objID)}

	rpcLoginSuccess([]interface{}{oldAgent})
	rpcLoginSuccess([]interface{}{newAgent})
	newPlayer := PlayerManager.Get(objID)

	gameMsg := func() *msg.GameMsg {
		// 真实链路上 Req 经 JSON 解码后是 map[string]interface{}，
		// mapstructure 再把它解码为方法入参结构。
		return &msg.GameMsg{Cmd: "Test", Req: map[string]interface{}{}}
	}

	// 旧连接的迟到业务消息：必须被拒绝，Player.Test 不得被调用。
	onClientMsg([]interface{}{gameMsg(), oldAgent})
	if newPlayer.testCallCount() != 0 {
		t.Fatalf("stale agent message must not reach player, calls=%d", newPlayer.testCallCount())
	}

	// 当前连接的消息：正常反射调用 Player.Test。
	onClientMsg([]interface{}{gameMsg(), newAgent})
	if newPlayer.testCallCount() != 1 {
		t.Fatalf("current agent message should reach player once, calls=%d", newPlayer.testCallCount())
	}

	// 当前连接下线后，其迟到消息同样被拒绝。
	rpcCloseAgent([]interface{}{newAgent})
	onClientMsg([]interface{}{gameMsg(), newAgent})
	if newPlayer.testCallCount() != 1 {
		t.Fatalf("message after logout must not reach player, calls=%d", newPlayer.testCallCount())
	}

	// 未登录连接（无 UserData）直接被拒绝且不 panic。
	anonymous := &fakeAgent{}
	onClientMsg([]interface{}{gameMsg(), anonymous})
	if mgr.Count() != 0 {
		t.Fatal("anonymous message must not create any session")
	}
}

// agentConn 必须正确实现 session.Conn，并把 KickMsg 翻译成协议层 KickNotify。
func TestAgentConnKickTranslation(t *testing.T) {
	a := &fakeAgent{}
	c := &agentConn{agent: a}

	c.WriteMsg(&session.KickMsg{Account: "x", Reason: session.KickReasonReplaced})
	kn, ok := a.lastMsg().(*msg.KickNotify)
	if !ok || kn.Reason != session.KickReasonReplaced {
		t.Fatalf("KickMsg should translate to KickNotify, got %#v", a.lastMsg())
	}

	other := &msg.RetMsg{Code: 7, Cmd: "ping"}
	c.WriteMsg(other)
	if a.lastMsg() != other {
		t.Fatal("non-kick messages must pass through unchanged")
	}

	c.Close()
	if a.closeCount() != 1 {
		t.Fatal("Close should delegate to agent")
	}
}

// 新玩家首次登录：加载返回零值玩家后，应触发一次注册落库（Write）。
func TestRPCNewPlayerFirstSave(t *testing.T) {
	store, mgr := resetEnv(t)
	store.newPlayer = true
	const objID = "obj-new"
	a := &fakeAgent{userData: newInfo(objID)}

	rpcLoginSuccess([]interface{}{a})

	if mgr.Count() != 1 {
		t.Fatalf("new player should be online, count=%d", mgr.Count())
	}
	player := PlayerManager.Get(objID)
	if player == nil || player.Account == "" || player.UID == 0 {
		t.Fatal("new player should be initialized with account and uid")
	}
	if store.writeCount() != 1 {
		t.Fatalf("new player must be persisted once on register, got %d", store.writeCount())
	}
}

// 加载未完成时再次登录的端到端时序：
// 第一次登录的加载回调被人为挂起，第二次登录抢占后，
// 第一次的迟到回调到达时必须作废，最终只有第二个会话在线。
func TestRPCDuplicateLoginWhileLoading(t *testing.T) {
	store, mgr := resetEnv(t)
	const objID = "obj-dup"

	// 用可控存储替换：所有 Load 挂起，由测试手动逐个放行。
	ctrl := newControllableStore(store)
	SessionStore = ctrl

	a1 := &fakeAgent{userData: newInfo(objID)}
	a2 := &fakeAgent{userData: newInfo(objID)}

	rpcLoginSuccess([]interface{}{a1}) // 第一次加载挂起
	if mgr.Current(objID) == nil || !mgr.Current(objID).Loading() {
		t.Fatal("first session should be waiting in loading state")
	}

	rpcLoginSuccess([]interface{}{a2}) // 第二次登录抢占，第二次加载也挂起
	if a1.closeCount() != 1 {
		t.Fatalf("first agent should be kicked and closed, got %d", a1.closeCount())
	}
	if _, ok := a1.lastMsg().(*msg.KickNotify); !ok {
		t.Fatalf("first agent should receive kick notify, got %#v", a1.lastMsg())
	}
	if mgr.Count() != 1 {
		t.Fatalf("only one placeholder session, got %d", mgr.Count())
	}

	// 放行第一次加载：其回调到达时登录已过期，不得建立在线玩家。
	ctrl.resolveFirst(t)
	if PlayerManager.Get(objID) != nil {
		t.Fatal("stale login must not add a player")
	}
	if mgr.Count() != 1 || !mgr.Current(objID).Loading() {
		t.Fatalf("second session should remain the sole loading session, count=%d", mgr.Count())
	}

	// 放行第二次加载，玩家上线。
	ctrl.resolveSecond(t)
	player := PlayerManager.Get(objID)
	if player == nil || !player.IsActive() {
		t.Fatal("second login should bring the player online")
	}
	if !mgr.IsCurrent(objID, &agentConn{agent: a2}) {
		t.Fatal("second agent must be the current connection")
	}
	if mgr.IsCurrent(objID, &agentConn{agent: a1}) {
		t.Fatal("first agent must never be current again")
	}
}

// controllableStore 把 Load 回调挂起，按到达顺序手动放行，
// 用于精确复现“数据加载完成前重复登录”的时间窗口。
type controllableStore struct {
	// inner 承接 Save/SaveSync 与 playerWriter 的记录职责。
	inner *gameStore

	mu      sync.Mutex
	pending []func()
	order   int
}

func newControllableStore(inner *gameStore) *controllableStore {
	return &controllableStore{inner: inner}
}

func (s *controllableStore) Load(account string, cb func(interface{}, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order++
	n := s.order
	s.pending = append(s.pending, func() {
		p := CreatePlayer()
		p.Account = "acct-" + account
		_ = n
		cb(p, nil)
	})
}

func (s *controllableStore) resolveFirst(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		t.Fatal("no pending load to resolve")
	}
	fn := s.pending[0]
	s.mu.Unlock()
	fn()
}

func (s *controllableStore) resolveSecond(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	if len(s.pending) < 2 {
		s.mu.Unlock()
		t.Fatal("second pending load missing")
	}
	fn := s.pending[1]
	s.mu.Unlock()
	fn()
}

func (s *controllableStore) Save(account string, data interface{}) { s.inner.Save(account, data) }
func (s *controllableStore) SaveSync(account string, data interface{}) error {
	return s.inner.SaveSync(account, data)
}
func (s *controllableStore) Write(p *Player)           { s.inner.Write(p) }
func (s *controllableStore) WriteSync(p *Player) error { return s.inner.WriteSync(p) }

var (
	_ session.Store = (*controllableStore)(nil)
	_ playerWriter  = (*controllableStore)(nil)
)
