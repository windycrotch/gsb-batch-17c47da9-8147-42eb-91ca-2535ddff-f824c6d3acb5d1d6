package gameapi

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/trainking/goboot/pkg/idgen"
	"google.golang.org/protobuf/proto"
)

// Session 网络会话
type Session struct {
	ID  int64 // session的唯一标识
	app *App

	conn          NetConn       // 网络连接
	closeOnce     sync.Once     // 关闭控制
	closeFlag     int32         // 关闭标志
	closeChan     chan struct{} // 关闭channel
	closeDone     chan struct{} // 关闭及断线通知完成
	notifyFlag    int32         // 断线回调是否已经触发
	appNotifyFlag int32         // App断线监听与计数是否已经处理
	cleanFlag     int32         // 在线归属清理是否已经执行
	runOnce       sync.Once     // 保证三套循环只启动一次
	isValid       int32         // 是否验证为有效连接
	validTimer    *time.Timer   // 设置验证超时
	expired       int64         // Session过期时间

	sendChan    chan Packet // 发送队列
	receiveChan chan Packet // 接收队列

	callback SessionCallback // 回调函数

	userID   int64       // sesssion所属的玩家ID
	userData interface{} // session所属玩家额外数据

	heartbeatCount int64 // 连接的心跳包计数器
}

// SessionCallback session触发外部事件调用
type SessionCallback interface {

	// OnConnect 当连接建立时调用
	OnConnect(*Session) bool

	// OnHeartbeat 当心跳包到达时处理
	OnHeartbeat(*Session)

	// OnMessage 当连接处理消息时
	OnMessage(*Session, Packet) bool

	// OnDisConnect 当连接断开时
	OnDisConnect(*Session)
}

// NewSession 新建一个Session
func NewSession(conn NetConn, app *App) *Session {
	sendLimit, receiveLimit := 1024, 1024
	if app != nil && app.Config != nil {
		sendLimit = app.Config.GetInt("SendLimit")
		receiveLimit = app.Config.GetInt("ReceiveLimit")
	}

	s := &Session{
		ID:          idgen.GenerateInt64(),
		app:         app,
		conn:        conn,
		closeChan:   make(chan struct{}),
		closeDone:   make(chan struct{}),
		sendChan:    make(chan Packet, sendLimit),
		receiveChan: make(chan Packet, receiveLimit),
	}
	if app != nil {
		s.callback = app
		app.addActiveSession(s)
	}

	return s
}

// Close 关闭Session。
// 资源关闭和断线通知都只会执行一次；重复调用以及在断线回调中重入调用都是安全的。
func (s *Session) Close() {
	if s == nil {
		return
	}

	s.closeOnce.Do(func() {
		if s.closeChan == nil {
			s.closeChan = make(chan struct{})
		}
		if s.closeDone == nil {
			s.closeDone = make(chan struct{})
		}
		atomic.StoreInt32(&s.closeFlag, 1)
		close(s.closeChan)
		if s.conn != nil {
			s.conn.Close()
		}
	})

	// 在线表是关闭流程的一部分，先同步完成归属清理，避免Close返回后仍指向已关闭会话。
	if s.app != nil {
		s.app.releaseSession(s)
	}

	if atomic.CompareAndSwapInt32(&s.notifyFlag, 0, 1) {
		go func() {
			defer func() {
				if s.closeDone != nil {
					close(s.closeDone)
				}
			}()
			if s.callback != nil {
				s.callback.OnDisConnect(s)
			}
		}()
	}
}

// CloseDone 返回断线回调执行完成的信号。
func (s *Session) CloseDone() <-chan struct{} {
	return s.closeDone
}

// WaitClose 等待断线回调和清理完成，主要用于测试和需要确认收尾的场景。
func (s *Session) WaitClose() {
	if s.closeDone != nil {
		<-s.closeDone
	}
}

// markCleaned 尝试占用在线表清理权，保证清理逻辑只执行一次。
func (s *Session) markCleaned() bool {
	return atomic.CompareAndSwapInt32(&s.cleanFlag, 0, 1)
}

// IsClosed 是否关闭
func (s *Session) IsClosed() bool {
	return atomic.LoadInt32(&s.closeFlag) > 0
}

// startValidTimer 开始验证计时
func (s *Session) startValidTimer() {
	timeout := 60 * time.Second
	if s.app != nil && s.app.Config != nil {
		timeout = time.Second * time.Duration(s.app.Config.GetInt("ValidTimeout"))
	}
	s.validTimer = time.NewTimer(timeout)
	go func() {
		exitChan := s.appExitChan()
		select {
		// 关服退出
		case <-exitChan:
			return
		case <-s.closeChan:
			return
		case <-s.validTimer.C:
			if !s.validTimer.Stop() && !s.IsValid() {
				s.Close()
			}
			return
		}
	}()
}

func (s *Session) App() *App {
	return s.app
}

// UserID 获取玩家的UserID
func (s *Session) UserID() int64 {
	return atomic.LoadInt64(&s.userID)
}

// UserData 获取玩家的额外数据
func (s *Session) UserData() interface{} {
	return s.userData
}

// HeartbeatCount 连接的心跳包计数
func (s *Session) HeartbeatCount() int64 {
	return atomic.LoadInt64(&s.heartbeatCount)
}

// SetUserData 设置玩家的额外数据
func (s *Session) SetUserData(d interface{}) {
	s.userData = d
}

// SetUserID 设置玩家ID
func (s *Session) SetUserID(userID int64) {
	atomic.StoreInt64(&s.userID, userID)
}

// SetExpired 设置session的过期时间
func (s *Session) SetExpired(expired int64) {
	s.expired = expired
}

// Send 向此session推送消息
func (s *Session) Send(opcode interface{}, msg proto.Message) error {
	_op := opcodeChange(opcode)
	if _op == 0 {
		return ErrWrongOpCode
	}
	p, err := CretaePbPacket(_op, msg)
	if err != nil {
		return err
	}

	return s.WritePacket(p)
}

// IsExipred 判断Session是否已经过期
func (s *Session) IsExipred() bool {
	if s.expired > 0 {
		if time.Now().Unix() > s.expired {
			return true
		}
	}

	return false
}

// IsValid 是否验证为有效连接
func (s *Session) IsValid() bool {
	return atomic.LoadInt32(&s.isValid) > 0
}

// Valid 设置为有效连接
func (s *Session) valid() {
	if s.validTimer != nil {
		s.validTimer.Stop()
	}

	atomic.StoreInt32(&s.isValid, 1)
}

// WritePacket 写入发送包
func (s *Session) WritePacket(p Packet) (err error) {
	if s.IsClosed() {
		return ErrConnClosing
	}

	defer func() {
		if e := recover(); e != nil {
			err = ErrConnClosing
		}
	}()

	exitChan := s.appExitChan()
	select {
	case s.sendChan <- p:
		return nil
	case <-s.closeChan:
		return ErrConnClosing
	case <-exitChan:
		return ErrConnClosing
	}
}

// deliverReceivePacket 将网络包投递给处理循环，关闭时返回错误而不是阻塞或 panic。
func (s *Session) deliverReceivePacket(p Packet) (err error) {
	if p == nil {
		return ErrNilPacket
	}
	if s.IsClosed() {
		return ErrConnClosing
	}

	defer func() {
		if recover() != nil {
			err = ErrConnClosing
		}
	}()

	exitChan := s.appExitChan()
	select {
	case s.receiveChan <- p:
		return nil
	case <-s.closeChan:
		return ErrConnClosing
	case <-exitChan:
		return ErrConnClosing
	}
}

func (s *Session) appExitChan() <-chan struct{} {
	if s.app == nil || s.app.exitChan == nil {
		return nil
	}
	return s.app.exitChan
}

// Run 执行主体逻辑，三套循环
// readLoop 读循环
// writeLoop 写循环
// handleLopp 处理消息循环
func (s *Session) Run() {
	s.runOnce.Do(func() {
		if s.callback != nil && !s.callback.OnConnect(s) {
			s.Close()
			return
		}

		asyncDo(s.handleLoop, &s.app.waitGroup)
		asyncDo(s.readLoop, &s.app.waitGroup)
		asyncDo(s.writeLoop, &s.app.waitGroup)
	})
}

// readLoop 读循环
func (s *Session) readLoop() {
	defer func() {
		recover()
		s.Close()
	}()

	for {
		exitChan := s.appExitChan()
		select {
		// 关服退出
		case <-exitChan:
			return
		case <-s.closeChan:
			return
		default:
		}

		if s.IsExipred() {
			return
		}

		p, err := s.conn.ReadPacket()
		if err != nil {
			return
		}

		if err := s.deliverReceivePacket(p); err != nil {
			return
		}
	}
}

// writeLoop 写循环
func (s *Session) writeLoop() {
	defer func() {
		recover()
		s.Close()
	}()

	for {
		exitChan := s.appExitChan()
		select {
		// 关服退出
		case <-exitChan:
			return
		case <-s.closeChan:
			return
		case p := <-s.sendChan:
			if s.IsClosed() {
				return
			}

			if err := s.conn.WritePacket(p); err != nil {
				return
			}
			p.Free()
		}
	}
}

// handleLoop 处理循环
func (s *Session) handleLoop() {
	defer func() {
		recover()
		s.Close()
	}()

	for {
		exitChan := s.appExitChan()
		select {
		// 关服退出
		case <-exitChan:
			return
		case <-s.closeChan:
			return
		case p := <-s.receiveChan:
			if s.IsClosed() {
				return
			}

			// 处理心跳包
			if p.OpCode() == 0 {
				// 心跳包过载，则跳出
				s.callback.OnHeartbeat(s)
				continue
			}

			if !s.callback.OnMessage(s, p) {
				return
			}
		}
	}
}

// asyncDo 异步执行循环
func asyncDo(fn func(), wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		fn()
		wg.Done()
	}()
}
