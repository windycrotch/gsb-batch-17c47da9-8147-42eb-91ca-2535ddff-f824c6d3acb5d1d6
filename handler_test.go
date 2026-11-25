package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// MockYtDlpStreamer provides a mock implementation for testing handlers.
type MockYtDlpStreamer struct {
	StreamFunc func(ctx context.Context, url string) (*StreamResult, error)
}

// mockStreamResult creates a StreamResult that returns predefined data.
type mockReader struct {
	data   []byte
	pos    int
	closed bool
}

func (m *mockReader) Read(p []byte) (n int, err error) {
	if m.closed {
		return 0, io.EOF
	}
	if m.pos >= len(m.data) {
		return 0, io.EOF
	}
	n = copy(p, m.data[m.pos:])
	m.pos += n
	return n, nil
}

func (m *mockReader) Close() error {
	m.closed = true
	return nil
}

// MockYtDlp is a testable mock of YtDlp.
type MockYtDlp struct {
	StreamFunc func(ctx context.Context, url string) (*StreamResult, error)
}

func (m *MockYtDlp) Stream(ctx context.Context, url string) (*StreamResult, error) {
	return m.StreamFunc(ctx, url)
}

// createMockYtDlp creates a YtDlp-compatible struct for testing.
// Since we can't easily mock the real YtDlp, we'll create wrapper tests.

func TestNewStreamHandler_MissingURL(t *testing.T) {
	ytdlp := &YtDlp{Path: "yt-dlp"}
	handler := NewStreamHandler(ytdlp, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Missing 'url' query parameter") {
		t.Errorf("Body = %q, want to contain 'Missing 'url' query parameter'", body)
	}
}

func TestNewStreamHandler_EmptyURL(t *testing.T) {
	ytdlp := &YtDlp{Path: "yt-dlp"}
	handler := NewStreamHandler(ytdlp, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream?url=", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestNewStreamHandler_InvalidURL_NoScheme(t *testing.T) {
	ytdlp := &YtDlp{Path: "yt-dlp"}
	handler := NewStreamHandler(ytdlp, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream?url=example.com/video", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Invalid URL") {
		t.Errorf("Body = %q, want to contain 'Invalid URL'", body)
	}
}

func TestNewStreamHandler_InvalidURL_FTPScheme(t *testing.T) {
	ytdlp := &YtDlp{Path: "yt-dlp"}
	handler := NewStreamHandler(ytdlp, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream?url=ftp://example.com/video", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Invalid URL") {
		t.Errorf("Body = %q, want to contain 'Invalid URL'", body)
	}
}

func TestNewStreamHandler_InvalidURL_FileScheme(t *testing.T) {
	ytdlp := &YtDlp{Path: "yt-dlp"}
	handler := NewStreamHandler(ytdlp, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream?url=file:///etc/passwd", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestNewStreamHandler_MethodNotAllowed_POST(t *testing.T) {
	ytdlp := &YtDlp{Path: "yt-dlp"}
	handler := NewStreamHandler(ytdlp, nil)

	req := httptest.NewRequest(http.MethodPost, "/stream?url=https://example.com/video", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestNewStreamHandler_MethodNotAllowed_PUT(t *testing.T) {
	ytdlp := &YtDlp{Path: "yt-dlp"}
	handler := NewStreamHandler(ytdlp, nil)

	req := httptest.NewRequest(http.MethodPut, "/stream?url=https://example.com/video", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestNewStreamHandler_MethodNotAllowed_DELETE(t *testing.T) {
	ytdlp := &YtDlp{Path: "yt-dlp"}
	handler := NewStreamHandler(ytdlp, nil)

	req := httptest.NewRequest(http.MethodDelete, "/stream?url=https://example.com/video", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestNewStreamHandler_MethodNotAllowed_PATCH(t *testing.T) {
	ytdlp := &YtDlp{Path: "yt-dlp"}
	handler := NewStreamHandler(ytdlp, nil)

	req := httptest.NewRequest(http.MethodPatch, "/stream?url=https://example.com/video", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestNewStreamHandler_ValidURLSchemes(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"HTTP URL", "http://example.com/video"},
		{"HTTPS URL", "https://example.com/video"},
		{"HTTPS with path", "https://www.youtube.com/watch?v=abc123"},
		{"HTTP with port", "http://localhost:8080/video"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Using nonexistent yt-dlp will cause Stream to fail,
			// but URL validation should pass first
			ytdlp := &YtDlp{Path: "/nonexistent/yt-dlp"}
			handler := NewStreamHandler(ytdlp, nil)

			req := httptest.NewRequest(http.MethodGet, "/stream?url="+tt.url, nil)
			rec := httptest.NewRecorder()

			handler(rec, req)

			// Should fail with 500 (stream error), not 400 (bad URL)
			if rec.Code == http.StatusBadRequest {
				t.Errorf("URL %q was incorrectly rejected as invalid", tt.url)
			}
		})
	}
}

func TestNewStreamHandler_URLEncodedParameter(t *testing.T) {
	ytdlp := &YtDlp{Path: "/nonexistent/yt-dlp"}
	handler := NewStreamHandler(ytdlp, nil)

	// URL-encoded YouTube URL
	encodedURL := "https%3A%2F%2Fwww.youtube.com%2Fwatch%3Fv%3DdQw4w9WgXcQ"
	req := httptest.NewRequest(http.MethodGet, "/stream?url="+encodedURL, nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	// Should fail with 500 (stream error), not 400 (bad URL)
	if rec.Code == http.StatusBadRequest {
		t.Errorf("URL-encoded URL was incorrectly rejected")
	}
}

func TestNewStreamHandler_StreamFailure(t *testing.T) {
	// Use nonexistent path to force stream failure
	ytdlp := &YtDlp{Path: "/nonexistent/yt-dlp"}
	handler := NewStreamHandler(ytdlp, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream?url=https://example.com/video", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Failed to start stream") {
		t.Errorf("Body = %q, want to contain 'Failed to start stream'", body)
	}
}

func TestNewStreamHandler_ResponseHeaders(t *testing.T) {
	// We need to test headers, but can't easily mock YtDlp.Stream
	// This test would require integration testing or refactoring to use interfaces
	t.Skip("Requires interface refactoring to mock YtDlp")
}

// TestableStreamHandler is a version that accepts an interface for testing.
// The Streamer interface and NewTestableStreamHandler live in handler.go.

// mockStreamer implements Streamer for testing.
type mockStreamer struct {
	data []byte
	err  error
}

func (m *mockStreamer) Stream(ctx context.Context, url string) (*StreamResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &StreamResult{
		Stdout: &mockReader{data: m.data},
		cancel: func() {},
	}, nil
}

func TestTestableStreamHandler_Success(t *testing.T) {
	streamer := &mockStreamer{data: []byte("mock mp3 data")}
	handler := NewTestableStreamHandler(streamer, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream?url=https://example.com/video", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Check headers
	if ct := rec.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want %q", ct, "audio/mpeg")
	}

	if ar := rec.Header().Get("Accept-Ranges"); ar != "none" {
		t.Errorf("Accept-Ranges = %q, want %q", ar, "none")
	}

	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache, no-store" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache, no-store")
	}

	// Check body
	if body := rec.Body.String(); body != "mock mp3 data" {
		t.Errorf("Body = %q, want %q", body, "mock mp3 data")
	}
}

func TestTestableStreamHandler_HEAD(t *testing.T) {
	streamer := &mockStreamer{data: []byte("mock mp3 data")}
	handler := NewTestableStreamHandler(streamer, nil)

	req := httptest.NewRequest(http.MethodHead, "/stream?url=https://example.com/video", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Check headers are set
	if ct := rec.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want %q", ct, "audio/mpeg")
	}

	// Body should be empty for HEAD
	if body := rec.Body.String(); body != "" {
		t.Errorf("Body = %q, want empty for HEAD request", body)
	}
}

func TestTestableStreamHandler_StreamError(t *testing.T) {
	streamer := &mockStreamer{err: errors.New("stream failed")}
	handler := NewTestableStreamHandler(streamer, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream?url=https://example.com/video", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestTestableStreamHandler_LargeStream(t *testing.T) {
	// Create 1MB of mock data
	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	streamer := &mockStreamer{data: data}
	handler := NewTestableStreamHandler(streamer, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream?url=https://example.com/video", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", rec.Code, http.StatusOK)
	}

	if rec.Body.Len() != len(data) {
		t.Errorf("Body length = %d, want %d", rec.Body.Len(), len(data))
	}
}

// BenchmarkHandler benchmarks the handler with mock data.
func BenchmarkHandler(b *testing.B) {
	data := make([]byte, 1024) // 1KB
	streamer := &mockStreamer{data: data}
	handler := NewTestableStreamHandler(streamer, nil)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/stream?url=https://example.com/video", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)
	}
}

// Table-driven test for various invalid URLs
func TestNewStreamHandler_InvalidURLs(t *testing.T) {
	tests := []struct {
		name       string
		urlParam   string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "missing URL",
			urlParam:   "",
			wantStatus: http.StatusBadRequest,
			wantBody:   "Missing 'url'",
		},
		{
			name:       "javascript scheme",
			urlParam:   "javascript:alert(1)",
			wantStatus: http.StatusBadRequest,
			wantBody:   "Invalid URL",
		},
		{
			name:       "data scheme",
			urlParam:   "data:text/html,<h1>test</h1>",
			wantStatus: http.StatusBadRequest,
			wantBody:   "Invalid URL",
		},
		{
			name:       "empty scheme",
			urlParam:   "://example.com",
			wantStatus: http.StatusBadRequest,
			wantBody:   "Invalid URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ytdlp := &YtDlp{Path: "yt-dlp"}
			handler := NewStreamHandler(ytdlp, nil)

			url := "/stream"
			if tt.urlParam != "" {
				url = "/stream?url=" + tt.urlParam
			}

			req := httptest.NewRequest(http.MethodGet, url, nil)
			rec := httptest.NewRecorder()

			handler(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("Status = %d, want %d", rec.Code, tt.wantStatus)
			}

			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("Body = %q, want to contain %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

// Table-driven test for HTTP methods
func TestNewStreamHandler_Methods(t *testing.T) {
	tests := []struct {
		method     string
		wantStatus int
	}{
		{http.MethodGet, http.StatusInternalServerError}, // Fails at Stream, not validation
		{http.MethodHead, http.StatusOK},                 // HEAD probes headers only, starts no processes
		{http.MethodPost, http.StatusMethodNotAllowed},
		{http.MethodPut, http.StatusMethodNotAllowed},
		{http.MethodDelete, http.StatusMethodNotAllowed},
		{http.MethodPatch, http.StatusMethodNotAllowed},
		{http.MethodOptions, http.StatusMethodNotAllowed},
		{http.MethodConnect, http.StatusMethodNotAllowed},
		{http.MethodTrace, http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			ytdlp := &YtDlp{Path: "/nonexistent/yt-dlp"}
			handler := NewStreamHandler(ytdlp, nil)

			req := httptest.NewRequest(tt.method, "/stream?url=https://example.com/video", nil)
			rec := httptest.NewRecorder()

			handler(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("Status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}
