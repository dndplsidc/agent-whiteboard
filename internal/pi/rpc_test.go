package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

func TestRPCClientCorrelatesOutOfOrderResponsesAndSeparatesEvents(t *testing.T) {
	child := newRPCFakeChild()
	client, err := newRPCClient(child)
	require.NoError(t, err)
	type answer struct {
		marker   string
		response rpcResponse
		err      error
	}
	answers := make(chan answer, 2)
	for _, marker := range []string{"first", "second"} {
		marker := marker
		go func() {
			response, _, callErr := client.call(context.Background(), "get_state", map[string]any{"marker": marker})
			answers <- answer{marker: marker, response: response, err: callErr}
		}()
	}
	commands := []map[string]any{child.readCommand(t), child.readCommand(t)}
	child.writeRecord(t, map[string]any{"type": "agent_start"})
	for index := len(commands) - 1; index >= 0; index-- {
		command := commands[index]
		child.writeRecord(t, map[string]any{
			"id": command["id"], "type": "response", "command": "get_state", "success": true,
			"data": map[string]any{"marker": command["marker"]},
		})
	}
	for range 2 {
		answer := <-answers
		require.NoError(t, answer.err)
		require.NotNil(t, answer.response.Success)
		var data struct {
			Marker string `json:"marker"`
		}
		require.NoError(t, decodeResponseData(answer.response, &data))
		require.Equal(t, answer.marker, data.Marker)
	}
	select {
	case event := <-client.events:
		require.JSONEq(t, `{"type":"agent_start"}`, string(event))
	case <-time.After(time.Second):
		t.Fatal("missing RPC event")
	}
	child.closeOutput()
	<-client.done
}

func TestRPCClientDoesNotSendPreCanceledCall(t *testing.T) {
	child := newRPCFakeChild()
	client, err := newRPCClient(child)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, wrote, err := client.call(ctx, "get_state", nil)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, wrote)
	client.mu.Lock()
	require.Empty(t, client.pending)
	client.mu.Unlock()
	child.closeOutput()
}

func TestRPCClientCancellationAndTerminalFailureBypassBlockedWrite(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancellation", true: "terminal"}[terminal], func(t *testing.T) {
			input := newBlockingInput()
			outputReader, outputWriter := io.Pipe()
			child := &specialRPCChild{input: input, output: outputReader}
			client, err := newRPCClient(child)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			answer := make(chan error, 1)
			go func() { _, _, callErr := client.call(ctx, "get_state", nil); answer <- callErr }()
			<-input.entered
			if terminal {
				_, err = outputWriter.Write([]byte("{}\n"))
				require.NoError(t, err)
			} else {
				cancel()
			}
			select {
			case callErr := <-answer:
				require.Error(t, callErr)
			case <-time.After(time.Second):
				t.Fatal("RPC call did not honor cancellation/terminal failure during blocked write")
			}
			_ = outputWriter.Close()
			_ = input.Close()
		})
	}
}

func TestWriteFullHandlesShortWritesAndNoProgress(t *testing.T) {
	writer := &shortWriter{maximum: 2}
	require.NoError(t, writeFull(writer, []byte("abcdef")))
	require.Equal(t, "abcdef", writer.value.String())
	require.ErrorIs(t, writeFull(zeroWriter{}, []byte("x")), io.ErrNoProgress)
}

func TestRPCClientPreservesFinalResponseBeforeImmediateEOF(t *testing.T) {
	for range 100 {
		child := newRPCFakeChild()
		client, err := newRPCClient(child)
		require.NoError(t, err)
		answer := make(chan rpcResult, 1)
		go func() {
			response, _, callErr := client.call(context.Background(), "get_state", nil)
			answer <- rpcResult{response: response, err: callErr}
		}()
		command := child.readCommand(t)
		child.writeRecord(t, map[string]any{"id": command["id"], "type": "response", "command": "get_state", "success": true, "data": map[string]any{}})
		child.closeOutput()
		resolved := <-answer
		require.NoError(t, resolved.err)
		require.NoError(t, requireSuccessfulResponse(resolved.response, "get_state"))
	}
}

func TestRPCClientIgnoresBoundedLateCanceledResponse(t *testing.T) {
	child := newRPCFakeChild()
	client, err := newRPCClient(child)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	answer := make(chan error, 1)
	go func() {
		_, _, callErr := client.call(ctx, "get_state", nil)
		answer <- callErr
	}()
	first := child.readCommand(t)
	cancel()
	require.ErrorIs(t, <-answer, context.Canceled)
	child.writeRecord(t, map[string]any{"id": first["id"], "type": "response", "command": "get_state", "success": true, "data": map[string]any{"late": true}})
	secondAnswer := make(chan error, 1)
	go func() {
		response, _, callErr := client.call(context.Background(), "get_commands", nil)
		if callErr == nil {
			callErr = requireSuccessfulResponse(response, "get_commands")
		}
		secondAnswer <- callErr
	}()
	second := child.readCommand(t)
	child.writeRecord(t, map[string]any{"id": second["id"], "type": "response", "command": "get_commands", "success": true, "data": map[string]any{"commands": []any{}}})
	require.NoError(t, <-secondAnswer)
	child.closeOutput()
	<-client.done
}

func TestRPCClientRejectsUnknownResponseAndResolvesPending(t *testing.T) {
	child := newRPCFakeChild()
	client, err := newRPCClient(child)
	require.NoError(t, err)
	answer := make(chan error, 1)
	go func() {
		_, _, callErr := client.call(context.Background(), "get_state", nil)
		answer <- callErr
	}()
	child.readCommand(t)
	child.writeRecord(t, map[string]any{"id": "unknown", "type": "response", "command": "get_state", "success": true})
	assertProviderCode(t, <-answer, provider.ErrorMalformedStream)
	<-client.done
	assertProviderCode(t, client.terminalError(), provider.ErrorMalformedStream)
}

func TestRPCClientRejectsMalformedResponseAndEventOverflow(t *testing.T) {
	t.Run("missing success", func(t *testing.T) {
		child := newRPCFakeChild()
		client, err := newRPCClient(child)
		require.NoError(t, err)
		answer := make(chan error, 1)
		go func() { _, _, callErr := client.call(context.Background(), "get_state", nil); answer <- callErr }()
		command := child.readCommand(t)
		child.writeRecord(t, map[string]any{"id": command["id"], "type": "response", "command": "get_state"})
		assertProviderCode(t, <-answer, provider.ErrorMalformedStream)
	})
	t.Run("event overflow", func(t *testing.T) {
		child := newRPCFakeChild()
		client, err := newRPCClientWithLimits(child, 1024, 1)
		require.NoError(t, err)
		child.writeRecord(t, map[string]any{"type": "agent_start"})
		child.writeRecord(t, map[string]any{"type": "turn_start"})
		<-client.done
		assertProviderCode(t, client.terminalError(), provider.ErrorMalformedStream)
	})
}

func TestRPCClientSerializesCompleteCommandRecords(t *testing.T) {
	child := newRPCFakeChild()
	client, err := newRPCClient(child)
	require.NoError(t, err)
	const calls = 50
	results := make(chan error, calls)
	for index := range calls {
		go func() {
			response, _, callErr := client.call(context.Background(), "get_state", map[string]any{"index": index})
			if callErr == nil {
				callErr = requireSuccessfulResponse(response, "get_state")
			}
			results <- callErr
		}()
	}
	for range calls {
		command := child.readCommand(t)
		child.writeRecord(t, map[string]any{"id": command["id"], "type": "response", "command": "get_state", "success": true, "data": map[string]any{}})
	}
	for range calls {
		require.NoError(t, <-results)
	}
}

func assertProviderCode(t *testing.T, err error, code provider.ProviderErrorCode) {
	t.Helper()
	var classified provider.ProviderError
	require.ErrorAs(t, err, &classified)
	require.Equal(t, code, classified.Code())
}

type blockingInput struct {
	entered   chan struct{}
	unblock   chan struct{}
	enterOnce sync.Once
	closeOnce sync.Once
}

func newBlockingInput() *blockingInput {
	return &blockingInput{entered: make(chan struct{}), unblock: make(chan struct{})}
}
func (input *blockingInput) Write([]byte) (int, error) {
	input.enterOnce.Do(func() { close(input.entered) })
	<-input.unblock
	return 0, io.ErrClosedPipe
}
func (input *blockingInput) Close() error {
	input.closeOnce.Do(func() { close(input.unblock) })
	return nil
}

type specialRPCChild struct {
	input  io.WriteCloser
	output io.Reader
}

func (child *specialRPCChild) Input() io.WriteCloser { return child.input }
func (child *specialRPCChild) Output() io.Reader     { return child.output }
func (child *specialRPCChild) Errors() io.Reader     { return strings.NewReader("") }
func (*specialRPCChild) Wait() error                 { return nil }
func (*specialRPCChild) Terminate() error            { return nil }
func (*specialRPCChild) Kill() error                 { return nil }

type shortWriter struct {
	maximum int
	value   strings.Builder
}

func (writer *shortWriter) Write(value []byte) (int, error) {
	if len(value) > writer.maximum {
		value = value[:writer.maximum]
	}
	return writer.value.Write(value)
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

type rpcFakeChild struct {
	inputReader  *bufio.Reader
	inputWriter  *io.PipeWriter
	outputReader *io.PipeReader
	outputWriter *io.PipeWriter
	errors       io.Reader
	wait         chan struct{}
	closeOnce    sync.Once
}

func newRPCFakeChild() *rpcFakeChild {
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	return &rpcFakeChild{inputReader: bufio.NewReader(inputReader), inputWriter: inputWriter, outputReader: outputReader, outputWriter: outputWriter, errors: strings.NewReader(""), wait: make(chan struct{})}
}
func (child *rpcFakeChild) Input() io.WriteCloser { return child.inputWriter }
func (child *rpcFakeChild) Output() io.Reader     { return child.outputReader }
func (child *rpcFakeChild) Errors() io.Reader     { return child.errors }
func (child *rpcFakeChild) Wait() error           { <-child.wait; return nil }
func (child *rpcFakeChild) Terminate() error      { child.closeOutput(); return nil }
func (child *rpcFakeChild) Kill() error           { child.closeOutput(); return nil }
func (child *rpcFakeChild) closeOutput() {
	child.closeOnce.Do(func() { _ = child.outputWriter.Close(); close(child.wait) })
}
func (child *rpcFakeChild) readCommand(t *testing.T) map[string]any {
	t.Helper()
	line, err := child.inputReader.ReadBytes('\n')
	require.NoError(t, err)
	var command map[string]any
	require.NoError(t, json.Unmarshal(line, &command))
	return command
}
func (child *rpcFakeChild) writeRecord(t *testing.T, record map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	encoded = append(encoded, '\n')
	_, err = child.outputWriter.Write(encoded)
	require.NoError(t, err)
}
