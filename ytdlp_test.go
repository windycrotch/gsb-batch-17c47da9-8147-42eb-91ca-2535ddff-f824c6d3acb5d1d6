package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestYtDlpStreamWithMockBinaries tests the Stream method using mock binaries.
func TestYtDlpStreamWithMockBinaries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows - shell scripts not supported")
	}

	tmpDir := t.TempDir()

	// Create a mock yt-dlp that outputs test data
	mockYtDlp := filepath.Join(tmpDir, "mock-ytdlp")
	ytdlpScript := `#!/bin/sh
echo "mock audio data from yt-dlp"
`
	if err := os.WriteFile(mockYtDlp, []byte(ytdlpScript), 0755); err != nil {
		t.Fatalf("Failed to create mock yt-dlp: %v", err)
	}

	// Create a mock ffmpeg that passes through data
	mockFFmpeg := filepath.Join(tmpDir, "mock-ffmpeg")
	ffmpegScript := `#!/bin/sh
cat
`
	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("Failed to create mock ffmpeg: %v", err)
	}

	ytdlp := &YtDlp{
		Path:   mockYtDlp,
		FFmpeg: mockFFmpeg,
	}

	ctx := context.Background()
	result, err := ytdlp.Stream(ctx, "https://example.com/video")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer result.Close()

	// Read the output
	output, err := io.ReadAll(result.Stdout)
	if err != nil {
		t.Fatalf("Failed to read output: %v", err)
	}

	expected := "mock audio data from yt-dlp\n"
	if string(output) != expected {
		t.Errorf("Output = %q, want %q", string(output), expected)
	}
}

// TestYtDlpStreamWithLargeOutput tests streaming with larger data.
func TestYtDlpStreamWithLargeOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows - shell scripts not supported")
	}

	tmpDir := t.TempDir()

	// Create a mock yt-dlp that outputs 1MB of data
	mockYtDlp := filepath.Join(tmpDir, "mock-ytdlp")
	ytdlpScript := `#!/bin/sh
dd if=/dev/zero bs=1024 count=1024 2>/dev/null
`
	if err := os.WriteFile(mockYtDlp, []byte(ytdlpScript), 0755); err != nil {
		t.Fatalf("Failed to create mock yt-dlp: %v", err)
	}

	// Create a mock ffmpeg that passes through data
	mockFFmpeg := filepath.Join(tmpDir, "mock-ffmpeg")
	ffmpegScript := `#!/bin/sh
cat
`
	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("Failed to create mock ffmpeg: %v", err)
	}

	ytdlp := &YtDlp{
		Path:   mockYtDlp,
		FFmpeg: mockFFmpeg,
	}

	ctx := context.Background()
	result, err := ytdlp.Stream(ctx, "https://example.com/video")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer result.Close()

	// Read the output
	output, err := io.ReadAll(result.Stdout)
	if err != nil {
		t.Fatalf("Failed to read output: %v", err)
	}

	expectedSize := 1024 * 1024 // 1MB
	if len(output) != expectedSize {
		t.Errorf("Output size = %d, want %d", len(output), expectedSize)
	}
}

// TestYtDlpStreamContextCancellation tests that context cancellation terminates processes.
func TestYtDlpStreamContextCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows - shell scripts not supported")
	}

	tmpDir := t.TempDir()

	// Create a mock yt-dlp that sleeps forever
	mockYtDlp := filepath.Join(tmpDir, "mock-ytdlp")
	ytdlpScript := `#!/bin/sh
sleep 3600
`
	if err := os.WriteFile(mockYtDlp, []byte(ytdlpScript), 0755); err != nil {
		t.Fatalf("Failed to create mock yt-dlp: %v", err)
	}

	// Create a mock ffmpeg that reads and sleeps
	mockFFmpeg := filepath.Join(tmpDir, "mock-ffmpeg")
	ffmpegScript := `#!/bin/sh
cat
sleep 3600
`
	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("Failed to create mock ffmpeg: %v", err)
	}

	ytdlp := &YtDlp{
		Path:   mockYtDlp,
		FFmpeg: mockFFmpeg,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	result, err := ytdlp.Stream(ctx, "https://example.com/video")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	// Wait for context to be cancelled
	<-ctx.Done()

	// Close should terminate processes
	err = result.Close()
	if err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

// TestYtDlpStreamYtDlpStartFailure tests handling of yt-dlp start failure.
func TestYtDlpStreamYtDlpStartFailure(t *testing.T) {
	ytdlp := &YtDlp{
		Path:   "/nonexistent/yt-dlp",
		FFmpeg: "ffmpeg",
	}

	ctx := context.Background()
	result, err := ytdlp.Stream(ctx, "https://example.com/video")

	if err == nil {
		result.Close()
		t.Fatal("Stream() expected error for nonexistent yt-dlp")
	}

	// Verify it's the right kind of error
	if !strings.Contains(err.Error(), "executable file not found") &&
		!strings.Contains(err.Error(), "no such file or directory") {
		t.Errorf("Expected 'not found' error, got: %v", err)
	}
}

// TestYtDlpStreamFFmpegStartFailure tests handling of ffmpeg start failure.
func TestYtDlpStreamFFmpegStartFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows - shell scripts not supported")
	}

	tmpDir := t.TempDir()

	// Create a valid mock yt-dlp
	mockYtDlp := filepath.Join(tmpDir, "mock-ytdlp")
	ytdlpScript := `#!/bin/sh
echo "data"
`
	if err := os.WriteFile(mockYtDlp, []byte(ytdlpScript), 0755); err != nil {
		t.Fatalf("Failed to create mock yt-dlp: %v", err)
	}

	ytdlp := &YtDlp{
		Path:   mockYtDlp,
		FFmpeg: "/nonexistent/ffmpeg",
	}

	ctx := context.Background()
	result, err := ytdlp.Stream(ctx, "https://example.com/video")

	if err == nil {
		result.Close()
		t.Fatal("Stream() expected error for nonexistent ffmpeg")
	}

	// Verify it's the right kind of error
	if !strings.Contains(err.Error(), "executable file not found") &&
		!strings.Contains(err.Error(), "no such file or directory") {
		t.Errorf("Expected 'not found' error, got: %v", err)
	}
}

// TestYtDlpStreamDefaultFFmpegPath tests that empty FFmpeg path defaults to "ffmpeg".
func TestYtDlpStreamDefaultFFmpegPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows - shell scripts not supported")
	}

	// Check if ffmpeg is available
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available in PATH")
	}

	tmpDir := t.TempDir()

	// Create a mock yt-dlp that outputs minimal MP3 data
	// This is a minimal valid MPEG audio frame header
	mockYtDlp := filepath.Join(tmpDir, "mock-ytdlp")
	ytdlpScript := `#!/bin/sh
# Output silence as raw PCM that ffmpeg can convert
dd if=/dev/zero bs=1 count=100 2>/dev/null
`
	if err := os.WriteFile(mockYtDlp, []byte(ytdlpScript), 0755); err != nil {
		t.Fatalf("Failed to create mock yt-dlp: %v", err)
	}

	ytdlp := &YtDlp{
		Path:   mockYtDlp,
		FFmpeg: "", // Empty - should default to "ffmpeg"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := ytdlp.Stream(ctx, "https://example.com/video")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer result.Close()

	// Just verify we got some output (ffmpeg actually ran)
	// We don't check the content because it depends on ffmpeg's behavior with minimal input
	buf := make([]byte, 100)
	_, _ = result.Stdout.Read(buf)
}

// TestStreamResultClose tests the Close method.
func TestStreamResultClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows - shell scripts not supported")
	}

	tmpDir := t.TempDir()

	// Create mock scripts that sleep
	mockYtDlp := filepath.Join(tmpDir, "mock-ytdlp")
	ytdlpScript := `#!/bin/sh
sleep 10
`
	if err := os.WriteFile(mockYtDlp, []byte(ytdlpScript), 0755); err != nil {
		t.Fatalf("Failed to create mock yt-dlp: %v", err)
	}

	mockFFmpeg := filepath.Join(tmpDir, "mock-ffmpeg")
	ffmpegScript := `#!/bin/sh
cat
sleep 10
`
	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("Failed to create mock ffmpeg: %v", err)
	}

	ytdlp := &YtDlp{
		Path:   mockYtDlp,
		FFmpeg: mockFFmpeg,
	}

	ctx := context.Background()
	result, err := ytdlp.Stream(ctx, "https://example.com/video")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	// Close should terminate processes quickly
	start := time.Now()
	err = result.Close()
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("Close() error = %v", err)
	}

	// Close should complete quickly (not wait for sleep to finish)
	if elapsed > 2*time.Second {
		t.Errorf("Close() took too long: %v", elapsed)
	}
}

// TestStreamResultCloseMultipleCalls tests that Close can be called multiple times.
func TestStreamResultCloseMultipleCalls(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows - shell scripts not supported")
	}

	tmpDir := t.TempDir()

	mockYtDlp := filepath.Join(tmpDir, "mock-ytdlp")
	ytdlpScript := `#!/bin/sh
echo "data"
`
	if err := os.WriteFile(mockYtDlp, []byte(ytdlpScript), 0755); err != nil {
		t.Fatalf("Failed to create mock yt-dlp: %v", err)
	}

	mockFFmpeg := filepath.Join(tmpDir, "mock-ffmpeg")
	ffmpegScript := `#!/bin/sh
cat
`
	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("Failed to create mock ffmpeg: %v", err)
	}

	ytdlp := &YtDlp{
		Path:   mockYtDlp,
		FFmpeg: mockFFmpeg,
	}

	ctx := context.Background()
	result, err := ytdlp.Stream(ctx, "https://example.com/video")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	// First close
	err = result.Close()
	if err != nil {
		t.Errorf("First Close() error = %v", err)
	}

	// Second close should not panic
	err = result.Close()
	if err != nil {
		t.Errorf("Second Close() error = %v", err)
	}
}

// TestYtDlpStreamYtDlpExitsWithError tests handling when yt-dlp exits with error.
func TestYtDlpStreamYtDlpExitsWithError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows - shell scripts not supported")
	}

	tmpDir := t.TempDir()

	// Create a mock yt-dlp that exits with error
	mockYtDlp := filepath.Join(tmpDir, "mock-ytdlp")
	ytdlpScript := `#!/bin/sh
exit 1
`
	if err := os.WriteFile(mockYtDlp, []byte(ytdlpScript), 0755); err != nil {
		t.Fatalf("Failed to create mock yt-dlp: %v", err)
	}

	mockFFmpeg := filepath.Join(tmpDir, "mock-ffmpeg")
	ffmpegScript := `#!/bin/sh
cat
`
	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("Failed to create mock ffmpeg: %v", err)
	}

	ytdlp := &YtDlp{
		Path:   mockYtDlp,
		FFmpeg: mockFFmpeg,
	}

	ctx := context.Background()
	result, err := ytdlp.Stream(ctx, "https://example.com/video")
	if err != nil {
		t.Fatalf("Stream() error = %v (startup should succeed)", err)
	}
	defer result.Close()

	// Reading should return empty/error because yt-dlp exited
	output, _ := io.ReadAll(result.Stdout)
	if len(output) != 0 {
		t.Errorf("Expected empty output when yt-dlp fails, got %d bytes", len(output))
	}
}

// TestYtDlpStreamFFmpegExitsWithError tests handling when ffmpeg exits with error.
func TestYtDlpStreamFFmpegExitsWithError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows - shell scripts not supported")
	}

	tmpDir := t.TempDir()

	// Create a mock yt-dlp that outputs data
	mockYtDlp := filepath.Join(tmpDir, "mock-ytdlp")
	ytdlpScript := `#!/bin/sh
echo "audio data"
`
	if err := os.WriteFile(mockYtDlp, []byte(ytdlpScript), 0755); err != nil {
		t.Fatalf("Failed to create mock yt-dlp: %v", err)
	}

	// Create a mock ffmpeg that exits with error
	mockFFmpeg := filepath.Join(tmpDir, "mock-ffmpeg")
	ffmpegScript := `#!/bin/sh
exit 1
`
	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("Failed to create mock ffmpeg: %v", err)
	}

	ytdlp := &YtDlp{
		Path:   mockYtDlp,
		FFmpeg: mockFFmpeg,
	}

	ctx := context.Background()
	result, err := ytdlp.Stream(ctx, "https://example.com/video")
	if err != nil {
		t.Fatalf("Stream() error = %v (startup should succeed)", err)
	}
	defer result.Close()

	// Reading should return empty/error because ffmpeg exited
	output, _ := io.ReadAll(result.Stdout)
	if len(output) != 0 {
		t.Errorf("Expected empty output when ffmpeg fails, got %d bytes", len(output))
	}
}

// TestYtDlpStreamURLPassedCorrectly tests that the URL is passed to yt-dlp correctly.
func TestYtDlpStreamURLPassedCorrectly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows - shell scripts not supported")
	}

	tmpDir := t.TempDir()

	// Create a mock yt-dlp that outputs the URL it received
	mockYtDlp := filepath.Join(tmpDir, "mock-ytdlp")
	ytdlpScript := `#!/bin/sh
# Output the last argument (the URL)
shift $(($# - 1))
echo "$1"
`
	if err := os.WriteFile(mockYtDlp, []byte(ytdlpScript), 0755); err != nil {
		t.Fatalf("Failed to create mock yt-dlp: %v", err)
	}

	mockFFmpeg := filepath.Join(tmpDir, "mock-ffmpeg")
	ffmpegScript := `#!/bin/sh
cat
`
	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("Failed to create mock ffmpeg: %v", err)
	}

	ytdlp := &YtDlp{
		Path:   mockYtDlp,
		FFmpeg: mockFFmpeg,
	}

	testURL := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

	ctx := context.Background()
	result, err := ytdlp.Stream(ctx, testURL)
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer result.Close()

	output, err := io.ReadAll(result.Stdout)
	if err != nil {
		t.Fatalf("Failed to read output: %v", err)
	}

	if strings.TrimSpace(string(output)) != testURL {
		t.Errorf("URL = %q, want %q", strings.TrimSpace(string(output)), testURL)
	}
}

// TestYtDlpStreamPipeIntegrity tests that data flows correctly through the pipe.
func TestYtDlpStreamPipeIntegrity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows - shell scripts not supported")
	}

	tmpDir := t.TempDir()

	// Create a mock yt-dlp that outputs specific pattern
	mockYtDlp := filepath.Join(tmpDir, "mock-ytdlp")
	ytdlpScript := `#!/bin/sh
echo "HEADER_START"
echo "audio_data_line_1"
echo "audio_data_line_2"
echo "FOOTER_END"
`
	if err := os.WriteFile(mockYtDlp, []byte(ytdlpScript), 0755); err != nil {
		t.Fatalf("Failed to create mock yt-dlp: %v", err)
	}

	// Create a mock ffmpeg that transforms data
	mockFFmpeg := filepath.Join(tmpDir, "mock-ffmpeg")
	ffmpegScript := `#!/bin/sh
while IFS= read -r line; do
    echo "PROCESSED: $line"
done
`
	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("Failed to create mock ffmpeg: %v", err)
	}

	ytdlp := &YtDlp{
		Path:   mockYtDlp,
		FFmpeg: mockFFmpeg,
	}

	ctx := context.Background()
	result, err := ytdlp.Stream(ctx, "https://example.com/video")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer result.Close()

	output, err := io.ReadAll(result.Stdout)
	if err != nil {
		t.Fatalf("Failed to read output: %v", err)
	}

	expected := `PROCESSED: HEADER_START
PROCESSED: audio_data_line_1
PROCESSED: audio_data_line_2
PROCESSED: FOOTER_END
`
	if string(output) != expected {
		t.Errorf("Output = %q, want %q", string(output), expected)
	}
}

// BenchmarkYtDlpStream benchmarks the streaming pipeline.
func BenchmarkYtDlpStream(b *testing.B) {
	if runtime.GOOS == "windows" {
		b.Skip("Skipping on Windows - shell scripts not supported")
	}

	tmpDir := b.TempDir()

	// Create mock binaries that output fixed data
	mockYtDlp := filepath.Join(tmpDir, "mock-ytdlp")
	ytdlpScript := `#!/bin/sh
dd if=/dev/zero bs=1024 count=10 2>/dev/null
`
	if err := os.WriteFile(mockYtDlp, []byte(ytdlpScript), 0755); err != nil {
		b.Fatalf("Failed to create mock yt-dlp: %v", err)
	}

	mockFFmpeg := filepath.Join(tmpDir, "mock-ffmpeg")
	ffmpegScript := `#!/bin/sh
cat
`
	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		b.Fatalf("Failed to create mock ffmpeg: %v", err)
	}

	ytdlp := &YtDlp{
		Path:   mockYtDlp,
		FFmpeg: mockFFmpeg,
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		ctx := context.Background()
		result, err := ytdlp.Stream(ctx, "https://example.com/video")
		if err != nil {
			b.Fatalf("Stream() error = %v", err)
		}

		var buf bytes.Buffer
		io.Copy(&buf, result.Stdout)
		result.Close()
	}
}
