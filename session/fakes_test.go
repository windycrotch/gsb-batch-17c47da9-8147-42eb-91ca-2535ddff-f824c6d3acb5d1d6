package session

import (
	"errors"
	"sync"
)

// fakeConn 是本地可控的假连接，记录收到的消息与关闭动作，
// 并允许人为控制 Close 的行为（可重入、可注入延迟回调）。
type fakeConn struct {
	mu       sync.Mutex
	name     string
	msgs     []interface{}
	closed   int
	closeErr error

	// onClose 非空时在 Close 中调用，用于模拟“关闭事件回灌 Manager”的时序。
	onClose func()
}

func newFakeConn(name string) *fakeConn {
	return &fakeConn{name: name}
}

func (c *fakeConn) WriteMsg(m interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
}

func (c *fakeConn) Close() {
	c.mu.Lock()
	c.closed++
	cb := c.onClose
	c.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func (c *fakeConn) msgCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

func (c *fakeConn) lastMsg() interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.msgs) == 0 {
		return nil
	}
	return c.msgs[len(c.msgs)-1]
}

func (c *fakeConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// fakeStore 是本地可控的假存储：
//   - Load 不立即回调，把待完成的加载挂起，测试可精确控制“数据加载完成前”的窗口；
//   - 记录每次异步 Save 与同步 SaveSync 的入参；
//   - 可按账号注入加载/保存错误。
type fakeStore struct {
	mu sync.Mutex

	// data 是各账号的“库内”存档。
	data map[string]interface{}

	loadCalls    []string
	pendingLoads map[string][]func(interface{}, error)

	saveCalls     []saveCall
	saveSyncCalls []saveCall

	// failSync 让指定账号的 SaveSync 返回错误。
	failSync map[string]error
	// failLoad 让指定账号的 Load 返回错误。
	failLoad map[string]error

	saveBlock chan struct{} // 非 nil 时 Save 会阻塞直到该通道关闭，模拟慢存储。
}

type saveCall struct {
	account string
	data    interface{}
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		data:         make(map[string]interface{}),
		pendingLoads: make(map[string][]func(interface{}, error)),
		failSync:     make(map[string]error),
		failLoad:     make(map[string]error),
	}
}

func (s *fakeStore) Load(account string, cb func(interface{}, error)) {
	s.mu.Lock()
	s.loadCalls = append(s.loadCalls, account)
	if err, ok := s.failLoad[account]; ok {
		s.mu.Unlock()
		cb(nil, err)
		return
	}
	s.pendingLoads[account] = append(s.pendingLoads[account], cb)
	s.mu.Unlock()
}

// resolveLoad 完成该账号所有挂起的加载，返回库内数据（无则给零值）。
func (s *fakeStore) resolveLoad(account string, fresh interface{}) {
	s.mu.Lock()
	cbs := s.pendingLoads[account]
	delete(s.pendingLoads, account)
	s.mu.Unlock()
	for _, cb := range cbs {
		cb(fresh, nil)
	}
}

func (s *fakeStore) pendingLoadCount(account string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pendingLoads[account])
}

func (s *fakeStore) Save(account string, data interface{}) {
	if s.saveBlock != nil {
		<-s.saveBlock
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCalls = append(s.saveCalls, saveCall{account, data})
}

func (s *fakeStore) SaveSync(account string, data interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveSyncCalls = append(s.saveSyncCalls, saveCall{account, data})
	if err, ok := s.failSync[account]; ok {
		return err
	}
	s.data[account] = data
	return nil
}

func (s *fakeStore) asyncSaveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saveCalls)
}

func (s *fakeStore) syncSaveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saveSyncCalls)
}

var (
	_ Conn  = (*fakeConn)(nil)
	_ Store = (*fakeStore)(nil)
)

var errBoom = errors.New("boom: injected storage failure")
