package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

const (
	maxRPCPending    = 1024
	maxRPCCanceled   = 1024
	maxRPCWriteQueue = 8
	defaultRPCEvents = 256
)

type rpcResponse struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

type rpcResult struct {
	response rpcResponse
	err      error
}

type rpcPending struct {
	command string
	result  chan rpcResult
}

type rpcWrite struct {
	encoded []byte
	result  chan error
}

type rpcClient struct {
	child provider.ManagedChild
	input io.WriteCloser

	mu          sync.Mutex
	inputOnce   sync.Once
	nextID      uint64
	pending     map[string]rpcPending
	canceled    map[string]string
	cancelOrder []string
	terminal    error
	eventsSeen  atomic.Uint64

	writes chan rpcWrite
	events chan json.RawMessage
	done   chan struct{}
}

func newRPCClient(child provider.ManagedChild) (*rpcClient, error) {
	return newRPCClientWithLimits(child, maxRPCRecordBytes, defaultRPCEvents)
}

func newRPCClientWithLimits(child provider.ManagedChild, recordLimit, eventCapacity int) (*rpcClient, error) {
	if common.IsNil(child) || child.Input() == nil || child.Output() == nil || eventCapacity <= 0 {
		return nil, errors.New("invalid Pi RPC child")
	}
	client := &rpcClient{
		child: child, input: child.Input(), pending: make(map[string]rpcPending), canceled: make(map[string]string),
		writes: make(chan rpcWrite, maxRPCWriteQueue), events: make(chan json.RawMessage, eventCapacity), done: make(chan struct{}),
	}
	go client.writeLoop()
	go client.readLoop(newJSONLReader(child.Output(), recordLimit))
	return client, nil
}

func (client *rpcClient) call(ctx context.Context, commandType string, fields map[string]any) (rpcResponse, bool, error) {
	if commandType == "" {
		return rpcResponse{}, false, errors.New("invalid Pi RPC call")
	}
	if err := ctx.Err(); err != nil {
		return rpcResponse{}, false, err
	}
	client.mu.Lock()
	if client.terminal != nil {
		err := client.terminal
		client.mu.Unlock()
		return rpcResponse{}, false, err
	}
	if len(client.pending) >= maxRPCPending {
		client.mu.Unlock()
		return rpcResponse{}, false, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	client.nextID++
	id := "awb-" + strconv.FormatUint(client.nextID, 10)
	result := make(chan rpcResult, 1)
	client.pending[id] = rpcPending{command: commandType, result: result}
	client.mu.Unlock()

	command := make(map[string]any, len(fields)+2)
	command["id"] = id
	command["type"] = commandType
	for key, value := range fields {
		if key == "id" || key == "type" {
			client.cancelPending(id, false)
			return rpcResponse{}, false, errors.New("invalid Pi RPC command field")
		}
		command[key] = value
	}
	encoded, err := json.Marshal(command)
	if err != nil {
		client.cancelPending(id, false)
		return rpcResponse{}, false, errors.New("encode Pi RPC command")
	}
	encoded = append(encoded, '\n')
	writeResult := make(chan error, 1)
	select {
	case client.writes <- rpcWrite{encoded: encoded, result: writeResult}:
	case <-ctx.Done():
		wipe(encoded)
		client.cancelPending(id, false)
		return rpcResponse{}, false, ctx.Err()
	case <-client.done:
		wipe(encoded)
		client.cancelPending(id, false)
		return rpcResponse{}, false, client.terminalError()
	}
	wrote := true
	select {
	case err = <-writeResult:
		if err != nil {
			client.cancelPending(id, false)
			return rpcResponse{}, wrote, err
		}
	case <-ctx.Done():
		client.cancelPending(id, true)
		return rpcResponse{}, wrote, ctx.Err()
	case <-client.done:
		select {
		case resolved := <-result:
			return resolveRPCResult(resolved, wrote)
		default:
		}
		select {
		case err = <-writeResult:
			if err != nil {
				client.cancelPending(id, false)
				return rpcResponse{}, wrote, err
			}
		default:
			client.cancelPending(id, true)
			return rpcResponse{}, wrote, client.terminalError()
		}
	}

	select {
	case resolved := <-result:
		return resolveRPCResult(resolved, wrote)
	case <-ctx.Done():
		client.cancelPending(id, true)
		return rpcResponse{}, wrote, ctx.Err()
	case <-client.done:
		// A final valid response is routed before the following EOF can make
		// done ready. Preserve that authoritative response.
		select {
		case resolved := <-result:
			return resolveRPCResult(resolved, wrote)
		default:
			client.cancelPending(id, true)
			return rpcResponse{}, wrote, client.terminalError()
		}
	}
}

func (client *rpcClient) notify(ctx context.Context, fields map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	encoded = append(encoded, '\n')
	result := make(chan error, 1)
	select {
	case client.writes <- rpcWrite{encoded: encoded, result: result}:
	case <-ctx.Done():
		wipe(encoded)
		return ctx.Err()
	case <-client.done:
		wipe(encoded)
		return client.terminalError()
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-client.done:
		select {
		case err := <-result:
			return err
		default:
			return client.terminalError()
		}
	}
}

func resolveRPCResult(resolved rpcResult, wrote bool) (rpcResponse, bool, error) {
	if resolved.err != nil {
		return rpcResponse{}, wrote, resolved.err
	}
	return resolved.response, wrote, nil
}

func (client *rpcClient) cancelPending(id string, remember bool) {
	client.mu.Lock()
	if pending, exists := client.pending[id]; exists {
		delete(client.pending, id)
		if remember {
			client.canceled[id] = pending.command
			client.cancelOrder = append(client.cancelOrder, id)
			if len(client.cancelOrder) > maxRPCCanceled {
				oldest := client.cancelOrder[0]
				client.cancelOrder = client.cancelOrder[1:]
				delete(client.canceled, oldest)
			}
		}
	}
	client.mu.Unlock()
}

func (client *rpcClient) writeLoop() {
	for {
		select {
		case write := <-client.writes:
			err := writeFull(client.input, write.encoded)
			wipe(write.encoded)
			if err != nil {
				classified := provider.NewProviderError(provider.ErrorProtocolFailure)
				write.result <- classified
				client.finish(classified)
				return
			}
			write.result <- nil
		case <-client.done:
			for {
				select {
				case write := <-client.writes:
					wipe(write.encoded)
					write.result <- client.terminalError()
				default:
					return
				}
			}
		}
	}
}

func writeFull(writer io.Writer, encoded []byte) error {
	for len(encoded) > 0 {
		written, err := writer.Write(encoded)
		if written > 0 {
			encoded = encoded[written:]
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func (client *rpcClient) readLoop(reader *jsonlReader) {
	defer close(client.events)
	for {
		record, err := reader.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				client.finish(provider.NewProviderError(provider.ErrorChildExited))
			} else {
				client.finish(provider.NewProviderError(provider.ErrorMalformedStream))
			}
			return
		}
		var header struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(record, &header); err != nil || header.Type == "" {
			wipe(record)
			client.finish(provider.NewProviderError(provider.ErrorMalformedStream))
			return
		}
		if header.Type == "response" {
			if !client.routeResponse(record) {
				wipe(record)
				client.finish(provider.NewProviderError(provider.ErrorMalformedStream))
				return
			}
			wipe(record)
			continue
		}
		client.eventsSeen.Add(1)
		select {
		case client.events <- record:
		default:
			wipe(record)
			client.finish(provider.NewProviderError(provider.ErrorMalformedStream))
			return
		}
	}
}

func (client *rpcClient) routeResponse(record json.RawMessage) bool {
	var response rpcResponse
	if err := json.Unmarshal(record, &response); err != nil || response.Type != "response" || response.ID == "" || response.Command == "" || response.Success == nil {
		return false
	}
	client.mu.Lock()
	if command, canceled := client.canceled[response.ID]; canceled {
		delete(client.canceled, response.ID)
		client.mu.Unlock()
		return response.Command == command
	}
	pending, exists := client.pending[response.ID]
	if exists {
		delete(client.pending, response.ID)
	}
	client.mu.Unlock()
	if !exists {
		return false
	}
	if pending.command != response.Command {
		pending.result <- rpcResult{err: provider.NewProviderError(provider.ErrorMalformedStream)}
		return false
	}
	pending.result <- rpcResult{response: response}
	return true
}

func (client *rpcClient) finish(err error) {
	if err == nil {
		err = provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	client.mu.Lock()
	if client.terminal != nil {
		client.mu.Unlock()
		return
	}
	client.terminal = err
	pending := client.pending
	client.pending = make(map[string]rpcPending)
	close(client.done)
	client.mu.Unlock()
	client.inputOnce.Do(func() { _ = client.input.Close() })
	for _, waiter := range pending {
		waiter.result <- rpcResult{err: err}
	}
}

func (client *rpcClient) eventCount() uint64 { return client.eventsSeen.Load() }

func (client *rpcClient) terminalError() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.terminal
}

func requireSuccessfulResponse(response rpcResponse, command string) error {
	if response.Command != command || response.Success == nil {
		return provider.NewProviderError(provider.ErrorMalformedStream)
	}
	if !*response.Success {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	return nil
}

func decodeResponseData(response rpcResponse, target any) error {
	if target == nil || len(response.Data) == 0 || string(response.Data) == "null" {
		return provider.NewProviderError(provider.ErrorMalformedStream)
	}
	decoder := json.NewDecoder(bytes.NewReader(response.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return provider.NewProviderError(provider.ErrorMalformedStream)
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return provider.NewProviderError(provider.ErrorMalformedStream)
	}
	return nil
}
