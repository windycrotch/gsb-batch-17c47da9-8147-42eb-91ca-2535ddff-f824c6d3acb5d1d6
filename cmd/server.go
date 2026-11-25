package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const janitorInterval = time.Minute

// Options configures a Server. The zero value is not usable; construct with
// NewServer.
type Options struct {
	// QueueSize is the number of tasks that may wait behind the running one.
	QueueSize int

	// Runner executes encodes. Defaults to the real ffmpeg runner.
	Runner Runner

	// Hub optionally supplies the connection hub (the production server uses
	// the package-level clients hub).
	Hub *hub

	// CheckOrigin validates the websocket Origin header.
	CheckOrigin func(r *http.Request) bool

	// AllowAllOrigins disables origin enforcement (tests only).
	AllowAllOrigins bool

	// ProgressInterval throttles progress reports; zero means one second.
	ProgressInterval time.Duration

	// TerminalTTL retains finished records for late cancel/reconnect lookups.
	// Zero uses a safe default; a negative value disables expiry.
	TerminalTTL time.Duration

	// Now overrides the clock (tests).
	Now func() time.Time
}

// Server is the ownership-aware task and websocket hub. The production daemon
// runs one defaultServer; tests create isolated servers.
type Server struct {
	opts     Options
	now      func() time.Time
	hub      *hub
	upgrader websocket.Upgrader
	tasks    *registry
	queue    chan *Task

	ownerMu   sync.Mutex
	owners    map[string]map[*websocket.Conn]bool
	connOwner map[*websocket.Conn]string
	ownerSeq  uint64

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	wg        sync.WaitGroup

	// admitMu serializes submission against shutdown so no record can be
	// created after the registry is cleared.
	admitMu  sync.RWMutex
	closedMu sync.Mutex
	closed   bool
}

// NewServer builds a server but does not start its dispatcher.
func NewServer(opts Options) *Server {
	if opts.QueueSize <= 0 {
		opts.QueueSize = maxQueuedJobs
	}
	if opts.Runner == nil {
		rr := realRunner{progressInterval: opts.ProgressInterval}
		opts.Runner = rr
	}
	hub := opts.Hub
	if hub == nil {
		hub = newHub()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ttl := opts.TerminalTTL
	if ttl == 0 {
		ttl = defaultTerminalTTL
	}

	s := &Server{
		opts:      opts,
		now:       now,
		hub:       hub,
		tasks:     newRegistry(ttl),
		queue:     make(chan *Task, opts.QueueSize),
		owners:    make(map[string]map[*websocket.Conn]bool),
		connOwner: make(map[*websocket.Conn]string),
		stopCh:    make(chan struct{}),
	}
	s.upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			switch {
			case opts.AllowAllOrigins:
				return true
			case opts.CheckOrigin != nil:
				return opts.CheckOrigin(r)
			default:
				return isAllowedOrigin(r.Header.Get("Origin"))
			}
		},
	}
	return s
}

const defaultTerminalTTL = 5 * time.Minute

// Handler exposes the websocket endpoint on an isolated mux (tests/embedding).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.ServeWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	return mux
}

// Start launches the serial dispatcher and cleanup loop. Safe to call once.
func (s *Server) Start() {
	s.startOnce.Do(func() {
		s.wg.Add(2)
		go s.dispatchLoop()
		go s.janitorLoop()
	})
}

// Shutdown stops admission, cancels every task, waits for the runner to settle
// and clears all task records so nothing can be triggered afterwards.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopOnce.Do(func() {
		// Block until any in-flight Submit has fully admitted its task, then
		// close admission so no record can be created after the reset below.
		s.admitMu.Lock()
		s.closedMu.Lock()
		s.closed = true
		s.closedMu.Unlock()
		close(s.stopCh)
		s.admitMu.Unlock()
	})

	for _, t := range s.tasks.all() {
		_, _, enc, cancelFn := t.applyCancel()
		if cancelFn != nil {
			cancelFn()
		}
		if enc != nil {
			enc.Cancel()
		}
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}

	s.tasks.reset()
	s.hub.closeAll()
	return nil
}

func (s *Server) isClosed() bool {
	s.closedMu.Lock()
	defer s.closedMu.Unlock()
	return s.closed
}

func (s *Server) janitorLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.tasks.reap(s.now())
		}
	}
}

// dispatchLoop runs encodes one at a time, matching the historical queue.
func (s *Server) dispatchLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stopCh:
			return
		case t := <-s.queue:
			if !s.beginRun(t) {
				// Cancelled while queued: skip without touching the runner.
				continue
			}
			// Guarantee the client has already received "accepted" before
			// any "started"/progress event.
			t.waitAdmitted(s.stopCh)
			select {
			case <-s.stopCh:
				return
			default:
			}
			s.execute(t)
		}
	}
}

func (s *Server) beginRun(t *Task) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != stateQueued {
		return false
	}
	t.state = stateRunning
	return true
}

func (s *Server) execute(t *Task) {
	s.emitPhase(t, eventStarted)

	err := s.opts.Runner.Run(t, func(p Status) { s.reportProgress(t, p) })
	final, status := classifyRunError(err)
	s.conclude(t, final, status)

	fmt.Printf("Waiting for next job...")
}

// Submit admits one encode. On a full queue the record is removed and
// ErrQueueFull is returned so no cancellable residue remains.
func (s *Server) Submit(msg Message, conn *websocket.Conn) (*Task, error) {
	// Hold the admission read lock through creation/enqueue so Shutdown cannot
	// clear the registry in the middle of admission.
	s.admitMu.RLock()
	defer s.admitMu.RUnlock()
	if s.isClosed() {
		return nil, ErrServerClosed
	}

	owner := msg.ownerHandle()
	if owner == "" && conn != nil {
		owner = s.ownerOf(conn)
	}
	if owner == "" {
		owner = s.generateOwner()
		if conn != nil {
			s.bindOwner(conn, owner)
		}
	}
	if conn != nil {
		if current := s.ownerOf(conn); current != owner && msg.ownerHandle() != "" {
			s.bindOwner(conn, owner)
		}
	}

	token := msg.clientToken()
	t := s.tasks.create(owner, token, msg, s.now())

	select {
	case s.queue <- t:
		s.emitPhase(t, eventAccepted)
		t.markAdmitted()
		return t, nil
	default:
		// Not enqueued: release any hypothetical waiter and drop the record.
		t.markAdmitted()
		s.tasks.removeRejected(t)
		return nil, ErrQueueFull
	}
}

// Cancel precisely targets one task reference.
func (s *Server) Cancel(ref, owner string) (*Task, error) {
	t, ok := s.tasks.resolve(ref, owner, s.now())
	if !ok {
		return nil, ErrTaskNotFound
	}

	action, state, enc, cancelFn := t.applyCancel()
	switch action {
	case cancelAlreadyFinished:
		return t, &TaskFinishedError{Ref: t.Ref, State: state}
	case cancelAlreadySignalled:
		if cancelFn != nil {
			cancelFn() // idempotent
		}
		return t, nil
	case cancelRunningSignalled:
		if cancelFn != nil {
			cancelFn()
		}
		if enc != nil {
			enc.Cancel()
		}
		return t, nil
	case cancelQueuedTerminated:
		// Preserve accepted -> terminal ordering if the cancel landed in the
		// tiny window before Submit emitted acceptance.
		t.waitAdmitted(s.stopCh)
		// State is already terminal so the dispatcher skips the task. Emit the
		// single cancelled outcome now.
		s.emitTerminalOnce(t, Status{Cancelled: true})
		return t, nil
	default:
		return t, nil
	}
}

// CancelActive implements the legacy id-less cancel: the submitter's running
// task, else its oldest queued task, else the process-wide running task. The
// process-wide fallback is the historical privileged behavior, so the located
// task is cancelled without an owner check.
func (s *Server) CancelActive(owner string) (*Task, error) {
	var t *Task
	if owner != "" {
		t = s.tasks.activeTaskForOwner(owner)
	}
	if t == nil {
		t = s.tasks.runningTask()
	}
	if t == nil {
		return nil, ErrTaskNotFound
	}
	return s.Cancel(t.Ref, "")
}

// ---------- websocket connection handling ----------

// ServeWS is the /ws endpoint. It keeps the historical function shape so the
// package-level handleConnections wrapper and http registrations stay stable.
func (s *Server) ServeWS(w http.ResponseWriter, r *http.Request) {
	if s.isClosed() {
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Printf("\rWaiting for connection...\u001b[31m websocket connection failed: %v!\u001b[0m", err)
		return
	}

	// Liveness: wait for pongs and periodically ping so a half-open peer is
	// detected and its owner entry released instead of lingering forever.
	_ = ws.SetReadDeadline(time.Now().Add(pongWait))
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(pongWait))
	})
	pingDone := make(chan struct{})
	go s.pingLoop(ws, pingDone)

	s.hub.add(ws)
	requested := ownerFromQuery(r)
	owner := s.addConn(ws, requested)
	defer func() {
		close(pingDone)
		s.removeConn(ws)
	}()

	fmt.Printf("\rWaiting for connection......\u001b[32mconnected!\u001b[0m")

	s.sendConn(ws, Status{Type: eventIdentified, Stage: eventIdentified, Status: eventIdentified, OK: boolPtr(true)}.withIdentity(&Task{OwnerID: owner}))
	s.replayOwner(ws, owner)

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			fmt.Printf("\rWaiting for connection...\u001b[31mdisconnected!\u001b[0m")
			return
		}
		// Any complete frame also proves the peer is alive.
		_ = ws.SetReadDeadline(time.Now().Add(pongWait))

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			s.sendConn(ws, errorEvent("", fmt.Errorf("invalid message: %w", err)))
			continue
		}

		s.dispatchMessage(ws, msg)
	}
}

const (
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
)

func (s *Server) pingLoop(ws *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				return
			}
		}
	}
}

func (s *Server) dispatchMessage(ws *websocket.Conn, msg Message) {
	owner := s.ownerOf(ws)

	switch msg.Type {
	case msgIdentify, msgHello, msgRegister:
		if h := msg.ownerHandle(); h != "" && h != owner {
			owner = s.bindOwner(ws, h)
		}
		s.sendConn(ws, Status{Type: eventIdentified, Stage: eventIdentified, Status: eventIdentified, OK: boolPtr(true)}.withIdentity(&Task{OwnerID: owner}))
		s.replayOwner(ws, owner)

	case msgEncode:
		_, err := s.Submit(msg, ws)
		if err != nil {
			ev := errorEvent(eventAccepted, err)
			if token := msg.clientToken(); token != "" {
				ev.ClientTaskID = token
				ev.ClientTaskIDSnake = token
			}
			s.sendConn(ws, ev)
		}

	case msgCancel:
		ref := msg.taskRef()
		if ref == "" {
			t, err := s.CancelActive(owner)
			if err != nil {
				s.sendConn(ws, s.cancelErrorEvent(msg, "", owner, err))
				return
			}
			ref = t.Ref
			s.sendCancelAck(ws, t)
			return
		}
		t, err := s.Cancel(ref, owner)
		if err != nil {
			s.sendConn(ws, s.cancelErrorEvent(msg, ref, owner, err))
			return
		}
		s.sendCancelAck(ws, t)

	case msgQuery, msgStatus:
		ref := msg.taskRef()
		t, ok := s.tasks.resolve(ref, owner, s.now())
		if !ok {
			s.sendConn(ws, errorEvent("query", ErrTaskNotFound, withRequestedRef(msg)))
			return
		}
		s.sendConn(ws, s.snapshotEvent(t))

	case msgList, msgTasks:
		for _, t := range s.tasks.activeForOwner(owner) {
			s.sendConn(ws, s.snapshotEvent(t))
		}

	default:
		s.sendConn(ws, errorEvent("", fmt.Errorf("unknown message type %q", msg.Type)))
	}
}

func (s *Server) cancelErrorEvent(msg Message, ref, owner string, err error) Status {
	ev := errorEvent(eventCancelled, err, withRequestedRef(msg))
	if ref != "" {
		ev.ID = ref
		ev.TaskID = ref
		ev.TaskIDSnake = ref
	}
	if owner != "" {
		ev = ev.withIdentity(&Task{OwnerID: owner})
		if ref != "" {
			ev.ID, ev.TaskID, ev.TaskIDSnake = ref, ref, ref
		}
	}
	return ev
}

func withRequestedRef(msg Message) func(*Status) {
	return func(st *Status) {
		ref := msg.taskRef()
		if ref != "" {
			st.ID = ref
			st.TaskID = ref
			st.TaskIDSnake = ref
		}
		if token := msg.clientToken(); token != "" {
			st.ClientTaskID = token
			st.ClientTaskIDSnake = token
		}
	}
}

func (s *Server) sendCancelAck(ws *websocket.Conn, t *Task) {
	state := t.State()
	ack := Status{
		Type:   eventCancelAck,
		Stage:  eventCancelAck,
		State:  string(state),
		Status: string(state),
		OK:     boolPtr(true),
	}
	if state == stateCancelled {
		ack.Cancelled = true
	}
	s.sendConn(ws, ack.withIdentity(t))
}

// ---------- event construction and delivery ----------

func (s *Server) emitPhase(t *Task, typ string) {
	t.emitMu.Lock()
	defer t.emitMu.Unlock()

	state := t.State()
	ev := Status{Type: typ, Stage: typ, State: string(state), Status: typ}
	if typ == eventAccepted {
		ev.Queued = len(s.queue)
	}
	s.sendOwner(t.OwnerID, ev.withIdentity(t))
}

func (s *Server) reportProgress(t *Task, p Status) {
	// Check-and-send happens under emitMu, the same lock terminal emission
	// uses, so a terminal event can never be followed by this task's progress.
	t.emitMu.Lock()
	defer t.emitMu.Unlock()

	t.mu.Lock()
	if t.state != stateRunning {
		t.mu.Unlock()
		return
	}
	t.progress = p
	t.mu.Unlock()

	p.Type = eventProgress
	p.Stage = eventProgress
	p.State = string(stateRunning)
	p.Status = eventProgress
	s.sendOwner(t.OwnerID, p.withIdentity(t))
}

// conclude performs the unique non-terminal -> terminal transition and emits
// the single terminal event. A losing caller emits nothing.
func (s *Server) conclude(t *Task, final TaskState, st Status) bool {
	t.emitMu.Lock()
	defer t.emitMu.Unlock()

	t.mu.Lock()
	if t.state.Terminal() {
		t.mu.Unlock()
		return false
	}
	t.state = final
	if final == stateCompleted {
		st.Percent = 100
		st.Completed = true
	}
	if st.Err != "" {
		t.errMsg = st.Err
	}
	t.mu.Unlock()

	s.tasks.touchTerminal(t, s.now())
	if t.emitted {
		return true
	}
	t.emitted = true

	st.Type = final.EventType()
	st.Stage = final.EventType()
	st.State = string(final)
	st.Status = final.EventType()
	s.sendOwner(t.OwnerID, st.withIdentity(t))
	return true
}

// emitTerminalOnce sends the terminal event for a task whose state was already
// made terminal by Cancel (queued path).
func (s *Server) emitTerminalOnce(t *Task, st Status) {
	t.emitMu.Lock()
	defer t.emitMu.Unlock()
	if t.emitted {
		return
	}
	t.emitted = true
	s.tasks.touchTerminal(t, s.now())

	state := t.State()
	if st.Type == "" {
		st.Type = state.EventType()
	}
	st.Stage = state.EventType()
	st.State = string(state)
	st.Status = state.EventType()
	s.sendOwner(t.OwnerID, st.withIdentity(t))
}

func (s *Server) snapshotEvent(t *Task) Status {
	p := t.latestProgress()
	state := t.State()
	ev := Status{
		Type:    eventSnapshot,
		Stage:   "status",
		State:   string(state),
		Status:  string(state),
		Replay:  true,
		Percent: p.Percent,
		Speed:   p.Speed,
		FPS:     p.FPS,
		Frame:   p.Frame,
	}
	if state == stateCompleted {
		ev.Percent = 100
		ev.Completed = true
	}
	if state == stateCancelled {
		ev.Cancelled = true
	}
	if msg := t.Err(); msg != "" {
		ev.Err = msg
		ev.Error = msg
	}
	return ev.withIdentity(t)
}

func (s *Server) replayOwner(ws *websocket.Conn, owner string) {
	for _, t := range s.tasks.activeForOwner(owner) {
		s.sendConn(ws, s.snapshotEvent(t))
	}
}

// ---------- owner connection set ----------

func (s *Server) addConn(c *websocket.Conn, requestedOwner string) string {
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()

	owner := requestedOwner
	if owner == "" {
		s.ownerSeq++
		owner = fmt.Sprintf("anon-%d", s.ownerSeq)
	}
	s.connOwner[c] = owner
	if s.owners[owner] == nil {
		s.owners[owner] = make(map[*websocket.Conn]bool)
	}
	s.owners[owner][c] = true
	return owner
}

func (s *Server) removeConn(c *websocket.Conn) {
	s.ownerMu.Lock()
	if owner, ok := s.connOwner[c]; ok {
		if set := s.owners[owner]; set != nil {
			delete(set, c)
			if len(set) == 0 {
				delete(s.owners, owner)
			}
		}
		delete(s.connOwner, c)
	}
	s.ownerMu.Unlock()
	s.hub.remove(c)
}

func (s *Server) bindOwner(c *websocket.Conn, owner string) string {
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()

	if old := s.connOwner[c]; old != "" && old != owner {
		if set := s.owners[old]; set != nil {
			delete(set, c)
			if len(set) == 0 {
				delete(s.owners, old)
			}
		}
	}
	s.connOwner[c] = owner
	if s.owners[owner] == nil {
		s.owners[owner] = make(map[*websocket.Conn]bool)
	}
	s.owners[owner][c] = true
	return owner
}

func (s *Server) ownerOf(c *websocket.Conn) string {
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	return s.connOwner[c]
}

func (s *Server) generateOwner() string {
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	s.ownerSeq++
	return fmt.Sprintf("anon-%d", s.ownerSeq)
}

func (s *Server) ownerConns(owner string) []*websocket.Conn {
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	set := s.owners[owner]
	out := make([]*websocket.Conn, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	return out
}

func (s *Server) sendOwner(owner string, v any) {
	for _, c := range s.ownerConns(owner) {
		s.hub.send(c, v)
	}
}

func (s *Server) sendConn(c *websocket.Conn, v any) {
	s.hub.send(c, v)
}

func ownerFromQuery(r *http.Request) string {
	q := r.URL.Query()
	return firstNonEmpty(
		q.Get("clientId"), q.Get("client_id"),
		q.Get("owner"), q.Get("ownerId"),
		q.Get("submitter"), q.Get("submitterId"), q.Get("submitter_id"),
		q.Get("cid"),
	)
}
