package transcoder

import (
	"bufio"
	"bytes"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/xfrr/goffmpeg/media"
	"github.com/xfrr/goffmpeg/pkg/duration"
)

// Progress represents a single transcoding progress update emitted by ffmpeg.
type Progress struct {
	FramesProcessed string
	CurrentTime     string
	CurrentBitrate  string
	Progress        float64
	Speed           string

	// Err carries a terminal, recognizable failure (for example a process
	// that could not be started). It is set on the last value delivered on
	// the progress channel immediately before the channel is closed.
	Err error
}

// progressLineRegexp collapses the whitespace following an "=" sign so that a
// line such as "frame=   42" can be split into "frame=42".
var progressLineRegexp = regexp.MustCompile(`=\s+`)

// parseProgressLine converts a single ffmpeg stderr line into a Progress value.
//
// It is a pure function on purpose: it has no goroutine or channel state so the
// parsing behaviour (including the live/unknown-duration guard) can be exercised
// directly in tests with injected input.
//
// When the media duration is unknown or not finite (e.g. a live stream) the
// percentage is left at its zero value instead of dividing by zero or producing
// an infinite/NaN percentage; all other fields (time, bitrate, frames, speed)
// are still reported.
func parseProgressLine(line string, mediaFile *media.File) (Progress, bool) {
	if !strings.Contains(line, "frame=") ||
		!strings.Contains(line, "time=") ||
		!strings.Contains(line, "bitrate=") {
		return Progress{}, false
	}

	collapsed := progressLineRegexp.ReplaceAllString(line, `=`)
	fields := strings.Fields(collapsed)

	var p Progress
	for _, field := range fields {
		parts := strings.Split(field, "=")
		if len(parts) < 2 {
			continue
		}
		name, value := parts[0], parts[1]
		switch name {
		case "frame":
			p.FramesProcessed = value
		case "time":
			p.CurrentTime = value
		case "bitrate":
			p.CurrentBitrate = value
		case "speed":
			p.Speed = value
		}
	}

	timesec := duration.DurToSec(p.CurrentTime)

	// Default to an unknown (zero) duration. A missing media file, an empty
	// duration string or an unparsable value must never cause a divide by zero.
	var dursec float64
	if mediaFile != nil {
		// Ignore the error: an invalid duration simply stays zero and is
		// treated as "unknown" below.
		dursec, _ = strconv.ParseFloat(mediaFile.Metadata().Format.Duration, 64)
	}

	if dursec > 0 && !math.IsInf(dursec, 0) && !math.IsNaN(dursec) {
		percentage := (timesec * 100) / dursec
		// Guard against a non-finite result (defensive: with a finite, positive
		// denominator this should not happen, but never emit Inf/NaN).
		if !math.IsInf(percentage, 0) && !math.IsNaN(percentage) {
			p.Progress = percentage
		}
	}

	return p, true
}

// scanProgress reads ffmpeg-style progress lines from r and sends every parsed
// Progress on the returned channel, which is always closed exactly once.
//
// Termination:
//   - On the normal end path the caller closes r (the process's stderr pipe);
//     the scanner reaches EOF and the output channel closes.
//   - Closing stop makes the reader stop waiting for further input.
//
// The output channel is unbuffered: the caller (the run's pump) owns the
// buffered progress channel and the back-pressure policy. This function never
// closes r itself; the caller owns the underlying pipe and releases it.
func scanProgress(r io.Reader, mediaFile *media.File, stop <-chan struct{}) <-chan Progress {
	out := make(chan Progress)

	go func() {
		defer close(out)

		scanner := bufio.NewScanner(r)
		scanner.Split(crTerminatedSplit)
		// ffmpeg progress lines can be long; start small but allow growth up to
		// the maximum scanner token size.
		scanner.Buffer(make([]byte, 2), bufio.MaxScanTokenSize)

		for scanner.Scan() {
			if isClosed(stop) {
				return
			}
			p, ok := parseProgressLine(scanner.Text(), mediaFile)
			if !ok {
				continue
			}
			select {
			case out <- p:
			case <-stop:
				return
			}
		}
	}()

	return out
}

// crTerminatedSplit splits on either '\r' (ffmpeg uses carriage returns to
// overwrite the progress line) or '\n' (Windows style "\r\n").
func crTerminatedSplit(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\r'); i >= 0 {
		return i + 1, data[0:i], nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[0:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// isClosed reports whether the given channel has already been closed. A nil
// channel is treated as "never closed".
func isClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
