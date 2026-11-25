package main

import (
	"context"
	"io"
	"os/exec"
	"sync"
)

// YtDlp wraps yt-dlp binary execution.
type YtDlp struct {
	Path   string
	FFmpeg string
}

// StreamResult holds the streaming output and process management.
type StreamResult struct {
	Stdout io.ReadCloser
	ytdlp  *exec.Cmd
	ffmpeg *exec.Cmd
	cancel context.CancelFunc

	closeOnce sync.Once
}

// Stream starts yt-dlp piped to ffmpeg and returns a reader for the MP3 stream.
// The caller must call Close() when done to clean up resources.
func (y *YtDlp) Stream(ctx context.Context, url string) (*StreamResult, error) {
	ctx, cancel := context.WithCancel(ctx)

	// yt-dlp command to fetch audio
	ytdlpCmd := exec.CommandContext(ctx, y.Path,
		"-f", "bestaudio[ext=m4a]/bestaudio",
		"--no-part",
		"--no-playlist",
		"--quiet",
		"--no-warnings",
		"-o", "-",
		url,
	)

	// ffmpeg command to convert to MP3 stream
	ffmpegPath := y.FFmpeg
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	ffmpegCmd := exec.CommandContext(ctx, ffmpegPath,
		"-i", "pipe:0", // Read from stdin
		"-f", "mp3", // Output format
		"-acodec", "libmp3lame",
		"-ab", "128k", // Bitrate
		"-ar", "44100", // Sample rate
		"-ac", "2", // Stereo
		"-", // Output to stdout
	)

	// Connect yt-dlp stdout to ffmpeg stdin
	ytdlpStdout, err := ytdlpCmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}

	ffmpegCmd.Stdin = ytdlpStdout

	// Get ffmpeg stdout for streaming
	ffmpegStdout, err := ffmpegCmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}

	// Start both processes
	if err := ytdlpCmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	if err := ffmpegCmd.Start(); err != nil {
		ytdlpCmd.Process.Kill()
		cancel()
		return nil, err
	}

	return &StreamResult{
		Stdout: ffmpegStdout,
		ytdlp:  ytdlpCmd,
		ffmpeg: ffmpegCmd,
		cancel: cancel,
	}, nil
}

// Close terminates both processes and cleans up.
//
// Close is idempotent: repeated calls are safe and the processes are only
// killed and reaped once.
func (s *StreamResult) Close() error {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.Stdout != nil {
			s.Stdout.Close()
		}
		if s.ytdlp != nil && s.ytdlp.Process != nil {
			s.ytdlp.Process.Kill()
			s.ytdlp.Wait()
		}
		if s.ffmpeg != nil && s.ffmpeg.Process != nil {
			s.ffmpeg.Process.Kill()
			s.ffmpeg.Wait()
		}
	})
	return nil
}
