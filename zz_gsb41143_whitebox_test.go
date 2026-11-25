package gopher

// White-box concurrency check for raw saveState interleaved with ShutDown.
// It intentionally exposes no top-level helpers: every helper is local to the
// single uniquely named test to avoid redeclaration conflicts with any other
// in-package acceptance test file.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hewiefreeman/GopherGameServer/actions"
	"github.com/hewiefreeman/GopherGameServer/core"
	"github.com/hewiefreeman/GopherGameServer/database"
)

func TestGSB41143_Whitebox_InterleavedSaveAndShutdown(t *testing.T) {
	// Inline global reset (start and end) so this test is self-contained.
	reset := func() {
		_ = ShutDown()
		lifeMu.Lock()
		serverStarted = false
		serverPaused = false
		serverStopping = false
		shutdownDone = nil
		shutdownErr = nil
		startExited = nil
		httpServer = nil
		lifeMu.Unlock()
		settings = nil
		conns.reset()
		core.ResetRooms()
		core.Resume()
		core.SetServerStarted(false)
		actions.Resume()
		actions.SetServerStarted(false)
		database.Resume()
		database.SetServerStarted(false)
	}
	reset()
	t.Cleanup(reset)

	dir := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	cfg := &ServerSettings{
		ServerName:       "!server!",
		HostName:         "127.0.0.1",
		IP:               "127.0.0.1",
		Port:             port,
		UserRoomControl:  true,
		EnableRecovery:   true,
		RecoveryLocation: dir,
		AdminLogin:       "admin",
		AdminPassword:    "password",
	}
	settings = cfg

	typeName := fmt.Sprintf("gsb41143_wb_type_%d", time.Now().UnixNano())
	core.SetServerStarted(false)
	core.NewRoomType(typeName, true)

	var wg sync.WaitGroup

	go Start(cfg)

	// Wait for the TCP listener.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	up := false
	for time.Now().Before(deadline) {
		if c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond); derr == nil {
			_ = c.Close()
			up = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !up {
		t.Fatal("server did not start")
	}

	want := []string{"wb-a", "wb-b"}
	for i, name := range want {
		r, rerr := core.NewRoom(name, typeName, i == 1, 0, "owner")
		if rerr != nil {
			t.Fatal(rerr)
		}
		r.SetVariable("i", float64(i))
	}

	errCh := make(chan error, 64)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if serr := ShutDown(); serr != nil {
				select {
				case errCh <- serr:
				default:
				}
			}
		}()
	}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if serr := saveState(); serr != nil {
				select {
				case errCh <- serr:
				default:
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for serr := range errCh {
		t.Errorf("interleaved shutdown/save error: %v", serr)
	}

	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	grfCount := 0
	for _, en := range entries {
		name := en.Name()
		if len(name) >= 8 && name[:8] == ".grf-tmp" {
			t.Fatalf("leftover temp file %s", name)
		}
		if filepath.Ext(name) != ".grf" {
			continue
		}
		grfCount++
		data, derr := os.ReadFile(filepath.Join(dir, name))
		if derr != nil {
			t.Fatal(derr)
		}
		var parsed serverRestore
		if uerr := json.Unmarshal(data, &parsed); uerr != nil {
			t.Fatalf("half/corrupt archive %s: %v", name, uerr)
		}
		if len(parsed.R) != len(want) {
			t.Fatalf("archive %s rooms=%d want %d", name, len(parsed.R), len(want))
		}
	}
	if grfCount == 0 {
		t.Fatal("no .grf archive written")
	}
}
