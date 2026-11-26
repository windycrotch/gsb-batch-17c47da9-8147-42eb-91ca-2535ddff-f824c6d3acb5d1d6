package gameapi

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/trainking/goboot/internal/pb"
	"github.com/trainking/goboot/pkg/log"
	"google.golang.org/protobuf/proto"
)

const h41151UserID int64 = 41151

type h41151WriteRecord struct {
	opcode uint16
	data   []byte
}

type h41151Conn struct {
	closed      chan struct{}
	closeOnce   sync.Once
	closeCount  int32
	readStarted chan struct{}
	readOnce    sync.Once
	incoming    chan Packet
	written     chan h41151WriteRecord
}

func newH41151Conn() *h41151Conn {
	return &h41151Conn{
		closed:      make(chan struct{}),
		readStarted: make(chan struct{}),
		incoming:    make(chan Packet, 64),
		written:     make(chan h41151WriteRecord, 256),
	}
}

func (c *h41151Conn) ReadPacket() (Packet, error) {
	c.readOnce.Do(func() { close(c.readStarted) })

	select {
	case p := <-c.incoming:
		if p == nil {
			return nil, net.ErrClosed
		}
		return p, nil
	case <-c.closed:
		return nil, net.ErrClosed
	}
}

func (c *h41151Conn) WritePacket(p Packet) error {
	record := h41151WriteRecord{opcode: p.OpCode()}
	if raw := p.Serialize(); raw != nil {
		record.data = append([]byte(nil), raw...)
	}

	select {
	case c.written <- record:
		return nil
	case <-c.closed:
		return net.ErrClosed
	}
}

func (c *h41151Conn) Close() {
	c.closeOnce.Do(func() {
		atomic.AddInt32(&c.closeCount, 1)
		close(c.closed)
	})
}

type h41151Listener struct {
	acceptChan chan NetConn
	closed     chan struct{}
	closeOnce  sync.Once
}

func newH41151Listener() *h41151Listener {
	return &h41151Listener{
		acceptChan: make(chan NetConn, 8),
		closed:     make(chan struct{}),
	}
}

func (l *h41151Listener) Accept() (NetConn, error) {
	select {
	case conn := <-l.acceptChan:
		if conn == nil {
			return nil, net.ErrClosed
		}
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *h41151Listener) Close() {
	l.closeOnce.Do(func() { close(l.closed) })
}

type h41151PushCall struct {
	userID int64
	opcode uint16
}

type h41151Publisher struct {
	mu    sync.Mutex
	calls []h41151PushCall
	err   error
}

func (p *h41151Publisher) Publish(userID int64, packet Packet) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, h41151PushCall{userID: userID, opcode: packet.OpCode()})
	return p.err
}

func (p *h41151Publisher) Calls() []h41151PushCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]h41151PushCall(nil), p.calls...)
}

func newH41151App(t *testing.T) *App {
	t.Helper()

	log.InitLogger(log.Config{Target: "gameapi-41151-test", OutPath: t.TempDir()})
	cfg := viper.New()
	cfg.Set("SendLimit", 32)
	cfg.Set("ReceiveLimit", 32)
	cfg.Set("ValidTimeout", 60)

	app := &App{
		exitChan:       make(chan struct{}),
		router:         make(map[uint16]Handler),
		sessions:       make(map[int64]*Session),
		activeSessions: make(map[int64]*Session),
	}
	app.Config = cfg

	t.Cleanup(app.Stop)
	return app
}

func h41151ValidSession(t *testing.T, app *App, userID int64) (*Session, *h41151Conn) {
	t.Helper()
	conn := newH41151Conn()
	session := NewSession(conn, app)
	NewDefaultContext(context.Background(), app, session, 1, nil).Valid(userID)
	return session, conn
}

func h41151WaitTrue(t *testing.T, timeout time.Duration, reason string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", reason)
}

func h41151WaitSignal(t *testing.T, ch <-chan struct{}, reason string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", reason)
	}
}

func h41151WaitRecord(t *testing.T, conn *h41151Conn, opcode uint16) h41151WriteRecord {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case record := <-conn.written:
			if record.opcode == opcode {
				return record
			}
		case <-deadline:
			t.Fatalf("timed out waiting for opcode %d", opcode)
		}
	}
}

func Test41151RepeatLoginTakeoverAndDelayedDisconnect(t *testing.T) {
	app := newH41151App(t)

	var disconnectCount int32
	app.SetDisconnectListener(func(*Session) error {
		atomic.AddInt32(&disconnectCount, 1)
		return nil
	})

	oldSession, oldConn := h41151ValidSession(t, app, h41151UserID)
	if got := app.sessions[h41151UserID]; got != oldSession {
		t.Fatalf("first login did not own session")
	}

	newSession, newConn := h41151ValidSession(t, app, h41151UserID)
	h41151WaitTrue(t, time.Second, "old session closed", oldSession.IsClosed)
	h41151WaitSignal(t, oldSession.CloseDone(), "old disconnect callback")

	if got := app.sessions[h41151UserID]; got != newSession {
		t.Fatalf("new login did not take over ownership")
	}
	if atomic.LoadInt32(&oldConn.closeCount) != 1 {
		t.Fatalf("old conn close count = %d, want 1", oldConn.closeCount)
	}
	if count := atomic.LoadInt32(&disconnectCount); count != 1 {
		t.Fatalf("disconnect count after takeover = %d, want 1", count)
	}

	// 旧连接的断线事件可能在接管之后才到达；重复触发也不能误删新会话。
	oldSession.Close()
	app.OnDisConnect(oldSession)
	app.DelSession(h41151UserID, oldSession.ID)
	if got := app.sessions[h41151UserID]; got != newSession {
		t.Fatalf("late old-session cleanup removed/replaced new session: got %p want %p", got, newSession)
	}
	if newSession.IsClosed() {
		t.Fatalf("new session was closed by old-session cleanup")
	}
	if count := atomic.LoadInt32(&disconnectCount); count != 1 {
		t.Fatalf("disconnect count after late cleanup = %d, want 1", count)
	}

	newSession.Close()
	h41151WaitSignal(t, newSession.CloseDone(), "new disconnect callback")
	h41151WaitTrue(t, time.Second, "online entry deleted", func() bool {
		_, ok := app.sessions[h41151UserID]
		return !ok
	})
	if count := atomic.LoadInt32(&disconnectCount); count != 2 {
		t.Fatalf("disconnect count after new close = %d, want 2", count)
	}
	if atomic.LoadInt32(&newConn.closeCount) != 1 {
		t.Fatalf("new conn close count = %d, want 1", newConn.closeCount)
	}
}

func Test41151SameSessionDuplicateValidDoesNotCloseOwner(t *testing.T) {
	app := newH41151App(t)
	session, _ := h41151ValidSession(t, app, h41151UserID+1)

	NewDefaultContext(context.Background(), app, session, 1, nil).Valid(h41151UserID + 1)
	NewDefaultContext(context.Background(), app, session, 1, nil).Valid(h41151UserID + 1)

	if session.IsClosed() {
		t.Fatalf("duplicate Valid on the current owner closed it")
	}
	if got := app.sessions[h41151UserID+1]; got != session {
		t.Fatalf("duplicate Valid changed ownership")
	}
}

func Test41151ConcurrentRepeatLoginConverges(t *testing.T) {
	app := newH41151App(t)
	const n = 40
	sessions := make([]*Session, n)
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		conn := newH41151Conn()
		sessions[i] = NewSession(conn, app)
		wg.Add(1)
		go func(s *Session) {
			defer wg.Done()
			NewDefaultContext(context.Background(), app, s, 1, nil).Valid(h41151UserID + 2)
		}(sessions[i])
	}
	wg.Wait()

	current := app.sessions[h41151UserID+2]
	if current == nil || current.IsClosed() {
		t.Fatalf("online owner is missing or closed: %v", current)
	}

	for _, s := range sessions {
		if s == current {
			continue
		}
		h41151WaitTrue(t, time.Second, "non-owner session closed", s.IsClosed)
	}
}

func Test41151SendAfterCloseReturnsClosingErrorAndDoesNotPanic(t *testing.T) {
	app := newH41151App(t)
	conn := newH41151Conn()
	session := NewSession(conn, app)
	packet := NewDefaultPacket(nil, 9999)

	if err := session.WritePacket(packet); err != nil {
		t.Fatalf("WritePacket before Close: %v", err)
	}

	session.Close()
	h41151WaitTrue(t, time.Second, "session closed flag", session.IsClosed)

	for i := 0; i < 20; i++ {
		if err := session.WritePacket(NewDefaultPacket(nil, 9999)); !errors.Is(err, ErrConnClosing) {
			t.Fatalf("WritePacket after Close error = %v, want ErrConnClosing", err)
		}
		if err := session.Send(9999, &pb.S2C_Login{Ok: true}); !errors.Is(err, ErrConnClosing) {
			t.Fatalf("Send after Close error = %v, want ErrConnClosing", err)
		}
	}

	// 并发关闭和投递也不能向已关闭 channel 发送、阻塞或 panic。
	raceSession := NewSession(newH41151Conn(), app)
	var raceWG sync.WaitGroup
	raceWG.Add(1)
	go func() {
		defer raceWG.Done()
		time.Sleep(time.Millisecond)
		raceSession.Close()
	}()
	for g := 0; g < 8; g++ {
		raceWG.Add(1)
		go func(id int) {
			defer raceWG.Done()
			for i := 0; i < 100; i++ {
				_ = raceSession.WritePacket(NewDefaultPacket(nil, 9999))
				if (id+i)%17 == 0 {
					raceSession.Close()
				}
			}
		}(g)
	}

	raceDone := make(chan struct{})
	go func() {
		raceWG.Wait()
		close(raceDone)
	}()
	h41151WaitSignal(t, raceDone, "concurrent writes and Close to finish")
}

func Test41151ClosedLocalSessionFallsBackToPush(t *testing.T) {
	app := newH41151App(t)
	publisher := &h41151Publisher{}
	app.pushClient = publisher

	userID := h41151UserID + 3
	session, _ := h41151ValidSession(t, app, userID)
	session.Close()
	h41151WaitSignal(t, session.CloseDone(), "close callback")

	// 人为恢复出题面中“在线表残留已关闭会话”的状态。
	app.sessionMu.Lock()
	app.sessions[userID] = session
	app.sessionMu.Unlock()

	locationPacket := NewDefaultPacket(nil, 8888)
	if err := app.sendActorLocation(userID, locationPacket); !errors.Is(err, ErrUserNoIn) {
		t.Fatalf("sendActorLocation closed session error = %v, want ErrUserNoIn", err)
	}
	if _, ok := app.sessions[userID]; ok {
		t.Fatalf("closed stale session was not evicted from online table")
	}

	if err := app.SendActor(userID, 8888, &pb.S2C_Login{Ok: true}); err != nil {
		t.Fatalf("SendActor with push publisher: %v", err)
	}
	calls := publisher.Calls()
	if len(calls) != 1 || calls[0].userID != userID || calls[0].opcode != 8888 {
		t.Fatalf("push calls = %+v, want one opcode 8888 for %d", calls, userID)
	}

	publisher.err = errors.New("push unavailable")
	app.sessionMu.Lock()
	app.sessions[userID] = session
	app.sessionMu.Unlock()
	if err := app.SendActor(userID, 8888, &pb.S2C_Login{Ok: true}); err == nil {
		t.Fatalf("SendActor swallowed push failure")
	}
}

func Test41151ReceiveDeliveryUnblocksOnClose(t *testing.T) {
	app := newH41151App(t)
	session, _ := h41151ValidSession(t, app, h41151UserID+40)

	for i := 0; i < cap(session.receiveChan); i++ {
		session.receiveChan <- NewDefaultPacket(nil, 9000+uint16(i%100))
	}

	done := make(chan error, 1)
	go func() {
		done <- session.deliverReceivePacket(NewDefaultPacket(nil, 9100))
	}()
	session.Close()

	select {
	case err := <-done:
		if !errors.Is(err, ErrConnClosing) {
			t.Fatalf("deliverReceivePacket error = %v, want ErrConnClosing", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("receive delivery blocked after Close")
	}
}

func Test41151ConcurrentSessionCloseAndStop(t *testing.T) {
	app := newH41151App(t)
	app.listener = newH41151Listener()

	const count = 12
	sessions := make([]*Session, count)
	conns := make([]*h41151Conn, count)
	var disconnectCount int32
	app.SetDisconnectListener(func(*Session) error {
		atomic.AddInt32(&disconnectCount, 1)
		return nil
	})

	for i := 0; i < count; i++ {
		conns[i] = newH41151Conn()
		sessions[i] = NewSession(conns[i], app)
		sessions[i].Run()
		h41151WaitSignal(t, conns[i].readStarted, "read loop start")
	}

	stopDone := make(chan struct{})
	go func() {
		app.Stop()
		close(stopDone)
	}()

	var closeWG sync.WaitGroup
	for _, session := range sessions {
		closeWG.Add(1)
		go func(s *Session) {
			defer closeWG.Done()
			s.Close()
		}(session)
	}
	closeWG.Wait()
	h41151WaitSignal(t, stopDone, "concurrent App.Stop")

	h41151WaitTrue(t, 2*time.Second, "exactly once disconnect callbacks", func() bool {
		return atomic.LoadInt32(&disconnectCount) == count
	})
	for i, conn := range conns {
		if got := atomic.LoadInt32(&conn.closeCount); got != 1 {
			t.Fatalf("conn %d close count = %d, want 1", i, got)
		}
	}
}

func Test41151StopExitsLoopsAndCleansOnlineSessions(t *testing.T) {
	app := newH41151App(t)
	listener := newH41151Listener()
	app.listener = listener

	const count = 3
	conns := make([]*h41151Conn, count)
	sessions := make([]*Session, count)
	var disconnectCount int32
	app.SetDisconnectListener(func(*Session) error {
		atomic.AddInt32(&disconnectCount, 1)
		return nil
	})

	for i := 0; i < count; i++ {
		conns[i] = newH41151Conn()
		sessions[i] = NewSession(conns[i], app)
		sessions[i].Run()
		h41151WaitSignal(t, conns[i].readStarted, "read loop start")

		userID := h41151UserID + 10 + int64(i)
		NewDefaultContext(context.Background(), app, sessions[i], 1, nil).Valid(userID)
	}

	stopDone := make(chan struct{})
	go func() {
		app.Stop()
		close(stopDone)
	}()
	h41151WaitSignal(t, stopDone, "App.Stop to finish")

	for i, conn := range conns {
		if atomic.LoadInt32(&conn.closeCount) != 1 {
			t.Fatalf("conn %d close count = %d, want 1", i, conn.closeCount)
		}
		h41151WaitTrue(t, time.Second, "session closed", sessions[i].IsClosed)
	}

	h41151WaitTrue(t, time.Second, "active session table empty", func() bool {
		app.activeMu.Lock()
		defer app.activeMu.Unlock()
		return len(app.activeSessions) == 0
	})
	h41151WaitTrue(t, time.Second, "online session table empty", func() bool {
		app.sessionMu.RLock()
		defer app.sessionMu.RUnlock()
		return len(app.sessions) == 0
	})
	h41151WaitTrue(t, time.Second, "all disconnect callbacks", func() bool {
		return atomic.LoadInt32(&disconnectCount) == count
	})
}

func Test41151StopThroughRealTCPListener(t *testing.T) {
	app := newH41151App(t)
	const requestOpcode = uint16(9151)
	const responseOpcode = uint16(9152)
	app.AddHandler(requestOpcode, func(ctx Context) error {
		return ctx.Send(responseOpcode, &pb.S2C_Login{Ok: true})
	})

	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	app.listener = &TcpNetListener{listener: tcpListener, config: &NetConfig{}}

	startErr := make(chan error, 1)
	go func() {
		startErr <- app.Start()
	}()

	client, err := net.DialTimeout("tcp", tcpListener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	h41151WaitTrue(t, 2*time.Second, "accepted TCP session", func() bool {
		return app.GetTotalConn() == 1
	})

	// 经过真实TCP封包入口验证读、处理、写三条循环。
	if _, err := client.Write(NewDefaultPacket(nil, requestOpcode).Serialize()); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	response, err := Packing(client)
	if err != nil {
		t.Fatalf("read response through real TCP: %v", err)
	}
	if response.OpCode() != responseOpcode {
		t.Fatalf("response opcode = %d, want %d", response.OpCode(), responseOpcode)
	}

	stopDone := make(chan struct{})
	go func() {
		app.Stop()
		close(stopDone)
	}()
	h41151WaitSignal(t, stopDone, "TCP App.Stop")

	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Start did not return after Stop")
	}
}

func Test41151UnauthenticatedRejectedAndLoginForwardingUnchanged(t *testing.T) {
	app := newH41151App(t)

	const loginOpcode = uint16(pb.OpCode_Op_C2S_Login)
	const loginAckOpcode = uint16(pb.OpCode_Op_S2C_Login)
	const sayOpcode = uint16(pb.OpCode_Op_C2S_Say)
	const sayAckOpcode = uint16(pb.OpCode_Op_S2C_Say)

	var rejectOnce sync.Once
	rejected := make(chan struct{})
	var sayCalls int32
	sayRan := make(chan struct{}, 1)

	app.AddBeforeMiddleware(Middleware{
		Condition: func(opcode uint16) bool { return opcode != loginOpcode },
		Do: func(ctx Context) error {
			if !ctx.Session().IsValid() {
				rejectOnce.Do(func() { close(rejected) })
				return errors.New("unauthenticated")
			}
			return nil
		},
	})

	app.AddHandler(loginOpcode, func(ctx Context) error {
		var req pb.C2S_Login
		if err := ctx.Params(&req); err != nil {
			return err
		}
		if req.Password != "123456" {
			return ctx.Send(loginAckOpcode, &pb.S2C_Login{Ok: false})
		}
		ctx.Valid(h41151UserID + 20)
		return ctx.Send(loginAckOpcode, &pb.S2C_Login{Ok: true})
	})

	targetID := h41151UserID + 21
	targetSession, targetConn := h41151ValidSession(t, app, targetID)
	targetSession.Run()

	app.AddHandler(sayOpcode, func(ctx Context) error {
		atomic.AddInt32(&sayCalls, 1)
		sayRan <- struct{}{}
		return ctx.SendActor(targetID, sayAckOpcode, &pb.S2C_Say{Word: "hello"})
	})

	attackerConn := newH41151Conn()
	attacker := NewSession(attackerConn, app)
	attacker.Run()

	// 未登录的非登录消息必须被前置中间件拒绝，Handler不能执行。
	unauthBody, err := proto.Marshal(&pb.C2S_Say{Actor: targetID, Word: "before-login"})
	if err != nil {
		t.Fatal(err)
	}
	if err := attacker.deliverReceivePacket(NewDefaultPacket(unauthBody, sayOpcode)); err != nil {
		t.Fatalf("deliver unauthenticated packet: %v", err)
	}
	h41151WaitSignal(t, rejected, "authentication middleware rejection")
	if got := atomic.LoadInt32(&sayCalls); got != 0 {
		t.Fatalf("say handler calls before login = %d, want 0", got)
	}

	// 登录包不受鉴权中间件限制，成功后会话有效并进入在线表。
	loginBody, err := proto.Marshal(&pb.C2S_Login{Account: "41151", Password: "123456"})
	if err != nil {
		t.Fatal(err)
	}
	if err := attacker.deliverReceivePacket(NewDefaultPacket(loginBody, loginOpcode)); err != nil {
		t.Fatalf("deliver login packet: %v", err)
	}
	ack := h41151WaitRecord(t, attackerConn, loginAckOpcode)
	if ack.opcode != loginAckOpcode {
		t.Fatalf("login ack opcode = %d, want %d", ack.opcode, loginAckOpcode)
	}
	h41151WaitTrue(t, time.Second, "attacker valid", attacker.IsValid)
	if got := app.sessions[h41151UserID+20]; got != attacker {
		t.Fatalf("login did not put attacker in online table")
	}

	// 登录后的转发路径保持不变，目标连接收到S2C_Say。
	sayBody, err := proto.Marshal(&pb.C2S_Say{Actor: targetID, Word: "after-login"})
	if err != nil {
		t.Fatal(err)
	}
	if err := attacker.deliverReceivePacket(NewDefaultPacket(sayBody, sayOpcode)); err != nil {
		t.Fatalf("deliver say packet: %v", err)
	}
	h41151WaitSignal(t, sayRan, "say handler")
	h41151WaitRecord(t, targetConn, sayAckOpcode)
	if got := atomic.LoadInt32(&sayCalls); got != 1 {
		t.Fatalf("say handler calls = %d, want 1", got)
	}
}
