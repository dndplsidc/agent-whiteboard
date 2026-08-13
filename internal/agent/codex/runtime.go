package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/edocsss/agent-whiteboard/internal/common"
)

const maxJSONLMessageBytes = 4 << 20
const maxPendingRPCRequests = 1024

var (
	errHistoryUnavailableBeforeFirstMessage = errors.New("Codex history is unavailable before the first user message")
	errMethodNotFound                       = errors.New("Codex App Server method unavailable")
)

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcEnvelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type responseBarrier struct {
	done chan struct{}
	once sync.Once
}

func newResponseBarrier() *responseBarrier { return &responseBarrier{done: make(chan struct{})} }
func (barrier *responseBarrier) release()  { barrier.once.Do(func() { close(barrier.done) }) }

type runtime struct {
	driver *Driver
	child  provider.ManagedChild
	input  io.WriteCloser

	writeOnce sync.Once
	writeGate chan struct{}
	mu        sync.Mutex
	nextID    int64
	pending   map[int64]chan rpcResult
	barriers  map[int64]*responseBarrier
	inbound   map[string]struct{}
	sessions  map[string]*Session
	leases    int

	catalogMu sync.RWMutex
	catalog   nativeCatalog

	skillsMu         sync.Mutex
	skillsGeneration uint64
	skillsRefreshing bool
	done             chan struct{}
	stopOnce         sync.Once
	err              error
}

func startRuntime(ctx context.Context, driver *Driver) (*runtime, error) {
	request := provider.LaunchRequest{
		Executable: driver.config.Executable, Arguments: []string{"app-server"}, Environment: append([]string(nil), driver.config.Environment...), WorkingDirectory: driver.config.ProviderRoot,
	}
	if request.Validate() != nil {
		return nil, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	child, err := driver.config.Launcher.Launch(ctx, request)
	if err != nil || common.IsNil(child) || child.Input() == nil || child.Output() == nil {
		return nil, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	runtime := &runtime{driver: driver, child: child, input: child.Input(), pending: make(map[int64]chan rpcResult), barriers: make(map[int64]*responseBarrier), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
	if stderr := child.Errors(); stderr != nil {
		go func() { _, _ = io.CopyBuffer(io.Discard, stderr, make([]byte, 32<<10)) }()
	}
	go runtime.readLoop(child.Output())
	go func() {
		err := child.Wait()
		runtime.stop(err)
	}()
	initialize := map[string]any{
		"clientInfo":   map[string]any{"name": "agent-whiteboard", "title": "Agent Whiteboard", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": false, "requestAttestation": false},
	}
	initialized, err := runtime.call(ctx, "initialize", initialize)
	if err != nil || !validInitializeResponse(initialized) {
		runtime.close()
		return nil, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	if err := runtime.notify(ctx, "initialized", map[string]any{}); err != nil {
		runtime.close()
		return nil, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	account, err := runtime.call(ctx, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		runtime.close()
		return nil, provider.NewProviderError(provider.ErrorReadinessFailed)
	}
	var readiness struct {
		Account            json.RawMessage `json:"account"`
		RequiresOpenAIAuth *bool           `json:"requiresOpenaiAuth"`
	}
	if json.Unmarshal(account, &readiness) != nil || readiness.RequiresOpenAIAuth == nil {
		runtime.close()
		return nil, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	if *readiness.RequiresOpenAIAuth && isJSONNull(readiness.Account) {
		runtime.close()
		return nil, provider.NewProviderError(provider.ErrorAuthenticationRequired)
	}
	catalog, err := loadModelCatalog(ctx, runtime)
	if err != nil {
		runtime.close()
		return nil, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	runtime.catalog = catalog
	return runtime, nil
}

func (runtime *runtime) modelCatalog() nativeCatalog {
	runtime.catalogMu.RLock()
	defer runtime.catalogMu.RUnlock()
	return runtime.catalog.clone()
}

func (runtime *runtime) refreshModelCatalog(ctx context.Context) error {
	catalog, err := loadModelCatalog(ctx, runtime)
	if err != nil {
		return err
	}
	runtime.catalogMu.Lock()
	runtime.catalog = catalog
	runtime.catalogMu.Unlock()
	return nil
}

func (runtime *runtime) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	result, release, err := runtime.callWithOrder(ctx, method, params, false)
	release()
	return result, err
}

func (runtime *runtime) callOrdered(ctx context.Context, method string, params any) (json.RawMessage, func(), error) {
	return runtime.callWithOrder(ctx, method, params, true)
}

func (runtime *runtime) callWithOrder(ctx context.Context, method string, params any, ordered bool) (json.RawMessage, func(), error) {
	noop := func() {}
	if err := ctx.Err(); err != nil {
		return nil, noop, err
	}
	runtime.mu.Lock()
	select {
	case <-runtime.done:
		err := runtime.err
		runtime.mu.Unlock()
		if err == nil {
			err = provider.NewProviderError(provider.ErrorChildExited)
		}
		return nil, noop, err
	default:
	}
	runtime.nextID++
	if runtime.nextID <= 0 || len(runtime.pending) >= maxPendingRPCRequests {
		runtime.mu.Unlock()
		return nil, noop, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	id := runtime.nextID
	response := make(chan rpcResult, 1)
	runtime.pending[id] = response
	var barrier *responseBarrier
	if ordered {
		barrier = newResponseBarrier()
		if runtime.barriers == nil {
			runtime.barriers = make(map[int64]*responseBarrier)
		}
		runtime.barriers[id] = barrier
	}
	runtime.mu.Unlock()
	if err := runtime.write(ctx, map[string]any{"id": id, "method": method, "params": params}); err != nil {
		runtime.mu.Lock()
		delete(runtime.pending, id)
		delete(runtime.barriers, id)
		runtime.mu.Unlock()
		if barrier != nil {
			barrier.release()
		}
		if ctx.Err() != nil {
			return nil, noop, ctx.Err()
		}
		return nil, noop, err
	}
	select {
	case result := <-response:
		resultErr := result.err
		if errors.Is(resultErr, errMethodNotFound) && method != "thread/compact/start" {
			resultErr = provider.NewProviderError(provider.ErrorProtocolFailure)
		}
		if barrier == nil {
			return result.result, noop, resultErr
		}
		return result.result, barrier.release, resultErr
	case <-ctx.Done():
		runtime.mu.Lock()
		delete(runtime.pending, id)
		delete(runtime.barriers, id)
		runtime.mu.Unlock()
		if barrier != nil {
			barrier.release()
		}
		return nil, noop, ctx.Err()
	case <-runtime.done:
		if barrier != nil {
			barrier.release()
		}
		return nil, noop, runtime.failure()
	}
}

func (runtime *runtime) notify(ctx context.Context, method string, params any) error {
	return runtime.write(ctx, map[string]any{"method": method, "params": params})
}

func (runtime *runtime) claimInbound(id json.RawMessage) bool {
	key, err := rpcRequestIDKey(id)
	if err != nil {
		return false
	}
	runtime.mu.Lock()
	_, pending := runtime.inbound[key]
	if pending {
		delete(runtime.inbound, key)
	}
	runtime.mu.Unlock()
	return pending
}

func (runtime *runtime) respondClaimed(ctx context.Context, id json.RawMessage, result any, responseErr *rpcError) error {
	encoded, err := encodeRPCResponse(id, result, responseErr)
	if err != nil {
		return err
	}
	return runtime.writeEncoded(ctx, encoded)
}

func encodeRPCResponse(id json.RawMessage, result any, responseErr *rpcError) ([]byte, error) {
	message := map[string]any{"id": json.RawMessage(bytes.Clone(id))}
	if responseErr != nil {
		message["error"] = responseErr
	} else {
		message["result"] = result
	}
	return encodeRPCMessage(message)
}

func (runtime *runtime) respond(ctx context.Context, id json.RawMessage, result any, responseErr *rpcError) error {
	encoded, err := encodeRPCResponse(id, result, responseErr)
	if err != nil {
		return err
	}
	if !runtime.claimInbound(id) {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	return runtime.writeEncoded(ctx, encoded)
}

func (runtime *runtime) write(ctx context.Context, message any) error {
	encoded, err := encodeRPCMessage(message)
	if err != nil {
		return err
	}
	return runtime.writeEncoded(ctx, encoded)
}

func encodeRPCMessage(message any) ([]byte, error) {
	encoded, err := json.Marshal(message)
	if err != nil || len(encoded) > maxJSONLMessageBytes {
		return nil, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	return append(encoded, '\n'), nil
}

func (runtime *runtime) writeEncoded(ctx context.Context, encoded []byte) error {
	runtime.writeOnce.Do(func() {
		runtime.writeGate = make(chan struct{}, 1)
		runtime.writeGate <- struct{}{}
	})
	select {
	case <-runtime.writeGate:
		defer func() { runtime.writeGate <- struct{}{} }()
	case <-ctx.Done():
		return ctx.Err()
	case <-runtime.done:
		return runtime.failure()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stopWatch := context.AfterFunc(ctx, runtime.close)
	defer stopWatch()
	if _, err := runtime.input.Write(encoded); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return provider.NewProviderError(provider.ErrorChildExited)
	}
	return nil
}

func (runtime *runtime) readLoop(output io.Reader) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), maxJSONLMessageBytes)
	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		if len(bytes.TrimSpace(line)) == 0 || len(line) > maxJSONLMessageBytes {
			runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
			return
		}
		var envelope rpcEnvelope
		if validateJSONStructure(line) != nil || json.Unmarshal(line, &envelope) != nil {
			runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
			return
		}
		switch {
		case len(envelope.ID) != 0 && envelope.Method == "":
			if (envelope.Error == nil) == (len(envelope.Result) == 0) {
				runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
				return
			}
			runtime.handleResponse(envelope)
		case len(envelope.ID) != 0 && envelope.Method != "":
			if len(envelope.Params) == 0 {
				runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
				return
			}
			runtime.handleServerRequest(envelope)
		case len(envelope.ID) == 0 && envelope.Method != "":
			if len(envelope.Params) == 0 {
				runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
				return
			}
			runtime.handleNotification(envelope.Method, envelope.Params)
		default:
			runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
			return
		}
	}
	if err := scanner.Err(); err != nil {
		runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
	} else {
		runtime.stop(provider.NewProviderError(provider.ErrorChildExited))
	}
}

func validateJSONStructure(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, duplicate := keys[key]; duplicate {
				return errors.New("duplicate JSON object key")
			}
			keys[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func (runtime *runtime) handleResponse(envelope rpcEnvelope) {
	var id int64
	if json.Unmarshal(envelope.ID, &id) != nil || id <= 0 {
		runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
		return
	}
	runtime.mu.Lock()
	waiter := runtime.pending[id]
	barrier := runtime.barriers[id]
	// Request IDs are allocated monotonically. nextID is therefore the
	// tombstone frontier for completed, canceled, and otherwise retired calls.
	allocated := id <= runtime.nextID
	delete(runtime.pending, id)
	delete(runtime.barriers, id)
	runtime.mu.Unlock()
	if waiter == nil {
		if !allocated {
			runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
		}
		return
	}
	if envelope.Error != nil {
		waiter <- rpcResult{err: classifyRPCError(envelope.Error)}
	} else {
		waiter <- rpcResult{result: bytes.Clone(envelope.Result)}
	}
	if barrier != nil {
		select {
		case <-barrier.done:
		case <-runtime.done:
		}
	}
}

func (runtime *runtime) handleNotification(method string, params json.RawMessage) {
	if method == "skills/changed" {
		if validateJSONStructure(params) != nil {
			runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
			return
		}
		runtime.scheduleSkillRefresh()
		return
	}
	threadID := extractString(params, "threadId")
	if threadID == "" {
		if method == "turn/completed" || method == "thread/settings/updated" || method == "model/rerouted" {
			runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
		}
		return
	}
	runtime.mu.Lock()
	session := runtime.sessions[threadID]
	runtime.mu.Unlock()
	if session != nil {
		session.handleNotification(method, params)
	}
}

func (runtime *runtime) handleServerRequest(envelope rpcEnvelope) {
	key, err := rpcRequestIDKey(envelope.ID)
	if err != nil {
		runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
		return
	}
	runtime.mu.Lock()
	_, duplicate := runtime.inbound[key]
	full := len(runtime.inbound) >= maxPendingRPCRequests
	if !duplicate && !full {
		runtime.inbound[key] = struct{}{}
	}
	runtime.mu.Unlock()
	if duplicate || full {
		runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
		return
	}
	threadID := extractString(envelope.Params, "threadId")
	runtime.mu.Lock()
	session := runtime.sessions[threadID]
	runtime.mu.Unlock()
	if session == nil {
		_ = runtime.respond(context.Background(), envelope.ID, nil, &rpcError{Code: -32602, Message: "thread unavailable"})
		return
	}
	if err := session.handleServerRequest(envelope.ID, envelope.Method, envelope.Params); err != nil {
		_ = runtime.respond(context.Background(), envelope.ID, nil, &rpcError{Code: -32601, Message: "unsupported request"})
		session.fail(provider.ErrorProtocolIncompatible)
	}
}

func (runtime *runtime) register(session *Session) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if session == nil || session.threadID == "" || runtime.sessions[session.threadID] != nil {
		return errors.New("invalid Codex session registration")
	}
	runtime.sessions[session.threadID] = session
	return nil
}

func (runtime *runtime) unregister(session *Session) {
	runtime.mu.Lock()
	if session != nil && runtime.sessions[session.threadID] == session {
		delete(runtime.sessions, session.threadID)
	}
	runtime.mu.Unlock()
}

func (runtime *runtime) close() {
	runtime.stop(provider.NewProviderError(provider.ErrorChildExited))
}

func (runtime *runtime) stopTransport() {
	runtime.stopOnce.Do(func() {
		if runtime.input != nil {
			_ = runtime.input.Close()
		}
		if !common.IsNil(runtime.child) {
			_ = runtime.child.Terminate()
			_ = runtime.child.Kill()
		}
	})
}

func (runtime *runtime) stop(cause error) {
	runtime.mu.Lock()
	select {
	case <-runtime.done:
		runtime.mu.Unlock()
		return
	default:
	}
	cause = providerRuntimeCause(cause)
	runtime.err = cause
	pending := runtime.pending
	barriers := runtime.barriers
	sessions := make([]*Session, 0, len(runtime.sessions))
	for _, session := range runtime.sessions {
		sessions = append(sessions, session)
	}
	runtime.pending = make(map[int64]chan rpcResult)
	runtime.barriers = make(map[int64]*responseBarrier)
	close(runtime.done)
	runtime.mu.Unlock()
	runtime.stopTransport()
	for _, waiter := range pending {
		waiter <- rpcResult{err: cause}
	}
	for _, barrier := range barriers {
		barrier.release()
	}
	for _, session := range sessions {
		session.runtimeStopped(cause)
	}
}

func (runtime *runtime) failure() error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return providerRuntimeCause(runtime.err)
}

func providerRuntimeCause(cause error) provider.ProviderError {
	var typed provider.ProviderError
	if errors.As(cause, &typed) && typed.Valid() {
		return typed
	}
	return provider.NewProviderError(provider.ErrorChildExited)
}

func classifyRPCError(failure *rpcError) error {
	if failure == nil {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	if failure.Code == -32601 {
		return errMethodNotFound
	}
	text := failure.Message + " " + string(failure.Data)
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "not materialized yet") && strings.Contains(lower, "includeturns is unavailable before first user message"):
		return errHistoryUnavailableBeforeFirstMessage
	case strings.Contains(lower, "unsupported model"), strings.Contains(lower, "model is not supported"), strings.Contains(lower, "unsupported reasoning effort"), strings.Contains(lower, "reasoning effort is not supported"), strings.Contains(lower, "unsupported service tier"), strings.Contains(lower, "service tier is not supported"):
		return provider.NewProviderError(provider.ErrorInvalidModelConfiguration)
	case strings.Contains(text, "contextWindowExceeded"):
		return provider.NewProviderError(provider.ErrorContextTooLarge)
	case strings.Contains(lower, "unauthorized"):
		return provider.NewProviderError(provider.ErrorAuthenticationRequired)
	case strings.Contains(lower, "threadnotfound"), strings.Contains(lower, "thread not found"), strings.Contains(lower, "thread_not_found"):
		return provider.NewProviderError(provider.ErrorNativeSessionMissing)
	default:
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
}

func extractString(raw json.RawMessage, key string) string {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return ""
	}
	var value string
	_ = json.Unmarshal(object[key], &value)
	return value
}

func isJSONNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func validInitializeResponse(raw json.RawMessage) bool {
	var response struct {
		CodexHome      string `json:"codexHome"`
		PlatformFamily string `json:"platformFamily"`
		PlatformOS     string `json:"platformOs"`
		UserAgent      string `json:"userAgent"`
	}
	return json.Unmarshal(raw, &response) == nil && validAbsolutePath(response.CodexHome) &&
		response.PlatformFamily != "" && response.PlatformOS != "" && response.UserAgent != ""
}

func rpcRequestIDKey(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" || len(text) > provider.MaxNativeReferenceBytes {
			return "", errors.New("invalid JSON-RPC request id")
		}
		return "s:" + text, nil
	}
	var number int64
	if json.Unmarshal(raw, &number) == nil {
		return "n:" + string(bytes.TrimSpace(raw)), nil
	}
	return "", errors.New("invalid JSON-RPC request id")
}
