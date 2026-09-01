package acp_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp/acptest"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

func options() acp.Options {
	return acp.Options{GracePeriod: time.Millisecond, TerminatePeriod: time.Millisecond, FinalPeriod: 20 * time.Millisecond, DrainPeriod: 20 * time.Millisecond, HandlerTimeout: time.Second}
}
func finishOnTerminate(p *acptest.Process) { go func() { <-p.TerminateCalled; p.Complete(nil) }() }

type marshalFailure struct{}

func (marshalFailure) MarshalJSON() ([]byte, error) { return nil, errors.New("marshal failed") }

type marshalSignal struct{ started chan<- struct{} }

func (m marshalSignal) MarshalJSON() ([]byte, error) {
	m.started <- struct{}{}
	return []byte(`{"ok":true}`), nil
}

func TestResponseWaitsForWireEarlierNotificationHandler(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	started := make(chan struct{})
	release := make(chan struct{})
	o := options()
	o.Handler = func(_ context.Context, r acp.Request) {
		if r.Method == "before" {
			close(started)
			<-release
		}
	}
	c, _ := acp.New(p, o)
	got := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "prompt", nil, nil); got <- err }()
	<-p.Stdin.Changed
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"before\",\"params\":{}}\n"))
	<-started
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":null}\n"))
	select {
	case err := <-got:
		t.Fatalf("later response crossed handler barrier: %v", err)
	default:
	}
	close(release)
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	_ = c.Shutdown(context.Background())
}

func TestPoisonFrameWaitsForWireEarlierHandler(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	started := make(chan struct{})
	release := make(chan struct{})
	o := options()
	o.Handler = func(context.Context, acp.Request) { close(started); <-release }
	c, _ := acp.New(p, o)
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"before\"}\n"))
	<-started
	_, _ = p.OutputWriter.Write([]byte("{not-json}\n"))
	select {
	case <-c.Done():
		t.Fatal("poison crossed handler barrier")
	default:
	}
	close(release)
	<-c.Done()
	if !errors.Is(c.Err(), acp.ErrMalformed) {
		t.Fatal(c.Err())
	}
	_ = c.Shutdown(context.Background())
}

func TestResponseWaitsForWireEarlierRequestAdmission(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	admitted := make(chan *acp.Responder, 1)
	release := make(chan struct{})
	o := options()
	o.Handler = func(_ context.Context, r acp.Request) { admitted <- r.Responder; <-release }
	c, _ := acp.New(p, o)
	got := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "prompt", nil, nil); got <- err }()
	<-p.Stdin.Changed
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"permission\",\"method\":\"ask\"}\n"))
	responder := <-admitted
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":null}\n"))
	if d, err := responder.Respond(context.Background(), true, nil); d != acp.Complete || err != nil {
		t.Fatalf("responder write while gated: %s %v", d, err)
	}
	select {
	case err := <-got:
		t.Fatalf("response crossed request admission: %v", err)
	default:
	}
	close(release)
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	outcome := responder.Outcome()
	if !outcome.Settled || outcome.Expired || outcome.Delivery != acp.Complete {
		t.Fatalf("outcome: %+v", outcome)
	}
	select {
	case <-responder.Done():
	default:
		t.Fatal("settled responder Done is open")
	}
	_ = c.Shutdown(context.Background())
}

func TestConcurrentHandlersStillGateLaterResponse(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	started := make(chan string, 2)
	releases := map[string]chan struct{}{"one": make(chan struct{}), "two": make(chan struct{})}
	o := options()
	o.MaxHandlerConcurrency = 2
	o.Handler = func(_ context.Context, r acp.Request) { started <- r.Method; <-releases[r.Method] }
	c, _ := acp.New(p, o)
	got := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "prompt", nil, nil); got <- err }()
	<-p.Stdin.Changed
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"one\",\"method\":\"one\"}\n{\"jsonrpc\":\"2.0\",\"id\":\"two\",\"method\":\"two\"}\n"))
	seen := map[string]bool{<-started: true, <-started: true}
	if !seen["one"] || !seen["two"] {
		t.Fatalf("handlers not concurrent: %v", seen)
	}
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":null}\n"))
	close(releases["two"])
	select {
	case err := <-got:
		t.Fatalf("response crossed first barrier: %v", err)
	default:
	}
	close(releases["one"])
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	_ = c.Shutdown(context.Background())
}

func TestDeadlineOwnsHandlerBoundary(t *testing.T) {
	for _, request := range []bool{false, true} {
		name := "notification"
		frame := "{\"jsonrpc\":\"2.0\",\"method\":\"boundary\"}\n"
		if request {
			name = "request"
			frame = "{\"jsonrpc\":\"2.0\",\"id\":\"boundary\",\"method\":\"boundary\"}\n"
		}
		t.Run(name, func(t *testing.T) {
			p := acptest.NewProcess()
			o := options()
			o.HandlerTimeout = 10 * time.Millisecond
			o.Handler = func(ctx context.Context, _ acp.Request) { <-ctx.Done() }
			c, _ := acp.New(p, o)
			_, _ = p.OutputWriter.Write([]byte(frame))
			<-c.Done()
			if !errors.Is(c.Err(), context.DeadlineExceeded) {
				t.Fatalf("late return released barrier: %v", c.Err())
			}
			<-p.TerminateCalled
			p.Complete(nil)
			_ = c.Shutdown(context.Background())
		})
	}
}

func TestDelayedResponderCannotAcquireAtDeadline(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	p.Stdin.Block()
	requests := make(chan acp.Request, 1)
	o := options()
	o.HandlerTimeout = 10 * time.Millisecond
	o.FinalPeriod = time.Second
	o.Handler = func(_ context.Context, r acp.Request) { requests <- r }
	c, _ := acp.New(p, o)
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"delayed\",\"method\":\"ask\"}\n"))
	r := <-requests
	<-p.Stdin.WriteStarted
	if d, err := r.Responder.Respond(context.Background(), true, nil); d != acp.NotWritten || !errors.Is(err, acp.ErrAlreadyResponded) {
		t.Fatalf("late explicit response: %s %v", d, err)
	}
	p.Stdin.Release()
	<-r.Responder.Done()
	outcome := r.Responder.Outcome()
	if !outcome.Settled || !outcome.Expired || outcome.Delivery != acp.Complete {
		t.Fatalf("deadline did not own outcome: %+v", outcome)
	}
	_ = c.Shutdown(context.Background())
}

func TestHandlerSaturationExpiresQueuedAdmissionWithoutStartingIt(t *testing.T) {
	p := acptest.NewProcess()
	o := options()
	o.MaxHandlerConcurrency = 1
	o.HandlerTimeout = 10 * time.Millisecond
	started := make(chan string, 2)
	o.Handler = func(ctx context.Context, r acp.Request) { started <- r.Method; <-ctx.Done() }
	c, _ := acp.New(p, o)
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"one\",\"method\":\"one\"}\n{\"jsonrpc\":\"2.0\",\"id\":\"two\",\"method\":\"two\"}\n"))
	if method := <-started; method != "one" {
		t.Fatalf("wire order: %s", method)
	}
	<-c.Done()
	select {
	case method := <-started:
		t.Fatalf("expired queued handler started: %s", method)
	default:
	}
	if !errors.Is(c.Err(), context.DeadlineExceeded) {
		t.Fatalf("saturation deadline: %v", c.Err())
	}
	<-p.TerminateCalled
	p.Complete(nil)
	_ = c.Shutdown(context.Background())
}

func TestAutomaticExpiryOutcome(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	requests := make(chan acp.Request, 1)
	o := options()
	o.HandlerTimeout = 20 * time.Millisecond
	o.Handler = func(_ context.Context, r acp.Request) { requests <- r }
	c, _ := acp.New(p, o)
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"expiry\",\"method\":\"ask\"}\n"))
	r := <-requests
	<-r.Responder.Done()
	outcome := r.Responder.Outcome()
	if !outcome.Settled || !outcome.Expired || outcome.Delivery != acp.Complete {
		t.Fatalf("expiry outcome: %+v", outcome)
	}
	if c.Err() != nil {
		t.Fatalf("complete expiry closed transport: %v", c.Err())
	}
	_ = c.Shutdown(context.Background())
}

func TestAutomaticExpiryUncertainDeliveryFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		want acp.Delivery
	}{
		{name: "not written", n: 0, want: acp.NotWritten},
		{name: "indeterminate", n: 1, want: acp.Indeterminate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := acptest.NewProcess()
			p.Stdin.CompleteBlockedWriteOnClose(tc.n, io.ErrClosedPipe)
			requests := make(chan acp.Request, 1)
			o := options()
			o.HandlerTimeout = 10 * time.Millisecond
			o.FinalPeriod = 10 * time.Millisecond
			o.Handler = func(_ context.Context, r acp.Request) { requests <- r }
			c, _ := acp.New(p, o)
			_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"expiry\",\"method\":\"ask\"}\n"))
			r := <-requests
			<-r.Responder.Done()
			outcome := r.Responder.Outcome()
			if !outcome.Settled || !outcome.Expired || outcome.Delivery != tc.want {
				t.Fatalf("expiry outcome: %+v", outcome)
			}
			<-c.Done()
			if c.Err() == nil {
				t.Fatal("uncertain expiry left transport open")
			}
			<-p.TerminateCalled
			p.Complete(nil)
			_ = c.Shutdown(context.Background())
		})
	}
}

func TestCallReportsCompleteAndCorrelatesOutOfOrder(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	c, err := acp.New(p, options())
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		delivery acp.Delivery
		value    string
		err      error
	}
	got := make(chan answer, 2)
	go func() {
		var v string
		d, e := c.Call(context.Background(), "one", map[string]int{"x": 1}, &v)
		got <- answer{d, v, e}
	}()
	<-p.Stdin.Changed
	go func() { var v string; d, e := c.Call(context.Background(), "two", nil, &v); got <- answer{d, v, e} }()
	<-p.Stdin.Changed
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":\"b\"}\r\n{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":\"a\"}\n"))
	a, b := <-got, <-got
	if a.err != nil || b.err != nil || a.delivery != acp.Complete || b.delivery != acp.Complete {
		t.Fatalf("%+v %+v", a, b)
	}
	values := map[string]bool{a.value: true, b.value: true}
	if !values["a"] || !values["b"] {
		t.Fatal(values)
	}
	if err = c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCancellationBeforeWriteIsNotWrittenAndClientStaysHealthy(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	c, _ := acp.New(p, options())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, err := c.Notify(ctx, "note", nil)
	if d != acp.NotWritten || !errors.Is(err, context.Canceled) {
		t.Fatalf("%s %v", d, err)
	}
	if c.Err() != nil {
		t.Fatalf("client closed: %v", c.Err())
	}
	go func() {
		<-p.Stdin.Changed
		_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":null}\n"))
	}()
	if d, err = c.Call(context.Background(), "ok", nil, nil); d != acp.Complete || err != nil {
		t.Fatalf("%s %v", d, err)
	}
	_ = c.Shutdown(context.Background())
}

func TestCancellationDuringWedgedWriteClosedBeforeBytesIsNotWrittenAndReaped(t *testing.T) {
	p := acptest.NewProcess()
	p.Stdin.Block()
	c, _ := acp.New(p, options())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		d acp.Delivery
		e error
	}, 1)
	go func() {
		d, e := c.Notify(ctx, "note", map[string]string{"x": "y"})
		done <- struct {
			d acp.Delivery
			e error
		}{d, e}
	}()
	<-p.Stdin.WriteStarted
	cancel()
	got := <-done
	if got.d != acp.NotWritten || !errors.Is(got.e, context.Canceled) {
		t.Fatalf("%s %v", got.d, got.e)
	}
	<-p.TerminateCalled
	p.Complete(nil)
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCancellationClassifiesWriterResultAfterCancellationWins(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		want acp.Delivery
	}{
		{name: "zero", n: 0, want: acp.NotWritten},
		{name: "full newline-terminated frame", n: -1, want: acp.Complete},
		{name: "partial", n: 1, want: acp.Indeterminate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := acptest.NewProcess()
			p.Stdin.CompleteBlockedWriteOnClose(tc.n, io.ErrClosedPipe)
			c, _ := acp.New(p, options())
			ctx, cancel := context.WithCancel(context.Background())
			got := make(chan struct {
				d acp.Delivery
				e error
			}, 1)
			go func() {
				d, err := c.Notify(ctx, "note", map[string]bool{"ok": true})
				got <- struct {
					d acp.Delivery
					e error
				}{d, err}
			}()
			<-p.Stdin.WriteStarted
			cancel()
			result := <-got
			if result.d != tc.want || !errors.Is(result.e, context.Canceled) {
				t.Fatalf("delivery=%s error=%v", result.d, result.e)
			}
			<-p.TerminateCalled
			p.Complete(nil)
			_ = c.Shutdown(context.Background())
		})
	}
}

func TestResponderValidationDoesNotConsumeOwnership(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options func(*acp.Options)
		value   any
		ctx     func() context.Context
		wantErr error
	}{
		{name: "marshal failure", value: marshalFailure{}, ctx: context.Background, wantErr: errors.New("marshal failed")},
		{name: "oversized response", options: func(o *acp.Options) { o.MaxOutboundFrameBytes = 256 }, value: strings.Repeat("x", 300), ctx: context.Background, wantErr: acp.ErrFrameTooLarge},
		{name: "pre-canceled context", value: nil, ctx: func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }, wantErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := acptest.NewProcess()
			finishOnTerminate(p)
			requests := make(chan acp.Request, 1)
			o := options()
			if tc.options != nil {
				tc.options(&o)
			}
			o.Handler = func(_ context.Context, r acp.Request) { requests <- r }
			c, _ := acp.New(p, o)
			_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"validation\",\"method\":\"ask\",\"params\":{}}\n"))
			r := <-requests
			if d, err := r.Responder.Respond(tc.ctx(), tc.value, nil); d != acp.NotWritten || (tc.name == "marshal failure" && (err == nil || !strings.Contains(err.Error(), tc.wantErr.Error()))) || (tc.name != "marshal failure" && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("first response: %s %v", d, err)
			}
			if d, err := r.Responder.Respond(context.Background(), map[string]bool{"retry": true}, nil); d != acp.Complete || err != nil {
				t.Fatalf("retry response: %s %v", d, err)
			}
			_ = c.Shutdown(context.Background())
		})
	}
}

func TestResponderReopensAfterDefinitiveNotWritten(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	p.Stdin.Block()
	requests := make(chan acp.Request, 1)
	o := options()
	o.Handler = func(_ context.Context, r acp.Request) { requests <- r }
	c, _ := acp.New(p, o)
	first := make(chan error, 1)
	go func() { _, err := c.Notify(context.Background(), "hold", nil); first <- err }()
	<-p.Stdin.WriteStarted
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"retry\",\"method\":\"ask\",\"params\":{}}\n"))
	r := <-requests
	marshaled := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	attempt := make(chan struct {
		d acp.Delivery
		e error
	}, 1)
	go func() {
		d, err := r.Responder.Respond(ctx, marshalSignal{started: marshaled}, nil)
		attempt <- struct {
			d acp.Delivery
			e error
		}{d, err}
	}()
	<-marshaled
	cancel()
	result := <-attempt
	if result.d != acp.NotWritten || !errors.Is(result.e, context.Canceled) {
		t.Fatalf("first response: %s %v", result.d, result.e)
	}
	p.Stdin.Release()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if d, err := r.Responder.Respond(context.Background(), map[string]bool{"retry": true}, nil); d != acp.Complete || err != nil {
		t.Fatalf("retry response: %s %v", d, err)
	}
	_ = c.Shutdown(context.Background())
}

func TestRequestDeadlineWaitsForNotWrittenAttemptThenResponds(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	p.Stdin.Block()
	marshaled := make(chan struct{}, 1)
	handlerResult := make(chan struct {
		d acp.Delivery
		e error
	}, 1)
	o := options()
	o.HandlerTimeout = 50 * time.Millisecond
	o.Handler = func(ctx context.Context, r acp.Request) {
		d, err := r.Responder.Respond(ctx, marshalSignal{started: marshaled}, nil)
		handlerResult <- struct {
			d acp.Delivery
			e error
		}{d, err}
	}
	c, _ := acp.New(p, o)
	held := make(chan error, 1)
	go func() { _, err := c.Notify(context.Background(), "hold", nil); held <- err }()
	<-p.Stdin.WriteStarted
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"deadline-race\",\"method\":\"ask\",\"params\":{}}\n"))
	<-marshaled
	result := <-handlerResult
	if result.d != acp.NotWritten || !errors.Is(result.e, context.DeadlineExceeded) {
		t.Fatalf("handler response: %s %v", result.d, result.e)
	}
	p.Stdin.Release()
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Stdin.Changed:
	case <-time.After(time.Second):
		t.Fatal("deadline response was not written")
	}
	for !bytes.Contains(p.Stdin.Bytes(), []byte("-32603")) {
		select {
		case <-p.Stdin.Changed:
		case <-time.After(time.Second):
			t.Fatal("missing deadline response")
		}
	}
	_ = c.Shutdown(context.Background())
}

func TestDelayedInboundResponderOwnsFullFrameAcknowledgment(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	requests := make(chan acp.Request, 1)
	handlerReturned := make(chan struct{})
	o := options()
	o.Handler = func(_ context.Context, r acp.Request) { requests <- r; close(handlerReturned) }
	c, _ := acp.New(p, o)
	_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"private\",\"method\":\"ask\",\"params\":{}}\n"))
	r := <-requests
	<-handlerReturned
	d, err := r.Responder.Respond(context.Background(), map[string]bool{"ok": true}, nil)
	if d != acp.Complete || err != nil {
		t.Fatalf("%s %v", d, err)
	}
	if !bytes.Contains(p.Stdin.Bytes(), []byte("\"result\":{\"ok\":true}")) {
		t.Fatal(string(p.Stdin.Bytes()))
	}
	if d, err = r.Responder.Respond(context.Background(), nil, nil); d != acp.NotWritten || !errors.Is(err, acp.ErrAlreadyResponded) {
		t.Fatalf("%s %v", d, err)
	}
	_ = c.Shutdown(context.Background())
}

func TestInboundResponderTimeoutFirstOwnershipAndShutdownCleanup(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		p := acptest.NewProcess()
		finishOnTerminate(p)
		requests := make(chan acp.Request, 1)
		o := options()
		o.HandlerTimeout = 20 * time.Millisecond
		o.Handler = func(_ context.Context, r acp.Request) { requests <- r }
		c, _ := acp.New(p, o)
		_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"timeout\",\"method\":\"ask\",\"params\":{}}\n"))
		r := <-requests
		<-p.Stdin.Changed
		if !bytes.Contains(p.Stdin.Bytes(), []byte("-32603")) {
			t.Fatal(string(p.Stdin.Bytes()))
		}
		if d, err := r.Responder.Respond(context.Background(), nil, nil); d != acp.NotWritten || !errors.Is(err, acp.ErrAlreadyResponded) {
			t.Fatalf("%s %v", d, err)
		}
		_ = c.Shutdown(context.Background())
	})
	t.Run("first response wins", func(t *testing.T) {
		p := acptest.NewProcess()
		finishOnTerminate(p)
		requests := make(chan acp.Request, 1)
		hold := make(chan struct{})
		o := options()
		o.Handler = func(_ context.Context, r acp.Request) { requests <- r; <-hold }
		c, _ := acp.New(p, o)
		_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"race\",\"method\":\"ask\",\"params\":{}}\n"))
		r := <-requests
		start := make(chan struct{})
		results := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func(value int) {
				<-start
				_, err := r.Responder.Respond(context.Background(), value, nil)
				results <- err
			}(i)
		}
		close(start)
		a, b := <-results, <-results
		if (a == nil) == (b == nil) || (!errors.Is(a, acp.ErrAlreadyResponded) && !errors.Is(b, acp.ErrAlreadyResponded)) {
			t.Fatalf("ownership results: %v %v", a, b)
		}
		close(hold)
		_ = c.Shutdown(context.Background())
	})
	t.Run("shutdown settles retained responder", func(t *testing.T) {
		p := acptest.NewProcess()
		finishOnTerminate(p)
		requests := make(chan acp.Request, 1)
		o := options()
		o.Handler = func(_ context.Context, r acp.Request) { requests <- r }
		c, _ := acp.New(p, o)
		_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"shutdown\",\"method\":\"ask\",\"params\":{}}\n"))
		r := <-requests
		if err := c.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if d, err := r.Responder.Respond(context.Background(), nil, nil); d != acp.NotWritten || !errors.Is(err, acp.ErrAlreadyResponded) {
			t.Fatalf("%s %v", d, err)
		}
	})
}

func TestMalformedDuplicateAndInboundLimitFailClosedAndCleanup(t *testing.T) {
	cases := []struct {
		name, frames string
		inbound      int
	}{{"duplicate JSON", "{\"jsonrpc\":\"2.0\",\"id\":1,\"id\":1,\"result\":{}}\n", 0}, {"duplicate inbound ID", "{\"jsonrpc\":\"2.0\",\"id\":\"x\",\"method\":\"a\",\"params\":{}}\n{\"jsonrpc\":\"2.0\",\"id\":\"x\",\"method\":\"a\",\"params\":{}}\n", 2}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := acptest.NewProcess()
			o := options()
			o.Handler = func(context.Context, acp.Request) {}
			c, _ := acp.New(p, o)
			_, _ = p.OutputWriter.Write([]byte(tc.frames))
			<-c.Done()
			<-p.TerminateCalled
			p.Complete(nil)
			_ = c.Shutdown(context.Background())
			if c.Err() == nil {
				t.Fatal("missing terminal error")
			}
		})
	}
}

func TestSeparateFrameBounds(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	o := options()
	o.MaxInboundFrameBytes = 256
	o.MaxOutboundFrameBytes = 512
	c, _ := acp.New(p, o)
	payload := map[string]string{"v": string(bytes.Repeat([]byte("x"), 300))}
	d, err := c.Notify(context.Background(), "large", payload)
	if d != acp.Complete || err != nil {
		t.Fatalf("%s %v", d, err)
	}
	_, _ = p.OutputWriter.Write(append(bytes.Repeat([]byte("x"), 257), '\n'))
	<-c.Done()
	_ = c.Shutdown(context.Background())
}

func TestResponseUnionAndIDAreValidated(t *testing.T) {
	p := acptest.NewProcess()
	c, _ := acp.New(p, options())
	go func() {
		<-p.Stdin.Changed
		_, _ = p.OutputWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":null,\"error\":{\"code\":1,\"message\":\"x\"}}\n"))
	}()
	_, err := c.Call(context.Background(), "x", nil, nil)
	if !errors.Is(err, acp.ErrMalformed) {
		t.Fatal(err)
	}
	<-p.TerminateCalled
	p.Complete(nil)
	_ = c.Shutdown(context.Background())
}

func TestFragmentedFrameAndStderrPressure(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	c, _ := acp.New(p, options())
	stderrDone := make(chan struct{})
	go func() { _, _ = p.ErrorOutput.Write(bytes.Repeat([]byte("e"), 2<<20)); close(stderrDone) }()
	got := make(chan error, 1)
	go func() {
		var value string
		_, err := c.Call(context.Background(), "fragmented", nil, &value)
		if err == nil && value != "ok" {
			err = errors.New("wrong value")
		}
		got <- err
	}()
	<-p.Stdin.Changed
	for _, fragment := range []string{"{\"jsonrpc\":", "\"2.0\",\"id\":1,", "\"result\":\"ok\"}\r\n"} {
		_, _ = p.OutputWriter.Write([]byte(fragment))
	}
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	<-stderrDone
	_ = c.Shutdown(context.Background())
}

func TestPendingLimitAndEscalationErrors(t *testing.T) {
	p := acptest.NewProcess()
	p.TerminateErr = errors.New("terminate denied")
	p.KillErr = errors.New("kill denied")
	o := options()
	o.MaxPending = 1
	c, _ := acp.New(p, o)
	first := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "held", nil, nil); first <- err }()
	<-p.Stdin.Changed
	if d, err := c.Call(context.Background(), "overflow", nil, nil); d != acp.NotWritten || !errors.Is(err, acp.ErrPendingLimit) {
		t.Fatalf("%s %v", d, err)
	}
	shutdown := make(chan error, 1)
	go func() { shutdown <- c.Shutdown(context.Background()) }()
	<-p.TerminateCalled
	<-p.KillCalled
	p.Complete(errors.New("exit failure"))
	if err := <-shutdown; err == nil || !bytes.Contains([]byte(err.Error()), []byte("terminate")) || !bytes.Contains([]byte(err.Error()), []byte("kill")) || !bytes.Contains([]byte(err.Error()), []byte("wait")) {
		t.Fatalf("missing escalation outcomes: %v", err)
	}
	<-first
}

func TestProviderMaximumFitsSupportedTransportBounds(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	o := options()
	o.MaxInboundFrameBytes = acp.SupportedMaxFrameBytes
	o.MaxOutboundFrameBytes = acp.SupportedMaxFrameBytes
	o.MaxRetainedBytes = acp.SupportedMaxRetainedBytes
	o.HandlerTimeout = 30 * time.Second
	received := make(chan struct{}, 1)
	o.Handler = func(context.Context, acp.Request) { received <- struct{}{} }
	c, err := acp.New(p, o)
	if err != nil {
		t.Fatal(err)
	}
	params := canonicalMaximumParams()
	d, err := c.Notify(context.Background(), "session/prompt", params)
	if d != acp.Complete || err != nil {
		t.Fatalf("outbound canonical maximum: %s %v", d, err)
	}
	frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": params})
	if len(frame)+1 <= 32<<20 {
		t.Fatalf("surrogate frame too small: %d", len(frame)+1)
	}
	go func() { _, _ = p.OutputWriter.Write(append(frame, '\n')) }()
	select {
	case <-received:
	case <-time.After(60 * time.Second):
		t.Fatalf("maximum inbound replay not delivered: %v", c.Err())
	}
	_ = c.Shutdown(context.Background())
}

func TestRetainedWireQueueOverflowFailsClosedAndSettles(t *testing.T) {
	p := acptest.NewProcess()
	o := options()
	o.MaxRetainedBytes = 256
	o.MaxHandlerQueue = 8
	started := make(chan struct{})
	release := make(chan struct{})
	o.Handler = func(context.Context, acp.Request) { close(started); <-release }
	c, _ := acp.New(p, o)
	frame := []byte("{\"jsonrpc\":\"2.0\",\"method\":\"retained\",\"params\":{\"v\":\"" + strings.Repeat("x", 80) + "\"}}\n")
	_, _ = p.OutputWriter.Write(frame)
	<-started
	_, _ = p.OutputWriter.Write(frame)
	<-c.Done()
	if !errors.Is(c.Err(), acp.ErrRetainedLimit) {
		t.Fatalf("retained overflow: %v", c.Err())
	}
	close(release)
	<-p.TerminateCalled
	p.Complete(nil)
	_ = c.Shutdown(context.Background())
}

func TestRetainedWireBytesReleaseAfterHandlerSettlement(t *testing.T) {
	p := acptest.NewProcess()
	finishOnTerminate(p)
	o := options()
	o.MaxRetainedBytes = 256
	received := make(chan struct{}, 2)
	o.Handler = func(context.Context, acp.Request) { received <- struct{}{} }
	c, _ := acp.New(p, o)
	frame := []byte("{\"jsonrpc\":\"2.0\",\"method\":\"retained\",\"params\":{\"v\":\"" + strings.Repeat("x", 140) + "\"}}\n")
	_, _ = p.OutputWriter.Write(frame)
	<-received
	_, _ = p.OutputWriter.Write(frame)
	<-received
	if c.Err() != nil {
		t.Fatalf("settled frame retained bytes: %v", c.Err())
	}
	_ = c.Shutdown(context.Background())
}

func TestHardOptionCeilingsRejectUnboundedConfiguration(t *testing.T) {
	for name, mutate := range map[string]func(*acp.Options){
		"inbound":  func(o *acp.Options) { o.MaxInboundFrameBytes = acp.HardMaxFrameBytes + 1 },
		"outbound": func(o *acp.Options) { o.MaxOutboundFrameBytes = acp.HardMaxFrameBytes + 1 },
		"retained": func(o *acp.Options) { o.MaxRetainedBytes = acp.HardMaxRetainedBytes + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			p := acptest.NewProcess()
			o := options()
			mutate(&o)
			if _, err := acp.New(p, o); err == nil {
				t.Fatal("accepted above hard ceiling")
			}
			p.Complete(nil)
		})
	}
}

func canonicalMaximumParams() map[string]any {
	return map[string]any{
		"source":         string(bytes.Repeat([]byte{0}, provider.MaxSourceBytes)),
		"creatorContext": string(bytes.Repeat([]byte{1}, provider.MaxCreatorContextBytes)),
		"content":        string(bytes.Repeat([]byte{'<'}, provider.MaxTurnMessageBytes)),
		"images":         []string{encodeBase64(bytes.Repeat([]byte{0xff}, provider.MaxImageBytes)), encodeBase64(bytes.Repeat([]byte{0xfe}, provider.MaxTurnImageBytes-provider.MaxImageBytes))},
	}
}
func encodeBase64(p []byte) string { return base64.StdEncoding.EncodeToString(p) }

func TestPartialEOFIsMalformedAndCleanupBounded(t *testing.T) {
	p := acptest.NewProcess()
	c, _ := acp.New(p, options())
	_, _ = p.OutputWriter.Write([]byte(`{"jsonrpc":"2.0"`))
	_ = p.OutputWriter.Close()
	<-c.Done()
	if !errors.Is(c.Err(), acp.ErrMalformed) {
		t.Fatalf("partial EOF classified as %v", c.Err())
	}
	<-p.TerminateCalled
	<-p.KillCalled
	if err := c.Shutdown(context.Background()); !errors.Is(err, acp.ErrUnreaped) {
		t.Fatalf("expected unreaped result, got %v", err)
	}
}

func TestWedgedCloseWriteAndStreamsReturnBoundedErrors(t *testing.T) {
	p := acptest.NewProcess()
	p.Stdin.Block()
	p.Stdin.BlockClose()
	c, _ := acp.New(p, options())
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		d acp.Delivery
		e error
	}, 1)
	go func() {
		d, e := c.Notify(ctx, "blocked", nil)
		result <- struct {
			d acp.Delivery
			e error
		}{d, e}
	}()
	<-p.Stdin.WriteStarted
	cancel()
	select {
	case got := <-result:
		if got.d != acp.Indeterminate || !errors.Is(got.e, context.Canceled) {
			t.Fatalf("%s %v", got.d, got.e)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited on wedged writer")
	}
	<-p.TerminateCalled
	<-p.KillCalled
	if err := c.Shutdown(context.Background()); !errors.Is(err, acp.ErrUnreaped) || !errors.Is(err, acp.ErrStreamDrain) {
		t.Fatalf("missing bounded cleanup errors: %v", err)
	}
}

func TestNoncooperativeRequestHandlerRetainsItsInboundSlot(t *testing.T) {
	p := acptest.NewProcess()
	o := options()
	o.MaxInboundPending = 1
	o.MaxHandlerConcurrency = 1
	o.HandlerTimeout = time.Millisecond
	started := make(chan struct{})
	release := make(chan struct{})
	o.Handler = func(ctx context.Context, r acp.Request) { close(started); <-ctx.Done(); <-release }
	c, _ := acp.New(p, o)
	first, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "one", "method": "ask", "params": map[string]string{"v": "x"}})
	_, _ = p.OutputWriter.Write(append(first, '\n'))
	<-started
	second, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "two", "method": "ask", "params": map[string]string{"v": "y"}})
	_, _ = p.OutputWriter.Write(append(second, '\n'))
	<-c.Done()
	if !errors.Is(c.Err(), acp.ErrPendingLimit) {
		t.Fatalf("noncooperative handler released its inbound slot: %v", c.Err())
	}
	close(release)
	<-p.TerminateCalled
	p.Complete(nil)
	_ = c.Shutdown(context.Background())
}

func TestHandlerConcurrencyAndNotificationQueueStayBoundedAndOrdered(t *testing.T) {
	p := acptest.NewProcess()
	o := options()
	o.MaxHandlerConcurrency = 1
	o.MaxHandlerQueue = 2
	o.MaxRetainedBytes = 1024
	started := make(chan string, 4)
	release := make(chan struct{})
	o.Handler = func(_ context.Context, r acp.Request) { started <- r.Method; <-release }
	c, _ := acp.New(p, o)
	frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "n1", "params": map[string]string{"v": "x"}})
	_, _ = p.OutputWriter.Write(append(frame, '\n'))
	if got := <-started; got != "n1" {
		t.Fatal(got)
	}
	go func() {
		for _, method := range []string{"n2", "n3", "n4", "n5", "n6", "n7"} {
			frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": map[string]string{"v": "x"}})
			_, _ = p.OutputWriter.Write(append(frame, '\n'))
		}
	}()
	select {
	case got := <-started:
		t.Fatalf("concurrency exceeded with %s", got)
	default:
	}
	<-c.Done()
	close(release)
	<-p.TerminateCalled
	p.Complete(nil)
	_ = c.Shutdown(context.Background())
	if !errors.Is(c.Err(), acp.ErrPendingLimit) {
		t.Fatalf("flood did not fail closed: %v", c.Err())
	}
}
