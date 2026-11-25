// Package session 保证每个账号在任一时刻至多只有一个有效在线会话，
// 并负责会话切换、连接关闭与服务器关停时的数据保存编排。
//
// 本包只依赖标准库，不绑定任何网络框架或数据库驱动：
// 连接由 Conn 抽象、存储由 Store 抽象，测试时可以用本地可控的假实现
// 复现登录接管、旧连接关闭、重复登录与关停保存等全部时序。
//
// 所有 Manager 方法都是 goroutine 安全的：内部用一把互斥锁保护状态，
// 连接 IO 与存储 IO 一律放在锁外执行，避免回调重入造成死锁。
package session

import (
	"errors"
	"fmt"
	"sync"
)

// 下线原因，随 Kick 通知发给被顶替的旧连接。
const (
	// KickReasonReplaced 该连接已被同账号的新登录顶替。
	// 无论旧会话是否已完成数据加载，顶替通知统一使用该原因。
	KickReasonReplaced = "replaced"
)

// KickMsg 是发给被顶替旧连接的下线通知。
type KickMsg struct {
	// Account 被下线的账号。
	Account string
	// Reason 取值为 KickReasonReplaced。
	Reason string
}

// ErrStaleLogin 登录请求在数据加载完成前被同账号的更新请求顶替，
// Load 回调到达时本次登录已不再有效。
var ErrStaleLogin = errors.New("session: superseded by a newer login before data loaded")

// Conn 是一条客户端连接在会话管理中需要满足的行为。
// 生产环境由 gate.Agent 适配，测试环境用假连接实现。
type Conn interface {
	// WriteMsg 向客户端下发一条消息（下线通知等）。
	WriteMsg(msg interface{})
	// Close 关闭底层连接。实现必须可重复调用且不 panic，
	// 且不得在调用栈内同步回调 Manager 的任何方法。
	Close()
}

// EquatableConn 由那些“同一底层连接可能被多次包装”的 Conn 实现。
// Manager 在判断两条连接是否指向同一底层连接时，会优先使用 Equal，
// 从而不依赖包装体自身的指针相等性。
type EquatableConn interface {
	Conn
	Equal(other Conn) bool
}

// connEqual 判断两条连接是否代表同一条底层连接。
func connEqual(a, b Conn) bool {
	if a == b {
		return true
	}
	if eq, ok := a.(EquatableConn); ok {
		return eq.Equal(b)
	}
	if eq, ok := b.(EquatableConn); ok {
		return eq.Equal(a)
	}
	return false
}

// Session 是一次在线会话的运行时句柄，由 Manager 持有。
type Session struct {
	// Account 账号唯一标识。
	Account string
	// Conn 本次会话绑定的连接。
	Conn Conn
	// Data 业务侧挂载的玩家数据（如 *Player），保存时原样传给 Store。
	Data interface{}

	// gen 会话代号，每次登录递增；Manager 靠它判断一次异步加载是否已过期。
	gen uint64
	// loading 数据是否仍在加载中。加载期间重复登录会走“抢占未完成登录”分支。
	loading bool
	// closed 会话是否已经结束（连接关闭或被顶替清理），保证清理幂等。
	closed bool
}

// Gen 返回会话代号（主要供测试与日志使用）。
func (s *Session) Gen() uint64 { return s.gen }

// Loading 报告该会话的数据是否仍在加载中。
func (s *Session) Loading() bool { return s.loading }

// Store 抽象玩家数据的加载与保存。
// 实现可以是异步的：Load 在数据就绪后调用回调，把结果交回 Manager。
type Store interface {
	// Load 加载账号 account 的玩家数据。
	// 数据不存在时应返回一个零值数据和 nil error（视为新玩家）。
	// 完成后必须调用 cb(data, err)，cb 需能在任意 goroutine 执行。
	Load(account string, cb func(data interface{}, err error))
	// Save 异步保存数据（例如断线退出时落库）。
	Save(account string, data interface{})
	// SaveSync 同步保存数据（服务器关停时使用），失败必须返回非 nil error，
	// 由 Manager 汇总后上抛，不得静默丢弃。
	SaveSync(account string, data interface{}) error
}

// Manager 管理全部在线会话，保证同一账号任一时刻至多一个有效会话。
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	gen      uint64
	store    Store

	// shutdown 为 true 后不再接受新登录。
	shutdown bool
}

// NewManager 创建会话管理器。
func NewManager(store Store) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		store:    store,
	}
}

// PendingLogin 由 BeginLogin 返回，记录一次进行中的登录。
type PendingLogin struct {
	account string
	gen     uint64
}

// Account 返回本次登录请求的账号。
func (p *PendingLogin) Account() string { return p.account }

// Gen 返回本次登录请求的代号。
func (p *PendingLogin) Gen() uint64 { return p.gen }

// BeginLogin 开始一次登录。
//
// 行为：
//  1. 若该账号已有会话（无论数据是否加载完成），旧会话立即被标记顶替、
//     收到 KickMsg 下线通知并关闭，且不再处理该玩家的业务消息；
//  2. 创建一个 loading 状态的新会话占位，保证加载完成前重复登录也只能
//     留下一个会话；
//  3. 返回 PendingLogin，调用方据此发起异步数据加载，完成后调用 FinishLogin。
//
// 连接的通知与关闭在锁外执行。
func (m *Manager) BeginLogin(account string, conn Conn) *PendingLogin {
	m.mu.Lock()
	var old *Session
	if old = m.sessions[account]; old != nil {
		old.closed = true
		delete(m.sessions, account)
	}

	m.gen++
	s := &Session{
		Account: account,
		Conn:    conn,
		gen:     m.gen,
		loading: true,
	}
	m.sessions[account] = s
	p := &PendingLogin{account: account, gen: s.gen}
	m.mu.Unlock()

	if old != nil {
		old.Conn.WriteMsg(&KickMsg{Account: account, Reason: KickReasonReplaced})
		old.Conn.Close()
	}
	return p
}

// FinishLogin 在数据加载回调中调用，把加载到的数据绑定到会话。
//
// 若加载期间同账号又发起了更新的登录（本次已被顶替），返回 ErrStaleLogin，
// 调用方不得再把数据挂到任何在线会话上；此时 data 由调用方自行处置
// （通常无需保存，因为新的登录会重新加载一份最新数据）。
// 加载本身失败时 err 非 nil：若该会话仍有效则清理并关闭连接，错误原样返回。
func (m *Manager) FinishLogin(p *PendingLogin, data interface{}, err error) (*Session, error) {
	m.mu.Lock()
	s := m.sessions[p.account]
	current := s != nil && s.gen == p.gen

	if err != nil {
		var dead *Session
		if current {
			dead = s
			s.closed = true
			delete(m.sessions, p.account)
		}
		m.mu.Unlock()
		if dead != nil {
			dead.Conn.Close()
		}
		return nil, err
	}

	if !current {
		// 加载完成前已被同账号的新登录顶替。
		m.mu.Unlock()
		return nil, ErrStaleLogin
	}

	s.Data = data
	s.loading = false
	m.mu.Unlock()
	return s, nil
}

// OnConnClosed 处理一条连接关闭事件（玩家主动退出或网络断线）。
//
// 只有该连接仍是账号当前有效会话时才执行“停定时器 -> 保存数据 -> 移除会话”；
// 若连接属于已被顶替的旧会话，则什么都不做，绝不能影响新会话的
// 在线状态、定时器与数据。返回被结束的会话；未匹配到时返回 nil。
//
// stopTimers 在保存数据之前调用，用于停掉该玩家的全部定时器。
func (m *Manager) OnConnClosed(account string, conn Conn, stopTimers func(*Session)) *Session {
	m.mu.Lock()
	s := m.sessions[account]
	if s == nil || !connEqual(s.Conn, conn) {
		m.mu.Unlock()
		return nil
	}
	s.closed = true
	delete(m.sessions, account)
	data := s.Data
	loading := s.loading
	m.mu.Unlock()

	// 先停定时器，再保存数据，保证保存期间不会再有定时器改动玩家数据。
	if stopTimers != nil {
		stopTimers(s)
	}
	// 加载尚未完成就断线：没有可信数据可存，避免用半份数据覆盖库里的存档。
	if !loading && data != nil {
		m.store.Save(account, data)
	}
	return s
}

// Current 返回账号当前有效会话，不存在时返回 nil。
func (m *Manager) Current(account string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[account]
}

// IsCurrent 报告 conn 是否仍是账号 account 的当前有效连接。
// 业务消息入口用它拦截已下线/被顶替连接上的迟到消息。
func (m *Manager) IsCurrent(account string, conn Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[account]
	return s != nil && connEqual(s.Conn, conn) && !s.closed
}

// Count 返回当前在线会话数。
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Shutdown 服务器关停：标记关停状态，并对所有仍在线玩家同步落库。
//
// 每个玩家的保存错误都会被收集到返回的 SaveErrors 中（按账号索引），
// 不会静默丢弃；全部保存尝试都会执行，单个玩家失败不影响其他玩家。
func (m *Manager) Shutdown() SaveErrors {
	m.mu.Lock()
	m.shutdown = true
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	// 清空在线表：关停后不再存在有效会话。
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	errs := make(SaveErrors)
	for _, s := range sessions {
		// 数据仍在加载中的会话没有可信快照，跳过并记录，避免覆盖存档。
		if s.loading || s.Data == nil {
			errs[s.Account] = fmt.Errorf("session: account %s data not ready at shutdown, skipped", s.Account)
			continue
		}
		if err := m.store.SaveSync(s.Account, s.Data); err != nil {
			errs[s.Account] = err
		}
	}
	return errs
}

// IsShutdown 报告管理器是否已关停。
func (m *Manager) IsShutdown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.shutdown
}

// SaveErrors 按账号汇总关停时的保存错误。
type SaveErrors map[string]error

// Error 实现 error 接口；无错误时返回空字符串。
func (e SaveErrors) Error() string {
	if len(e) == 0 {
		return ""
	}
	msg := "session: shutdown save failed for accounts:"
	for account, err := range e {
		msg += fmt.Sprintf(" [%s: %v]", account, err)
	}
	return msg
}

// HasErrors 报告是否存在保存失败。
func (e SaveErrors) HasErrors() bool { return len(e) > 0 }
