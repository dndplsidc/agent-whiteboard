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
type wireItem struct {
	msg      envelope
	err      error
	retained int
	deadline time.Time
}

type handlerBarrier struct {
	client   *Client
	deadline time.Time
	done     chan struct{}
	mu       sync.Mutex
	settled  bool
	onExpiry func()
	onSettle func()
}

type Client struct {
	child            common.ManagedProcess
	input            io.WriteCloser
	opts             Options
	writeToken       chan struct{}
	mu               sync.Mutex
	nextID           int64
	pending          map[int64]chan result
	pendingBytes     int
	inbound          map[string]*Responder
	closed           bool
	terminal         error
	done             chan struct{}
	waitResult       chan error
	stdoutDone       chan struct{}
	stderrDone       chan struct{}
	termOnce         sync.Once
	termDone         chan struct{}
	termErr          error
	handlerCtx       context.Context
	cancelHandlers   context.CancelFunc
	wireQueue        chan wireItem
	handlerSlots     chan struct{}
	handlerAdmission chan struct{}
}

type responderState uint8

const (
	responderOpen responderState = iota
	responderInFlight
	responderSettled
)

type ResponderOutcome struct {
	Delivery Delivery
	Expired  bool
	Settled  bool
}

type Responder struct {
	client        *Client
	id            json.RawMessage
	key           string
	retained      int
	mu            sync.Mutex
	state         responderState
	stateChanged  chan struct{}
	done          chan struct{}
	outcome       ResponderOutcome
	deadline      time.Time
	expiryClaimed bool
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
	c := &Client{child: child, input: child.Input(), opts: o, writeToken: make(chan struct{}, 1), pending: map[int64]chan result{}, inbound: map[string]*Responder{}, done: make(chan struct{}), waitResult: make(chan error, 1), stdoutDone: make(chan struct{}), stderrDone: make(chan struct{}), termDone: make(chan struct{}), handlerCtx: handlerCtx, cancelHandlers: cancelHandlers, wireQueue: make(chan wireItem, o.MaxHandlerQueue), handlerSlots: make(chan struct{}, o.MaxHandlerConcurrency)}
	c.handlerAdmission = make(chan struct{})
	close(c.handlerAdmission)
	c.writeToken <- struct{}{}
	go func() { defer close(c.stdoutDone); c.readLoop(child.Output()) }()
	go func() { defer close(c.stderrDone); _, _ = io.Copy(io.Discard, child.Errors()) }()
	go func() { err := child.Wait(); c.waitResult <- err; c.fail(ErrChildExited) }()
	go c.coordinateWire()
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
			cause := got.err
			if ctx.Err() != nil {
				cause = ctx.Err()
			}
			c.fail(cause)
			return d, cause
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
		item := wireItem{retained: len(line), deadline: time.Now().Add(c.opts.HandlerTimeout)}
		if len(line) == 0 || !json.Valid(line) || duplicateJSONKey(line) {
			item.err = ErrMalformed
		} else if json.Unmarshal(line, &item.msg) != nil || item.msg.JSONRPC != "2.0" || len(item.msg.Method) > c.opts.MaxMethodBytes || len(item.msg.ID) > c.opts.MaxIDBytes {
			item.err = ErrMalformed
		}
		if !c.enqueueWire(item) {
			return
		}
	}
}

func (c *Client) enqueueWire(item wireItem) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	if c.pendingBytes+item.retained > c.opts.MaxRetainedBytes {
		c.mu.Unlock()
		c.fail(ErrRetainedLimit)
		return false
	}
	c.pendingBytes += item.retained
	c.mu.Unlock()
	select {
	case c.wireQueue <- item:
		return true
	default:
		c.releaseHandlerBytes(item.retained)
		c.fail(ErrPendingLimit)
		return false
	}
}

func (c *Client) coordinateWire() {
	barriers := make([]*handlerBarrier, 0, c.opts.MaxHandlerQueue)
	var notificationTail *handlerBarrier
	for {
		barriers = pruneBarriers(barriers)
		if len(barriers) >= c.opts.MaxHandlerQueue {
			if !c.waitBarrier(barriers[0]) {
				return
			}
			continue
		}
		select {
		case <-c.done:
			return
		case item := <-c.wireQueue:
			if item.err != nil {
				if !c.waitBarriers(barriers) {
					return
				}
				c.releaseHandlerBytes(item.retained)
				c.fail(item.err)
				return
			}
			msg := item.msg
			switch {
			case msg.Method != "" && len(msg.ID) > 0:
				if msg.Result != nil || msg.Error != nil {
					if !c.waitBarriers(barriers) {
						return
					}
					c.releaseHandlerBytes(item.retained)
					c.fail(ErrMalformed)
					return
				}
				barrier, ok := c.dispatchRequest(msg, item.retained, item.deadline)
				if !ok {
					return
				}
				barriers = append(barriers, barrier)
			case msg.Method != "" && len(msg.ID) == 0:
				if msg.Result != nil || msg.Error != nil {
					if !c.waitBarriers(barriers) {
						return
					}
					c.releaseHandlerBytes(item.retained)
					c.fail(ErrMalformed)
					return
				}
				barrier := c.dispatchNotification(msg, item.retained, item.deadline, notificationTail)
				notificationTail = barrier
				barriers = append(barriers, barrier)
			case msg.Method == "" && len(msg.ID) > 0:
				if !c.waitBarriers(barriers) {
					return
				}
				barriers = barriers[:0]
				if msg.Params != nil {
					c.releaseHandlerBytes(item.retained)
					c.fail(ErrMalformed)
					return
				}
				ok := c.handleResponse(msg)
				c.releaseHandlerBytes(item.retained)
				if !ok {
					return
				}
			default:
				if !c.waitBarriers(barriers) {
					return
				}
				c.releaseHandlerBytes(item.retained)
				c.fail(ErrMalformed)
				return
			}
		}
	}
}

func pruneBarriers(barriers []*handlerBarrier) []*handlerBarrier {
	for len(barriers) > 0 {
		select {
		case <-barriers[0].done:
			barriers = barriers[1:]
		default:
			return barriers
		}
	}
	return barriers
}

func (c *Client) waitBarrier(barrier *handlerBarrier) bool {
	select {
	case <-barrier.done:
		select {
		case <-c.done:
			return false
		default:
			return true
		}
	case <-c.done:
		return false
	}
}
func (c *Client) waitBarriers(barriers []*handlerBarrier) bool {
	for _, barrier := range barriers {
		if !c.waitBarrier(barrier) {
			return false
		}
	}
	return true
}

func (c *Client) newBarrier(deadline time.Time, onExpiry, onSettle func()) *handlerBarrier {
	b := &handlerBarrier{client: c, deadline: deadline, done: make(chan struct{}), onExpiry: onExpiry, onSettle: onSettle}
	go func() {
		t := time.NewTimer(time.Until(deadline))
		defer t.Stop()
		select {
		case <-t.C:
			b.finish(time.Time{})
		case <-b.done:
		case <-c.done:
		}
	}()
	return b
}

func (b *handlerBarrier) returned() { b.finish(time.Now()) }
func (b *handlerBarrier) finish(completedAt time.Time) {
	b.mu.Lock()
	if b.settled {
		b.mu.Unlock()
		return
	}
	onTime := !completedAt.IsZero() && completedAt.Before(b.deadline)
	b.settled = true
	onExpiry, onSettle := b.onExpiry, b.onSettle
	if onTime {
		close(b.done)
	}
	b.mu.Unlock()
	if onSettle != nil {
		onSettle()
	}
	if !onTime {
		if onExpiry != nil {
			onExpiry()
		}
		b.client.fail(context.DeadlineExceeded)
		close(b.done)
	}
}

func (c *Client) runHandler(barrier *handlerBarrier, previous *handlerBarrier, request Request) {
	previousAdmission := c.handlerAdmission
	admitted := make(chan struct{})
	c.handlerAdmission = admitted
	go func() {
		<-previousAdmission
		if previous != nil && !c.waitBarrier(previous) {
			close(admitted)
			return
		}
		select {
		case c.handlerSlots <- struct{}{}:
			close(admitted)
		case <-barrier.done:
			close(admitted)
			return
		case <-c.done:
			close(admitted)
			return
		}
		select {
		case <-barrier.done:
			<-c.handlerSlots
			return
		default:
		}
		if !time.Now().Before(barrier.deadline) {
			<-c.handlerSlots
			barrier.finish(time.Time{})
			return
		}
		ctx, cancel := context.WithDeadline(c.handlerCtx, barrier.deadline)
		func() {
			defer barrier.returned()
			c.opts.Handler(ctx, request)
		}()
		cancel()
		<-c.handlerSlots
	}()
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

func (c *Client) dispatchNotification(msg envelope, retained int, deadline time.Time, previous *handlerBarrier) *handlerBarrier {
	barrier := c.newBarrier(deadline, nil, func() { c.releaseHandlerBytes(retained) })
	if c.opts.Handler == nil {
		barrier.returned()
		return barrier
	}
	c.runHandler(barrier, previous, Request{Method: msg.Method, Params: clone(msg.Params)})
	return barrier
}

func (c *Client) releaseHandlerBytes(n int) {
	c.mu.Lock()
	if c.pendingBytes >= n {
		c.pendingBytes -= n
	}
	c.mu.Unlock()
}

func (c *Client) dispatchRequest(msg envelope, retained int, deadline time.Time) (*handlerBarrier, bool) {
	key, valid := requestIDKey(msg.ID)
	if !valid {
		c.releaseHandlerBytes(retained)
		c.fail(ErrMalformed)
		return nil, false
	}
	c.mu.Lock()
	if c.closed || len(c.inbound) >= c.opts.MaxInboundPending {
		c.mu.Unlock()
		c.releaseHandlerBytes(retained)
		c.fail(ErrPendingLimit)
		return nil, false
	}
	if _, ok := c.inbound[key]; ok {
		c.mu.Unlock()
		c.releaseHandlerBytes(retained)
		c.fail(ErrDuplicateID)
		return nil, false
	}
	r := &Responder{client: c, id: clone(msg.ID), key: key, retained: retained, stateChanged: make(chan struct{}), done: make(chan struct{}), deadline: deadline}
	c.inbound[key] = r
	c.mu.Unlock()
	barrier := c.newBarrier(deadline, r.expire, nil)
	request := Request{ID: clone(msg.ID), Method: msg.Method, Params: clone(msg.Params), Responder: r}
	if c.opts.Handler == nil {
		barrier.returned()
		go func() {
			_, _ = r.Respond(context.Background(), nil, &RPCError{Code: -32601, Message: "method not found"})
		}()
		return barrier, true
	}
	c.runHandler(barrier, nil, request)
	go func() {
		t := time.NewTimer(time.Until(deadline))
		defer t.Stop()
		select {
		case <-t.C:
			r.expire()
		case <-r.done:
		case <-c.done:
		}
	}()
	return barrier, true
}

func (r *Responder) expire() {
	r.mu.Lock()
	if r.state == responderOpen {
		r.expiryClaimed = true
	}
	r.mu.Unlock()
	responseCtx, cancel := context.WithTimeout(context.Background(), r.client.opts.FinalPeriod)
	delivery, err := r.respond(responseCtx, nil, &RPCError{Code: -32603, Message: "request expired"}, true, true)
	cancel()
	if errors.Is(err, ErrAlreadyResponded) {
		outcome := r.Outcome()
		if outcome.Settled && outcome.Delivery == Complete {
			return
		}
	}
	if delivery != Complete {
		r.client.fail(context.DeadlineExceeded)
	}
}

func (r *Responder) Done() <-chan struct{} { return r.done }
func (r *Responder) Outcome() ResponderOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcome
}
func (r *Responder) Respond(ctx context.Context, value any, rpcErr *RPCError) (Delivery, error) {
	return r.respond(ctx, value, rpcErr, false, false)
}

func (r *Responder) respond(ctx context.Context, value any, rpcErr *RPCError, waitInFlight, expired bool) (Delivery, error) {
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
		if !expired && !time.Now().Before(r.deadline) {
			r.expiryClaimed = true
			r.mu.Unlock()
			go r.expire()
			return NotWritten, ErrAlreadyResponded
		}
		switch r.state {
		case responderOpen:
			r.state = responderInFlight
			r.signalStateChangeLocked()
			r.mu.Unlock()
			delivery, writeErr := r.client.writeFrame(ctx, frame)
			r.finishAttempt(delivery, expired)
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

func (r *Responder) finishAttempt(delivery Delivery, expired bool) {
	closed := r.client.isClosed()
	release := false
	r.mu.Lock()
	if r.state == responderInFlight {
		if delivery == NotWritten && !closed && !expired {
			r.state = responderOpen
			r.signalStateChangeLocked()
		} else {
			r.state = responderSettled
			r.outcome = ResponderOutcome{Delivery: delivery, Expired: expired || r.expiryClaimed, Settled: true}
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
	if r.state == responderOpen {
		r.state = responderSettled
		r.outcome = ResponderOutcome{Delivery: NotWritten, Expired: r.expiryClaimed, Settled: true}
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
		inbound := c.inbound
		c.pending = map[int64]chan result{}
		c.inbound = map[string]*Responder{}
		c.pendingBytes = 0
		close(c.done)
		c.mu.Unlock()
		for _, r := range inbound {
			r.settle()
		}
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
