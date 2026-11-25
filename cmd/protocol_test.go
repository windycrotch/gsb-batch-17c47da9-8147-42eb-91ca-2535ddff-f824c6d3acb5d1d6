package cmd

import (
	"encoding/json"
	"testing"
)

func TestMessageAliases(t *testing.T) {
	cases := []struct {
		name  string
		json  string
		tok   string
		ref   string
		owner string
	}{
		{"camel taskId", `{"type":"cancel","taskId":"7"}`, "", "7", ""},
		{"snake task_id", `{"type":"cancel","task_id":"8"}`, "", "8", ""},
		{"id", `{"type":"cancel","id":"abc"}`, "", "abc", ""},
		{"jobId", `{"type":"cancel","jobId":"9"}`, "", "9", ""},
		{"numeric seq", `{"type":"cancel","seq":42}`, "", "42", ""},
		{"client token", `{"type":"encode","clientTaskId":"tok"}`, "tok", "tok", ""},
		{"owner camel", `{"type":"encode","clientId":"alice"}`, "", "", "alice"},
		{"owner snake", `{"type":"encode","client_id":"bob"}`, "", "", "bob"},
		{"submitter id snake", `{"type":"encode","submitter_id":"carol"}`, "", "", "carol"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m Message
			if err := json.Unmarshal([]byte(tc.json), &m); err != nil {
				t.Fatal(err)
			}
			if got := m.clientToken(); tc.tok != "" && got != tc.tok {
				t.Fatalf("token = %q want %q", got, tc.tok)
			}
			if got := m.taskRef(); tc.ref != "" && got != tc.ref {
				t.Fatalf("ref = %q want %q", got, tc.ref)
			}
			if got := m.ownerHandle(); tc.owner != "" && got != tc.owner {
				t.Fatalf("owner = %q want %q", got, tc.owner)
			}
		})
	}
}

func TestPayloadObjectAccepted(t *testing.T) {
	var m Message
	raw := `{"type":"encode","payload":{"video":{"codec":"libx264"}}}`
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m.Payload == "" || m.Payload[0] != '{' {
		t.Fatalf("object payload not compacted to string: %q", m.Payload)
	}

	var m2 Message
	if err := json.Unmarshal([]byte(`{"type":"encode","payload":"{\"raw\":[\"-c copy\"]}"}`), &m2); err != nil {
		t.Fatal(err)
	}
	if m2.Payload != `{"raw":["-c copy"]}` {
		t.Fatalf("string payload = %q", m2.Payload)
	}
}

func TestStatusWireIdentityKeys(t *testing.T) {
	st := Status{Type: eventAccepted}.withIdentity(&Task{ID: 5, Ref: "5", OwnerID: "alice", ClientToken: "ct"})
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "taskId", "task_id", "jobId", "seq", "clientTaskId", "clientId", "client_id", "ownerId", "submitterId", "submitter_id"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("wire status missing %q: %s", key, data)
		}
	}
	if raw["seq"].(float64) != 5 {
		t.Fatalf("seq = %v", raw["seq"])
	}
}
