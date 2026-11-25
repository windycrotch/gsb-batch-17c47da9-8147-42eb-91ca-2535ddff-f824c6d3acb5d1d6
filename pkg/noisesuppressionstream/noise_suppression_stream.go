package noisesuppressionstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/facebookincubator/go-belt/tool/logger"
	"github.com/xaionaro-go/audio/pkg/audio"
	"github.com/xaionaro-go/audio/pkg/noisesuppression"
	"github.com/xaionaro-go/observability"
)

const (
	debugBypassNoiseSuppression = false
)

// NoiseSuppressionStream is an io.Reader that runs a noise suppressor over an
// upstream PCM stream in the background:
//
//   - a reader loop pulls PCM from the upstream reader into an input buffer;
//   - a noise suppression loop pulls fixed-size frames out of the input
//     buffer, processes them and pushes the result into an output buffer;
//   - Read serves the processed frames to one or more callers.
//
// Coordination between the parties uses condition variables (sync.Cond) so
// that backpressure blocks instead of dropping frames or busy-looping, and a
// shared terminal state guarantees deterministic shutdown for the three
// possible endings: clean upstream EOF, upstream/processing failure and
// external context cancellation.
type NoiseSuppressionStream struct {
	noisesuppression.NoiseSuppression
	encoding audio.Encoding
	channels audio.Channel

	// inputLocker guards the input buffer together with two condition
	// variables that model distinct predicates, so a broadcast can only wake
	// waiters whose predicate may have changed:
	//   - inputDataCond: new input bytes were appended (fillFrame waits);
	//   - inputSpaceCond: input bytes were consumed (writeInput waits).
	inputLocker    sync.Mutex
	inputBuffer    *boundedBuffer
	inputDataCond  *sync.Cond
	inputSpaceCond *sync.Cond
	inputClosed    bool

	// outputLocker guards the output buffer with two conditions:
	//   - outputDataCond: processed bytes were appended (Read waits);
	//   - outputSpaceCond: output bytes were consumed (writeOutput waits).
	outputLocker    sync.Mutex
	outputBuffer    *boundedBuffer
	outputDataCond  *sync.Cond
	outputSpaceCond *sync.Cond

	// stateLocker guards the terminal bookkeeping. It is a separate lock so
	// it can be taken from any party without risking a lock-ordering cycle,
	// and it is never held while waiting on a condition variable.
	stateLocker  sync.Mutex
	resultError  error
	runningLoops int32
	terminalOnce sync.Once
	terminalDone chan struct{}
	cancelLoop   context.CancelFunc
	// loopCtx drives the background goroutines and condition waits. It is
	// canceled both by external cancellation (via externalCtx) and by internal
	// termination. externalCtx is the user-supplied context, kept separate so
	// the terminal state can tell an external cancel apart from an internal
	// shutdown.
	loopCtx     context.Context
	externalCtx context.Context
}

var _ io.Reader = (*NoiseSuppressionStream)(nil)

func NewNoiseSuppressionStream(
	ctx context.Context,
	input io.Reader,
	noiseSuppression noisesuppression.NoiseSuppression,
	inputBufferSize uint,
	outputBufferSize uint,
) (*NoiseSuppressionStream, error) {
	encoding, err := noiseSuppression.Encoding(context.Background())
	if err != nil {
		return nil, fmt.Errorf("unable to get the encoding of the noise suppression: %w", err)
	}
	channels, err := noiseSuppression.Channels(context.Background())
	if err != nil {
		return nil, fmt.Errorf("unable to get the amount of channels of the noise suppression: %w", err)
	}
	if noiseSuppression.ChunkSize() == 0 {
		return nil, fmt.Errorf("the noise suppression frame size (ChunkSize) must not be zero")
	}

	loopCtx, cancelLoop := context.WithCancel(ctx)
	s := &NoiseSuppressionStream{
		NoiseSuppression: noiseSuppression,
		encoding:         encoding,
		channels:         channels,
		inputBuffer:      newBoundedBuffer(nil, int(inputBufferSize)),
		outputBuffer:     newBoundedBuffer(nil, int(outputBufferSize)),
		cancelLoop:       cancelLoop,
		loopCtx:          loopCtx,
		externalCtx:      ctx,
		terminalDone:     make(chan struct{}),
		runningLoops:     2,
	}
	s.inputBuffer.mu = &s.inputLocker
	s.outputBuffer.mu = &s.outputLocker
	s.inputDataCond = sync.NewCond(&s.inputLocker)
	s.inputSpaceCond = sync.NewCond(&s.inputLocker)
	s.outputDataCond = sync.NewCond(&s.outputLocker)
	s.outputSpaceCond = sync.NewCond(&s.outputLocker)

	// External-cancellation watcher: when the caller-supplied context ends,
	// finalize the stream immediately. This makes a blocked Read return
	// promptly with the caller's cancellation reason even if a loop is parked
	// inside non-interruptible upstream I/O.
	observability.Go(loopCtx, func(_ context.Context) {
		select {
		case <-ctx.Done():
			s.finalize()
		case <-s.terminalDone:
		}
	})

	// Internal-cancellation watcher: a failure or clean shutdown cancels the
	// loop context, which releases every condition-variable waiter.
	observability.Go(loopCtx, func(_ context.Context) {
		select {
		case <-loopCtx.Done():
			s.broadcastAll()
		case <-s.terminalDone:
		}
	})

	observability.Go(loopCtx, func(loopCtx context.Context) {
		defer s.loopExited()
		if err := s.readerLoop(loopCtx, input); err != nil && !isBenignLoopExit(err) {
			s.fail(fmt.Errorf("got an error from the reader loop: %w", err))
		}
	})
	observability.Go(loopCtx, func(loopCtx context.Context) {
		defer s.loopExited()
		if err := s.noiseSuppressionLoop(loopCtx); err != nil && !isBenignLoopExit(err) {
			s.fail(fmt.Errorf("got an error from the noise suppressor loop: %w", err))
		}
	})
	return s, nil
}

// isBenignLoopExit reports whether a loop's returned error is an expected
// termination reason (external cancellation or a clean drain to EOF) rather
// than a failure that must be surfaced to the caller.
func isBenignLoopExit(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF)
}

// fail records the terminal failure and finalizes the stream immediately,
// without waiting for the sibling loop (which may be blocked in third-party
// I/O that context cancellation cannot interrupt).
func (s *NoiseSuppressionStream) fail(err error) {
	s.setTerminalError(err)
	s.finalize()
}

// setTerminalError records the first terminal error of the stream. The
// recorded value is stable: every subsequent Read returns the same error.
func (s *NoiseSuppressionStream) setTerminalError(err error) {
	if err == nil {
		return
	}
	s.stateLocker.Lock()
	if s.resultError == nil {
		s.resultError = err
	}
	s.stateLocker.Unlock()
}

func (s *NoiseSuppressionStream) terminalError() error {
	s.stateLocker.Lock()
	defer s.stateLocker.Unlock()
	return s.resultError
}

// waitTerminated blocks until the stream reaches its terminal state. Used by
// tests to assert a deterministic shutdown.
func (s *NoiseSuppressionStream) waitTerminated() {
	<-s.terminalDone
}

// loopExited is run by every background loop when it stops. A clean shutdown
// finalizes once both loops are gone; an external cancellation or a failure
// finalizes independently of this counter (see fail and the watcher), so a
// loop blocked in non-interruptible upstream I/O cannot hold the stream open.
func (s *NoiseSuppressionStream) loopExited() {
	if remaining := atomic.AddInt32(&s.runningLoops, -1); remaining != 0 {
		return
	}
	s.finalize()
}

// finalize settles the terminal state exactly once. It picks the error to
// expose (an explicit failure, the external cancellation reason, or clean
// io.EOF), stops the internal context, releases every condition waiter and
// signals terminalDone. Safe to call from the loops, the failure path or the
// external-cancellation watcher.
func (s *NoiseSuppressionStream) finalize() {
	s.terminalOnce.Do(func() {
		s.stateLocker.Lock()
		if s.resultError == nil {
			if extErr := s.externalCtx.Err(); extErr != nil {
				s.resultError = extErr
			} else {
				s.resultError = io.EOF
			}
		}
		err := s.resultError
		s.stateLocker.Unlock()

		s.cancelLoop()

		s.inputLocker.Lock()
		s.inputClosed = true
		s.inputLocker.Unlock()
		s.broadcastAll()

		close(s.terminalDone)
		logger.Tracef(context.Background(), "noise suppression stream terminated: %v", err)
	})
}

// broadcastAll releases every condition-variable waiter so they can
// re-evaluate their predicates after a state change.
func (s *NoiseSuppressionStream) broadcastAll() {
	s.inputDataCond.Broadcast()
	s.inputSpaceCond.Broadcast()
	s.outputDataCond.Broadcast()
	s.outputSpaceCond.Broadcast()
}

// loopWaitDone reports whether the stream has terminated or its internal loop
// context was canceled. It is used by the background loops.
func (s *NoiseSuppressionStream) loopWaitDone() bool {
	select {
	case <-s.terminalDone:
		return true
	case <-s.loopCtx.Done():
		return true
	default:
		return false
	}
}

// externalSignalError returns the error to surface when a background wait
// returned because of an external signal (context cancellation, failure or
// termination).
func (s *NoiseSuppressionStream) externalSignalError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.loopWaitDone() {
		if termErr := s.terminalError(); termErr != nil {
			return termErr
		}
		return ctx.Err()
	}
	return nil
}

func (s *NoiseSuppressionStream) readerLoop(
	ctx context.Context,
	input io.Reader,
) (_err error) {
	logger.Tracef(ctx, "readerLoop")
	defer func() { logger.Tracef(ctx, "/readerLoop %v", _err) }()

	// On exit mark the input side closed and wake up the noise suppression
	// loop in case it is waiting for the remainder of a frame.
	defer func() {
		s.inputLocker.Lock()
		s.inputClosed = true
		s.inputLocker.Unlock()
		s.inputDataCond.Broadcast()
	}()

	readBuf := make([]byte, 65536)
	shortestMessageSize := s.encoding.BytesPerSample() * uint(s.channels)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		logger.Tracef(ctx, "readerLoop: Read()")
		n, err := input.Read(readBuf)
		logger.Tracef(ctx, "/readerLoop: Read(): %v %v", n, err)
		if n < 0 {
			return fmt.Errorf("received invalid value of received bytes: %d", n)
		}
		if n%int(shortestMessageSize) != 0 {
			return fmt.Errorf("received a message of size %d that is not a multiple of the sample size %d*%d: %w",
				n, s.encoding.BytesPerSample(), uint(s.channels), io.ErrUnexpectedEOF)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Clean end of the upstream input. Returning nil means "no
				// failure": the noise suppression loop still drains buffered
				// frames, and finalization turns this into io.EOF for the
				// caller.
				return nil
			}
			return fmt.Errorf("unable to read the backend: %w", err)
		}
		if n == 0 {
			// A well-behaved Reader should not do this, but tolerate it
			// instead of spinning.
			continue
		}

		if err := s.writeInput(ctx, readBuf[:n]); err != nil {
			return err
		}
	}
}

// writeInput copies data into the input buffer, applying backpressure
// (waiting instead of dropping bytes) when it is full.
func (s *NoiseSuppressionStream) writeInput(ctx context.Context, data []byte) error {
	offset := 0
	s.inputLocker.Lock()
	defer s.inputLocker.Unlock()
	for offset < len(data) {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Predicate first: write whenever any space is available. A partial
		// write is normal backpressure; we keep the offset and continue.
		if s.inputBuffer.Space() > 0 {
			w := s.inputBuffer.Write(data[offset:])
			if w == 0 {
				return fmt.Errorf("input buffer accepted no bytes despite reported space")
			}
			offset += w
			// New bytes are available: always wake the frame filler, even if
			// this was only a partial write before backpressure parks us.
			// Otherwise fillFrame (waiting for data) and this loop (waiting
			// for space) would deadlock.
			s.inputDataCond.Broadcast()
			continue
		}

		// Buffer full: park, then re-check the space predicate on the next
		// iteration so no broadcast can be lost.
		s.inputSpaceCond.Wait()
		if err := s.externalSignalError(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *NoiseSuppressionStream) noiseSuppressionLoop(ctx context.Context) (_err error) {
	logger.Tracef(ctx, "noiseSuppressionLoop")
	defer func() { logger.Tracef(ctx, "/noiseSuppressionLoop: %v", _err) }()

	frameSize := s.ChunkSize()
	logger.Debugf(ctx, "frameSize: %d", frameSize)

	inputBuf := make([]byte, frameSize)
	outputBuf := make([]byte, frameSize)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := s.fillFrame(ctx, inputBuf); err != nil {
			return err
		}

		if debugBypassNoiseSuppression {
			copy(outputBuf, inputBuf)
		} else {
			logger.Tracef(ctx, "s.NoiseSuppression.SuppressNoise")
			_, err := s.NoiseSuppression.SuppressNoise(ctx, inputBuf, outputBuf)
			logger.Tracef(ctx, "/s.NoiseSuppression.SuppressNoise: %v", err)
			if err != nil {
				return fmt.Errorf("unable to noise-suppress: %w", err)
			}
		}

		if err := s.writeOutput(ctx, outputBuf); err != nil {
			return err
		}
	}
}

// fillFrame reads exactly len(frame) bytes from the input buffer into frame.
// It blocks (without busy-looping) while the frame is incomplete. When the
// input is closed before a full frame could be assembled it returns
// io.ErrUnexpectedEOF; io.EOF means every buffered byte has already been
// processed.
func (s *NoiseSuppressionStream) fillFrame(ctx context.Context, frame []byte) error {
	frameSize := len(frame)
	receivedCount := 0

	s.inputLocker.Lock()
	defer s.inputLocker.Unlock()
	for receivedCount < frameSize {
		// Predicate first, Wait second — every iteration re-checks the
		// available bytes before parking, so a broadcast that happened
		// between iterations cannot be lost.
		if s.inputBuffer.Len() > 0 {
			n := s.inputBuffer.Read(frame[receivedCount:])
			if n < 0 {
				return fmt.Errorf("received a negative count: %d", n)
			}
			receivedCount += n
			if receivedCount >= frameSize {
				// Input was consumed: wake a back-pressured reader loop.
				s.inputSpaceCond.Broadcast()
				return nil
			}
			continue
		}

		if s.inputClosed {
			// A real terminal error (upstream failure, cancellation) always
			// takes precedence over the generic EOF signals.
			if termErr := s.terminalError(); termErr != nil {
				return termErr
			}
			if receivedCount == 0 {
				// Everything produced so far has been processed: the input
				// ended on a frame boundary.
				return io.EOF
			}
			// A partial frame can never be fed to the noise suppressor
			// without changing the frame size, so this is truncated input.
			return io.ErrUnexpectedEOF
		}

		s.inputDataCond.Wait()
		if err := s.externalSignalError(ctx); err != nil {
			return err
		}
	}
	return nil
}

// writeOutput writes a fully processed frame to the output buffer. On
// backpressure it waits for the caller to consume data instead of
// busy-looping: frames are never dropped and their order is preserved.
func (s *NoiseSuppressionStream) writeOutput(ctx context.Context, frame []byte) error {
	offset := 0
	s.outputLocker.Lock()
	defer s.outputLocker.Unlock()
	for offset < len(frame) {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Predicate first: write whenever any space is available. A partial
		// write is normal backpressure; the offset is kept and retried.
		if s.outputBuffer.Space() > 0 {
			w := s.outputBuffer.Write(frame[offset:])
			if w == 0 {
				return fmt.Errorf("output buffer accepted no bytes despite reported space")
			}
			offset += w
			// New output bytes are available even if the frame is not fully
			// written yet; wake a blocked Read so it cannot deadlock against
			// our backpressure wait.
			s.outputDataCond.Broadcast()
			continue
		}

		// Buffer full: park, then re-check the space predicate next round.
		s.outputSpaceCond.Wait()
		if err := s.externalSignalError(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Read implements io.Reader. It delivers every processed byte before
// surfacing the terminal state (io.EOF or an error), blocks without
// busy-looping while no output is available, and returns immediately with
// the cancellation reason once the context is canceled. Concurrent Read
// callers are serialized by outputLocker, so no byte is delivered twice or
// interleaved.
func (s *NoiseSuppressionStream) Read(pcm []byte) (_ret int, _err error) {
	logger.Tracef(s.externalCtx, "Read, len:%d", len(pcm))
	defer func() { logger.Tracef(s.externalCtx, "/Read, len:%d: %d, %v", len(pcm), _ret, _err) }()

	if len(pcm) == 0 {
		return 0, nil
	}

	s.outputLocker.Lock()
	defer s.outputLocker.Unlock()

	for {
		// Predicate first: drain every produced byte before surfacing the
		// terminal state, so no produced data is discarded.
		if s.outputBuffer.Len() > 0 {
			n := s.outputBuffer.Read(pcm)
			if n > 0 {
				// The caller consumed output: wake a back-pressured writer.
				s.outputSpaceCond.Broadcast()
				return n, nil
			}
			// Len reported data but Read yielded none: re-evaluate.
			continue
		}

		// Buffer empty. Surface the stable terminal state once work stopped;
		// checks happen before every wait so no signal can be lost.
		if termErr := s.terminalError(); termErr != nil {
			return 0, termErr
		}
		// External cancellation must be reported with the caller's reason.
		if ctxErr := s.externalCtx.Err(); ctxErr != nil {
			return 0, ctxErr
		}

		s.outputDataCond.Wait()
	}
}
