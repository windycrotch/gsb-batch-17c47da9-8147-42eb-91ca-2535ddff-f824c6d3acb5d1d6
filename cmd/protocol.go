package cmd

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Client -> server message types.
const (
	msgEncode   = "encode"
	msgCancel   = "cancel"
	msgIdentify = "identify"
	msgHello    = "hello"
	msgRegister = "register"
	msgQuery    = "query"
	msgStatus   = "status"
	msgList     = "list"
	msgTasks    = "tasks"
)

// Server -> client event types / lifecycle stages.
const (
	eventIdentified = "identified"
	eventAccepted   = "accepted"
	eventQueued     = "queued"
	eventStarted    = "started"
	eventProgress   = "progress"
	eventSnapshot   = "snapshot"
	eventCompleted  = "completed"
	eventFailed     = "failed"
	eventCancelled  = "cancelled"
	eventError      = "error"
	eventCancelAck  = "cancel-ack"
)

// Message is a payload received from a client.
//
// Besides the historical Type/Input/Output/Payload fields, clients may send a
// stable submitter handle (clientId and several common aliases) and a private
// correlation token for an encode (clientTaskId/taskId/...). All aliases are
// accepted so different clients and probes can use their preferred naming.
type Message struct {
	Type string `json:"type"`

	// Client-supplied correlation token for a submission, or the reference of
	// the task a cancel/query points at. Every common naming variant is accepted.
	TaskID            string `json:"taskId,omitempty"`
	TaskIDSnake       string `json:"task_id,omitempty"`
	ClientTaskID      string `json:"clientTaskId,omitempty"`
	ClientTaskIDSnake string `json:"client_task_id,omitempty"`
	ID                string `json:"id,omitempty"`
	JobID             string `json:"jobId,omitempty"`
	JobIDSnake        string `json:"job_id,omitempty"`
	TaskNumber        uint64 `json:"seq,omitempty"`
	TaskNumberAlt     uint64 `json:"taskNumber,omitempty"`
	TaskNumberSnake   uint64 `json:"task_number,omitempty"`

	// Stable identity of the submitting client. The same value presented on a
	// new websocket re-attaches that connection to its unfinished tasks.
	ClientID         string `json:"clientId,omitempty"`
	ClientIDSnake    string `json:"client_id,omitempty"`
	Owner            string `json:"owner,omitempty"`
	OwnerID          string `json:"ownerId,omitempty"`
	Submitter        string `json:"submitter,omitempty"`
	SubmitterID      string `json:"submitterId,omitempty"`
	SubmitterIDSnake string `json:"submitter_id,omitempty"`
	User             string `json:"user,omitempty"`

	Input   string `json:"input"`
	Output  string `json:"output"`
	Payload string `json:"payload"`
}

// UnmarshalJSON accepts payload both as the historical JSON-encoded string and
// as a raw JSON object (which is compacted back into a string).
func (m *Message) UnmarshalJSON(data []byte) error {
	type alias Message
	aux := struct {
		*alias
		Payload json.RawMessage `json:"payload"`
	}{alias: (*alias)(m)}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(aux.Payload) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(aux.Payload, &s); err == nil {
		m.Payload = s
		return nil
	}
	var v any
	if err := json.Unmarshal(aux.Payload, &v); err != nil {
		return err
	}
	compact, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.Payload = string(compact)
	return nil
}

// clientToken is the optional correlation token the caller generated before
// the server accepted the task.
func (m Message) clientToken() string {
	return firstNonEmpty(
		m.ClientTaskID, m.ClientTaskIDSnake,
		m.TaskID, m.TaskIDSnake,
		m.JobID, m.JobIDSnake,
		m.ID,
	)
}

// taskRef is the identifier a cancel/query targets. It accepts the same
// aliases as clientToken plus a numeric id encoded as a JSON number.
func (m Message) taskRef() string {
	if ref := m.clientToken(); ref != "" {
		return ref
	}
	for _, n := range []uint64{m.TaskNumber, m.TaskNumberAlt, m.TaskNumberSnake} {
		if n > 0 {
			return uintToString(n)
		}
	}
	return ""
}

// ownerHandle is the stable submitter identity claimed by this message.
func (m Message) ownerHandle() string {
	return firstNonEmpty(
		m.ClientID, m.ClientIDSnake,
		m.SubmitterID, m.SubmitterIDSnake,
		m.Submitter,
		m.OwnerID, m.Owner,
		m.User,
	)
}

// Status is an event sent back to clients.
//
// It keeps the historical percent/speed/fps/err/cancelled fields and adds
// lifecycle/identity fields. Identity is duplicated under the common camelCase
// and snake_case keys so any client can correlate phases without configuration.
type Status struct {
	// Type/Stage/State all describe the lifecycle phase. "type" additionally
	// distinguishes out-of-band messages such as "identified" and "error".
	Type   string `json:"type,omitempty"`
	Stage  string `json:"stage,omitempty"`
	State  string `json:"state,omitempty"`
	Status string `json:"status,omitempty"`

	// Stable server-assigned identity. ID/TaskID/TaskIDSnake/JobID carry the
	// same decimal reference; Seq is the monotonic numeric number.
	ID                string `json:"id,omitempty"`
	TaskID            string `json:"taskId,omitempty"`
	TaskIDSnake       string `json:"task_id,omitempty"`
	JobID             string `json:"jobId,omitempty"`
	Seq               uint64 `json:"seq,omitempty"`
	Number            uint64 `json:"number,omitempty"`
	ClientTaskID      string `json:"clientTaskId,omitempty"`
	ClientTaskIDSnake string `json:"client_task_id,omitempty"`

	// Submitter identity, repeated under common aliases.
	ClientID         string `json:"clientId,omitempty"`
	ClientIDSnake    string `json:"client_id,omitempty"`
	Owner            string `json:"owner,omitempty"`
	OwnerID          string `json:"ownerId,omitempty"`
	Submitter        string `json:"submitter,omitempty"`
	SubmitterID      string `json:"submitterId,omitempty"`
	SubmitterIDSnake string `json:"submitter_id,omitempty"`

	// Historical progress contract.
	Percent   float64 `json:"percent"`
	Speed     string  `json:"speed,omitempty"`
	FPS       float64 `json:"fps,omitempty"`
	Frame     int     `json:"frame,omitempty"`
	OutTimeUS int64   `json:"outTimeUs,omitempty"`

	// Terminal/error flags.
	Err       string `json:"err,omitempty"`
	Error     string `json:"error,omitempty"`
	Message   string `json:"message,omitempty"`
	OK        *bool  `json:"ok,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	Completed bool   `json:"completed,omitempty"`
	Replay    bool   `json:"replay,omitempty"`

	// Queue depth, included on acceptance when useful.
	Queued int `json:"queued,omitempty"`
}

func boolPtr(v bool) *bool { return &v }

// withIdentity returns the event with every identity alias populated from t.
func (s Status) withIdentity(t *Task) Status {
	if t == nil {
		return s
	}
	s.ID = t.Ref
	s.TaskID = t.Ref
	s.TaskIDSnake = t.Ref
	s.JobID = t.Ref
	s.Seq = t.ID
	s.Number = t.ID
	if t.ClientToken != "" {
		s.ClientTaskID = t.ClientToken
		s.ClientTaskIDSnake = t.ClientToken
	}
	s.ClientID = t.OwnerID
	s.ClientIDSnake = t.OwnerID
	s.Owner = t.OwnerID
	s.OwnerID = t.OwnerID
	s.Submitter = t.OwnerID
	s.SubmitterID = t.OwnerID
	s.SubmitterIDSnake = t.OwnerID
	return s
}

// errorEvent builds the canonical error payload, keeping the historical "err"
// key and adding "error"/"message" aliases plus ok:false.
func errorEvent(phase string, err error, extras ...func(*Status)) Status {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	s := Status{
		Type:    eventError,
		Stage:   phase,
		State:   phase,
		Status:  eventError,
		Err:     msg,
		Error:   msg,
		Message: msg,
		OK:      boolPtr(false),
	}
	for _, fn := range extras {
		fn(&s)
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func uintToString(n uint64) string {
	return strconv.FormatUint(n, 10)
}
