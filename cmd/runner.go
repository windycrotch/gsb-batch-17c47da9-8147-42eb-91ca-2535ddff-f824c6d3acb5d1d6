package cmd

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/alfg/ffmpegd/ffmpeg"
)

// ProgressFunc delivers one progress snapshot for a task.
type ProgressFunc func(Status)

// Runner executes one admitted task. The production implementation shells out
// to ffmpeg; tests substitute an in-memory runner for deterministic lifecycle
// and concurrency coverage.
type Runner interface {
	Run(t *Task, report ProgressFunc) error
}

// realRunner runs actual ffmpeg/ffprobe processes.
type realRunner struct {
	// progressInterval controls ticker frequency; zero means the default.
	progressInterval time.Duration

	// probe is overridable by tests that put a shim earlier on PATH; nil uses
	// the real ffprobe binary.
	probe func(input string) (*ffmpeg.FFProbeResponse, error)
}

func (r realRunner) probeInput(input string) (*ffmpeg.FFProbeResponse, error) {
	if r.probe != nil {
		return r.probe(input)
	}
	return ffmpeg.FFProbe{}.Run(input)
}

func (r realRunner) interval() time.Duration {
	if r.progressInterval > 0 {
		return r.progressInterval
	}
	return progressInterval
}

func (r realRunner) Run(t *Task, report ProgressFunc) error {
	// Registered before anything slow, so a cancel that arrives while the input
	// is still being probed is not lost.
	f := &ffmpeg.FFmpeg{}
	t.setEncoder(f)

	fmt.Printf("\rEncoding %s -> %s%s\n", t.Input, t.Output, clearLine)

	e, err := ffmpeg.NewEncode(t.Input, t.Output, t.Payload)
	if err != nil {
		return err
	}
	defer e.Close()

	probe, err := r.probeInput(t.Input)
	if err != nil {
		return err
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		trackEncodeProgress(t, f, e.OutputDuration(probe.Duration()), totalFrames(probe), done, r.interval(), report)
	}()

	err = f.RunEncode(e)

	// Stop progress updates before the final status so none can follow it.
	close(done)
	wg.Wait()
	return err
}

// totalFrames returns the frame count of the first video stream, or 0.
func totalFrames(p *ffmpeg.FFProbeResponse) int {
	for _, s := range p.Streams {
		if s.CodecType == "video" {
			n, _ := strconv.Atoi(s.NbFrames)
			return n
		}
	}
	return 0
}

// trackEncodeProgress reports progress until done is closed. Progress is
// measured against the output duration when known, and the frame count
// otherwise. Each pass of a two-pass encode is half the total.
func trackEncodeProgress(t *Task, f *ffmpeg.FFmpeg, duration float64, frames int, done <-chan struct{}, interval time.Duration, report ProgressFunc) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-t.Context().Done():
			return
		case <-ticker.C:
			p := f.Progress()
			if p.Passes == 0 {
				continue
			}

			frac := -1.0
			switch {
			case duration > 0:
				frac = float64(p.OutTimeUS) / 1e6 / duration
			case frames > 0:
				frac = float64(p.Frame) / float64(frames)
			}

			var pct float64
			if frac >= 0 {
				pct = (float64(p.Pass-1) + math.Min(frac, 1)) / float64(p.Passes) * 100
				// 100 tells the client the job is done, so hold just short of
				// it until ffmpeg exits.
				pct = math.Min(math.Round(pct*100)/100, 99.99)
			}

			pass := ""
			if p.Passes > 1 {
				pass = fmt.Sprintf(" pass %d/%d", p.Pass, p.Passes)
			}
			if frac >= 0 {
				fmt.Printf("\rEncoding...%s %0.2f%% %s @ %0.2f fps%s", pass, pct, p.Speed, p.FPS, clearLine)
			} else {
				fmt.Printf("\rEncoding...%s %0.1fs done %s @ %0.2f fps%s", pass, float64(p.OutTimeUS)/1e6, p.Speed, p.FPS, clearLine)
			}

			if report != nil {
				report(Status{Percent: pct, Speed: p.Speed, FPS: p.FPS, Frame: p.Frame, OutTimeUS: p.OutTimeUS})
			}
		}
	}
}

// classifyRunError maps a runner error to its terminal state and status.
func classifyRunError(err error) (TaskState, Status) {
	switch {
	case err == nil:
		return stateCompleted, Status{Percent: 100, Completed: true}
	case errors.Is(err, ffmpeg.ErrCancelled):
		fmt.Printf("\rEncode cancelled.%s\n", clearLine)
		return stateCancelled, Status{Cancelled: true}
	default:
		fmt.Printf("\r\u001b[31mEncode failed:\u001b[0m %s%s\n", err, clearLine)
		return stateFailed, Status{Err: err.Error(), Error: err.Error(), Message: err.Error()}
	}
}
