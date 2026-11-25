package transcoder

import (
	"bytes"
	"io"
	"sync"
)

// countingReadCloser wraps an io.ReadCloser and records how many times Close is
// invoked, so tests can assert pipes are closed exactly once (no leak, no
// duplicate close). It forwards Close to the underlying resource so its
// goroutines also terminate, mirroring a real OS pipe close.
type countingReadCloser struct {
	io.Reader
	underlying io.Closer
	mu         sync.Mutex
	closeN     int
	closeErr   error
	closeOnce  sync.Once
}

func newCountingReadCloser(r io.ReadCloser) *countingReadCloser {
	return &countingReadCloser{Reader: r, underlying: r}
}

func (c *countingReadCloser) Close() error {
	c.mu.Lock()
	c.closeN++
	c.mu.Unlock()
	c.closeOnce.Do(func() {
		c.closeErr = c.underlying.Close()
	})
	return c.closeErr
}

func (c *countingReadCloser) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeN
}

// countingWriteCloser is the writer-side counterpart of countingReadCloser.
type countingWriteCloser struct {
	io.Writer
	underlying io.Closer
	mu         sync.Mutex
	closeN     int
	written    []byte
	closeOnce  sync.Once
	closeErr   error
}

func newCountingWriteCloser(w io.WriteCloser) *countingWriteCloser {
	return &countingWriteCloser{Writer: w, underlying: w}
}

func (c *countingWriteCloser) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.written = append(c.written, p...)
	c.mu.Unlock()
	return c.Writer.Write(p)
}

func (c *countingWriteCloser) Close() error {
	c.mu.Lock()
	c.closeN++
	c.mu.Unlock()
	c.closeOnce.Do(func() {
		c.closeErr = c.underlying.Close()
	})
	return c.closeErr
}

func (c *countingWriteCloser) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeN
}

func (c *countingWriteCloser) bytesWritten() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, len(c.written))
	copy(out, c.written)
	return out
}

// bufferedPipeWriter models the kernel-buffered stderr pipe of a real process:
// Write appends to an in-memory queue and a single pump goroutine streams the
// queue into an io.PipeReader. Writes therefore never block on scanner
// scheduling, and closing either side cleanly ends the pump without leaking.
type bufferedPipeWriter struct {
	mu         sync.Mutex
	cond       *sync.Cond
	buf        bytes.Buffer
	closed     bool
	readerGone chan struct{}

	pr *io.PipeReader
	pw *io.PipeWriter
}

func newBufferedPipeWriter() *bufferedPipeWriter {
	pr, pw := io.Pipe()
	b := &bufferedPipeWriter{
		pr:         pr,
		pw:         pw,
		readerGone: make(chan struct{}),
	}
	b.cond = sync.NewCond(&b.mu)
	go b.pump()
	return b
}

// pump is the single goroutine moving buffered bytes to the pipe reader.
func (b *bufferedPipeWriter) pump() {
	// Wake a parked cond.Wait if the reader side is closed, so the pump cannot
	// be stranded after the transcoder releases the pipe.
	go func() {
		<-b.readerGone
		b.cond.Broadcast()
	}()

	for {
		b.mu.Lock()
		for b.buf.Len() == 0 && !b.closed {
			b.cond.Wait()
			// Reader was closed while empty: nothing left to deliver.
			select {
			case <-b.readerGone:
				b.mu.Unlock()
				_ = b.pw.Close()
				return
			default:
			}
		}
		if b.buf.Len() == 0 && b.closed {
			b.mu.Unlock()
			_ = b.pw.Close()
			return
		}
		// Take the pending bytes and release the lock before the (potentially
		// blocking) pipe write so producers are not held up.
		data := make([]byte, b.buf.Len())
		_, _ = b.buf.Read(data)
		b.mu.Unlock()

		if _, err := b.pw.Write(data); err != nil {
			// Reader was closed: stop pumping.
			return
		}
	}
}

func (b *bufferedPipeWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	n, err := b.buf.Write(p)
	b.mu.Unlock()
	b.cond.Broadcast()
	return n, err
}

// Close signals end of stream; the pump flushes remaining data then closes the
// reader with EOF.
func (b *bufferedPipeWriter) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cond.Broadcast()
	return nil
}

// reader returns the io.ReadCloser consumed by the transcoder's scanner. Closing
// it signals the pump to unwind even if no end-of-stream was written.
func (b *bufferedPipeWriter) reader() io.ReadCloser {
	return &bufferedReadCloser{PipeReader: b.pr, gone: b.readerGone}
}

type bufferedReadCloser struct {
	*io.PipeReader
	once sync.Once
	gone chan<- struct{}
}

func (r *bufferedReadCloser) Close() error {
	r.once.Do(func() { close(r.gone) })
	return r.PipeReader.Close()
}

// fakeProcess is a fully controllable stand-in for an ffmpeg *exec.Cmd. It never
// spawns a real binary: all streams are in-memory io.Pipes driven by the test.
type fakeProcess struct {
	name string
	args []string

	stderrWriter *bufferedPipeWriter
	stderrHandle *countingReadCloser
	stdinReader  *io.PipeReader
	stdinWriter  *io.PipeWriter

	// Instrumented handle handed to the transcoder, so stdin close counts are tracked.
	stdinHandle *countingWriteCloser

	startErr error // returned by Start to simulate a launch failure

	mu        sync.Mutex
	started   bool
	waitErr   error
	stdinData []byte
	exited    chan struct{}
	once      sync.Once
	killed    chan struct{}
}

func newFakeProcess(name string, args []string) *fakeProcess {
	// Buffered stderr mimics the kernel pipe buffer of a real process: the fake
	// can write progress lines without blocking on whether the Go scanner has
	// been scheduled to read yet.
	bpw := newBufferedPipeWriter()
	sr, sw := io.Pipe()
	fp := &fakeProcess{
		name:         name,
		args:         args,
		stderrWriter: bpw,
		stdinReader:  sr,
		stdinWriter:  sw,
		exited:       make(chan struct{}),
		killed:       make(chan struct{}),
	}
	fp.stderrHandle = newCountingReadCloser(bpw.reader())
	fp.stdinHandle = newCountingWriteCloser(sw)

	// Mimic a real process consuming its stdin (ffmpeg reads keystrokes such as
	// "q"). This keeps StdinPipe writes from blocking; the data is recorded so
	// tests can assert the graceful request was sent.
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := sr.Read(buf)
			if n > 0 {
				fp.mu.Lock()
				fp.stdinData = append(fp.stdinData, buf[:n]...)
				fp.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	return fp
}

func (f *fakeProcess) StderrPipe() (io.ReadCloser, error) {
	return f.stderrHandle, nil
}

func (f *fakeProcess) StdinPipe() (io.WriteCloser, error) {
	return f.stdinHandle, nil
}

func (f *fakeProcess) SetStdout(io.Writer) {}
func (f *fakeProcess) SetStdin(io.Reader)  {}

func (f *fakeProcess) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return f.startErr
}

func (f *fakeProcess) Wait() error {
	<-f.exited
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waitErr
}

func (f *fakeProcess) Done() <-chan struct{} { return f.exited }

// ProcessKill simulates a forced kill by marking the process killed and
// releasing waiters.
func (f *fakeProcess) ProcessKill() error {
	f.once.Do(func() {
		close(f.killed)
		close(f.exited)
	})
	return nil
}

// exit simulates a natural process termination.
func (f *fakeProcess) exit(err error) {
	f.mu.Lock()
	f.waitErr = err
	f.mu.Unlock()
	f.once.Do(func() { close(f.exited) })
}

// emitProgress writes a progress line asynchronously, the way a real ffmpeg
// process writes to stderr independently of when the Go side happens to read.
// It returns once the data has been handed off (or the pipe is closed), so the
// test does not race the scanner goroutine's scheduling.
func (f *fakeProcess) emitProgress(line string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.stderrWriter.Write([]byte(line))
	}()
	return done
}

// closeStderr simulates the process closing its stderr stream on exit.
func (f *fakeProcess) closeStderr() {
	_ = f.stderrWriter.Close()
}

// wasKilled reports whether ProcessKill was invoked.
func (f *fakeProcess) wasKilled() bool {
	select {
	case <-f.killed:
		return true
	default:
		return false
	}
}

// receivedGracefulQ reports whether a "q" keystroke was delivered on stdin.
func (f *fakeProcess) receivedGracefulQ() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return bytes.Contains(f.stdinData, []byte("q"))
}

// newFakeFactory returns a processFactory plus a handle to the single fake
// process that the factory will produce.
func newFakeFactory() (processFactory, *fakeProcess) {
	fp := newFakeProcess("", nil)
	factory := func(name string, args []string) process {
		fp.name = name
		fp.args = args
		return fp
	}
	return factory, fp
}
