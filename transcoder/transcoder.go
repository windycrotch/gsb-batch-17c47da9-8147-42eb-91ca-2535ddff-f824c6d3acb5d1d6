package transcoder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/xfrr/goffmpeg"
	"github.com/xfrr/goffmpeg/media"
)

// defaultStopGrace is how long Stop waits for the process to honour the graceful
// "q" request before killing it forcibly. Tests may override stopGrace on a
// Transcoder to make the watchdog shorter.
const defaultStopGrace = 5 * time.Second

// process is the subset of *exec.Cmd behaviour the transcoder relies on.
// Abstracting it lets tests inject a fully controllable fake process without
// spawning a real ffmpeg binary.
//
// Wait must be called exactly once; it blocks until the process exits and
// returns its terminal error. Done returns a channel closed at that same moment
// without itself consuming the wait, so the Stop watchdog can observe the exit
// racing against the normal completion path.
type process interface {
	StderrPipe() (io.ReadCloser, error)
	StdinPipe() (io.WriteCloser, error)
	SetStdout(io.Writer)
	SetStdin(io.Reader)
	Start() error
	Wait() error
	Done() <-chan struct{}
	ProcessKill() error
}

// execProcess adapts *exec.Cmd to the process interface. A single waiter
// goroutine owns the only cmd.Wait() call; both Wait and Done are derived from
// it so the standard library's "Wait may not be called concurrently" rule is
// never violated.
type execProcess struct {
	cmd     *exec.Cmd
	exited  chan struct{}
	exitErr error
	once    sync.Once
}

func newExecProcess(name string, args []string) process {
	return &execProcess{
		cmd:    exec.Command(name, args...),
		exited: make(chan struct{}),
	}
}

func (e *execProcess) StderrPipe() (io.ReadCloser, error) { return e.cmd.StderrPipe() }
func (e *execProcess) StdinPipe() (io.WriteCloser, error) { return e.cmd.StdinPipe() }
func (e *execProcess) SetStdout(w io.Writer)              { e.cmd.Stdout = w }
func (e *execProcess) SetStdin(r io.Reader)               { e.cmd.Stdin = r }
func (e *execProcess) Start() error                       { return e.cmd.Start() }
func (e *execProcess) Done() <-chan struct{}              { return e.exited }
func (e *execProcess) ProcessKill() error {
	if e.cmd.Process == nil {
		return nil
	}
	return e.cmd.Process.Kill()
}

// Wait blocks until the process exits. The underlying cmd.Wait() is performed at
// most once regardless of how many goroutines observe the exit.
func (e *execProcess) Wait() error {
	e.once.Do(func() {
		e.exitErr = e.cmd.Wait()
		close(e.exited)
	})
	<-e.exited
	return e.exitErr
}

// processFactory builds the process for a given command line. It is a field so
// tests can replace the real ffmpeg invocation with a fake.
type processFactory func(name string, args []string) process

// runState holds every resource belonging to a single Run invocation. Keeping
// run-scoped data here (instead of on the Transcoder itself) guarantees that a
// previous run's goroutines and pipes can never touch a later run's output.
type runState struct {
	proc process

	// Pipes owned by this run. They are released exactly once through closeOnce.
	stdErrPipe   io.ReadCloser
	stdStdinPipe io.WriteCloser

	// progress is the single, shared progress channel for the run. Every call to
	// Output returns this same channel (including after the run finished, until a
	// new run begins) so buffered values such as a start failure are never lost.
	// It is closed exactly once via progressOnce.
	progress chan Progress

	// stop is closed by Stop (or by the finalizer) to unblock the progress
	// reader promptly.
	stop chan struct{}

	// pumpDone is closed when the progress pump goroutine has fully exited. It is
	// nil for runs without a pump (progress disabled or start failure); finalize
	// waits on it before closing progress so no send can race a channel close.
	pumpDone chan struct{}

	// done is the channel returned to Run callers. It is buffered so the
	// finalizer never blocks even if nobody reads it.
	done chan error

	closeOnce    sync.Once
	stopOnce     sync.Once
	progressOnce sync.Once
}

// closeProgress closes the run's progress channel at most once.
func (rs *runState) closeProgress() {
	rs.progressOnce.Do(func() { close(rs.progress) })
}

// Transcoder Main struct
type Transcoder struct {
	// Legacy, externally configurable pipes. They are retained for backwards
	// compatibility but new runs create and own their own pipes internally.
	stdErrPipe         io.ReadCloser
	stdStdinPipe       io.WriteCloser
	process            *exec.Cmd
	mediafile          *media.File
	configuration      goffmpeg.Configuration
	whiteListProtocols []string

	// stopGrace bounds how long Stop waits for a graceful exit before killing.
	stopGrace time.Duration

	// newProcess builds the OS process. Defaults to exec.Command; tests override
	// it to inject a fake process.
	newProcess processFactory

	mu sync.Mutex
	// run is the currently active run (nil once it has been finalized).
	run *runState
	// lastRun retains the most recent run after it finishes so Output() can still
	// hand back its channel (and any buffered terminal failure) until the next
	// Run replaces it.
	lastRun  *runState
	stopping bool
}

// SetProcessStderrPipe Set the STDERR pipe
func (t *Transcoder) SetProcessStderrPipe(v io.ReadCloser) {
	t.stdErrPipe = v
}

// SetProcessStdinPipe Set the STDIN pipe
func (t *Transcoder) SetProcessStdinPipe(v io.WriteCloser) {
	t.stdStdinPipe = v
}

// SetProcess Set the transcoding process
func (t *Transcoder) SetProcess(cmd *exec.Cmd) {
	t.process = cmd
}

// SetMediaFile Set the media file
func (t *Transcoder) SetMediaFile(v *media.File) {
	t.mediafile = v
}

// SetConfiguration Set the transcoding configuration
func (t *Transcoder) SetConfiguration(v goffmpeg.Configuration) {
	t.configuration = v
}

func (t *Transcoder) SetWhiteListProtocols(availableProtocols []string) {
	t.whiteListProtocols = availableProtocols
}

// Process Get transcoding process
func (t *Transcoder) Process() *exec.Cmd {
	return t.process
}

// MediaFile Get the ttranscoding media file.
func (t *Transcoder) MediaFile() *media.File {
	return t.mediafile
}

// FFmpegExec Get FFmpeg Bin path
func (t *Transcoder) FFmpegExec() string {
	return t.configuration.FFmpegBinPath()
}

// FFprobeExec Get FFprobe Bin path
func (t *Transcoder) FFprobeExec() string {
	return t.configuration.FFprobeBinPath()
}

// GetCommand Build and get command
func (t *Transcoder) GetCommand() []string {
	media := t.mediafile
	rcommand := append([]string{"-y"}, media.ToStrCommand()...)

	if t.whiteListProtocols != nil {
		rcommand = append([]string{"-protocol_whitelist", strings.Join(t.whiteListProtocols, ",")}, rcommand...)
	}

	return rcommand
}

// InitializeEmptyTranscoder initializes the fields necessary for a blank transcoder
func (t *Transcoder) InitializeEmptyTranscoder() error {
	var Metadata media.Metadata

	var err error
	cfg := t.configuration
	if len(cfg.FFmpegBinPath()) == 0 || len(cfg.FFprobeBinPath()) == 0 {
		cfg, err = goffmpeg.Configure(context.Background())
		if err != nil {
			return err
		}
	}
	// Set new File
	MediaFile := new(media.File)
	MediaFile.SetMetadata(Metadata)

	// Set transcoder configuration
	t.SetMediaFile(MediaFile)
	t.SetConfiguration(cfg)
	return nil
}

// SetInputPath sets the input path for transcoding
func (t *Transcoder) SetInputPath(inputPath string) error {
	if t.mediafile.InputPipe() {
		return errors.New("cannot set an input path when an input pipe command has been set")
	}
	t.mediafile.SetInputPath(inputPath)
	return nil
}

// SetOutputPath sets the output path for transcoding
func (t *Transcoder) SetOutputPath(inputPath string) error {
	if t.mediafile.OutputPipe() {
		return errors.New("cannot set an input path when an input pipe command has been set")
	}
	t.mediafile.SetOutputPath(inputPath)
	return nil
}

// CreateInputPipe creates an input pipe for the transcoding process
func (t *Transcoder) CreateInputPipe() (*io.PipeWriter, error) {
	if t.mediafile.InputPath() != "" {
		return nil, errors.New("cannot set an input pipe when an input path exists")
	}
	inputPipeReader, inputPipeWriter := io.Pipe()
	t.mediafile.SetInputPipe(true)
	t.mediafile.SetInputPipeReader(inputPipeReader)
	t.mediafile.SetInputPipeWriter(inputPipeWriter)
	return inputPipeWriter, nil
}

// CreateOutputPipe creates an output pipe for the transcoding process
func (t *Transcoder) CreateOutputPipe(containerFormat string) (*io.PipeReader, error) {
	if t.mediafile.OutputPath() != "" {
		return nil, errors.New("cannot set an output pipe when an output path exists")
	}
	t.mediafile.SetOutputFormat(containerFormat)

	t.mediafile.SetMovFlags("frag_keyframe")
	outputPipeReader, outputPipeWriter := io.Pipe()
	t.mediafile.SetOutputPipe(true)
	t.mediafile.SetOutputPipeReader(outputPipeReader)
	t.mediafile.SetOutputPipeWriter(outputPipeWriter)
	return outputPipeReader, nil
}

// Initialize Init the transcoding process
func (t *Transcoder) Initialize(inputPath string, outputPath string) error {
	var err error
	var outb, errb bytes.Buffer
	var Metadata media.Metadata

	cfg := t.configuration

	if len(cfg.FFmpegBinPath()) == 0 || len(cfg.FFprobeBinPath()) == 0 {
		cfg, err = goffmpeg.Configure(context.Background())
		if err != nil {
			return err
		}
	}

	if inputPath == "" {
		return errors.New("error on transcoder.Initialize: inputPath missing")
	}

	command := []string{"-i", inputPath, "-print_format", "json", "-show_format", "-show_streams", "-show_error"}

	if t.whiteListProtocols != nil {
		command = append([]string{"-protocol_whitelist", strings.Join(t.whiteListProtocols, ",")}, command...)
	}

	cmd := exec.Command(cfg.FFprobeBinPath(), command...)
	cmd.Stdout = &outb
	cmd.Stderr = &errb

	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("error executing (%s) | error: %s | message: %s %s", command, err, outb.String(), errb.String())
	}

	if err = json.Unmarshal(outb.Bytes(), &Metadata); err != nil {
		return err
	}

	// Set new File
	MediaFile := new(media.File)
	MediaFile.SetMetadata(Metadata)
	MediaFile.SetInputPath(inputPath)
	MediaFile.SetOutputPath(outputPath)

	// Set transcoder configuration
	t.SetMediaFile(MediaFile)
	t.SetConfiguration(cfg)

	return nil

}

// Run Starts the transcoding process.
//
// The returned channel receives the terminal error (nil on success) exactly
// once and is then closed. Progress updates are consumed through Output.
//
// A Transcoder cannot run two tasks at once: starting a new run while a previous
// one is still active returns an error instead of letting the two runs share
// pipes and channels. Reuse the Transcoder only after the previous run finished
// (its Run and Output channels have both closed).
func (t *Transcoder) Run(progress bool) <-chan error {
	done := make(chan error, 1)

	t.mu.Lock()
	if t.run != nil {
		t.mu.Unlock()
		done <- errors.New("transcoder: a transcoding task is already running")
		close(done)
		return done
	}

	factory := t.newProcess
	if factory == nil {
		factory = newExecProcess
	}
	grace := t.stopGrace
	if grace <= 0 {
		grace = defaultStopGrace
	}

	rs := &runState{
		// Buffered so progress producers (the pump) do not block waiting for a
		// consumer to be scheduled, and a terminal failure is retained even if no
		// caller reads Output yet.
		progress: make(chan Progress, 64),
		stop:     make(chan struct{}),
		done:     done,
	}
	t.run = rs
	t.lastRun = rs
	t.stopping = false
	t.mu.Unlock()

	command := t.GetCommand()
	if !progress {
		command = append([]string{"-nostats", "-loglevel", "0"}, command...)
	}

	proc := factory(t.configuration.FFmpegBinPath(), command)
	rs.proc = proc
	// Keep the legacy exported field populated for callers using Process().
	if ep, ok := proc.(*execProcess); ok {
		t.SetProcess(ep.cmd)
	}

	var startErr error
	if progress {
		errStream, err := proc.StderrPipe()
		if err != nil {
			startErr = fmt.Errorf("progress not available: %w", err)
		} else {
			rs.stdErrPipe = errStream
			t.stdErrPipe = errStream
		}
	}

	stdin, err := proc.StdinPipe()
	if err != nil {
		// Stdin is only needed for a graceful Stop; record but do not fatal.
		fmt.Println("Stdin not available: " + err.Error())
	} else {
		rs.stdStdinPipe = stdin
		t.stdStdinPipe = stdin
	}

	// Buffer non-progress stdout; route the configured input/output pipes.
	var outb, errb bytes.Buffer
	if progress {
		proc.SetStdout(&outb)
	}
	if t.mediafile != nil && t.mediafile.InputPipe() {
		proc.SetStdin(t.mediafile.InputPipeReader())
	}
	if t.mediafile != nil && t.mediafile.OutputPipe() {
		proc.SetStdout(t.mediafile.OutputPipeWriter())
	}

	if startErr == nil {
		startErr = proc.Start()
	}

	if startErr != nil {
		failure := fmt.Errorf("failed start ffmpeg (%s) with %s, message %s %s",
			command, startErr, outb.String(), errb.String())
		// Deliver a recognizable failure on the progress channel (if anyone is
		// listening), then run the full cleanup so pipes are released and both
		// channels close promptly rather than hanging forever.
		t.finishWithError(rs, failure)
		return done
	}

	if progress && rs.stdErrPipe != nil {
		t.pumpProgress(rs)
	}

	go t.waitForCompletion(rs, command, &outb, &errb)

	return done
}

// pumpProgress forwards parsed progress lines into the run's shared, buffered
// channel. It signals pumpDone when it has fully exited. finalize waits on that
// signal before closing the channel, so a send can never race a close; the
// stop-aware sends guarantee the pump unwinds once the run is shutting down.
func (t *Transcoder) pumpProgress(rs *runState) {
	parsed := scanProgress(rs.stdErrPipe, t.mediafile, rs.stop)
	rs.pumpDone = make(chan struct{})

	go func() {
		defer close(rs.pumpDone)
		for {
			select {
			case <-rs.stop:
				return
			case p, ok := <-parsed:
				if !ok {
					return
				}
				select {
				case rs.progress <- p:
				case <-rs.stop:
					return
				}
			}
		}
	}()
}

// waitForCompletion blocks until the process exits, then finalizes the run.
func (t *Transcoder) waitForCompletion(rs *runState, command []string, outb, errb *bytes.Buffer) {
	waitErr := rs.proc.Wait()
	if waitErr != nil {
		waitErr = fmt.Errorf("failed finish ffmpeg (%s) with %s message %s %s",
			command, waitErr, outb.String(), errb.String())
	}
	t.finalize(rs, waitErr)
}

// finishWithError is used when the process never starts: report a recognizable
// terminal failure, then release everything.
func (t *Transcoder) finishWithError(rs *runState, failure error) {
	// The progress channel is buffered, so this terminal failure is retained even
	// if no caller has started reading Output yet. This send cannot block.
	rs.progress <- Progress{Err: failure}
	t.finalize(rs, failure)
}

// finalize releases every resource of a run exactly once and closes both the
// progress and done channels a single time. It is safe to call concurrently from
// the completion goroutine, the Stop watchdog or a start-failure path.
func (t *Transcoder) finalize(rs *runState, runErr error) {
	rs.closeOnce.Do(func() {
		// 1. Signal the progress reader to stop waiting for more input.
		rs.stopOnce.Do(func() { close(rs.stop) })

		// 2. Release process pipes exactly once. Closing stderr ends the
		//    scanner's blocking read so its pump goroutine can unwind.
		if rs.stdErrPipe != nil {
			_ = rs.stdErrPipe.Close()
		}
		if rs.stdStdinPipe != nil {
			_ = rs.stdStdinPipe.Close()
		}
		t.closeMediaPipes()

		// 3. Wait for the pump (if any) to exit before closing the progress
		//    channel, so no send can race the close. The pump's stop-aware sends
		//    and the pipe close above guarantee it unwinds promptly. Runs without
		//    a pump have a nil pumpDone.
		if rs.pumpDone != nil {
			<-rs.pumpDone
		}
		rs.closeProgress()

		// 4. Detach the active run but keep it as lastRun so Output() still
		//    returns its (now closed) channel and any buffered terminal failure.
		t.mu.Lock()
		if t.run == rs {
			t.run = nil
			t.stopping = false
		}
		t.lastRun = rs
		t.mu.Unlock()

		// 5. Deliver the terminal result. done is buffered, so this cannot block.
		rs.done <- runErr
		close(rs.done)
	})
}

// Stop requests an end to the running transcoding process.
//
// It first asks ffmpeg to quit gracefully ("q" over stdin). If the process does
// not exit within the grace period it is killed forcibly. Either way the progress
// channel is closed promptly, Stop never blocks beyond the grace window and it is
// safe to call repeatedly or concurrently with a natural completion.
func (t *Transcoder) Stop() error {
	t.mu.Lock()
	rs := t.run
	if rs == nil || t.stopping {
		t.mu.Unlock()
		return nil
	}
	t.stopping = true
	grace := t.stopGrace
	if grace <= 0 {
		grace = defaultStopGrace
	}
	t.mu.Unlock()

	rs.stopOnce.Do(func() { close(rs.stop) })

	// Graceful request first. The write is performed in its own goroutine so a
	// process that never reads stdin cannot block Stop; finalize closes the
	// stdin pipe, which unblocks a pending write with ErrClosedPipe.
	if rs.stdStdinPipe != nil {
		go func() { _, _ = rs.stdStdinPipe.Write([]byte("q\n")) }()
	}

	// Hard-kill fallback if the process ignores the graceful request.
	go func() {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-rs.proc.Done():
		case <-timer.C:
			_ = rs.proc.ProcessKill()
		}
	}()

	return nil
}

// Output Returns the transcoding progress channel.
//
// For an active run every call returns the same channel, so there is a single
// owner of the underlying stderr pipe and the channel is closed exactly once.
// After a run finishes the same (now closed) channel is still returned until the
// next Run, so buffered values — including a start failure — remain readable.
// Only when no run has ever been started is a freshly closed channel returned,
// so range loops end immediately instead of hanging.
func (t *Transcoder) Output() <-chan Progress {
	t.mu.Lock()
	rs := t.run
	if rs == nil {
		rs = t.lastRun
	}
	t.mu.Unlock()

	if rs == nil {
		closed := make(chan Progress)
		close(closed)
		return closed
	}
	return rs.progress
}

// closeMediaPipes releases any user-created input/output media pipes. It is
// idempotent and nil-safe: io.Pipe closes are no-ops on the second call and the
// media file may be absent in partially initialized runs.
func (t *Transcoder) closeMediaPipes() {
	if t.mediafile == nil {
		return
	}
	if t.mediafile.InputPipe() && t.mediafile.InputPipeReader() != nil {
		_ = t.mediafile.InputPipeReader().Close()
	}
	if t.mediafile.OutputPipe() && t.mediafile.OutputPipeWriter() != nil {
		_ = t.mediafile.OutputPipeWriter().Close()
	}
}
