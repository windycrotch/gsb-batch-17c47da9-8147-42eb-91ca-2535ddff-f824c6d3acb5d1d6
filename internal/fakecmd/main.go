// Fake ffmpeg/ffprobe used by the hermetic end-to-end tests. The same
// binary plays either role: it behaves as ffprobe when invoked through
// a name containing "ffprobe", and as ffmpeg otherwise.
//
// Behavior is driven by a JSON control file pointed at by the
// FFCVT_FAKE_CONTROL environment variable, so tests can script success,
// failure (with partial output), probe failure and indefinite hangs
// without any real ffmpeg, shell pipeline or network access.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Control is the on-disk script for a fake run.
type Control struct {
	// FailProbe makes the ffprobe role exit non-zero.
	FailProbe bool `json:"fail_probe"`
	// FailInputs maps an input base name to failure behavior.
	FailInputs map[string]FailSpec `json:"fail_inputs"`
	// HangInputs maps an input base name to a hang (process sleeps
	// until killed), used to simulate interruption.
	HangInputs map[string]int `json:"hang_inputs"`
	// OutputSize is the size in bytes a successful fake transcode writes.
	OutputSize int `json:"output_size"`
	// CounterDir, when set, receives one file per ffmpeg invocation
	// (named after the input) so tests can assert how many times each
	// input was actually encoded.
	CounterDir string `json:"counter_dir"`
	// EncodeDelayMs makes each successful encode take at least this
	// long, widening the overlap window in concurrency tests.
	EncodeDelayMs int `json:"encode_delay_ms"`
	// BarrierFile, when set, makes every fake ffmpeg wait until that
	// file appears before encoding. Tests create both processes first
	// and release the barrier once, guaranteeing real overlap.
	BarrierFile string `json:"barrier_file"`
	// ReadyDir, when set, gets a marker file per process that reaches
	// the barrier, letting tests wait deterministically instead of
	// sleeping for a fixed duration.
	ReadyDir string `json:"ready_dir"`
}

// FailSpec describes how a given input fails.
type FailSpec struct {
	// PartialBytes > 0 makes the fake write that many bytes to the
	// output before failing, simulating a corrupt half-finished file.
	PartialBytes int `json:"partial_bytes"`
	// FailOnce makes only the first attempt fail (a counter file is
	// used); subsequent attempts succeed.
	FailOnce bool `json:"fail_once"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Println("fake-ffmpeg version 0.0")
		fmt.Println("built for hermetic ffcvt tests")
		return
	}

	control := loadControl()

	if strings.Contains(strings.ToLower(filepath.Base(os.Args[0])), "ffprobe") {
		runProbe(control)
		return
	}
	runFFmpeg(control)
}

func loadControl() Control {
	var c Control
	// Control file may come from an environment variable or, when the
	// environment cannot be reliably shared (nested go test processes),
	// from an -ffcvt-fake-control argument ffcvt passes through.
	p := os.Getenv("FFCVT_FAKE_CONTROL")
	if p == "" {
		rest := os.Args[1:]
		for i, a := range rest {
			if a == "-ffcvt-fake-control" && i+1 < len(rest) {
				p = rest[i+1]
				break
			}
		}
	}
	if p == "" {
		// No script: be a well-behaved encoder by default.
		c.OutputSize = 4096
		return c
	}
	data, err := os.ReadFile(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake: cannot read control:", err)
		os.Exit(3)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		fmt.Fprintln(os.Stderr, "fake: bad control json:", err)
		os.Exit(3)
	}
	if c.OutputSize == 0 {
		c.OutputSize = 4096
	}
	return c
}

func runProbe(c Control) {
	if c.FailProbe {
		fmt.Fprintln(os.Stderr, "fake ffprobe: simulated probe failure")
		os.Exit(1)
	}
	// Single audio stream keeps the production code on the simple path.
	fmt.Println("Input #0, matroska,webm, from 'fake':")
	fmt.Println("  Stream #0:0: Video: vp9, yuv420p, 1920x1080")
	fmt.Println("  Stream #0:1: Audio: opus, 48000 Hz, stereo, fltp")
}

// inputFromArgs extracts the input file: the token following "-i".
func inputFromArgs(args []string) string {
	for i, a := range args {
		if a == "-i" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// outputFromArgs returns the last argument that looks like an output
// path (ffmpeg's output is the final positional argument).
func outputFromArgs(args []string) string {
	for i := len(args) - 1; i >= 0; i-- {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			continue
		}
		if i > 0 && args[i-1] == "-i" {
			continue
		}
		return a
	}
	return ""
}

func runFFmpeg(c Control) {
	input := filepath.Base(inputFromArgs(os.Args[1:]))
	output := outputFromArgs(os.Args[1:])

	if c.BarrierFile != "" {
		// Announce arrival, then block until the test releases the
		// shared start barrier.
		if c.ReadyDir != "" {
			_ = os.MkdirAll(c.ReadyDir, 0755)
			_ = os.WriteFile(
				filepath.Join(c.ReadyDir, fmt.Sprintf("%d.ready", os.Getpid())),
				[]byte("1"), 0644)
		}
		for {
			if _, err := os.Stat(c.BarrierFile); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	bumpCounter(c.CounterDir, input)

	if _, hang := c.HangInputs[input]; hang {
		// Simulate a long-running transcode that gets killed mid-flight.
		// Write something first so cleanup of a partial file is tested.
		if output != "" {
			_ = os.MkdirAll(filepath.Dir(output), 0755)
			_ = os.WriteFile(output, []byte("partial-before-hang"), 0644)
		}
		for {
			time.Sleep(time.Hour)
		}
	}

	if spec, fail := c.FailInputs[input]; fail {
		if spec.FailOnce && hasFailureMarker(c.CounterDir, input) {
			// The earlier run already failed once: succeed now.
		} else {
			if spec.FailOnce {
				markFailure(c.CounterDir, input)
			}
			doFail(output, spec)
			return
		}
	}

	if output == "" {
		fmt.Fprintln(os.Stderr, "fake ffmpeg: no output argument")
		os.Exit(2)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		fmt.Fprintln(os.Stderr, "fake ffmpeg mkdir:", err)
		os.Exit(2)
	}
	// Deterministic, fully-readable payload of the requested size.
	payload := strings.Repeat("FAKEDATA-", (c.OutputSize/9)+1)
	if len(payload) > c.OutputSize {
		payload = payload[:c.OutputSize]
	}
	// ffmpeg's segment muxer is handed a printf-style pattern
	// (e.g. name%02d_.mkv); emulate it by writing two numbered parts.
	if strings.Contains(output, "%0") {
		for _, idx := range []int{0, 1} {
			part := segmentPartName(output, idx)
			if err := os.WriteFile(part, []byte(payload), 0644); err != nil {
				fmt.Fprintln(os.Stderr, "fake ffmpeg segment write:", err)
				os.Exit(2)
			}
		}
	} else if err := os.WriteFile(output, []byte(payload), 0644); err != nil {
		fmt.Fprintln(os.Stderr, "fake ffmpeg write:", err)
		os.Exit(2)
	}
	if c.EncodeDelayMs > 0 {
		time.Sleep(time.Duration(c.EncodeDelayMs) * time.Millisecond)
	}
	fmt.Println("fake ffmpeg: encoded", input, "->", output)
}

// segmentPartName turns "dir/name%02d_.mkv" into "dir/name00_.mkv".
func segmentPartName(pattern string, idx int) string {
	pos := strings.Index(pattern, "%0")
	if pos < 0 {
		return pattern
	}
	end := pos + 2
	for end < len(pattern) && pattern[end] >= '0' && pattern[end] <= '9' {
		end++
	}
	if end < len(pattern) && pattern[end] == 'd' {
		end++
	}
	width := 2
	return pattern[:pos] + fmt.Sprintf("%0*d", width, idx) + pattern[end:]
}

func doFail(output string, spec FailSpec) {
	if output != "" && spec.PartialBytes > 0 {
		_ = os.MkdirAll(filepath.Dir(output), 0755)
		_ = os.WriteFile(output,
			[]byte(strings.Repeat("X", spec.PartialBytes)), 0644)
	}
	fmt.Fprintln(os.Stderr, "fake ffmpeg: simulated transcode failure")
	os.Exit(1)
}

// bumpCounter records one invocation. The per-input counter file holds
// the running attempt count.
func bumpCounter(dir, input string) {
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0755)
	p := filepath.Join(dir, input+".count")
	n := 1
	if data, err := os.ReadFile(p); err == nil {
		fmt.Sscanf(string(data), "%d", &n)
		n++
	}
	_ = os.WriteFile(p, []byte(fmt.Sprintf("%d", n)), 0644)
}

// hasFailureMarker reports whether a previous fake run already failed
// once for the input (used by FailOnce).
func hasFailureMarker(dir, input string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, input+".failed"))
	return err == nil
}

func markFailure(dir, input string) {
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0755)
	_ = os.WriteFile(filepath.Join(dir, input+".failed"),
		[]byte("1"), 0644)
}
