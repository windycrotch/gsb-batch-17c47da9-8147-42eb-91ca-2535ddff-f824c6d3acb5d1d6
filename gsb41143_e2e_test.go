package gopher_test

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hewiefreeman/GopherGameServer/core"
)

func gsb41143Dialer() *websocket.Dialer {
	// Nil Proxy means direct dialing, avoiding environment proxy surprises.
	return &websocket.Dialer{HandshakeTimeout: 3 * time.Second}
}

func gsb41143Send(t *testing.T, c *websocket.Conn, v interface{}) {
	t.Helper()
	if err := c.WriteJSON(v); err != nil {
		t.Fatalf("write json: %v", err)
	}
}

func gsb41143ReadUntil(t *testing.T, c *websocket.Conn, timeout time.Duration,
	pred func(map[string]interface{}) bool) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
		var msg map[string]interface{}
		if err := c.ReadJSON(&msg); err != nil {
			continue
		}
		if pred(msg) {
			return msg
		}
	}
	t.Fatal("timed out waiting for expected websocket message")
	return nil
}

func gsb41143AckAction(msg map[string]interface{}) string {
	body, ok := msg["c"].(map[string]interface{})
	if !ok {
		return ""
	}
	a, _ := body["a"].(string)
	return a
}

func gsb41143WaitAck(t *testing.T, c *websocket.Conn, action string) {
	t.Helper()
	gsb41143ReadUntil(t, c, 3*time.Second, func(m map[string]interface{}) bool {
		return gsb41143AckAction(m) == action
	})
}

func gsb41143GuestLogin(t *testing.T, c *websocket.Conn, name string) {
	t.Helper()
	gsb41143Send(t, c, map[string]interface{}{
		"A": "li",
		"P": map[string]interface{}{"n": name, "g": true},
	})
	gsb41143WaitAck(t, c, "li")
}

func gsb41143Join(t *testing.T, c *websocket.Conn, room string) {
	t.Helper()
	gsb41143Send(t, c, map[string]interface{}{"A": "j", "P": room})
	gsb41143WaitAck(t, c, "j")
}

// A6: recovered room accepts joins and broadcasts user entry over real sockets.
func TestGSB41143_RecoveredRoomJoinAndBroadcast(t *testing.T) {
	env := gsb41143NewEnv(t, true)
	rt := gsb41143RoomType(t, func(r *core.RoomType) {
		r.EnableBroadcastUserEnter()
	})

	env.start()
	if _, err := core.NewRoom("gsb41143_play", rt, false, 0, "!server!"); err != nil {
		t.Fatal(err)
	}
	env.shutdown()
	env.restart()

	d := gsb41143Dialer()
	u1, _, err := d.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("dial u1: %v", err)
	}
	u2, _, err := d.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("dial u2: %v", err)
	}
	t.Cleanup(func() {
		_ = u1.Close()
		_ = u2.Close()
	})

	gsb41143GuestLogin(t, u1, "gsb41143_user_1")
	gsb41143Join(t, u1, "gsb41143_play")

	gsb41143GuestLogin(t, u2, "gsb41143_user_2")
	gsb41143Join(t, u2, "gsb41143_play")

	msg := gsb41143ReadUntil(t, u1, 3*time.Second, func(m map[string]interface{}) bool {
		ev, ok := m["e"].(map[string]interface{})
		return ok && ev["u"] == "gsb41143_user_2"
	})
	if ev := msg["e"].(map[string]interface{}); ev["u"] != "gsb41143_user_2" {
		t.Fatalf("unexpected enter event: %#v", ev)
	}

	room, err := core.GetRoom("gsb41143_play")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && room.NumUsers() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if room.NumUsers() != 2 {
		t.Fatalf("users in room=%d want 2", room.NumUsers())
	}
}
