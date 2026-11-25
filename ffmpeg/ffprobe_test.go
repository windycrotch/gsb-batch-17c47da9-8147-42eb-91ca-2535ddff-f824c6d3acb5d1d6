package ffmpeg

import (
	"os/exec"
	"testing"
)

func TestFFProbeRun(t *testing.T) {
	if _, err := exec.LookPath(ffprobeCmd); err != nil {
		t.Skip("ffprobe not on PATH")
	}
	ffprobe := &FFProbe{}
	probe, err := ffprobe.Run(testFile)
	if err != nil {
		t.Error(err)
	}

	if len(probe.Streams) != 2 {
		t.Error()
	}

	if probe.Streams[0].Index != 0 {
		t.Error()
	}

	if probe.Streams[0].Width != 1280 {
		t.Error()
	}

	if probe.Streams[0].Height != 534 {
		t.Error()
	}
}
