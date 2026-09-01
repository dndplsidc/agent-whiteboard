// Command scripted-cursor-acp is a hermetic ACP v1 agent used by integration tests.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"golang.org/x/sys/unix"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}
type session struct {
	ID, Model string
	Replay    []json.RawMessage
}
type state struct {
	Next     int
	Sessions []session
}
type agent struct {
	mu, fileMu                                 sync.Mutex
	out                                        *bufio.Writer
	statePath, evidencePath, control, scenario string
	state                                      state
	pending                                    map[string]chan request
	nextRequest                                int
}

func main() {
	if len(os.Args) != 2 || os.Args[1] != "acp" {
		fmt.Fprintln(os.Stderr, "expected argv: acp")
		os.Exit(2)
	}
	a := &agent{out: bufio.NewWriter(os.Stdout), statePath: os.Getenv("AWB_CURSOR_STATE"), evidencePath: os.Getenv("AWB_CURSOR_EVIDENCE"), control: os.Getenv("AWB_CURSOR_CONTROL"), scenario: os.Getenv("AWB_CURSOR_SCENARIO"), pending: map[string]chan request{}}
	_ = readJSON(a.statePath, &a.state)
	a.record(map[string]any{"method": "process_start", "pid": os.Getpid(), "process_ordinal": a.ordinal(), "workspace_identity": digest(mustGetwd())})
	s := bufio.NewScanner(os.Stdin)
	s.Buffer(make([]byte, 64<<10), 128<<20)
	for s.Scan() {
		var r request
		if json.Unmarshal(s.Bytes(), &r) != nil || r.JSONRPC != "2.0" {
			os.Exit(3)
		}
		if r.Method == "" {
			a.deliver(r)
			continue
		}
		go a.handle(r)
	}
	if s.Err() != nil {
		os.Exit(4)
	}
}
func mustGetwd() string      { p, _ := os.Getwd(); return p }
func digest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func readJSON(path string, v any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func writeJSON(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	tmp := path + "." + strconv.Itoa(os.Getpid())
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	return os.Rename(tmp, path)
}
func (a *agent) withFileLock(fn func()) {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	lock, err := os.OpenFile(a.statePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture lock open failed")
		os.Exit(5)
	}
	defer lock.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			fmt.Fprintln(os.Stderr, "fixture lock failed")
			os.Exit(5)
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "fixture lock timeout")
			os.Exit(5)
		}
		time.Sleep(2 * time.Millisecond)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	fn()
}
func (a *agent) ordinal() int {
	var n int
	a.withFileLock(func() {
		p := a.statePath + ".ordinal"
		b, _ := os.ReadFile(p)
		n, _ = strconv.Atoi(string(b))
		n++
		_ = os.WriteFile(p, []byte(strconv.Itoa(n)), 0600)
	})
	return n
}
func (a *agent) record(v any) {
	if a.evidencePath == "" {
		return
	}
	b, _ := json.Marshal(v)
	a.withFileLock(func() {
		f, e := os.OpenFile(a.evidencePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if e == nil {
			_, _ = f.Write(append(b, '\n'))
			_ = f.Close()
		}
	})
}
func (a *agent) send(v any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, _ := json.Marshal(v)
	_, _ = a.out.Write(append(b, '\n'))
	_ = a.out.Flush()
}
func (a *agent) ok(id json.RawMessage, result any) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func (a *agent) rpcError(id json.RawMessage, code int) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": "scripted"}})
}
func (a *agent) notify(id string, update any) {
	a.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": id, "update": update}})
}
func options(model string) []any {
	return []any{map[string]any{"id": "model", "name": "Model", "description": "", "category": "model", "type": "select", "currentValue": model, "options": []any{map[string]any{"value": "cursor-small", "name": "Cursor Small", "description": ""}, map[string]any{"value": "cursor-large", "name": "Cursor Large", "description": ""}}}}
}
func (a *agent) stateTxn(fn func(*state)) {
	a.withFileLock(func() {
		var current state
		_ = readJSON(a.statePath, &current)
		fn(&current)
		_ = writeJSON(a.statePath, current)
		a.state = current
	})
}
func (a *agent) stateSnapshot() state {
	var current state
	a.withFileLock(func() { _ = readJSON(a.statePath, &current) })
	return current
}
func (a *agent) find(id string) *session {
	for i := range a.state.Sessions {
		if a.state.Sessions[i].ID == id {
			return &a.state.Sessions[i]
		}
	}
	return nil
}
func params(raw json.RawMessage) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	return p
}
func (a *agent) handle(r request) {
	p := params(r.Params)
	a.record(map[string]any{"method": r.Method, "pid": os.Getpid(), "workspace_identity": digest(mustGetwd())})
	switch r.Method {
	case "initialize":
		if a.scenario == "auth" {
			a.rpcError(r.ID, -32000)
			return
		}
		version := 1
		if a.scenario == "protocol" {
			version = 2
		}
		caps := map[string]any{"loadSession": true, "promptCapabilities": map[string]any{"image": true, "embeddedContext": false}, "sessionCapabilities": map[string]any{"list": map[string]any{}}}
		if a.scenario == "missing_load" {
			caps["loadSession"] = false
		}
		if a.scenario == "missing_list" {
			caps["sessionCapabilities"] = map[string]any{}
		}
		a.ok(r.ID, map[string]any{"protocolVersion": version, "agentCapabilities": caps})
	case "session/list":
		if a.scenario == "auth_list" {
			a.rpcError(r.ID, -32000)
			return
		}
		current := a.stateSnapshot()
		items := []any{}
		for _, s := range current.Sessions {
			items = append(items, map[string]any{"sessionId": s.ID, "configOptions": options(s.Model)})
		}
		a.ok(r.ID, map[string]any{"sessions": items})
	case "session/new":
		var s session
		a.stateTxn(func(current *state) {
			current.Next++
			s = session{ID: fmt.Sprintf("fixture-%d", current.Next), Model: "cursor-small", Replay: []json.RawMessage{}}
			current.Sessions = append(current.Sessions, s)
		})
		a.record(map[string]any{"method": "session/new.semantic", "workspace_identity": digest(fmt.Sprint(p["cwd"])), "model_option": s.Model})
		a.ok(r.ID, map[string]any{"sessionId": s.ID, "configOptions": options(s.Model)})
	case "session/load":
		current := a.stateSnapshot()
		id, _ := p["sessionId"].(string)
		var loaded *session
		for i := range current.Sessions {
			if current.Sessions[i].ID == id {
				copy := current.Sessions[i]
				loaded = &copy
				break
			}
		}
		if loaded == nil {
			a.rpcError(r.ID, -32602)
			return
		}
		for _, u := range loaded.Replay {
			var v any
			_ = json.Unmarshal(u, &v)
			a.notify(id, v)
		}
		a.record(map[string]any{"method": "session/load.semantic", "workspace_identity": digest(fmt.Sprint(p["cwd"])), "model_option": loaded.Model, "content_block_count": len(loaded.Replay)})
		a.ok(r.ID, map[string]any{"sessionId": id, "configOptions": options(loaded.Model)})
	case "session/set_config_option":
		id, _ := p["sessionId"].(string)
		model, _ := p["value"].(string)
		found := false
		a.stateTxn(func(current *state) {
			for i := range current.Sessions {
				if current.Sessions[i].ID == id {
					current.Sessions[i].Model = model
					found = true
					break
				}
			}
		})
		if !found {
			a.rpcError(r.ID, -32602)
			return
		}
		a.record(map[string]any{"method": "session/set_config_option.semantic", "model_option": model})
		a.ok(r.ID, map[string]any{"configOptions": options(model)})
	case "session/prompt":
		a.prompt(r, p)
	case "session/cancel":
		a.record(map[string]any{"method": "session/cancel.semantic", "cancel_outcome": "received"})
		a.release("cancel")
		a.ok(r.ID, map[string]any{})
	default:
		a.rpcError(r.ID, -32601)
	}
}
func (a *agent) prompt(r request, p map[string]any) {
	id, _ := p["sessionId"].(string)
	blocks, _ := p["prompt"].([]any)
	types := []string{}
	imageType := ""
	imageBytes := 0
	turnID := ""
	for _, raw := range blocks {
		b, _ := raw.(map[string]any)
		typ, _ := b["type"].(string)
		types = append(types, typ)
		if typ == "image" {
			imageType, _ = b["mimeType"].(string)
			data, _ := b["data"].(string)
			decoded, _ := base64.StdEncoding.DecodeString(data)
			imageBytes = len(decoded)
		}
		if typ == "text" {
			text, _ := b["text"].(string)
			if envelope, err := provider.Parse([]byte(text)); err == nil {
				turnID = envelope.TurnID
			}
		}
	}
	a.record(map[string]any{"method": "session/prompt.semantic", "content_block_types": types, "content_block_count": len(blocks), "image_media_type": imageType, "image_decoded_byte_count": imageBytes, "broker_turn_id": turnID})
	if a.scenario == "crash_prompt" {
		a.barrier("crash")
		os.Exit(17)
	}
	a.notify(id, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "native-tool", "title": "Run fixture", "kind": "execute", "status": "in_progress"})
	a.notify(id, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "native-tool", "status": "completed"})
	if strings.Contains(a.scenario, "permission") {
		outcome := a.permission(id)
		a.record(map[string]any{"method": "permission.semantic", "permission_outcome": outcome})
		if outcome == "cancelled" {
			a.record(map[string]any{"method": "session/prompt.return", "cancel_outcome": "cancelled"})
			a.ok(r.ID, map[string]any{"stopReason": "cancelled"})
			return
		}
	}
	a.notify(id, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "scripted answer"}})
	if strings.Contains(a.scenario, "hold") {
		a.barrier("prompt")
		if strings.Contains(a.scenario, "cancel") {
			a.record(map[string]any{"method": "session/prompt.return", "cancel_outcome": "cancelled"})
			a.ok(r.ID, map[string]any{"stopReason": "cancelled"})
			return
		}
	}
	env, _ := json.Marshal(map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": firstText(blocks)}})
	ans, _ := json.Marshal(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "scripted answer"}})
	a.stateTxn(func(current *state) {
		for i := range current.Sessions {
			if current.Sessions[i].ID == id {
				current.Sessions[i].Replay = append(current.Sessions[i].Replay, env, ans)
				break
			}
		}
	})
	a.ok(r.ID, map[string]any{"stopReason": "end_turn"})
}
func firstText(blocks []any) string {
	for _, raw := range blocks {
		b, _ := raw.(map[string]any)
		if b["type"] == "text" {
			v, _ := b["text"].(string)
			return v
		}
	}
	return ""
}
func (a *agent) permission(id string) string {
	a.mu.Lock()
	a.nextRequest++
	n := a.nextRequest
	ch := make(chan request, 1)
	key := strconv.Itoa(n)
	a.pending[key] = ch
	a.mu.Unlock()
	a.send(map[string]any{"jsonrpc": "2.0", "id": n, "method": "session/request_permission", "params": map[string]any{"sessionId": id, "toolCall": map[string]any{"toolCallId": "native-tool", "title": "Run fixture", "kind": "execute"}, "options": []any{map[string]any{"optionId": "allow", "name": "Allow once", "kind": "allow_once"}, map[string]any{"optionId": "reject", "name": "Reject", "kind": "reject_once"}}}})
	if strings.Contains(a.scenario, "hold_permission") {
		a.barrier("permission")
	}
	resp := <-ch
	var result map[string]any
	_ = json.Unmarshal(resp.Result, &result)
	outcome, _ := result["outcome"].(map[string]any)
	option, _ := outcome["optionId"].(string)
	if option != "" {
		return option
	}
	return "cancelled"
}
func (a *agent) deliver(r request) {
	key := string(r.ID)
	a.mu.Lock()
	ch := a.pending[key]
	delete(a.pending, key)
	a.mu.Unlock()
	if ch != nil {
		ch <- r
	}
}
func (a *agent) barrier(name string) {
	if a.control == "" {
		return
	}
	ready := filepath.Join(a.control, name+".ready")
	release := filepath.Join(a.control, name+".release")
	_ = os.WriteFile(ready, []byte("ready"), 0600)
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	a.mu.Lock()
	ch := a.pending["barrier"]
	if ch == nil {
		ch = make(chan request, 1)
		a.pending["barrier"] = ch
	}
	a.mu.Unlock()
	for {
		if _, e := os.Stat(release); e == nil {
			return
		}
		select {
		case <-ch:
			return
		case <-ticker.C:
		}
	}
}
func (a *agent) release(name string) {
	a.mu.Lock()
	ch := a.pending["barrier"]
	delete(a.pending, "barrier")
	a.mu.Unlock()
	if ch != nil {
		ch <- request{}
	}
	_ = os.WriteFile(filepath.Join(a.control, name+".release"), []byte("release"), 0600)
	if name == "cancel" {
		_ = os.WriteFile(filepath.Join(a.control, "permission.release"), []byte("release"), 0600)
		_ = os.WriteFile(filepath.Join(a.control, "prompt.release"), []byte("release"), 0600)
	}
}
