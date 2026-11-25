package cmd

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"
)

// TaskState is the lifecycle position of an encode task.
type TaskState string

const (
	stateQueued     TaskState = "queued"
	stateRunning    TaskState = "running"
	stateCancelling TaskState = "cancelling"
	stateCompleted  TaskState = "completed"
	stateFailed     TaskState = "failed"
	stateCancelled  TaskState = "cancelled"
)

// Terminal reports whether the state is one of the unique final outcomes.
func (s TaskState) Terminal() bool {
	switch s {
	case stateCompleted, stateFailed, stateCancelled:
		return true
	default:
		return false
	}
}

// EventType maps a state to the wire event type used for it.
func (s TaskState) EventType() string {
	switch s {
	case stateQueued:
		return eventQueued
	case stateRunning, stateCancelling:
		return eventStarted
	case stateCompleted:
		return eventCompleted
	case stateFailed:
		return eventFailed
	case stateCancelled:
		return eventCancelled
	default:
		return string(s)
	}
}

// ErrTaskNotFound means no live or retained task matches the reference.
var ErrTaskNotFound = errors.New("task not found")

// ErrQueueFull means the bounded waiting queue has no free slot.
var ErrQueueFull = errors.New("too many encodes queued, try again later")

// ErrServerClosed means the server is shutting down.
var ErrServerClosed = errors.New("server is closed")

// TaskFinishedError is returned by Cancel when the task already reached a
// terminal state. It makes "already ended" distinguishable from "missing".
type TaskFinishedError struct {
	Ref   string
	State TaskState
}

func (e *TaskFinishedError) Error() string {
	return "task " + e.Ref + " already " + string(e.State)
}

// encoderHandle is the cancellable part of the real ffmpeg runner.
type encoderHandle interface {
	Cancel()
}

// Task is one encode with stable ownership and a revocable lifecycle.
//
// All state transitions happen under mu. emitMu orders wire events for one
// task so a progress event can never overtake its terminal event.
type Task struct {
	ID          uint64
	Ref         string
	ClientToken string
	OwnerID     string

	Input   string
	Output  string
	Payload string

	CreatedAt time.Time

	ctx       context.Context
	cancelCtx context.CancelFunc

	emitMu sync.Mutex
	mu     sync.Mutex
	state  TaskState

	// admitted is closed once the accepted event has been emitted. The
	// dispatcher waits on it before emitting started, so phase events can
	// never overtake acceptance.
	admitted  chan struct{}
	admitOnce sync.Once

	encoder  encoderHandle
	progress Status
	errMsg   string

	finishedAt time.Time
	expiresAt  time.Time
	emitted    bool
}

// State returns the current lifecycle state.
func (t *Task) State() TaskState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// Context is cancelled when cancellation is requested or the server shuts down.
func (t *Task) Context() context.Context {
	return t.ctx
}

// markAdmitted releases the dispatcher to emit "started" after "accepted" has
// been written.
func (t *Task) markAdmitted() {
	t.admitOnce.Do(func() { close(t.admitted) })
}

// waitAdmitted blocks until acceptance was emitted or the server stops.
func (t *Task) waitAdmitted(stop <-chan struct{}) {
	select {
	case <-t.admitted:
	case <-stop:
	case <-t.ctx.Done():
	}
}

// Err returns the stored terminal error message, if any.
func (t *Task) Err() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.errMsg
}

// latestProgress returns the most recent progress snapshot.
func (t *Task) latestProgress() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.progress
}

func (t *Task) setEncoder(e encoderHandle) {
	t.mu.Lock()
	t.encoder = e
	// Cancel may have arrived in the window after the state became running
	// but before the encoder existed. Apply it to the fresh handle so the
	// underlying process is still stopped.
	cancelNow := t.state == stateCancelling
	t.mu.Unlock()
	if cancelNow && e != nil {
		e.Cancel()
	}
}

// cancelTransition is the atomic decision made by Cancel. The caller performs
// the returned side effects (signalling the encoder / emitting the event).
type cancelAction int

const (
	cancelAlreadyFinished cancelAction = iota
	cancelQueuedTerminated
	cancelRunningSignalled
	cancelAlreadySignalled
)

// applyCancel atomically moves the task as far toward cancellation as possible.
func (t *Task) applyCancel() (cancelAction, TaskState, encoderHandle, context.CancelFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch {
	case t.state.Terminal():
		return cancelAlreadyFinished, t.state, nil, nil
	case t.state == stateCancelling:
		return cancelAlreadySignalled, t.state, nil, t.cancelCtx
	case t.state == stateRunning:
		t.state = stateCancelling
		return cancelRunningSignalled, t.state, t.encoder, t.cancelCtx
	default: // queued: terminate atomically so the dispatcher can never start it.
		t.state = stateCancelled
		t.finishedAt = time.Now()
		return cancelQueuedTerminated, t.state, nil, nil
	}
}

// registry owns task records and lookup indexes.
type registry struct {
	mu     sync.Mutex
	seq    uint64
	ttl    time.Duration
	tasks  map[string]*Task
	tokens map[string]string // owner + "\x00" + client token -> ref
}

func newRegistry(terminalTTL time.Duration) *registry {
	return &registry{
		ttl:    terminalTTL,
		tasks:  make(map[string]*Task),
		tokens: make(map[string]string),
	}
}

func tokenKey(owner, token string) string { return owner + "\x00" + token }

// create allocates the unique id and stores the task as queued.
func (r *registry) create(owner, token string, msg Message, now time.Time) *Task {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seq++
	ref := strconv.FormatUint(r.seq, 10)
	ctx, cancel := context.WithCancel(context.Background())
	t := &Task{
		ID:          r.seq,
		Ref:         ref,
		ClientToken: token,
		OwnerID:     owner,
		Input:       msg.Input,
		Output:      msg.Output,
		Payload:     msg.Payload,
		CreatedAt:   now,
		ctx:         ctx,
		cancelCtx:   cancel,
		state:       stateQueued,
		admitted:    make(chan struct{}),
	}
	r.tasks[ref] = t
	if token != "" {
		r.tokens[tokenKey(owner, token)] = ref
	}
	return t
}

// removeRejected drops a task that never made it into the queue. If a
// concurrent cancel already transitioned it to a terminal state, the record is
// retained for its terminal lookup/event instead of being deleted.
func (r *registry) removeRejected(t *Task) {
	t.mu.Lock()
	untouched := t.state == stateQueued
	t.mu.Unlock()
	if !untouched {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.tasks[t.Ref]; ok && current != t {
		return
	}
	delete(r.tasks, t.Ref)
	if t.ClientToken != "" {
		key := tokenKey(t.OwnerID, t.ClientToken)
		if r.tokens[key] == t.Ref {
			delete(r.tokens, key)
		}
	}
}

// delete removes a task record unconditionally (explicit cleanup).
func (r *registry) delete(ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.tasks[ref]; ok {
		if t.ClientToken != "" {
			key := tokenKey(t.OwnerID, t.ClientToken)
			if r.tokens[key] == ref {
				delete(r.tokens, key)
			}
		}
		delete(r.tasks, ref)
	}
}

// get returns a task by server reference, lazily expiring cleaned records.
func (r *registry) get(ref string, now time.Time) (*Task, bool) {
	r.mu.Lock()
	t, ok := r.tasks[ref]
	r.mu.Unlock()
	if !ok {
		return nil, false
	}
	if r.expired(t, now) {
		r.delete(ref)
		return nil, false
	}
	return t, true
}

// resolve finds a task by server id/number or by a client correlation token.
//
// When owner is non-empty the lookup is owner-scoped: another submitter's task
// is treated as not found so ids cannot be enumerated to read or cancel other
// clients' work. An empty owner is the privileged internal path (legacy
// process-wide cancel).
func (r *registry) resolve(refOrToken, owner string, now time.Time) (*Task, bool) {
	if refOrToken == "" {
		return nil, false
	}
	if t, ok := r.get(refOrToken, now); ok {
		if owner == "" || t.OwnerID == owner {
			return t, true
		}
		return nil, false
	}

	r.mu.Lock()
	ref := ""
	if owner != "" {
		ref = r.tokens[tokenKey(owner, refOrToken)]
	} else {
		for candidateRef, t := range r.tasks {
			if t.ClientToken == refOrToken {
				ref = candidateRef
				break
			}
		}
	}
	r.mu.Unlock()

	if ref == "" {
		return nil, false
	}
	t, ok := r.get(ref, now)
	if !ok {
		return nil, false
	}
	if owner != "" && t.OwnerID != owner {
		return nil, false
	}
	return t, true
}

func (r *registry) expired(t *Task, now time.Time) bool {
	if r.ttl <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state.Terminal() && !t.expiresAt.IsZero() && now.After(t.expiresAt)
}

// touchTerminal stores completion time and retention expiry.
func (r *registry) touchTerminal(t *Task, now time.Time) {
	t.mu.Lock()
	t.finishedAt = now
	if r.ttl > 0 {
		t.expiresAt = now.Add(r.ttl)
	}
	t.mu.Unlock()
}

// reap removes terminal tasks whose retention has elapsed. It returns how many
// records were removed.
func (r *registry) reap(now time.Time) int {
	r.mu.Lock()
	var dead []string
	for ref, t := range r.tasks {
		t.mu.Lock()
		terminal := t.state.Terminal()
		expires := t.expiresAt
		t.mu.Unlock()
		if terminal && r.ttl > 0 && !expires.IsZero() && now.After(expires) {
			dead = append(dead, ref)
		}
	}
	r.mu.Unlock()
	for _, ref := range dead {
		r.delete(ref)
	}
	return len(dead)
}

// activeForOwner returns the owner's non-terminal tasks ordered by id.
func (r *registry) activeForOwner(owner string) []*Task {
	r.mu.Lock()
	var out []*Task
	for _, t := range r.tasks {
		if t.OwnerID != owner {
			continue
		}
		t.mu.Lock()
		active := !t.state.Terminal()
		t.mu.Unlock()
		if active {
			out = append(out, t)
		}
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// activeTaskForOwner returns one cancellable task for a legacy id-less cancel:
// a running/cancelling task is preferred, otherwise the oldest queued one.
func (r *registry) activeTaskForOwner(owner string) *Task {
	tasks := r.activeForOwner(owner)
	var queued *Task
	for _, t := range tasks {
		switch t.State() {
		case stateRunning, stateCancelling:
			return t
		case stateQueued:
			if queued == nil || t.ID < queued.ID {
				queued = t
			}
		}
	}
	return queued
}

// runningTask returns the single running task (dispatcher is serial).
func (r *registry) runningTask() *Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		switch t.State() {
		case stateRunning, stateCancelling:
			return t
		}
	}
	return nil
}

// all returns a snapshot of all task records.
func (r *registry) all() []*Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Task, 0, len(r.tasks))
	for _, t := range r.tasks {
		out = append(out, t)
	}
	return out
}

// reset drops every record (used by shutdown).
func (r *registry) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		t.cancelCtx()
	}
	r.tasks = make(map[string]*Task)
	r.tokens = make(map[string]string)
}
