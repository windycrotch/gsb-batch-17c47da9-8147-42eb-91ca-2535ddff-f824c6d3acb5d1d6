package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// mustListen opens a loopback listener on a random free port.
func mustListen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func mapGetenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestServerRoutesHealthAndStream guards the top-level routing wired in main():
// /health must not fall through to the stream handler.
func TestServerRoutesHealthAndStream(t *testing.T) {
	sessions := NewSessionManager(1)
	handler := NewStreamHandler(&YtDlp{Path: "yt-dlp"}, sessions)
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", handler)
	mux.HandleFunc("/health", HealthHandler)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("health request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("/health = %d %q, want 200 ok (route fell through?)", resp.StatusCode, body)
	}

	resp, err = http.Get(srv.URL + "/stream")
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("/stream without url = %d, want 400", resp.StatusCode)
	}
}

// TestRunShutsDownOnSignal exercises the real signal-to-shutdown wiring: run()
// serves a live stream, receives a shutdown signal on an injected channel,
// terminates the stream, releases its slot and returns promptly.
func TestRunShutsDownOnSignal(t *testing.T) {
	logs := newMockLog(t)
	ytdlpPath, ffmpegPath := installMockBinaries(t)

	// Expose the mock ffmpeg on PATH under its real name so the server's
	// default ffmpeg resolution finds it.
	dir := filepath.Dir(ytdlpPath)
	if err := os.Rename(ffmpegPath, filepath.Join(dir, "ffmpeg"+filepath.Ext(ffmpegPath))); err != nil {
		t.Fatalf("renaming mock ffmpeg: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ln := mustListen(t)
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close() // run() will bind the port itself

	cfg := config{Port: port, YtDlpPath: ytdlpPath, MaxSessions: 1}

	signals := make(chan os.Signal, 1)
	runErr := make(chan error, 1)
	go func() { runErr <- run(context.Background(), cfg, signals) }()

	// Wait for the server to accept connections.
	base := "http://127.0.0.1:" + port
	waitReady(t, base)

	client := &http.Client{Timeout: 10 * time.Second}
	resp := openStream(t, client, base+"/stream?url="+
		"https%3A%2F%2Fexample.com%2Fwatch%3Fv%3Dx%26mockmode%3Dlong")
	readChunk(t, resp)
	logs.waitForCount(t, "ytdlp-start", 1)
	logs.waitForCount(t, "ffmpeg-start", 1)

	// Deliver the shutdown signal.
	signals <- syscall.SIGTERM

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run() returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run() did not return after shutdown signal")
	}

	// The stream's subprocesses must be gone and the client stream must end.
	logs.waitForExit(t, "ytdlp-start", 1)
	logs.waitForExit(t, "ffmpeg-start", 1)

	ended := make(chan struct{})
	go func() {
		defer close(ended)
		io.Copy(io.Discard, resp.Body)
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Error("client stream did not end after shutdown")
	}
	resp.Body.Close()
}

func waitReady(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not become ready")
}

func TestParseConfig_Defaults(t *testing.T) {
	cfg, err := parseConfig(nil, mapGetenv(nil))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want 8080", cfg.Port)
	}
	if cfg.YtDlpPath != "yt-dlp" {
		t.Errorf("YtDlpPath = %q, want yt-dlp", cfg.YtDlpPath)
	}
	if cfg.MaxSessions != DefaultMaxSessions {
		t.Errorf("MaxSessions = %d, want %d", cfg.MaxSessions, DefaultMaxSessions)
	}
}

func TestParseConfig_EnvOverridesDefault(t *testing.T) {
	cfg, err := parseConfig(nil, mapGetenv(map[string]string{
		"PORT":        "9090",
		"YTDLP_PATH":  "/opt/yt-dlp",
		"MAX_STREAMS": "7",
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Port != "9090" || cfg.YtDlpPath != "/opt/yt-dlp" || cfg.MaxSessions != 7 {
		t.Errorf("cfg = %+v, want port 9090, ytdlp /opt/yt-dlp, max 7", cfg)
	}
}

func TestParseConfig_FlagOverridesEnv(t *testing.T) {
	// Same precedence as -port/PORT: an explicit flag beats the environment.
	cfg, err := parseConfig(
		[]string{"-port", "1234", "-max-sessions", "2", "-ytdlp", "/custom/yt-dlp"},
		mapGetenv(map[string]string{
			"PORT":        "9090",
			"YTDLP_PATH":  "/opt/yt-dlp",
			"MAX_STREAMS": "7",
		}),
	)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Port != "1234" {
		t.Errorf("Port = %q, want 1234 (flag should win)", cfg.Port)
	}
	if cfg.YtDlpPath != "/custom/yt-dlp" {
		t.Errorf("YtDlpPath = %q, want /custom/yt-dlp (flag should win)", cfg.YtDlpPath)
	}
	if cfg.MaxSessions != 2 {
		t.Errorf("MaxSessions = %d, want 2 (flag should win)", cfg.MaxSessions)
	}
}

func TestParseConfig_InvalidMaxStreamsFallsBack(t *testing.T) {
	cfg, err := parseConfig(nil, mapGetenv(map[string]string{"MAX_STREAMS": "not-a-number"}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.MaxSessions != DefaultMaxSessions {
		t.Errorf("MaxSessions = %d, want fallback %d", cfg.MaxSessions, DefaultMaxSessions)
	}
}

func TestParseConfig_NonPositiveClamped(t *testing.T) {
	for _, v := range []string{"0", "-3"} {
		cfg, err := parseConfig(nil, mapGetenv(map[string]string{"MAX_STREAMS": v}))
		if err != nil {
			t.Fatalf("parseConfig(%q): %v", v, err)
		}
		if cfg.MaxSessions < 1 {
			t.Errorf("MAX_STREAMS=%q gave MaxSessions=%d, want >= 1", v, cfg.MaxSessions)
		}
	}
}

// TestGracefulShutdownCancelsStreamsAndDrains drives the real shutdown sequence
// (sessions.Shutdown then server.Shutdown) against a live HTTP server and a
// real streaming session, asserting it terminates promptly.
func TestGracefulShutdownCancelsStreamsAndDrains(t *testing.T) {
	logs := newMockLog(t)
	ytdlpPath, ffmpegPath := installMockBinaries(t)
	sessions := NewSessionManager(2)

	handler := NewStreamHandler(&YtDlp{Path: ytdlpPath, FFmpeg: ffmpegPath}, sessions)
	server := newServer("0", handler)

	// Listen explicitly so we can learn the port.
	ln := mustListen(t)
	server.Addr = ln.Addr().String()
	go server.Serve(ln)

	client := &http.Client{Timeout: 10 * time.Second}
	resp := openStream(t, client, "http://"+ln.Addr().String()+"/stream?url="+
		"https%3A%2F%2Fexample.com%2Fwatch%3Fv%3Dx%26mockmode%3Dlong")
	readChunk(t, resp)
	logs.waitForCount(t, "ffmpeg-start", 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- gracefulShutdown(ctx, server, sessions) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("gracefulShutdown: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("gracefulShutdown did not return; shutdown was blocked")
	}

	if got := sessions.Active(); got != 0 {
		t.Errorf("active sessions after shutdown = %d, want 0", got)
	}
	logs.waitForExit(t, "ytdlp-start", 1)
	logs.waitForExit(t, "ffmpeg-start", 1)

	// Drain the client response so the transport doesn't leak.
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
