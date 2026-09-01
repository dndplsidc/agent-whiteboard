// Package acp implements a bounded JSON-RPC 2.0 transport over a managed
// process's private newline-delimited standard input and output.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
)

const (
	DefaultMaxInboundFrameBytes  = 1 << 20
	DefaultMaxOutboundFrameBytes = 1 << 20
	DefaultMaxPending            = 64
	DefaultMaxRetainedBytes      = 32 << 20
	DefaultMaxIDBytes            = 256
	DefaultMaxMethodBytes        = 256
	DefaultMaxHandlerConcurrency = 4
	DefaultMaxHandlerQueue       = 64

	// SupportedMaxFrameBytes and SupportedMaxRetainedBytes are the largest
	// bounded transport configuration supported without weakening hard limits.
	SupportedMaxFrameBytes    = 128 << 20
	SupportedMaxRetainedBytes = 256 << 20
	HardMaxFrameBytes         = SupportedMaxFrameBytes
	HardMaxRetainedBytes      = 512 << 20
)

var (
	ErrClosed           = errors.New("ACP transport closed")
	ErrMalformed        = errors.New("malformed ACP frame")
	ErrFrameTooLarge    = errors.New("ACP frame too large")
	ErrPendingLimit     = errors.New("ACP pending request limit reached")
	ErrRetainedLimit    = errors.New("ACP retained byte limit reached")
	ErrDuplicateID      = errors.New("duplicate ACP request ID")
	ErrChildExited      = errors.New("ACP child exited")
	ErrAlreadyResponded = errors.New("ACP request already responded")
	ErrUnreaped         = errors.New("ACP child was not reaped")
	ErrStreamDrain      = errors.New("ACP process stream did not drain")
	ErrCleanupTimeout   = errors.New("ACP cleanup operation timed out")
)

type Delivery string

const (
	NotWritten    Delivery = "not_written"
	Complete      Delivery = "complete"
	Indeterminate Delivery = "indeterminate"
)

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("ACP RPC error %d", e.Code) }

type Request struct {
	ID        json.RawMessage
	Method    string
	Params    json.RawMessage
	Responder *Responder
}
type RequestHandler func(context.Context, Request)

type Options struct {
	MaxInboundFrameBytes  int
	MaxOutboundFrameBytes int
	MaxPending            int
	MaxInboundPending     int
	MaxRetainedBytes      int
	MaxIDBytes            int
	MaxMethodBytes        int
	GracePeriod           time.Duration
	TerminatePeriod       time.Duration
	HandlerTimeout        time.Duration
	FinalPeriod           time.Duration
	DrainPeriod           time.Duration
	MaxHandlerConcurrency int
	MaxHandlerQueue       int
	Handler               RequestHandler
}

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}
type result struct {
	value json.RawMessage
	err   error
}
type handlerJob struct {
	request   Request
	responder *Responder
	retained  int
}

type Client struct {
	child             common.ManagedProcess
	input             io.WriteCloser
	opts              Options
	writeToken        chan struct{}
	mu                sync.Mutex
	nextID            int64
	pending           map[int64]chan result
	pendingBytes      int
	inbound           map[string]*Responder
	closed            bool
	terminal          error
	done              chan struct{}
	waitResult        chan error
	stdoutDone        chan struct{}
	stderrDone        chan struct{}
	termOnce          sync.Once
	termDone          chan struct{}
	termErr           error
	handlerCtx        context.Context
	cancelHandlers    context.CancelFunc
	handlerSlots      chan struct{}
	notificationQueue chan handlerJob
	requestQueue      chan handlerJob
}

type responderState uint8

const (
	responderOpen responderState = iota
	responderInFlight
	responderSettled
)

type Responder struct {
	client       *Client
	id           json.RawMessage
	key          string
	retained     int
	mu           sync.Mutex
	state        responderState
	stateChanged chan struct{}
	done         chan struct{}
}

func New(child common.ManagedProcess, o Options) (*Client, error) {
	if common.IsNil(child) || child.Input() == nil || child.Output() == nil || child.Errors() == nil {
		return nil, errors.New("invalid ACP child")
	}
	if o.MaxInboundFrameBytes == 0 {
		o.MaxInboundFrameBytes = DefaultMaxInboundFrameBytes
	}
	if o.MaxOutboundFrameBytes == 0 {
		o.MaxOutboundFrameBytes = DefaultMaxOutboundFrameBytes
	}
	if o.MaxPending == 0 {
		o.MaxPending = DefaultMaxPending
	}
	if o.MaxInboundPending == 0 {
		o.MaxInboundPending = DefaultMaxPending
	}
	if o.MaxRetainedBytes == 0 {
		o.MaxRetainedBytes = DefaultMaxRetainedBytes
	}
	if o.MaxIDBytes == 0 {
		o.MaxIDBytes = DefaultMaxIDBytes
	}
	if o.MaxMethodBytes == 0 {
		o.MaxMethodBytes = DefaultMaxMethodBytes
	}
	if o.GracePeriod == 0 {
		o.GracePeriod = 500 * time.Millisecond
	}
	if o.TerminatePeriod == 0 {
		o.TerminatePeriod = 500 * time.Millisecond
	}
	if o.HandlerTimeout == 0 {
		o.HandlerTimeout = 30 * time.Second
	}
	if o.FinalPeriod == 0 {
		o.FinalPeriod = 500 * time.Millisecond
	}
	if o.DrainPeriod == 0 {
		o.DrainPeriod = 500 * time.Millisecond
	}
	if o.MaxHandlerConcurrency == 0 {
		o.MaxHandlerConcurrency = DefaultMaxHandlerConcurrency
	}
	if o.MaxHandlerQueue == 0 {
		o.MaxHandlerQueue = DefaultMaxHandlerQueue
	}
	if o.MaxInboundFrameBytes < 256 || o.MaxInboundFrameBytes > HardMaxFrameBytes || o.MaxOutboundFrameBytes < 256 || o.MaxOutboundFrameBytes > HardMaxFrameBytes || o.MaxPending < 1 || o.MaxPending > 1024 || o.MaxInboundPending < 1 || o.MaxInboundPending > 1024 || o.MaxRetainedBytes < 256 || o.MaxRetainedBytes > HardMaxRetainedBytes || o.MaxIDBytes < 1 || o.MaxIDBytes > 4096 || o.MaxMethodBytes < 1 || o.MaxMethodBytes > 4096 || o.GracePeriod < 0 || o.TerminatePeriod < 0 || o.FinalPeriod <= 0 || o.DrainPeriod <= 0 || o.HandlerTimeout <= 0 || o.MaxHandlerConcurrency < 1 || o.MaxHandlerConcurrency > 64 || o.MaxHandlerQueue < 1 || o.MaxHandlerQueue > 1024 {
		return nil, errors.New("invalid ACP options")
	}
	handlerCtx, cancelHandlers := context.WithCancel(context.Background())
	c := &Client{child: child, input: child.Input(), opts: o, writeToken: make(chan struct{}, 1), pending: map[int64]chan result{}, inbound: map[string]*Responder{}, done: make(chan struct{}), waitResult: make(chan error, 1), stdoutDone: make(chan struct{}), stderrDone: make(chan struct{}), termDone: make(chan struct{}), handlerCtx: handlerCtx, cancelHandlers: cancelHandlers, handlerSlots: make(chan struct{}, o.MaxHandlerConcurrency), notificationQueue: make(chan handlerJob, o.MaxHandlerQueue), requestQueue: make(chan handlerJob, o.MaxHandlerQueue)}
	c.writeToken <- struct{}{}
	go func() { defer close(c.stdoutDone); c.readLoop(child.Output()) }()
	go func() { defer close(c.stderrDone); _, _ = io.Copy(io.Discard, child.Errors()) }()
	go func() { err := child.Wait(); c.waitResult <- err; c.fail(ErrChildExited) }()
	go c.notificationWorker()
	for i := 0; i < o.MaxHandlerConcurrency; i++ {
		go c.requestWorker()
	}
	return c, nil
}

func (c *Client) Done() <-chan struct{} { return c.done }
func (c *Client) Err() error            { c.mu.Lock(); defer c.mu.Unlock(); return c.terminal }

func (c *Client) Call(ctx context.Context, method string, params any, out any) (Delivery, error) {
	if !validMethod(method, c.opts.MaxMethodBytes) {
		return NotWritten, ErrMalformed
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return NotWritten, err
	}
	c.mu.Lock()
	if c.closed {
		err = c.terminal
		if err == nil {
			err = ErrClosed
		}
		c.mu.Unlock()
		return NotWritten, err
	}
	if len(c.pending) >= c.opts.MaxPending {
		c.mu.Unlock()
		return NotWritten, ErrPendingLimit
	}
	retained := len(encoded) + len(method) + 64
	if c.pendingBytes+retained > c.opts.MaxRetainedBytes {
		c.mu.Unlock()
		return NotWritten, ErrRetainedLimit
	}
	c.nextID++
	id := c.nextID
	waiter := make(chan result, 1)
	c.pending[id] = waiter
	c.pendingBytes += retained
	c.mu.Unlock()
	defer c.remove(id, retained)
	frame, _ := json.Marshal(envelope{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", id)), Method: method, Params: encoded})
	delivery, werr := c.writeFrame(ctx, frame)
	if werr != nil {
		return delivery, werr
	}
	select {
	case got := <-waiter:
		if got.err != nil {
			return Complete, got.err
		}
		if out != nil {
			if err = json.Unmarshal(got.value, out); err != nil {
				return Complete, ErrMalformed
			}
		}
		return Complete, nil
	case <-ctx.Done():
		return Complete, ctx.Err()
	case <-c.done:
		return Complete, c.closedErr()
	}
}

func (c *Client) Notify(ctx context.Context, method string, params any) (Delivery, error) {
	if !validMethod(method, c.opts.MaxMethodBytes) {
		return NotWritten, ErrMalformed
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return NotWritten, err
	}
	frame, _ := json.Marshal(envelope{JSONRPC: "2.0", Method: method, Params: encoded})
	return c.writeFrame(ctx, frame)
}

func (c *Client) writeFrame(ctx context.Context, frame []byte) (Delivery, error) {
	if len(frame)+1 > c.opts.MaxOutboundFrameBytes {
		return NotWritten, ErrFrameTooLarge
	}
	select {
	case <-ctx.Done():
		return NotWritten, ctx.Err()
	case <-c.done:
		return NotWritten, c.closedErr()
	case <-c.writeToken:
	}
	defer func() { c.writeToken <- struct{}{} }()
	select {
	case <-ctx.Done():
		return NotWritten, ctx.Err()
	default:
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return NotWritten, c.closedErr()
	}
	payload := append(append([]byte(nil), frame...), '\n')
	type wr struct {
		n   int
		err error
	}
	ch := make(chan wr, 1)
	go func() {
		total := 0
		for total < len(payload) {
			n, e := c.input.Write(payload[total:])
			total += n
			if e != nil || n <= 0 {
				if e == nil {
					e = io.ErrShortWrite
				}
				ch <- wr{total, e}
				return
			}
		}
		ch <- wr{total, nil}
	}()
	classify := func(n int) Delivery {
		switch {
		case n == 0:
			return NotWritten
		case n == len(payload):
			return Complete
		default:
			return Indeterminate
		}
	}
	select {
	case got := <-ch:
		d := classify(got.n)
		if got.err != nil {
			c.fail(got.err)
			return d, got.err
		}
		return d, nil
	case <-ctx.Done():
		cause := ctx.Err()
		c.fail(cause)
		select {
		case got := <-ch:
			return classify(got.n), cause
		case <-time.After(c.opts.FinalPeriod):
			return Indeterminate, cause
		}
	case <-c.done:
		cause := c.closedErr()
		select {
		case got := <-ch:
			return classify(got.n), cause
		case <-time.After(c.opts.FinalPeriod):
			return Indeterminate, cause
		}
	}
}

func (c *Client) readLoop(reader io.Reader) {
	br := bufio.NewReaderSize(reader, 32<<10)
	for {
		line, err := readBoundedLine(br, c.opts.MaxInboundFrameBytes)
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.fail(ErrChildExited)
			} else {
				c.fail(err)
			}
			return
		}
		if len(line) == 0 || !json.Valid(line) || duplicateJSONKey(line) {
			c.fail(ErrMalformed)
			return
		}
		var msg envelope
		if json.Unmarshal(line, &msg) != nil || msg.JSONRPC != "2.0" || len(msg.Method) > c.opts.MaxMethodBytes || len(msg.ID) > c.opts.MaxIDBytes {
			c.fail(ErrMalformed)
			return
		}
		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			if msg.Result != nil || msg.Error != nil || !c.dispatchRequest(msg, len(line)) {
				if msg.Result != nil || msg.Error != nil {
					c.fail(ErrMalformed)
				}
				return
			}
		case msg.Method != "" && len(msg.ID) == 0:
			if msg.Result != nil || msg.Error != nil {
				c.fail(ErrMalformed)
				return
			}
			if !c.dispatchNotification(msg, len(line)) {
				return
			}
		case msg.Method == "" && len(msg.ID) > 0:
			if msg.Params != nil {
				c.fail(ErrMalformed)
				return
			}
			if !c.handleResponse(msg) {
				return
			}
		default:
			c.fail(ErrMalformed)
			return
		}
	}
}

func readBoundedLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		p, e := br.ReadSlice('\n')
		if len(line)+len(p) > max {
			return nil, ErrFrameTooLarge
		}
		line = append(line, p...)
		if errors.Is(e, bufio.ErrBufferFull) {
			continue
		}
		if e != nil {
			if errors.Is(e, io.EOF) && len(line) > 0 {
				return nil, ErrMalformed
			}
			return nil, e
		}
		break
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	return line, nil
}
func (c *Client) dispatchNotification(msg envelope, retained int) bool {
	if c.opts.Handler == nil {
		return true
	}
	c.mu.Lock()
	if c.closed || c.pendingBytes+retained > c.opts.MaxRetainedBytes {
		c.mu.Unlock()
		c.fail(ErrRetainedLimit)
		return false
	}
	c.pendingBytes += retained
	c.mu.Unlock()
	job := handlerJob{request: Request{Method: msg.Method, Params: clone(msg.Params)}, retained: retained}
	select {
	case c.notificationQueue <- job:
		return true
	default:
		c.releaseHandlerBytes(retained)
		c.fail(ErrPendingLimit)
		return false
	}
}

func (c *Client) notificationWorker() {
	for {
		select {
		case <-c.done:
			return
		case job := <-c.notificationQueue:
			if !c.acquireHandler() {
				c.releaseHandlerBytes(job.retained)
				return
			}
			ctx, cancel := context.WithTimeout(c.handlerCtx, c.opts.HandlerTimeout)
			c.opts.Handler(ctx, job.request)
			cancel()
			c.releaseHandler()
			c.releaseHandlerBytes(job.retained)
		}
	}
}
func (c *Client) requestWorker() {
	for {
		select {
		case <-c.done:
			return
		case job := <-c.requestQueue:
			if !c.acquireHandler() {
				return
			}
			ctx, cancel := context.WithTimeout(c.handlerCtx, c.opts.HandlerTimeout)
			if c.opts.Handler == nil {
				_, _ = job.responder.Respond(ctx, nil, &RPCError{Code: -32601, Message: "method not found"})
				cancel()
				c.releaseHandler()
				continue
			}
			job.request.Responder = job.responder
			go func() { defer c.releaseHandler(); c.opts.Handler(ctx, job.request) }()
			select {
			case <-job.responder.done:
			case <-ctx.Done():
				responseCtx, responseCancel := context.WithTimeout(context.Background(), c.opts.FinalPeriod)
				_, _ = job.responder.respond(responseCtx, nil, &RPCError{Code: -32603, Message: "request expired"}, true)
				responseCancel()
			}
			cancel()
		}
	}
}
func (c *Client) acquireHandler() bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.handlerSlots <- struct{}{}:
		select {
		case <-c.done:
			c.releaseHandler()
			return false
		default:
			return true
		}
	case <-c.done:
		return false
	}
}
func (c *Client) releaseHandler() { <-c.handlerSlots }
func (c *Client) releaseHandlerBytes(n int) {
	c.mu.Lock()
	if c.pendingBytes >= n {
		c.pendingBytes -= n
	}
	c.mu.Unlock()
}
func (c *Client) dispatchRequest(msg envelope, retained int) bool {
	key, valid := requestIDKey(msg.ID)
	if !valid {
		c.fail(ErrMalformed)
		return false
	}
	c.mu.Lock()
	if c.closed || len(c.inbound) >= c.opts.MaxInboundPending || c.pendingBytes+retained > c.opts.MaxRetainedBytes {
		c.mu.Unlock()
		c.fail(ErrPendingLimit)
		return false
	}
	if _, ok := c.inbound[key]; ok {
		c.mu.Unlock()
		c.fail(ErrDuplicateID)
		return false
	}
	r := &Responder{client: c, id: clone(msg.ID), key: key, retained: retained, stateChanged: make(chan struct{}), done: make(chan struct{})}
	c.inbound[key] = r
	c.pendingBytes += retained
	c.mu.Unlock()
	job := handlerJob{request: Request{ID: clone(msg.ID), Method: msg.Method, Params: clone(msg.Params)}, responder: r, retained: retained}
	select {
	case c.requestQueue <- job:
		return true
	default:
		c.releaseInbound(key)
		c.fail(ErrPendingLimit)
		return false
	}
}
func (r *Responder) Respond(ctx context.Context, value any, rpcErr *RPCError) (Delivery, error) {
	return r.respond(ctx, value, rpcErr, false)
}

func (r *Responder) respond(ctx context.Context, value any, rpcErr *RPCError, waitInFlight bool) (Delivery, error) {
	if rpcErr != nil && value != nil {
		return NotWritten, ErrMalformed
	}
	response := envelope{JSONRPC: "2.0", ID: clone(r.id)}
	if rpcErr != nil {
		response.Error = rpcErr
	} else {
		raw, err := json.Marshal(value)
		if err != nil {
			return NotWritten, err
		}
		response.Result = raw
	}
	frame, err := json.Marshal(response)
	if err != nil {
		return NotWritten, err
	}
	if len(frame)+1 > r.client.opts.MaxOutboundFrameBytes {
		return NotWritten, ErrFrameTooLarge
	}
	select {
	case <-ctx.Done():
		return NotWritten, ctx.Err()
	default:
	}

	for {
		r.mu.Lock()
		switch r.state {
		case responderOpen:
			r.state = responderInFlight
			r.signalStateChangeLocked()
			r.mu.Unlock()
			delivery, writeErr := r.client.writeFrame(ctx, frame)
			r.finishAttempt(delivery)
			return delivery, writeErr
		case responderSettled:
			r.mu.Unlock()
			return NotWritten, ErrAlreadyResponded
		default:
			changed := r.stateChanged
			r.mu.Unlock()
			if !waitInFlight {
				return NotWritten, ErrAlreadyResponded
			}
			select {
			case <-changed:
			case <-ctx.Done():
				return NotWritten, ctx.Err()
			case <-r.client.done:
				return NotWritten, r.client.closedErr()
			}
		}
	}
}

func (r *Responder) finishAttempt(delivery Delivery) {
	closed := r.client.isClosed()
	release := false
	r.mu.Lock()
	if r.state == responderInFlight {
		if delivery == NotWritten && !closed {
			r.state = responderOpen
			r.signalStateChangeLocked()
		} else {
			r.state = responderSettled
			r.signalStateChangeLocked()
			close(r.done)
			release = true
		}
	}
	r.mu.Unlock()
	if release {
		r.client.releaseInbound(r.key)
	}
}

func (r *Responder) settle() {
	r.mu.Lock()
	if r.state != responderSettled {
		r.state = responderSettled
		r.signalStateChangeLocked()
		close(r.done)
	}
	r.mu.Unlock()
}

func (r *Responder) signalStateChangeLocked() {
	close(r.stateChanged)
	r.stateChanged = make(chan struct{})
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
func (c *Client) releaseInbound(key string) {
	c.mu.Lock()
	if r, ok := c.inbound[key]; ok {
		delete(c.inbound, key)
		c.pendingBytes -= r.retained
	}
	c.mu.Unlock()
}
func (c *Client) handleResponse(msg envelope) bool {
	var id int64
	if json.Unmarshal(msg.ID, &id) != nil || id <= 0 || (msg.Error == nil) == (msg.Result == nil) {
		c.fail(ErrMalformed)
		return false
	}
	c.mu.Lock()
	waiter, ok := c.pending[id]
	allocated := id <= c.nextID
	c.mu.Unlock()
	if !allocated {
		c.fail(ErrMalformed)
		return false
	}
	if !ok {
		return true
	}
	if msg.Error != nil {
		waiter <- result{err: msg.Error}
	} else {
		waiter <- result{value: clone(msg.Result)}
	}
	return true
}
func (c *Client) remove(id int64, n int) {
	c.mu.Lock()
	if _, ok := c.pending[id]; ok {
		delete(c.pending, id)
		c.pendingBytes -= n
	}
	c.mu.Unlock()
}
func (c *Client) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminal != nil {
		return c.terminal
	}
	return ErrClosed
}
func (c *Client) fail(err error) {
	if err == nil {
		err = ErrClosed
	}
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.terminal = err
		c.cancelHandlers()
		pending := c.pending
		c.pending = map[int64]chan result{}
		c.pendingBytes = 0
		close(c.done)
		for _, r := range c.inbound {
			r.settle()
		}
		c.inbound = map[string]*Responder{}
		c.mu.Unlock()
		for _, w := range pending {
			w <- result{err: err}
		}
	} else {
		c.mu.Unlock()
	}
	c.startTerminalization()
}
func (c *Client) startTerminalization() { c.termOnce.Do(func() { go c.terminalize() }) }
func (c *Client) terminalize() {
	var errs []error
	if e := boundedOperation("close stdin", c.opts.FinalPeriod, c.input.Close); e != nil {
		errs = append(errs, e)
	}
	if e, ok := c.awaitProcess(c.opts.GracePeriod); ok {
		c.finishReaped(errs, e)
		return
	}
	if e := boundedOperation("terminate", c.opts.FinalPeriod, c.child.Terminate); e != nil {
		errs = append(errs, e)
	}
	if e, ok := c.awaitProcess(c.opts.TerminatePeriod); ok {
		c.finishReaped(errs, e)
		return
	}
	if e := boundedOperation("kill", c.opts.FinalPeriod, c.child.Kill); e != nil {
		errs = append(errs, e)
	}
	if e, ok := c.awaitProcess(c.opts.FinalPeriod); ok {
		c.finishReaped(errs, e)
		return
	}
	errs = append(errs, ErrUnreaped)
	c.finishTerminal(errs)
}
func boundedOperation(name string, d time.Duration, operation func() error) error {
	result := make(chan error, 1)
	go func() { result <- operation() }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case err := <-result:
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	case <-t.C:
		return fmt.Errorf("%s: %w", name, ErrCleanupTimeout)
	}
}
func (c *Client) finishReaped(errs []error, waitErr error) {
	if waitErr != nil {
		errs = append(errs, fmt.Errorf("wait: %w", waitErr))
	}
	c.finishTerminal(errs)
}
func (c *Client) awaitProcess(d time.Duration) (error, bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case e := <-c.waitResult:
		return e, true
	case <-t.C:
		return nil, false
	}
}
func (c *Client) finishTerminal(errs []error) {
	if !awaitDone(c.stdoutDone, c.opts.DrainPeriod) {
		errs = append(errs, fmt.Errorf("stdout: %w", ErrStreamDrain))
	}
	if !awaitDone(c.stderrDone, c.opts.DrainPeriod) {
		errs = append(errs, fmt.Errorf("stderr: %w", ErrStreamDrain))
	}
	c.mu.Lock()
	c.termErr = errors.Join(errs...)
	c.mu.Unlock()
	close(c.termDone)
}
func awaitDone(done <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}
func (c *Client) Shutdown(ctx context.Context) error {
	c.fail(ErrClosed)
	select {
	case <-c.termDone:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.termErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

var integerID = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func requestIDKey(raw json.RawMessage) (string, bool) {
	var text string
	if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &text) == nil {
		return "s:" + text, true
	}
	if integerID.Match(raw) {
		return "n:" + string(raw), true
	}
	return "", false
}
func validMethod(s string, max int) bool { return s != "" && len(s) <= max }
func clone(v []byte) json.RawMessage     { return append(json.RawMessage(nil), v...) }
func duplicateJSONKey(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func() bool
	walk = func() bool {
		tok, e := dec.Token()
		if e != nil {
			return true
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return false
		}
		switch d {
		case '{':
			seen := map[string]struct{}{}
			for dec.More() {
				t, e := dec.Token()
				k, ok := t.(string)
				if e != nil || !ok {
					return true
				}
				if _, ok = seen[k]; ok {
					return true
				}
				seen[k] = struct{}{}
				if walk() {
					return true
				}
			}
			_, e = dec.Token()
			return e != nil
		case '[':
			for dec.More() {
				if walk() {
					return true
				}
			}
			_, e = dec.Token()
			return e != nil
		}
		return false
	}
	return walk()
}
