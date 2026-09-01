//go:build unix

package acp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
)

// TestACPSubprocessHelper is re-executed as the hermetic ACP peer. It is not a
// test itself in the parent process: the -- argument is required to activate it.
func TestACPSubprocessHelper(t *testing.T) {
	args := os.Args
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(args) {
		t.Skip("subprocess helper")
	}
	if err := runACPSubprocessHelper(args[separator+1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(71)
	}
	// Bypass the go test harness so it cannot append PASS to the ACP stream.
	os.Exit(0)
}

func runACPSubprocessHelper(scenario string) error {
	in := bufio.NewScanner(os.Stdin)
	read := func() (map[string]json.RawMessage, error) {
		if !in.Scan() {
			if err := in.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(in.Bytes(), &frame); err != nil {
			return nil, err
		}
		return frame, nil
	}
	write := func(frame string) error {
		_, err := io.WriteString(os.Stdout, frame+"\n")
		return err
	}
	response := func(id json.RawMessage, result string) error {
		return write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result))
	}
	finish := func(err error) error {
		if err != nil {
			return err
		}
		for in.Scan() {
		}
		return in.Err()
	}

	switch scenario {
	case "fragmented":
		request, err := read()
		if err != nil {
			return err
		}
		if err = write(`{"jsonrpc":"2.0","method":"fragment-held","params":{}}`); err != nil {
			return err
		}
		if _, err = io.WriteString(os.Stdout, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"res`, request["id"])); err != nil {
			return err
		}
		release, err := read()
		if err != nil || string(release["method"]) != `"release"` {
			return fmt.Errorf("fragment release: %w", err)
		}
		_, err = io.WriteString(os.Stdout, `ult":{"fragmented":true}}`+"\n")
		return finish(err)
	case "oversized":
		_, err := io.WriteString(os.Stdout, strings.Repeat("x", 257)+"\n")
		return finish(err)
	case "malformed":
		return finish(write(`{"jsonrpc":"2.0","id":`))
	case "duplicate":
		return finish(write(`{"jsonrpc":"2.0","id":1,"id":1,"result":null}`))
	case "reverse":
		first, err := read()
		if err != nil {
			return err
		}
		second, err := read()
		if err != nil {
			return err
		}
		if err = response(second["id"], string(second["params"])); err != nil {
			return err
		}
		return finish(response(first["id"], string(first["params"])))
	case "routing":
		start, err := read()
		if err != nil || string(start["method"]) != `"start"` {
			return fmt.Errorf("routing start: %w", err)
		}
		if err = write(`{"jsonrpc":"2.0","method":"event","params":{"value":7}}`); err != nil {
			return err
		}
		if err = write(`{"jsonrpc":"2.0","id":"child-1","method":"question","params":{"value":8}}`); err != nil {
			return err
		}
		answer, err := read()
		if err != nil || string(answer["id"]) != `"child-1"` || string(answer["result"]) != `{"answer":9}` {
			return fmt.Errorf("routing answer: %w", err)
		}
		return finish(write(`{"jsonrpc":"2.0","method":"routed","params":{}}`))
	case "cancel":
		held, err := read()
		if err != nil {
			return err
		}
		if err = write(`{"jsonrpc":"2.0","method":"held","params":{}}`); err != nil {
			return err
		}
		release, err := read()
		if err != nil || string(release["method"]) != `"release"` {
			return fmt.Errorf("cancel release: %w", err)
		}
		if err = response(held["id"], `{"late":true}`); err != nil {
			return err
		}
		healthy, err := read()
		if err != nil {
			return err
		}
		return finish(response(healthy["id"], `{"healthy":true}`))
	case "stderr":
		request, err := read()
		if err != nil {
			return err
		}
		if _, err = os.Stderr.Write([]byte(strings.Repeat("e", 2<<20))); err != nil {
			return err
		}
		return finish(response(request["id"], `true`))
	case "exit":
		os.Exit(23)
		return nil
	case "shutdown":
		for in.Scan() {
		}
		return in.Err()
	default:
		return fmt.Errorf("unknown scenario %q", scenario)
	}
}

func subprocessOptions() acp.Options {
	return acp.Options{
		MaxInboundFrameBytes: acp.DefaultMaxInboundFrameBytes, MaxOutboundFrameBytes: acp.DefaultMaxOutboundFrameBytes,
		MaxPending: acp.DefaultMaxPending, MaxInboundPending: acp.DefaultMaxPending,
		MaxRetainedBytes: acp.DefaultMaxRetainedBytes, MaxIDBytes: acp.DefaultMaxIDBytes,
		MaxMethodBytes: acp.DefaultMaxMethodBytes, MaxHandlerConcurrency: acp.DefaultMaxHandlerConcurrency,
		MaxHandlerQueue: acp.DefaultMaxHandlerQueue, GracePeriod: 2 * time.Second,
		TerminatePeriod: time.Second, HandlerTimeout: time.Second,
		FinalPeriod: time.Second, DrainPeriod: time.Second,
	}
}

func newSubprocessClient(t *testing.T, scenario string, editOptions func(*acp.Options)) *acp.Client {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(cwd, "home")
	if err = os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	child, err := common.NewProcessGroupLauncher().Launch(context.Background(), common.ProcessRequest{
		Executable:       executable,
		Arguments:        []string{"-test.run=^TestACPSubprocessHelper$", "--", scenario},
		Environment:      []string{"HOME=" + home},
		WorkingDirectory: cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	o := subprocessOptions()
	if editOptions != nil {
		editOptions(&o)
	}
	client, err := acp.New(child, o)
	if err != nil {
		_ = child.Kill()
		_ = child.Wait()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := client.Shutdown(ctx)
		if err != nil && scenario != "exit" {
			t.Errorf("subprocess cleanup: %v", err)
		}
	})
	return client
}

func TestACPSubprocessFragmentedResponseUsesProtocolBarrier(t *testing.T) {
	held := make(chan struct{})
	c := newSubprocessClient(t, "fragmented", func(o *acp.Options) {
		o.Handler = func(_ context.Context, request acp.Request) {
			if request.Method == "fragment-held" {
				close(held)
			}
		}
	})
	result := make(chan struct {
		value map[string]bool
		err   error
	}, 1)
	go func() {
		var value map[string]bool
		_, err := c.Call(context.Background(), "fragment", nil, &value)
		result <- struct {
			value map[string]bool
			err   error
		}{value, err}
	}()
	// The two notifications form a deterministic protocol barrier: the child
	// announces that it has emitted the first fragment, then awaits release.
	<-held
	if d, err := c.Notify(context.Background(), "release", nil); d != acp.Complete || err != nil {
		t.Fatalf("release: %s %v", d, err)
	}
	got := <-result
	if got.err != nil || !got.value["fragmented"] {
		t.Fatalf("fragmented response: %#v, %v", got.value, got.err)
	}
}

func TestACPSubprocessInvalidFramesFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, scenario string
		want           error
		edit           func(*acp.Options)
	}{
		{"oversized", "oversized", acp.ErrFrameTooLarge, func(o *acp.Options) { o.MaxInboundFrameBytes = 256 }},
		{"malformed JSON", "malformed", acp.ErrMalformed, nil},
		{"duplicate fields", "duplicate", acp.ErrMalformed, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newSubprocessClient(t, tc.scenario, tc.edit)
			<-c.Done()
			if !errors.Is(c.Err(), tc.want) {
				t.Fatalf("terminal error %v, want %v", c.Err(), tc.want)
			}
			if d, err := c.Call(context.Background(), "after", nil, nil); d != acp.NotWritten || !errors.Is(err, tc.want) {
				t.Fatalf("closed transport retained state: %s %v", d, err)
			}
		})
	}
}

func TestACPSubprocessReverseResponsesRemainCorrelated(t *testing.T) {
	c := newSubprocessClient(t, "reverse", nil)
	type outcome struct {
		sent, got int
		err       error
	}
	outcomes := make(chan outcome, 2)
	var started sync.WaitGroup
	started.Add(2)
	for _, value := range []int{11, 22} {
		go func() {
			started.Done()
			var got int
			_, err := c.Call(context.Background(), "reverse", value, &got)
			outcomes <- outcome{value, got, err}
		}()
	}
	started.Wait()
	for range 2 {
		got := <-outcomes
		if got.err != nil || got.got != got.sent {
			t.Fatalf("correlation: %+v", got)
		}
	}
}

func TestACPSubprocessRoutesNotificationsAndInboundResponder(t *testing.T) {
	events := make(chan string, 2)
	responders := make(chan *acp.Responder, 1)
	c := newSubprocessClient(t, "routing", func(o *acp.Options) {
		o.Handler = func(ctx context.Context, request acp.Request) {
			events <- request.Method
			if request.Responder != nil {
				responders <- request.Responder
				if d, err := request.Responder.Respond(ctx, map[string]int{"answer": 9}, nil); d != acp.Complete || err != nil {
					t.Errorf("response: %s %v", d, err)
				}
			}
		}
	})
	if _, err := c.Notify(context.Background(), "start", nil); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for len(seen) < 3 {
		seen[<-events] = true
	}
	if !seen["event"] || !seen["question"] || !seen["routed"] {
		t.Fatal(seen)
	}
	r := <-responders
	select {
	case <-r.Done():
		if outcome := r.Outcome(); !outcome.Settled || outcome.Delivery != acp.Complete {
			t.Fatalf("responder outcome: %+v", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("responder retained after complete delivery")
	}
}

func TestACPSubprocessCanceledCallDoesNotPoisonCorrelation(t *testing.T) {
	held := make(chan struct{})
	c := newSubprocessClient(t, "cancel", func(o *acp.Options) {
		// One slot makes the healthy follow-up prove cancellation released all
		// pending-call bookkeeping rather than merely leaving spare capacity.
		o.MaxPending = 1
		o.Handler = func(_ context.Context, request acp.Request) {
			if request.Method == "held" {
				close(held)
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() { _, err := c.Call(ctx, "held", nil, nil); started <- err }()
	// The child confirms receipt through a protocol notification before cancel.
	<-held
	cancel()
	if err := <-started; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call: %v", err)
	}
	if _, err := c.Notify(context.Background(), "release", nil); err != nil {
		t.Fatal(err)
	}
	var healthy map[string]bool
	if _, err := c.Call(context.Background(), "healthy", nil, &healthy); err != nil || !healthy["healthy"] {
		t.Fatalf("healthy call after stale response: %#v %v", healthy, err)
	}
}

func TestACPSubprocessDrainsStderrPressureWhileResponding(t *testing.T) {
	c := newSubprocessClient(t, "stderr", nil)
	var got bool
	if _, err := c.Call(context.Background(), "pressure", nil, &got); err != nil || !got {
		t.Fatalf("stderr pressure response: %v %v", got, err)
	}
}

func TestACPSubprocessClassifiesChildExit(t *testing.T) {
	c := newSubprocessClient(t, "exit", nil)
	<-c.Done()
	if !errors.Is(c.Err(), acp.ErrChildExited) {
		t.Fatalf("child exit classified as %v", c.Err())
	}
}

func TestACPSubprocessShutdownIsBoundedAndReaped(t *testing.T) {
	c := newSubprocessClient(t, "shutdown", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) >= 3*time.Second {
		t.Fatal("shutdown exceeded bound")
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("shutdown left transport open")
	}
	if d, err := c.Call(context.Background(), "after", nil, nil); d != acp.NotWritten || !errors.Is(err, acp.ErrClosed) {
		t.Fatalf("shutdown retained pending/native state: %s %v", d, err)
	}
}
