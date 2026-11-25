package gopher_test

// Acceptance tests for the shutdown/save -> start/recover room lifecycle.
//
// This file is in the EXTERNAL test package and every identifier carries a
// gsb41143 prefix so it cannot collide with an in-package acceptance suite.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gopher "github.com/hewiefreeman/GopherGameServer"
	"github.com/hewiefreeman/GopherGameServer/actions"
	"github.com/hewiefreeman/GopherGameServer/core"
	"github.com/hewiefreeman/GopherGameServer/database"
)

var gsb41143TypeCounter uint64

func gsb41143Reset() {
	_ = gopher.ShutDown()
	core.ResetRooms()
	core.Resume()
	core.SetServerStarted(false)
	actions.Resume()
	actions.SetServerStarted(false)
	database.Resume()
	database.SetServerStarted(false)
}

func gsb41143FreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func gsb41143WaitTCP(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not listen on %s within %s", addr, timeout)
}

func gsb41143AssertClosed(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 120*time.Millisecond)
		if err != nil {
			return
		}
		_ = c.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected %s to stop accepting connections", addr)
}

func gsb41143Settings(dir string, port int, recovery bool) *gopher.ServerSettings {
	return &gopher.ServerSettings{
		ServerName:        "!server!",
		HostName:          "127.0.0.1",
		HostAlias:         "localhost",
		IP:                "127.0.0.1",
		Port:              port,
		UserRoomControl:   true,
		RoomDeleteOnLeave: false,
		EnableSqlFeatures: false,
		EncryptionCost:    4,
		EnableRecovery:    recovery,
		RecoveryLocation:  dir,
		AdminLogin:        "admin",
		AdminPassword:     "password",
	}
}

type gsb41143Env struct {
	t        *testing.T
	dir      string
	port     int
	settings *gopher.ServerSettings
}

func gsb41143NewEnv(t *testing.T, recovery bool) *gsb41143Env {
	t.Helper()
	gsb41143Reset()
	dir := t.TempDir()
	port := gsb41143FreePort(t)
	cfg := gsb41143Settings(dir, port, recovery)
	env := &gsb41143Env{t: t, dir: dir, port: port, settings: cfg}
	t.Cleanup(gsb41143Reset)
	return env
}

func (e *gsb41143Env) start() {
	e.t.Helper()
	go gopher.Start(e.settings)
	gsb41143WaitTCP(e.t, e.port, 5*time.Second)
}

func (e *gsb41143Env) shutdown() {
	e.t.Helper()
	if err := gopher.ShutDown(); err != nil {
		e.t.Fatalf("ShutDown error: %v", err)
	}
}

func (e *gsb41143Env) restart() {
	e.t.Helper()
	e.port = gsb41143FreePort(e.t)
	e.settings.Port = e.port
	go gopher.Start(e.settings)
	gsb41143WaitTCP(e.t, e.port, 5*time.Second)
}

func (e *gsb41143Env) wsURL() string {
	return fmt.Sprintf("ws://127.0.0.1:%d/ws", e.port)
}

func gsb41143RoomType(t *testing.T, configure func(*core.RoomType)) string {
	t.Helper()
	n := atomic.AddUint64(&gsb41143TypeCounter, 1)
	name := fmt.Sprintf("gsb41143_type_%d", n)
	rt := core.NewRoomType(name, true)
	if configure != nil {
		configure(rt)
	}
	return name
}

func gsb41143GRFFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	out := []string{}
	for _, en := range entries {
		if !en.IsDir() && filepath.Ext(en.Name()) == ".grf" {
			out = append(out, en.Name())
		}
	}
	sort.Strings(out)
	return out
}

func gsb41143TempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	out := []string{}
	for _, en := range entries {
		if !en.IsDir() && len(en.Name()) >= 8 && en.Name()[:8] == ".grf-tmp" {
			out = append(out, en.Name())
		}
	}
	return out
}

func gsb41143WriteFile(t *testing.T, path, contents string, modTime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if !modTime.IsZero() {
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
}

// gsb41143ArchiveJSON builds a recovery document using the historical
// single-letter field names (R/T/P/O/M/I/V).
func gsb41143ArchiveJSON(rooms map[string]map[string]interface{}) []byte {
	doc := map[string]interface{}{"R": rooms}
	b, _ := json.Marshal(doc)
	return b
}

func gsb41143JSONEqual(a, b interface{}) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	var ai, bi interface{}
	_ = json.Unmarshal(ab, &ai)
	_ = json.Unmarshal(bb, &bi)
	return fmt.Sprintf("%v", ai) == fmt.Sprintf("%v", bi)
}

// A1
func TestGSB41143_FullLifecyclePreservesRoomState(t *testing.T) {
	env := gsb41143NewEnv(t, true)
	rt := gsb41143RoomType(t, nil)
	env.start()

	pub, err := core.NewRoom("gsb41143_public", rt, false, 16, "server-bot")
	if err != nil {
		t.Fatal(err)
	}
	pub.SetVariable("map", "arena")
	pub.SetVariable("round", float64(3))
	pub.SetVariable("config", map[string]interface{}{"friendlyFire": false})

	priv, err := core.NewRoom("gsb41143_private", rt, true, 4, "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"bob", "carol"} {
		if err := priv.AddInvite(u); err != nil {
			t.Fatal(err)
		}
	}
	priv.SetVariable("score", float64(42))
	priv.SetVariable("mode", "ranked")

	if core.RoomCount() != 2 {
		t.Fatalf("pre-shutdown rooms=%d", core.RoomCount())
	}
	env.shutdown()

	if files := gsb41143GRFFiles(t, env.dir); len(files) != 1 {
		t.Fatalf("want exactly one archive, got %v", files)
	}
	if tmp := gsb41143TempFiles(t, env.dir); len(tmp) != 0 {
		t.Fatalf("leftover temp files: %v", tmp)
	}

	env.restart()

	if core.RoomCount() != 2 {
		t.Fatalf("recovered rooms=%d", core.RoomCount())
	}
	gotPub, err := core.GetRoom("gsb41143_public")
	if err != nil {
		t.Fatalf("public not recovered: %v", err)
	}
	if gotPub.Type() != rt || gotPub.IsPrivate() || gotPub.Owner() != "server-bot" || gotPub.MaxUsers() != 16 {
		t.Fatalf("public fields wrong: %s/%v/%s/%d", gotPub.Type(), gotPub.IsPrivate(), gotPub.Owner(), gotPub.MaxUsers())
	}
	vars, _ := gotPub.GetVariables(nil)
	wantVars := map[string]interface{}{
		"map":   "arena",
		"round": float64(3),
		"config": map[string]interface{}{
			"friendlyFire": false,
		},
	}
	if !gsb41143JSONEqual(vars, wantVars) {
		t.Fatalf("public vars mismatch: %#v", vars)
	}

	gotPriv, err := core.GetRoom("gsb41143_private")
	if err != nil {
		t.Fatalf("private not recovered: %v", err)
	}
	if !gotPriv.IsPrivate() || gotPriv.Owner() != "alice" || gotPriv.MaxUsers() != 4 || gotPriv.Type() != rt {
		t.Fatal("private fields mismatch")
	}
	inv, _ := gotPriv.InviteList()
	sort.Strings(inv)
	if len(inv) != 2 || inv[0] != "bob" || inv[1] != "carol" {
		t.Fatalf("invites mismatch: %#v", inv)
	}
	pv, _ := gotPriv.GetVariables(nil)
	if pv["score"] != float64(42) || pv["mode"] != "ranked" {
		t.Fatalf("private vars mismatch: %#v", pv)
	}
}

// A2
func TestGSB41143_CorruptLatestFallsBackToOlderArchive(t *testing.T) {
	env := gsb41143NewEnv(t, true)
	rt := gsb41143RoomType(t, nil)

	good := gsb41143ArchiveJSON(map[string]map[string]interface{}{
		"gsb41143_fallback_good": {
			"T": rt, "P": true, "O": "owner-x", "M": 0,
			"I": []string{"zoe"},
			"V": map[string]interface{}{"stage": "final"},
		},
	})
	goodPath := filepath.Join(env.dir, "Gopher Recovery - 2020-01-01 00-00-00.grf")
	gsb41143WriteFile(t, goodPath, string(good), time.Now().Add(-time.Hour))

	corruptName := "Gopher Recovery - 2099-01-02 03-04-05.grf"
	gsb41143WriteFile(t, filepath.Join(env.dir, corruptName),
		"{ broken json ,,, ", time.Now().Add(2*time.Hour))

	env.start()

	if _, err := core.GetRoom("gsb41143_fallback_good"); err != nil {
		t.Fatalf("fallback room missing: %v", err)
	}
	if core.RoomCount() != 1 {
		t.Fatalf("want 1 room, got %d", core.RoomCount())
	}
	r := gopher.LastRecoveryReport()
	if r.Archive != filepath.Base(goodPath) {
		t.Fatalf("selected archive=%q want %q (skipped=%v)", r.Archive, filepath.Base(goodPath), r.SkippedArchives)
	}
	found := false
	for _, n := range r.SkippedArchives {
		if n == corruptName {
			found = true
		}
	}
	if !found {
		t.Fatalf("corrupt file not reported skipped: %#v", r.SkippedArchives)
	}
}

// A3 (concurrent duplicate shutdown through the exported entry)
func TestGSB41143_ConcurrentShutdownLeavesCompleteArchivesAndZeroConns(t *testing.T) {
	env := gsb41143NewEnv(t, true)
	rt := gsb41143RoomType(t, nil)
	env.start()

	want := []string{"gsb41143_c1", "gsb41143_c2", "gsb41143_c3"}
	for i, name := range want {
		r, err := core.NewRoom(name, rt, i == 1, 0, "owner")
		if err != nil {
			t.Fatal(err)
		}
		r.SetVariable("idx", float64(i))
		if i == 1 {
			_ = r.AddInvite("inv")
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := gopher.ShutDown(); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent ShutDown error: %v", err)
	}

	if n := gopher.ClientsConnected(); n != 0 {
		t.Fatalf("ClientsConnected=%d want 0", n)
	}
	if tmp := gsb41143TempFiles(t, env.dir); len(tmp) != 0 {
		t.Fatalf("temp leftovers: %v", tmp)
	}
	archives := gsb41143GRFFiles(t, env.dir)
	if len(archives) == 0 {
		t.Fatal("no archives")
	}
	for _, name := range archives {
		b, err := os.ReadFile(filepath.Join(env.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			R map[string]json.RawMessage `json:"R"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("archive %s not complete JSON: %v", name, err)
		}
		if len(doc.R) != len(want) {
			t.Fatalf("archive %s rooms=%d want %d", name, len(doc.R), len(want))
		}
		for _, w := range want {
			if _, ok := doc.R[w]; !ok {
				t.Fatalf("archive %s missing %s", name, w)
			}
		}
	}

	// Restart via the real entry and confirm the newest archive restores fully.
	env.restart()
	if core.RoomCount() != len(want) {
		t.Fatalf("after restart rooms=%d want %d", core.RoomCount(), len(want))
	}
}

// A4
func TestGSB41143_PartialRebuildFailureRollsBack(t *testing.T) {
	env := gsb41143NewEnv(t, true)
	goodType := gsb41143RoomType(t, nil)
	panicType := gsb41143RoomType(t, func(rt *core.RoomType) {
		rt.SetCreateCallback(func(r *core.Room) {
			if r.Name() == "gsb41143_panic_room" {
				panic("boom")
			}
		})
	})

	archive := gsb41143ArchiveJSON(map[string]map[string]interface{}{
		"gsb41143_good_room": {
			"T": goodType, "P": false, "O": "owner", "M": 8,
			"V": map[string]interface{}{"k": "v"},
		},
		"gsb41143_bad_type": {
			"T": "gsb41143_missing_type", "P": false, "O": "owner",
		},
		"gsb41143_panic_room": {
			"T": panicType, "P": true, "O": "boss", "I": []string{"x"},
		},
	})
	gsb41143WriteFile(t, filepath.Join(env.dir, "Gopher Recovery - 2021-01-01 00-00-00.grf"),
		string(archive), time.Time{})

	env.start()

	if core.RoomCount() != 1 {
		t.Fatalf("want only good room, count=%d state=%#v", core.RoomCount(), core.GetRoomsState())
	}
	good, err := core.GetRoom("gsb41143_good_room")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := good.GetVariable("k"); v != "v" {
		t.Fatalf("good var=%v", v)
	}
	for _, bad := range []string{"gsb41143_bad_type", "gsb41143_panic_room"} {
		if _, err := core.GetRoom(bad); err == nil {
			t.Errorf("failed room %s still present", bad)
		}
		if _, ok := core.GetRoomsState()[bad]; ok {
			t.Errorf("failed room %s residual state", bad)
		}
	}
	if rep := gopher.LastRecoveryReport(); rep.Restored != 1 || len(rep.Failed) != 2 {
		t.Fatalf("report restored=%d failed=%d", rep.Restored, len(rep.Failed))
	}
}

// A5 empty
func TestGSB41143_NoArchiveStartsAndReportsNoRooms(t *testing.T) {
	env := gsb41143NewEnv(t, true)
	env.start()
	rep := gopher.LastRecoveryReport()
	if rep.Archive != "" || rep.Restored != 0 {
		t.Fatalf("unexpected report: %#v", rep)
	}
	env.shutdown()
}

// A5 corrupt-only
func TestGSB41143_OnlyCorruptArchivesStillStart(t *testing.T) {
	env := gsb41143NewEnv(t, true)
	gsb41143WriteFile(t, filepath.Join(env.dir, "Gopher Recovery - 2098-01-01 00-00-00.grf"), "nope", time.Time{})
	gsb41143WriteFile(t, filepath.Join(env.dir, "Gopher Recovery - 2099-01-01 00-00-00.grf"), "{bad", time.Time{})
	env.start()
	if gopher.LastRecoveryReport().Archive != "" {
		t.Fatal("expected no selected archive")
	}
	env.shutdown()
}

// A7
func TestGSB41143_ShutdownZeroesConnsAndRejectsNew(t *testing.T) {
	env := gsb41143NewEnv(t, false)
	env.start()

	d := gsb41143Dialer()
	c, _, err := d.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && gopher.ClientsConnected() != 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if n := gopher.ClientsConnected(); n != 1 {
		t.Fatalf("ClientsConnected=%d want 1", n)
	}
	env.shutdown()
	if n := gopher.ClientsConnected(); n != 0 {
		t.Fatalf("after shutdown ClientsConnected=%d", n)
	}
	gsb41143AssertClosed(t, env.port, 2*time.Second)
}
