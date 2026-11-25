package hls

import (
	"fmt"
	"path/filepath"
)

// getOptions builds the ffmpeg argument list for one resolution variant.
// outputDir is the directory where the variant playlist and its segments are
// written (typically the task staging directory).
func getOptions(srcPath, outputDir, res string) ([]string, error) {
	cfg, err := getConfig(res)
	if err != nil {
		return nil, err
	}

	filenameTS := filepath.Join(outputDir, res+"_%03d.ts")
	filenameM3U8 := filepath.Join(outputDir, res+".m3u8")

	options := []string{
		"-hide_banner",
		"-y",
		"-i", srcPath,
		// Scale to the preset height, keep the aspect ratio and force an even
		// width (required by H.264). The height used to be hardcoded to 1080
		// regardless of the requested preset.
		"-vf", fmt.Sprintf("scale=trunc(oh*a/2)*2:%d", cfg.Height),
		"-c:a", "aac",
		"-ar", "48000",
		"-c:v", "h264",
		"-profile:v", "main",
		"-crf", "20",
		"-sc_threshold", "0",
		"-g", "48",
		"-keyint_min", "48",
		"-hls_time", "10",
		"-hls_playlist_type", "vod",
		"-b:v", cfg.VideoBitrate,
		"-maxrate", cfg.Maxrate,
		"-bufsize", cfg.BufSize,
		"-b:a", cfg.AudioBitrate,
		"-preset", "ultrafast",
		"-hls_segment_filename", filenameTS,
		filenameM3U8,
	}

	return options, nil
}
