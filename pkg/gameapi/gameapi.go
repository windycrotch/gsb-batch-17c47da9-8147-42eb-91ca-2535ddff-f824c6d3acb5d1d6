package gameapi

import (
	"context"
	"crypto/tls"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"github.com/trainking/goboot/pkg/boot"
	"github.com/trainking/goboot/pkg/etcdx"
	"github.com/trainking/goboot/pkg/log"
	"github.com/trainking/goboot/pkg/utils"
	"google.golang.org/protobuf/proto"
)

type (
	// App 游戏服务器实现
	App struct {
		boot.BaseInstance

		gd             GameMetaData          // 服务器元数据
		un             *UserNats             // 用户消息分发
		listener       NetListener           // 网络监听
		exitChan       chan struct{}         // 退出信号
		waitGroup      sync.WaitGroup        // 等待携程控制
		closeOnce      sync.Once             // 保证关闭只执行一次
		serviceManager *etcdx.ServiceManager // etcd注册管理器

		connectListener    Listener     // 会话建立时的连接监听
		disconnectListener Listener     // 会话断开时的连接监听器
		heartbeatListener  Listener     // 会话发送心跳时的监听器
		beforeMiddleware   []Middleware // 会话处理消息之前执行的中间件
		afterMiddleware    []Middleware // 会话处理消息之后执行的中间件

		router map[uint16]Handler // 处理器映射

		totalConn int64 // 连接数量

		modules []Module // 服务上的模块

		sessions  map[int64]*Session // 有效Session映射
		sessionMu sync.RWMutex

		activeSessions map[int64]*Session // 全部活动Session，包含尚未登录的连接
		activeMu       sync.Mutex

		pushClient PacketPublisher // 跨实例推送出口，默认使用NATS
	}

	// Middleware 中间件处理
	Middleware struct {
		// Condition 是否要处理的opcode
		Condition func(uint16) bool
		// Do 处理执行
		Do func(Context) error
	}

	// Listener 监听器
	Listener func(*Session) error

	// Module 模块
	Module interface {

		// 初始化模块
		Init(app *App)
	}

	// Handler 处理类型
	Handler func(Context) error

	// PacketPublisher 跨实例消息发布器。
	PacketPublisher interface {
		Publish(userID int64, p Packet) error
	}

	// SessionCreator session创建生成器，做一些预处理
	SessionCreator func(net.Conn, *App) *Session
)

// New 创建一个游戏服务器接口实例
func New(name string, configPath string, addr string, instancdID int64) *App {
	// 加载配置
	v, err := utils.LoadConfigFileViper(configPath)
	if err != nil {
		panic(err)
	}

	un, err := NewUserNats(v.GetString("NatsUrl"), name)
	if err != nil {
		panic(err)
	}

	app := new(App)
	app.Name = name
	app.gd = GameMetaData{
		ID:       instancdID,
		State:    StateZero,
		Password: v.GetString("Password"),
		OutUrl:   v.GetString("OutUrl"),
		Version:  v.GetString("Version"),
	}
	app.un = un
	app.Config = v
	app.Addr = addr
	app.IntanceID = instancdID
	app.router = make(map[uint16]Handler)
	app.exitChan = make(chan struct{})
	app.sessions = make(map[int64]*Session)
	app.activeSessions = make(map[int64]*Session)
	app.pushClient = un
	return app
}

// SetConnectListener 设置连接监听器
func (a *App) SetConnectListener(l Listener) {
	a.connectListener = l
}

// SetDisconnectListener 设置断连监听器
func (a *App) SetDisconnectListener(l Listener) {
	a.disconnectListener = l
}

// SetHeartbeatListener 设置心跳监听器
func (a *App) SetHeartbeatListener(l Listener) {
	a.heartbeatListener = l
}

// SetBeforeMiddleware 设置消息处理前中间件
func (a *App) AddBeforeMiddleware(m Middleware) {
	a.beforeMiddleware = append(a.beforeMiddleware, m)
}

// AddAfterMiddleware 设置详细处理后的中间件
func (a *App) AddAfterMiddleware(m Middleware) {
	a.afterMiddleware = append(a.afterMiddleware, m)
}

// AddHandler 增加处理器
func (a *App) AddHandler(opcode interface{}, h Handler) {
	_op := opcodeChange(opcode)
	if _op == 0 {
		return
	}
	a.router[_op] = h
}

// AddModule 增加所有模块
func (a *App) AddModule(module Module) {
	a.modules = append(a.modules, module)
}

// AddSession 加入Session。
// 同一账号以最近一次登录成功的会话为准；新会话接管时会关闭旧会话。
func (a *App) AddSession(userID int64, session *Session) {
	if a == nil || session == nil || session.IsClosed() {
		return
	}

	session.SetUserID(userID)

	a.sessionMu.Lock()
	if a.sessions == nil {
		a.sessions = make(map[int64]*Session)
	}

	oldSession := a.sessions[userID]
	if oldSession == session {
		a.sessionMu.Unlock()
		return
	}

	a.sessions[userID] = session
	a.sessionMu.Unlock()

	// 不能持锁关闭：Close会同步做归属清理，清理路径需要获取sessionMu。
	if oldSession != nil {
		oldSession.Close()
	}
}

// DelSession 删除Session。只有当前在线表仍指向传入的sessionID时才删除，
// 因此旧连接延迟断线不会误删已经接管账号的新会话。
func (a *App) DelSession(userID int64, sessionid int64) {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()

	if _session, ok := a.sessions[userID]; ok && _session != nil && _session.ID == sessionid {
		delete(a.sessions, userID)
	}
}

// releaseSession 执行会话资源在在线表和活动连接表中的核心清理。
// 每个Session最多执行一次，可由Close同步调用，也可由OnDisConnect兜底调用。
func (a *App) releaseSession(session *Session) {
	if a == nil || session == nil || !session.markCleaned() {
		return
	}

	atomic.AddInt64(&a.totalConn, -1)
	a.removeActiveSession(session)
	// DelSession会同时校验userID和sessionID，因此未登录会话或旧会话都不会误删当前归属。
	a.DelSession(session.UserID(), session.ID)
}

func (a *App) addActiveSession(session *Session) {
	if a == nil || session == nil {
		return
	}

	a.activeMu.Lock()
	defer a.activeMu.Unlock()
	if a.activeSessions == nil {
		a.activeSessions = make(map[int64]*Session)
	}
	a.activeSessions[session.ID] = session
}

func (a *App) removeActiveSession(session *Session) {
	if a == nil || session == nil {
		return
	}

	a.activeMu.Lock()
	defer a.activeMu.Unlock()
	delete(a.activeSessions, session.ID)
}

func (a *App) closeAllSessions() {
	a.activeMu.Lock()
	sessions := make([]*Session, 0, len(a.activeSessions))
	for _, session := range a.activeSessions {
		sessions = append(sessions, session)
	}
	a.activeMu.Unlock()

	for _, session := range sessions {
		session.Close()
	}
}

// GetTotalConn 获取连接总数
func (a *App) GetTotalConn() int64 {
	return atomic.LoadInt64(&a.totalConn)
}

// SendActor 向指定玩家发送消息
func (a *App) SendActor(userID int64, opcode uint16, msg proto.Message) error {
	p, err := CretaePbPacket(opcode, msg)
	if err != nil {
		return err
	}

	// 如果在本实例，则直接发送；本地没有存活会话时，走跨实例推送路径。
	err = a.sendActorLocation(userID, p)
	if err == ErrUserNoIn || errors.Is(err, ErrConnClosing) {
		// 发送给其他实例
		return a.sendActorPush(userID, p)
	}

	return err
}

// SendAllActor 向所有玩家发送消息
func (a *App) SendAllActor(opcode uint16, msg proto.Message) error {
	p, err := CretaePbPacket(opcode, msg)
	if err != nil {
		return err
	}

	return a.sendActorPush(-1, p)
}

// sendActorLocation 发送消息给本地实例
func (a *App) sendActorLocation(userID int64, p Packet) error {
	a.sessionMu.RLock()
	session := a.sessions[userID]
	a.sessionMu.RUnlock()

	if session == nil {
		return ErrUserNoIn
	}

	// 在线表可能因并发关闭尚未完成兜底清理；这里按“不在本实例”处理，
	// 并条件删除当前已关闭的归属，避免继续向死会话投递。
	if session.IsClosed() {
		a.DelSession(userID, session.ID)
		return ErrUserNoIn
	}

	var err error
	// 如果定义handler，必须发送给handler处理
	if _, ok := a.router[p.OpCode()]; ok {
		err = session.deliverReceivePacket(p)
	} else {
		err = session.WritePacket(p)
	}
	if err != nil {
		if session.IsClosed() {
			a.DelSession(userID, session.ID)
			return ErrUserNoIn
		}
		return err
	}

	return nil
}

// sendAllActorLocation 像所有的本地玩家发送消息
func (a *App) sendAllActorLocation(m PushActorMessage) error {
	a.sessionMu.RLock()
	sessions := make([]*Session, 0, len(a.sessions))
	for _, session := range a.sessions {
		sessions = append(sessions, session)
	}
	a.sessionMu.RUnlock()

	for _, session := range sessions {
		if session == nil || session.IsClosed() {
			continue
		}

		p := NewDefaultPacket(m.Msg, m.OpCode)
		var err error
		// 如果定义handler，必须发送给handler处理
		if _, ok := a.router[p.OpCode()]; ok {
			err = session.deliverReceivePacket(p)
		} else {
			err = session.WritePacket(p)
		}
		if err != nil {
			p.Free()
			if session.IsClosed() || errors.Is(err, ErrConnClosing) {
				a.DelSession(session.UserID(), session.ID)
				continue
			}
			return err
		}
	}

	return ErrUserNoIn
}

// sendActorPush 发送消息给远程实例
func (a *App) sendActorPush(userID int64, p Packet) error {
	// 生产路径始终使用NATS；pushClient仅用于在无外部依赖的单测中替换推送出口。
	if a.un != nil {
		return a.un.Publish(userID, p)
	}
	if a.pushClient != nil {
		return a.pushClient.Publish(userID, p)
	}
	return ErrUserNoIn
}

// Init 初始化服务
func (a *App) Init() (err error) {
	if err = a.BaseInstance.Init(); err != nil {
		return err
	}

	// 初始化各个模块
	for _, m := range a.modules {
		m.Init(a)
	}

	var netConfig = NetConfig{
		Addr:         a.Addr,
		WriteTimeout: time.Duration(a.Config.GetInt("ConnWriteTimeout") * int(time.Second)),
		ReadeTimeout: time.Duration(a.Config.GetInt("ConnReadTimeout") * int(time.Second)),
	}

	// 加载tls配置
	tlsConfigMap := a.Config.GetStringMapString("TLS")
	if len(tlsConfigMap) > 0 {
		cert, err := tls.LoadX509KeyPair(tlsConfigMap["certfile"], tlsConfigMap["keyfile"])
		if err != nil {
			return err
		}
		netConfig.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
		a.gd.UseTLS = true
	}

	// 根据配置传输层协议
	network := a.Config.GetString("Network")
	switch network {
	case "tcp":
		a.listener, err = NewTcpNetListener(netConfig)
		if err != nil {
			return err
		}
	case "kcp":
		netConfig.KcpMode = a.Config.GetString("KcpMode")
		a.listener, err = NewKcpNetListener(netConfig)
		if err != nil {
			return err
		}
	case "websocket":
		netConfig.WebSocketPath = a.Config.GetString("WebsocketPath")
		a.listener, err = NewWebSocketNetListener(netConfig)
		if err != nil {
			return err
		}
	default:
		return errors.Wrap(ErrNoImplementNetwork, network)
	}
	a.gd.Network = network

	// 监听广播的其他实例消息
	go a.subscribePushUserMsg()

	// 注册服务
	if err := a.registerEtcd(); err != nil {
		return err
	}

	return nil
}

// subscribePushUserMsg 订阅消费推送玩家的消息
func (a *App) subscribePushUserMsg() {
	// 开启消费, 消费limit使用接收消息的一半
	if err := a.un.StartSubscribe(a.Config.GetInt("ReceiveLimit") / 2); err != nil {
		log.Errorf("StartSubscribe Error: %v", err)
		return
	}
	defer func() {
		e := recover()
		if e != nil {
			log.Errorf("App.subscribePushUserMsg Error: %v", e)
		}
		a.un.Close()
		a.Stop()
	}()

	for {
		select {
		case <-a.exitChan:
			return
		case m := <-a.un.Consume():
			if m.UserID != -1 {
				p := NewDefaultPacket(m.Msg, m.OpCode)
				// 向特定玩家发送消息
				if err := a.sendActorLocation(m.UserID, p); err != nil && err != ErrUserNoIn {
					log.Errorf("PushActorMessage sendActorLocation Error: %v", err)
					return
				}
			} else {
				// 向所有玩家广播消息
				if err := a.sendAllActorLocation(m); err != nil && err != ErrUserNoIn {
					log.Errorf("PushActorMessage sendAllActorLocation Error: %v", err)
					return
				}
			}

		}
	}
}

// Start 启动服务
func (a *App) Start() error {
	a.waitGroup.Add(1)
	defer func() {
		a.waitGroup.Done()
	}()

	for {
		select {
		case <-a.exitChan:
			return nil
		default:
		}

		conn, err := a.listener.Accept()
		if err != nil {
			continue
		}

		a.waitGroup.Add(1)
		go func() {
			// 处理连接数据
			NewSession(conn, a).Run()
			a.waitGroup.Done()
		}()
	}
}

// Stop 停止服务
func (a *App) Stop() {
	if a == nil {
		return
	}

	a.closeOnce.Do(func() {
		if a.exitChan == nil {
			a.exitChan = make(chan struct{})
		}
		close(a.exitChan)

		// 先停止接收新连接，再关闭所有现有连接并解除读循环阻塞。
		if a.listener != nil {
			a.listener.Close()
		}
		a.closeAllSessions()

		if a.un != nil {
			a.un.Close()
		}
		if a.serviceManager != nil {
			_ = a.serviceManager.Destory(a.Addr)
		}
	})

	// 等待读、写、处理等所有协程执行完。
	a.waitGroup.Wait()
}

// OnConnect 连接时处理
func (a *App) OnConnect(session *Session) bool {
	atomic.AddInt64(&a.totalConn, 1)
	session.startValidTimer()

	if a.connectListener != nil {
		if err := a.connectListener(session); err != nil {
			return false
		}
	}

	return true
}

// OnMessage 消息处理
func (a *App) OnMessage(session *Session, p Packet) bool {
	// 消息的分发
	if h, ok := a.router[p.OpCode()]; ok {
		go func() {
			// 释放Packet
			defer p.Free()

			ctx := NewDefaultContext(context.Background(), a, session, p.OpCode(), p.Body())
			// 处理消息之前，中间件过滤。任一前置中间件拒绝后，不再进入Handler。
			for _, m := range a.beforeMiddleware {
				if m.Condition != nil && !m.Condition(p.OpCode()) {
					continue
				}

				if err := m.Do(ctx); err != nil {
					log.Errorf("Middle %d is Error: %v", p.OpCode(), err)
					return
				}
			}

			// 处理消息
			if err := h(ctx); err != nil {
				log.Errorf("Handler %d Error: %s ", p.OpCode(), err)
				return
			}

			// 处理之后，中间件操作
			for _, m := range a.afterMiddleware {
				if m.Condition != nil && !m.Condition(p.OpCode()) {
					continue
				}

				if err := m.Do(ctx); err != nil {
					log.Errorf("Middle %d is Error: %v", p.OpCode(), err)
					continue
				}
			}
		}()
	} else {
		log.Errorf("Handler %d is No Handler", p.OpCode())
		return false
	}
	return true
}

// OnDisConnect 断线处理。
// Session.Close保证该方法只会被调度一次；这里再做一次幂等保护，
// 避免旧连接的延迟清理或测试/外部重复调用导致回调和计数重复。
func (a *App) OnDisConnect(session *Session) {
	if a == nil || session == nil {
		return
	}
	if !atomic.CompareAndSwapInt32(&session.appNotifyFlag, 0, 1) {
		return
	}

	a.releaseSession(session)

	if a.disconnectListener != nil {
		_ = a.disconnectListener(session)
	}
}

// OnHeartbeat 心跳处理
func (a *App) OnHeartbeat(session *Session) {
	atomic.AddInt64(&session.heartbeatCount, 1)
	if a.heartbeatListener != nil {
		a.heartbeatListener(session)
	}
}

// opcodeChange opcode的类型转换
func opcodeChange(opcode interface{}) uint16 {
	var _op uint16
	switch reflect.TypeOf(opcode).Kind() {
	case reflect.Int32, reflect.Int, reflect.Int64:
		_op = uint16(reflect.ValueOf(opcode).Int())
	case reflect.Uint, reflect.Uint16, reflect.Uint32:
		_op = uint16(reflect.ValueOf(opcode).Uint())
	default:
		log.Errorf("wrong opcode %v kind: %v", opcode, reflect.TypeOf(opcode).Kind())
	}
	return _op
}
