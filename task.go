// Package hls provides HLS transcoding on top of ffmpeg, exposed as
// awaitable, cancellable, success-or-failure tasks.
package hls

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Status describes the lifecycle state of a transcode task.
type Status int

const (
	// StatusRunning means the task has been accepted and at least one ffmpeg
	// process is (or is about to be) running.
	StatusRunning Status = iota
	// StatusCompleted means every requested variant finished and the master
	// playlist plus all variant artifacts were published to the target dir.
	StatusCompleted
	// StatusFailed means at least one variant failed; all task artifacts have
	// been removed from the target directory.
	StatusFailed
	// StatusCanceled means the task was canceled before completion; ffmpeg
	// processes were terminated/reaped and all task artifacts removed.
	StatusCanceled
)

func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

// Sentinel errors returned by the task API.
var (
	// ErrTargetBusy is returned by Submit when another task already owns the
	// target directory. The caller may retry once the running task finishes.
	ErrTargetBusy = errors.New("hls: target directory is already used by another transcode task")
	// ErrTaskCanceled is returned by Wait when the task was canceled.
	ErrTaskCanceled = errors.New("hls: transcode task canceled")
	// ErrNoResolutions is returned when no resolution preset is requested.
	ErrNoResolutions = errors.New("hls: at least one resolution is required")
)

// Config configures a transcode task.
type Config struct {
	// FFmpegPath is the ffmpeg executable. When empty, "ffmpeg" is looked up
	// from PATH.
	FFmpegPath string
	// SrcPath is the source media file.
	SrcPath string
	// TargetPath is the directory that will hold the published variant
	// playlists, segments and the master playlist.
	TargetPath string
	// Resolutions lists the requested presets; only 360p, 480p, 720p and
	// 1080p are accepted. Unknown presets make Submit fail.
	Resolutions []string
	// MasterPlaylist is the master playlist file name inside TargetPath.
	// Defaults to "playlist.m3u8".
	MasterPlaylist string
}

// VariantError describes the failure of a single resolution variant.
type VariantError struct {
	Resolution string
	Err        error
}

func (e *VariantError) Error() string {
	return fmt.Sprintf("hls: variant %q failed: %v", e.Resolution, e.Err)
}

func (e *VariantError) Unwrap() error { return e.Err }

// Task is an awaitable, cancellable transcode job.
type Task struct {
	id       string
	cfg      Config
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	statusMu sync.RWMutex
	status   Status
	err      error

	// externalCancel is set only when cancellation was requested by the
	// caller (Task.Cancel or the context passed to Submit). A variant failure
	// also cancels the internal context to stop siblings, but must not flip
	// the outcome into StatusCanceled.
	externalCancel atomic.Bool

	// release frees the target-directory lock; set only after the lock is
	// acquired and always invoked from run().
	release func()
}

// globalDirLocks enforces one task per target directory process-wide.
var globalDirLocks = newDirLockRegistry()

// newTaskID returns a random, filesystem-safe task identifier.
func newTaskID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail; fall back to time so staging dirs
		// still stay unique.
		return "task-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return "task-" + hex.EncodeToString(b[:])
}

// Submit validates the request, acquires exclusive ownership of the target
// directory and starts the transcode asynchronously. It does not wait for
// ffmpeg: use Task.Wait to block until every variant has finished.
//
// Submit returns an error (without starting anything) when configuration is
// invalid or when the target directory is already owned by another running
// task (ErrTargetBusy).
func Submit(ctx context.Context, cfg Config) (*Task, error) {
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	if cfg.SrcPath == "" {
		return nil, errors.New("hls: source path is required")
	}
	if cfg.TargetPath == "" {
		return nil, errors.New("hls: target path is required")
	}
	if len(cfg.Resolutions) == 0 {
		return nil, ErrNoResolutions
	}
	// Validate every requested resolution up front: unknown presets are an
	// error, never silently skipped.
	seen := make(map[string]struct{}, len(cfg.Resolutions))
	for _, res := range cfg.Resolutions {
		if _, err := getConfig(res); err != nil {
			return nil, err
		}
		if _, dup := seen[res]; dup {
			return nil, fmt.Errorf("hls: duplicate resolution %q", res)
		}
		seen[res] = struct{}{}
	}
	if cfg.MasterPlaylist == "" {
		cfg.MasterPlaylist = "playlist.m3u8"
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	release, err := globalDirLocks.tryAcquire(cfg.TargetPath)
	if err != nil {
		return nil, err
	}

	// The task owns an independent context: internal failure cancels siblings
	// without being mistaken for a caller-initiated cancellation.
	taskCtx, cancel := context.WithCancel(context.Background())
	t := &Task{
		id:      newTaskID(),
		cfg:     cfg,
		ctx:     taskCtx,
		cancel:  cancel,
		done:    make(chan struct{}),
		status:  StatusRunning,
		release: release,
	}

	// Propagate caller-context cancellation (and only that) as an external
	// cancellation.
	go func() {
		select {
		case <-ctx.Done():
			t.externalCancel.Store(true)
			t.cancel()
		case <-taskCtx.Done():
		}
	}()

	go t.run()
	return t, nil
}

// Transcode submits the task and waits for it to finish. The returned error
// is nil only when every requested variant completed successfully. Canceling
// ctx cancels the task.
func Transcode(ctx context.Context, cfg Config) (*Task, error) {
	t, err := Submit(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return t, t.Wait()
}

// ID returns the unique task identifier (also used as the staging dir name).
func (t *Task) ID() string { return t.id }

// Status returns the current task status.
func (t *Task) Status() Status {
	t.statusMu.RLock()
	defer t.statusMu.RUnlock()
	return t.status
}

// Wait blocks until the task reaches a terminal state and returns its final
// error. It returns nil on full success, ErrTaskCanceled on cancellation and
// a *VariantError (or wrapped error) on failure. It is safe to call Wait from
// multiple goroutines.
func (t *Task) Wait() error {
	<-t.done
	t.statusMu.RLock()
	defer t.statusMu.RUnlock()
	return t.err
}

// Done returns a channel closed when the task reaches a terminal state.
func (t *Task) Done() <-chan struct{} { return t.done }

// Cancel requests task termination. Any running ffmpeg process is killed and
// reaped, in-flight variants are stopped and everything the task wrote is
// removed. Canceling an already finished task is a no-op returning nil.
func (t *Task) Cancel() error {
	t.externalCancel.Store(true)
	t.cancel()
	return nil
}

// Resolutions returns the requested resolution presets.
func (t *Task) Resolutions() []string {
	out := make([]string, len(t.cfg.Resolutions))
	copy(out, t.cfg.Resolutions)
	return out
}

func (t *Task) setTerminal(status Status, err error) {
	t.statusMu.Lock()
	t.status = status
	t.err = err
	t.statusMu.Unlock()
}

// run owns the full task lifecycle: staging directory, parallel variants,
// atomic publish or full cleanup, and finally the directory lock.
func (t *Task) run() {
	defer close(t.done)
	defer t.release()

	if err := os.MkdirAll(t.cfg.TargetPath, 0o755); err != nil {
		t.setTerminal(StatusFailed, err)
		return
	}

	// All artifacts are produced inside a unique staging directory under the
	// target. Nothing visible in the target directory exists until every
	// variant succeeds and the results are published atomically.
	staging := filepath.Join(t.cfg.TargetPath, "."+t.id+".staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.setTerminal(StatusFailed, err)
		return
	}

	verr := t.runVariants(staging)

	switch {
	case t.externalCancel.Load():
		// Caller-initiated cancellation takes precedence over a variant error
		// that happened to race with the cancel.
		if err := t.cleanup(staging); err != nil {
			t.setTerminal(StatusCanceled, fmt.Errorf("%w: %v", ErrTaskCanceled, err))
			return
		}
		t.setTerminal(StatusCanceled, ErrTaskCanceled)
	case verr != nil:
		if err := t.cleanup(staging); err != nil {
			verr = fmt.Errorf("%w (cleanup failed: %v)", verr, err)
		}
		t.setTerminal(StatusFailed, verr)
	default:
		if err := t.publish(staging); err != nil {
			// publish already rolls back any files it moved; only the staging
			// directory remains to be removed.
			if cleanupErr := t.cleanup(staging); cleanupErr != nil {
				err = fmt.Errorf("%w (cleanup failed: %v)", err, cleanupErr)
			}
			t.setTerminal(StatusFailed, err)
			return
		}
		if err := t.cleanup(staging); err != nil {
			t.setTerminal(StatusFailed, err)
			return
		}
		t.setTerminal(StatusCompleted, nil)
	}
}

// runVariants runs one ffmpeg per requested resolution concurrently. The first
// failure (or cancellation) cancels the shared context so sibling variants are
// stopped as well; the first variant error is reported.
func (t *Task) runVariants(staging string) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)

	for _, res := range t.cfg.Resolutions {
		wg.Add(1)
		go func() {
			defer wg.Done()

			args, err := getOptions(t.cfg.SrcPath, staging, res)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = &VariantError{Resolution: res, Err: err}
				}
				mu.Unlock()
				t.cancel()
				return
			}

			if err := runFFmpeg(t.ctx, t.cfg.FFmpegPath, args); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = &VariantError{Resolution: res, Err: err}
				}
				mu.Unlock()
				// Stop sibling variants: the overall task can never succeed
				// once a single variant has failed.
				t.cancel()
			}
		}()
	}

	wg.Wait()
	return firstErr
}

// publish moves every variant artifact from staging into the target directory
// and then writes the master playlist last (and atomically), so the master
// playlist can never point at a variant whose playlist is not already in
// place. Any error rolls back every file this task moved into the target
// directory, guaranteeing a failed task leaves none of its artifacts behind.
func (t *Task) publish(staging string) (err error) {
	var moved []string
	rollback := func() {
		for _, name := range moved {
			// Best-effort removal of artifacts this task had published.
			_ = os.Remove(filepath.Join(t.cfg.TargetPath, name))
		}
	}

	entries, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		from := filepath.Join(staging, entry.Name())
		to := filepath.Join(t.cfg.TargetPath, entry.Name())
		if err := moveFile(from, to); err != nil {
			rollback()
			return err
		}
		moved = append(moved, entry.Name())
	}

	variants, err := GenerateHLSVariant(t.cfg.Resolutions, "")
	if err != nil {
		rollback()
		return err
	}
	// Atomic rename inside the target directory: readers see either the old
	// playlist or the complete new one, never a partial file.
	if err := GeneratePlaylist(variants, t.cfg.TargetPath, t.cfg.MasterPlaylist); err != nil {
		rollback()
		return err
	}
	return nil
}

// moveFile moves a file across directories. os.Rename is atomic within the
// same volume (staging lives inside the target directory). On Windows a
// rename onto an existing file fails, so the destination is removed first.
func moveFile(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	if err := os.Remove(to); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(from, to)
}

// cleanup removes the staging directory. On Windows ffmpeg file handles can be
// released slightly after process exit, so removal is retried for a short
// window before giving up. The returned error (if any) is folded into the
// task's terminal error by run().
func (t *Task) cleanup(staging string) error {
	const attempts = 5
	var err error
	for i := 0; i < attempts; i++ {
		err = os.RemoveAll(staging)
		if err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return fmt.Errorf("hls: failed to remove staging directory %q: %w", staging, err)
}
