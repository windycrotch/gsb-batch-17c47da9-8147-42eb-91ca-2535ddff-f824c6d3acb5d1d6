package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestGetEnv_WithEnvVar(t *testing.T) {
	key := "TEST_GETENV_VAR"
	expected := "test_value"

	os.Setenv(key, expected)
	defer os.Unsetenv(key)

	result := getEnv(key, "fallback")
	if result != expected {
		t.Errorf("getEnv(%q, \"fallback\") = %q, want %q", key, result, expected)
	}
}

func TestGetEnv_WithEmptyEnvVar(t *testing.T) {
	key := "TEST_GETENV_EMPTY"

	os.Setenv(key, "")
	defer os.Unsetenv(key)

	// Empty string is still a set value
	result := getEnv(key, "fallback")
	if result != "" {
		t.Errorf("getEnv(%q, \"fallback\") = %q, want empty string", key, result)
	}
}

func TestGetEnv_WithoutEnvVar(t *testing.T) {
	key := "TEST_GETENV_NONEXISTENT_12345"
	fallback := "fallback_value"

	// Ensure it's not set
	os.Unsetenv(key)

	result := getEnv(key, fallback)
	if result != fallback {
		t.Errorf("getEnv(%q, %q) = %q, want %q", key, fallback, result, fallback)
	}
}

func TestGetEnv_EmptyFallback(t *testing.T) {
	key := "TEST_GETENV_EMPTY_FALLBACK"

	os.Unsetenv(key)

	result := getEnv(key, "")
	if result != "" {
		t.Errorf("getEnv(%q, \"\") = %q, want empty string", key, result)
	}
}

func TestHealthHandler_StatusCode(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	HealthHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHealthHandler_Body(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	HealthHandler(rec, req)

	body := rec.Body.String()
	if body != "ok" {
		t.Errorf("Body = %q, want %q", body, "ok")
	}
}

func TestHealthHandler_AllMethods(t *testing.T) {
	methods := []string{
		http.MethodGet,
		http.MethodHead,
		http.MethodPost,
		http.MethodPut,
		http.MethodDelete,
	}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/health", nil)
			rec := httptest.NewRecorder()

			HealthHandler(rec, req)

			// Health handler doesn't check method, always returns 200
			if rec.Code != http.StatusOK {
				t.Errorf("Status = %d, want %d", rec.Code, http.StatusOK)
			}
		})
	}
}

func TestServerRoutes(t *testing.T) {
	ytdlp := &YtDlp{Path: "yt-dlp"}

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", NewStreamHandler(ytdlp, nil))
	mux.HandleFunc("/health", HealthHandler)

	server := httptest.NewServer(mux)
	defer server.Close()

	t.Run("health endpoint", func(t *testing.T) {
		resp, err := http.Get(server.URL + "/health")
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
		}

		body, _ := io.ReadAll(resp.Body)
		if string(body) != "ok" {
			t.Errorf("Body = %q, want %q", string(body), "ok")
		}
	})

	t.Run("stream endpoint - missing url", func(t *testing.T) {
		resp, err := http.Get(server.URL + "/stream")
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
	})

	t.Run("unknown endpoint", func(t *testing.T) {
		resp, err := http.Get(server.URL + "/unknown")
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusNotFound)
		}
	})
}

func TestServerConfiguration(t *testing.T) {
	ytdlp := &YtDlp{Path: "yt-dlp"}

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", NewStreamHandler(ytdlp, nil))
	mux.HandleFunc("/health", HealthHandler)

	server := &http.Server{
		Addr:         ":0", // Random port
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0,
	}

	// Verify configuration
	if server.ReadTimeout != 10*time.Second {
		t.Errorf("ReadTimeout = %v, want %v", server.ReadTimeout, 10*time.Second)
	}

	if server.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (no timeout)", server.WriteTimeout)
	}
}

func TestServerGracefulShutdown(t *testing.T) {
	ytdlp := &YtDlp{Path: "yt-dlp"}

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", NewStreamHandler(ytdlp, nil))
	mux.HandleFunc("/health", HealthHandler)

	server := httptest.NewServer(mux)

	// Verify server is running
	resp, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("Server not responding: %v", err)
	}
	resp.Body.Close()

	// Close the server
	server.Close()

	// Verify server is closed (connection should fail)
	_, err = http.Get(server.URL + "/health")
	if err == nil {
		t.Error("Expected connection error after server close")
	}
}

func TestServerWithContext(t *testing.T) {
	ytdlp := &YtDlp{Path: "yt-dlp"}

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", NewStreamHandler(ytdlp, nil))
	mux.HandleFunc("/health", HealthHandler)

	server := &http.Server{
		Handler: mux,
	}

	// Test shutdown with timeout context
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start server in background
	go func() {
		_ = server.ListenAndServe()
	}()

	// Give server time to start
	time.Sleep(10 * time.Millisecond)

	// Shutdown should complete within context timeout
	err := server.Shutdown(ctx)
	if err != nil {
		t.Errorf("Shutdown error: %v", err)
	}
}

func TestYtDlpConfiguration(t *testing.T) {
	t.Run("default configuration", func(t *testing.T) {
		ytdlp := &YtDlp{Path: "yt-dlp"}

		if ytdlp.Path != "yt-dlp" {
			t.Errorf("Path = %q, want %q", ytdlp.Path, "yt-dlp")
		}

		if ytdlp.FFmpeg != "" {
			t.Errorf("FFmpeg = %q, want empty (default)", ytdlp.FFmpeg)
		}
	})

	t.Run("custom configuration", func(t *testing.T) {
		ytdlp := &YtDlp{
			Path:   "/usr/local/bin/yt-dlp",
			FFmpeg: "/usr/local/bin/ffmpeg",
		}

		if ytdlp.Path != "/usr/local/bin/yt-dlp" {
			t.Errorf("Path = %q, want %q", ytdlp.Path, "/usr/local/bin/yt-dlp")
		}

		if ytdlp.FFmpeg != "/usr/local/bin/ffmpeg" {
			t.Errorf("FFmpeg = %q, want %q", ytdlp.FFmpeg, "/usr/local/bin/ffmpeg")
		}
	})
}

// BenchmarkHealthHandler benchmarks the health endpoint.
func BenchmarkHealthHandler(b *testing.B) {
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()
		HealthHandler(rec, req)
	}
}

// BenchmarkGetEnv benchmarks the getEnv function.
func BenchmarkGetEnv(b *testing.B) {
	key := "BENCHMARK_ENV_VAR"
	os.Setenv(key, "benchmark_value")
	defer os.Unsetenv(key)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		getEnv(key, "fallback")
	}
}

// BenchmarkGetEnv_Fallback benchmarks getEnv with fallback path.
func BenchmarkGetEnv_Fallback(b *testing.B) {
	key := "NONEXISTENT_BENCHMARK_VAR"
	os.Unsetenv(key)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		getEnv(key, "fallback")
	}
}

// Integration test for the full server setup
func TestIntegration_FullServer(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ytdlp := &YtDlp{Path: "yt-dlp"}

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", NewStreamHandler(ytdlp, nil))
	mux.HandleFunc("/health", HealthHandler)

	server := httptest.NewServer(mux)
	defer server.Close()

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	// Test concurrent health checks
	const numRequests = 10
	results := make(chan error, numRequests)

	for i := 0; i < numRequests; i++ {
		go func() {
			resp, err := client.Get(server.URL + "/health")
			if err != nil {
				results <- err
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				results <- err
				return
			}
			results <- nil
		}()
	}

	for i := 0; i < numRequests; i++ {
		if err := <-results; err != nil {
			t.Errorf("Concurrent request failed: %v", err)
		}
	}
}

// Test environment variable precedence
func TestEnvVarPrecedence(t *testing.T) {
	// This tests the pattern used in main()
	// port := flag.String("port", getEnv("PORT", "8080"), "Server port")

	t.Run("default when no env", func(t *testing.T) {
		os.Unsetenv("PORT")
		result := getEnv("PORT", "8080")
		if result != "8080" {
			t.Errorf("Expected default 8080, got %q", result)
		}
	})

	t.Run("env var overrides default", func(t *testing.T) {
		os.Setenv("PORT", "9090")
		defer os.Unsetenv("PORT")

		result := getEnv("PORT", "8080")
		if result != "9090" {
			t.Errorf("Expected env value 9090, got %q", result)
		}
	})

	t.Run("ytdlp path default", func(t *testing.T) {
		os.Unsetenv("YTDLP_PATH")
		result := getEnv("YTDLP_PATH", "yt-dlp")
		if result != "yt-dlp" {
			t.Errorf("Expected default yt-dlp, got %q", result)
		}
	})

	t.Run("ytdlp path from env", func(t *testing.T) {
		os.Setenv("YTDLP_PATH", "/custom/path/yt-dlp")
		defer os.Unsetenv("YTDLP_PATH")

		result := getEnv("YTDLP_PATH", "yt-dlp")
		if result != "/custom/path/yt-dlp" {
			t.Errorf("Expected custom path, got %q", result)
		}
	})
}
