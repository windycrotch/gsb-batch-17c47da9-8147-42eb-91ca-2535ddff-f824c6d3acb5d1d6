package hls

import (
	"context"
	"os/exec"
)

// GenerateHLS will generate HLS file based on resolution presets.
// The available resolutions are: 360p, 480p, 720p and 1080p.
//
// This is a synchronous, single-variant convenience wrapper over the task API:
// it blocks until ffmpeg actually exits and returns its real exit status.
func GenerateHLS(ffmpegPath, srcPath, targetPath, resolution string) error {
	_, err := Transcode(context.Background(), Config{
		FFmpegPath:  ffmpegPath,
		SrcPath:     srcPath,
		TargetPath:  targetPath,
		Resolutions: []string{resolution},
	})
	return err
}

// GenerateHLSCustom will generate HLS using the flexible options params.
// options is an array of strings accepted by the ffmpeg command.
//
// The call is synchronous: it waits for ffmpeg to exit and returns the exit
// error instead of merely reporting that the process was started.
func GenerateHLSCustom(ffmpegPath string, options []string) error {
	cmd := exec.Command(ffmpegPath, options...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}
