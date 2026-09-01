// Package acptest provides deterministic managed-process fixtures for ACP transport tests.
package acptest

import (
	"bytes"
	"errors"
	"io"
	"sync"
)

// Process is a channel-controlled in-memory managed process.
type Process struct {
	Stdin           *Writer
	Stdout          *io.PipeReader
	OutputWriter    *io.PipeWriter
	Stderr          *io.PipeReader
	ErrorOutput     *io.PipeWriter
	WaitStarted     chan struct{}
	TerminateCalled chan struct{}
	KillCalled      chan struct{}
	Exit            chan error
	TerminateErr    error
	KillErr         error
	terminateOnce   sync.Once
	killOnce        sync.Once
}

func NewProcess() *Process {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	return &Process{Stdin: NewWriter(), Stdout: outR, OutputWriter: outW, Stderr: errR, ErrorOutput: errW, WaitStarted: make(chan struct{}), TerminateCalled: make(chan struct{}), KillCalled: make(chan struct{}), Exit: make(chan error, 1)}
}
func (p *Process) Input() io.WriteCloser   { return p.Stdin }
func (p *Process) OutputReader() io.Reader { return p.Stdout }
func (p *Process) Output() io.Reader       { return p.Stdout }
func (p *Process) Errors() io.Reader       { return p.Stderr }
func (p *Process) Wait() error {
	select {
	case <-p.WaitStarted:
	default:
		close(p.WaitStarted)
	}
	return <-p.Exit
}
func (p *Process) Terminate() error {
	p.terminateOnce.Do(func() { close(p.TerminateCalled) })
	return p.TerminateErr
}
func (p *Process) Kill() error { p.killOnce.Do(func() { close(p.KillCalled) }); return p.KillErr }
func (p *Process) Complete(err error) {
	p.Exit <- err
	_ = p.OutputWriter.Close()
	_ = p.ErrorOutput.Close()
}

// Writer records writes and can deterministically wedge them.
type Writer struct {
	mu            sync.Mutex
	buf           bytes.Buffer
	closed        bool
	block         chan struct{}
	closeBlock    chan struct{}
	closeWriteN   int
	closeWriteErr error
	closeWriteSet bool
	Changed       chan struct{}
	WriteStarted  chan struct{}
	writeOnce     sync.Once
}

func NewWriter() *Writer {
	return &Writer{Changed: make(chan struct{}, 1), WriteStarted: make(chan struct{})}
}
func (w *Writer) Block()      { w.mu.Lock(); w.block = make(chan struct{}); w.mu.Unlock() }
func (w *Writer) BlockClose() { w.mu.Lock(); w.closeBlock = make(chan struct{}); w.mu.Unlock() }
func (w *Writer) Release() {
	w.mu.Lock()
	if w.block != nil {
		close(w.block)
		w.block = nil
	}
	w.mu.Unlock()
}

// CompleteBlockedWriteOnClose makes a blocked write return a deterministic
// cumulative result when Close releases it. A negative n writes the full input.
func (w *Writer) CompleteBlockedWriteOnClose(n int, err error) {
	w.mu.Lock()
	w.block = make(chan struct{})
	w.closeWriteN = n
	w.closeWriteErr = err
	w.closeWriteSet = true
	w.mu.Unlock()
}
func (w *Writer) Write(p []byte) (int, error) {
	w.writeOnce.Do(func() { close(w.WriteStarted) })
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	block := w.block
	w.mu.Unlock()
	if block != nil {
		<-block
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		if !w.closeWriteSet {
			return 0, io.ErrClosedPipe
		}
		n := w.closeWriteN
		if n < 0 || n > len(p) {
			n = len(p)
		}
		_, _ = w.buf.Write(p[:n])
		return n, w.closeWriteErr
	}
	n, _ := w.buf.Write(p)
	select {
	case w.Changed <- struct{}{}:
	default:
	}
	return n, nil
}
func (w *Writer) Close() error {
	w.mu.Lock()
	closeBlock := w.closeBlock
	w.mu.Unlock()
	if closeBlock != nil {
		<-closeBlock
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("already closed")
	}
	w.closed = true
	if w.block != nil {
		close(w.block)
		w.block = nil
	}
	return nil
}
func (w *Writer) Bytes() []byte { w.mu.Lock(); defer w.mu.Unlock(); return bytes.Clone(w.buf.Bytes()) }
