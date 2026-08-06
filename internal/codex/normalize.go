package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/edocsss/agent-whiteboard/internal/contentturn"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

const maxTrackedToolItems = 1024

func (session *Session) handleNotification(method string, params json.RawMessage) {
	if method == "serverRequest/resolved" {
		session.handleServerRequestResolved(params)
		return
	}
	nativeTurnID := notificationTurnID(method, params)
	session.mu.Lock()
	turn := session.active
	if turn != nil && nativeTurnID != "" && turn.nativeID == "" {
		size := len(method) + len(params)
		if len(turn.buffered) >= provider.MaxLaunchItems || turn.bytes+size > maxJSONLMessageBytes {
			session.mu.Unlock()
			session.fail(provider.ErrorMalformedStream)
			return
		}
		turn.buffered = append(turn.buffered, bufferedNativeEvent{method: method, params: bytes.Clone(params)})
		turn.bytes += size
		session.mu.Unlock()
		return
	}
	if nativeTurnID != "" && (turn == nil || turn.nativeID != nativeTurnID) {
		session.mu.Unlock()
		return
	}
	brokerTurnID := ""
	if turn != nil {
		brokerTurnID = turn.request.TurnID
	}
	session.mu.Unlock()

	switch method {
	case "item/agentMessage/delta":
		var notification struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(params, &notification) != nil || notification.Delta == "" || brokerTurnID == "" {
			return
		}
		for _, delta := range splitText(notification.Delta, provider.MaxDeltaBytes) {
			session.emit(provider.NewAssistantDeltaEvent(brokerTurnID, contentturn.AssistantMessageID(brokerTurnID), delta))
		}
	case "item/started":
		session.handleItem(params, brokerTurnID, provider.ToolRunning)
	case "item/completed":
		session.handleItem(params, brokerTurnID, "")
	case "item/commandExecution/outputDelta", "item/fileChange/outputDelta", "item/plan/delta":
		session.handleToolUpdate(params, brokerTurnID, "delta")
	case "item/commandExecution/terminalInteraction":
		session.handleToolUpdate(params, brokerTurnID, "stdin")
	case "item/fileChange/patchUpdated":
		session.handleToolUpdate(params, brokerTurnID, "changes")
	case "item/mcpToolCall/progress":
		session.handleToolUpdate(params, brokerTurnID, "message")
	case "thread/compacted":
		session.emit(provider.NewActivityEvent(brokerTurnID, provider.ActivityCompaction, "Codex compacted the conversation context."))
	case "turn/completed":
		session.handleTurnCompleted(params, brokerTurnID)
	}
}

func notificationTurnID(method string, params json.RawMessage) string {
	if method != "turn/completed" {
		return extractString(params, "turnId")
	}
	var notification struct {
		Turn *struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(params, &notification) != nil || notification.Turn == nil {
		return ""
	}
	return notification.Turn.ID
}

func (session *Session) handleServerRequestResolved(params json.RawMessage) {
	var notification struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &notification) != nil || len(notification.RequestID) == 0 {
		return
	}
	key, err := rpcRequestIDKey(notification.RequestID)
	if err != nil {
		return
	}
	var requestID string
	var pending nativeInteraction
	session.mu.Lock()
	for candidateID, candidate := range session.interactions {
		candidateKey, candidateErr := rpcRequestIDKey(candidate.rpcID)
		if candidateErr == nil && candidateKey == key && session.runtime.claimInbound(notification.RequestID) {
			requestID = candidateID
			pending = candidate
			delete(session.interactions, candidateID)
			break
		}
	}
	session.mu.Unlock()
	if requestID == "" {
		return
	}
	session.emit(provider.NewInteractionResolvedEvent(provider.InteractionResolution{
		RequestID: requestID,
		Kind:      pending.request.Kind,
	}))
}

func (session *Session) handleItem(params json.RawMessage, brokerTurnID string, forced provider.ToolStatus) {
	var notification struct {
		Item json.RawMessage `json:"item"`
	}
	if json.Unmarshal(params, &notification) != nil || len(notification.Item) == 0 {
		return
	}
	var item struct {
		ID               string          `json:"id"`
		Type             string          `json:"type"`
		Text             string          `json:"text"`
		Status           string          `json:"status"`
		Command          string          `json:"command"`
		CWD              string          `json:"cwd"`
		AggregatedOutput string          `json:"aggregatedOutput"`
		Changes          json.RawMessage `json:"changes"`
		Arguments        json.RawMessage `json:"arguments"`
		Result           json.RawMessage `json:"result"`
		Error            json.RawMessage `json:"error"`
		Server           string          `json:"server"`
		Tool             string          `json:"tool"`
		Query            string          `json:"query"`
		Path             string          `json:"path"`
	}
	if json.Unmarshal(notification.Item, &item) != nil || item.Type == "" {
		return
	}
	switch item.Type {
	case "agentMessage":
		if forced == "" && brokerTurnID != "" && item.Text != "" {
			session.emit(provider.NewAssistantMessageEvent(brokerTurnID, contentturn.AssistantMessageID(brokerTurnID), bounded(item.Text, provider.MaxEventTextBytes), session.driver.config.Clock.Now().UTC()))
		}
		return
	case "reasoning", "userMessage":
		return
	case "contextCompaction":
		summary := "Codex is compacting the conversation context."
		if forced == "" {
			summary = "Codex completed conversation compaction."
		}
		session.emit(provider.NewActivityEvent(brokerTurnID, provider.ActivityCompaction, summary))
		return
	}
	if item.ID == "" || brokerTurnID == "" {
		return
	}
	activityID, err := session.activityID(item.ID)
	if err != nil {
		session.fail(provider.ErrorProtocolFailure)
		return
	}
	kind, title, summary, detail := normalizeToolItem(item)
	status := forced
	if status == "" {
		status = normalizeToolStatus(item.Status)
	}
	activity := provider.ToolActivity{ID: activityID, TurnID: brokerTurnID, Kind: kind, Status: status, Title: bounded(cleanText(title), provider.MaxTitleBytes), Summary: bounded(cleanText(summary), provider.MaxSummaryBytes), Detail: bounded(cleanText(detail), provider.MaxInteractionTextBytes)}
	if activity.Validate() == nil {
		session.mu.Lock()
		if session.toolStates == nil {
			session.toolStates = make(map[string]provider.ToolActivity)
		}
		session.toolStates[item.ID] = activity
		session.mu.Unlock()
		session.emit(provider.NewToolActivityEvent(activity))
	}
}

func (session *Session) handleToolUpdate(params json.RawMessage, brokerTurnID, field string) {
	var notification struct {
		ItemID  string          `json:"itemId"`
		Delta   string          `json:"delta"`
		Stdin   string          `json:"stdin"`
		Message string          `json:"message"`
		Changes json.RawMessage `json:"changes"`
	}
	if json.Unmarshal(params, &notification) != nil || notification.ItemID == "" {
		return
	}
	var addition string
	switch field {
	case "delta":
		addition = notification.Delta
	case "stdin":
		addition = notification.Stdin
	case "message":
		addition = notification.Message
	case "changes":
		addition = stringify(notification.Changes)
	}
	addition = cleanText(addition)
	if addition == "" {
		return
	}
	session.mu.Lock()
	activity, exists := session.toolStates[notification.ItemID]
	if !exists || (activity.TurnID != "" && activity.TurnID != brokerTurnID) {
		session.mu.Unlock()
		return
	}
	activity.Detail = appendBounded(activity.Detail, addition, provider.MaxInteractionTextBytes)
	session.toolStates[notification.ItemID] = activity
	session.mu.Unlock()
	if activity.Validate() == nil {
		session.emit(provider.NewToolActivityEvent(activity))
	}
}

func normalizeToolItem(item struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Status           string          `json:"status"`
	Command          string          `json:"command"`
	CWD              string          `json:"cwd"`
	AggregatedOutput string          `json:"aggregatedOutput"`
	Changes          json.RawMessage `json:"changes"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Error            json.RawMessage `json:"error"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Query            string          `json:"query"`
	Path             string          `json:"path"`
}) (provider.ToolKind, string, string, string) {
	switch item.Type {
	case "commandExecution":
		detail := item.Command
		if item.CWD != "" {
			detail += "\nWorking directory: " + item.CWD
		}
		if item.AggregatedOutput != "" {
			detail += "\n\n" + item.AggregatedOutput
		}
		return provider.ToolCommand, "Command", item.Command, detail
	case "fileChange":
		return provider.ToolFileChange, "File changes", "Codex updated files.", stringify(item.Changes)
	case "mcpToolCall", "dynamicToolCall":
		name := strings.Trim(strings.Join([]string{item.Server, item.Tool}, "/"), "/")
		detail := stringify(item.Arguments)
		if len(item.Result) != 0 && !isJSONNull(item.Result) {
			detail += "\n\nResult: " + stringify(item.Result)
		}
		if len(item.Error) != 0 && !isJSONNull(item.Error) {
			detail += "\n\nError: " + stringify(item.Error)
		}
		return provider.ToolMCP, "Tool call", name, detail
	case "webSearch":
		return provider.ToolWeb, "Web search", item.Query, item.Query
	case "imageView":
		return provider.ToolImage, "Image view", item.Path, item.Path
	case "collabAgentToolCall", "subAgentActivity":
		return provider.ToolCollaboration, "Collaboration", item.Tool, stringify(item.Arguments)
	case "plan":
		return provider.ToolPlan, "Plan", "Codex updated its plan.", item.Text
	default:
		return provider.ToolOther, "Tool activity", item.Type, stringify(item.Arguments)
	}
}

func normalizeToolStatus(status string) provider.ToolStatus {
	switch status {
	case "completed", "success":
		return provider.ToolCompleted
	case "failed", "errored":
		return provider.ToolFailed
	case "declined", "interrupted", "cancelled":
		return provider.ToolInterrupted
	default:
		return provider.ToolRunning
	}
}

func (session *Session) handleTurnCompleted(params json.RawMessage, brokerTurnID string) {
	var notification struct {
		Turn *struct {
			ID     string          `json:"id"`
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"turn"`
	}
	if brokerTurnID == "" {
		return
	}
	session.mu.Lock()
	turn := session.active
	if turn == nil {
		session.mu.Unlock()
		return
	}
	malformed := json.Unmarshal(params, &notification) != nil || notification.Turn == nil || notification.Turn.ID == "" || notification.Turn.ID != turn.nativeID
	if !malformed {
		switch notification.Turn.Status {
		case "completed", "interrupted":
		case "failed":
			malformed = len(bytes.TrimSpace(notification.Turn.Error)) == 0 || isJSONNull(notification.Turn.Error)
		default:
			malformed = true
		}
	}
	session.active = nil
	session.activities = make(map[string]string)
	session.toolStates = make(map[string]provider.ToolActivity)
	session.mu.Unlock()
	if malformed {
		session.emit(provider.NewTerminalFailureEvent(brokerTurnID, provider.NewProviderError(provider.ErrorMalformedStream)))
		return
	}
	switch notification.Turn.Status {
	case "completed":
		session.emit(provider.NewCompletionEvent(brokerTurnID))
	case "interrupted":
		session.emit(provider.NewInterruptionEvent(brokerTurnID, provider.InterruptionRequested))
	case "failed":
		code := provider.ErrorProtocolFailure
		if bytes.Contains(notification.Turn.Error, []byte("contextWindowExceeded")) {
			code = provider.ErrorContextTooLarge
		}
		session.emit(provider.NewTerminalFailureEvent(brokerTurnID, provider.NewProviderError(code)))
	default:
		session.emit(provider.NewTerminalFailureEvent(brokerTurnID, provider.NewProviderError(provider.ErrorProtocolFailure)))
	}
}

func (session *Session) activityID(nativeID string) (string, error) {
	session.mu.Lock()
	if existing := session.activities[nativeID]; existing != "" {
		session.mu.Unlock()
		return existing, nil
	}
	if len(session.activities) >= maxTrackedToolItems {
		session.mu.Unlock()
		return "", errors.New("too many Codex tool items")
	}
	session.mu.Unlock()
	id, err := session.driver.newID()
	if err != nil {
		return "", err
	}
	session.mu.Lock()
	if existing := session.activities[nativeID]; existing != "" {
		session.mu.Unlock()
		return existing, nil
	}
	if len(session.activities) >= maxTrackedToolItems {
		session.mu.Unlock()
		return "", errors.New("too many Codex tool items")
	}
	session.activities[nativeID] = id
	session.mu.Unlock()
	return id, nil
}

func (session *Session) handleServerRequest(rpcID json.RawMessage, method string, params json.RawMessage) error {
	nativeTurnID := extractString(params, "turnId")
	session.mu.Lock()
	turn := session.active
	if turn != nil && nativeTurnID != "" && turn.nativeID == "" {
		size := len(rpcID) + len(method) + len(params)
		if len(turn.buffered) >= provider.MaxLaunchItems || turn.bytes+size > maxJSONLMessageBytes {
			session.mu.Unlock()
			return errors.New("buffered interaction exceeds supported bounds")
		}
		turn.buffered = append(turn.buffered, bufferedNativeEvent{rpcID: bytes.Clone(rpcID), method: method, params: bytes.Clone(params)})
		turn.bytes += size
		session.mu.Unlock()
		return nil
	}
	session.mu.Unlock()
	requestID, err := session.driver.newID()
	if err != nil {
		return err
	}
	request, pending, err := session.normalizeInteraction(requestID, method, params)
	if err != nil {
		return err
	}
	session.mu.Lock()
	if session.closed || len(session.interactions) >= provider.MaxInteractionAnswers || session.interactions[requestID].method != "" {
		session.mu.Unlock()
		return errors.New("interaction unavailable")
	}
	pending.rpcID = bytes.Clone(rpcID)
	pending.method = method
	pending.params = bytes.Clone(params)
	session.interactions[requestID] = pending
	session.mu.Unlock()
	session.emit(provider.NewInteractionRequestEvent(request))
	return nil
}

func (session *Session) normalizeInteraction(requestID, method string, params json.RawMessage) (provider.InteractionRequest, nativeInteraction, error) {
	if err := validateInteractionParams(method, params); err != nil {
		return provider.InteractionRequest{}, nativeInteraction{}, err
	}
	nativeTurnID := extractString(params, "turnId")
	session.mu.Lock()
	turn := session.active
	brokerTurnID := ""
	if turn != nil && (nativeTurnID == "" || turn.nativeID == "" || turn.nativeID == nativeTurnID) {
		brokerTurnID = turn.request.TurnID
	}
	session.mu.Unlock()
	if nativeTurnID != "" && brokerTurnID == "" && method != "mcpServer/elicitation/request" {
		return provider.InteractionRequest{}, nativeInteraction{}, errors.New("interaction turn unavailable")
	}
	var base struct {
		Command         string          `json:"command"`
		CWD             string          `json:"cwd"`
		Reason          string          `json:"reason"`
		Questions       json.RawMessage `json:"questions"`
		Permissions     json.RawMessage `json:"permissions"`
		Message         string          `json:"message"`
		Mode            string          `json:"mode"`
		URL             string          `json:"url"`
		RequestedSchema json.RawMessage `json:"requestedSchema"`
		ServerName      string          `json:"serverName"`
	}
	if json.Unmarshal(params, &base) != nil {
		return provider.InteractionRequest{}, nativeInteraction{}, errors.New("invalid interaction params")
	}
	pending := nativeInteraction{method: method, params: bytes.Clone(params), responseKey: make(map[string]string), choices: make(map[string]map[string]string), fieldTypes: make(map[string]provider.InteractionFieldType)}
	request := provider.InteractionRequest{ID: requestID, TurnID: brokerTurnID, Summary: bounded(base.Reason, provider.MaxSummaryBytes), Command: bounded(base.Command, provider.MaxInteractionTextBytes), WorkingDirectory: bounded(base.CWD, provider.MaxURLBytes)}
	switch method {
	case "item/commandExecution/requestApproval":
		request.Kind, request.Title = provider.InteractionCommandApproval, "Approve command"
		if request.Summary == "" {
			request.Summary = "Codex wants to run a command."
		}
		request.Options = approvalOptions()
	case "item/fileChange/requestApproval":
		request.Kind, request.Title = provider.InteractionFileApproval, "Approve file changes"
		if request.Summary == "" {
			request.Summary = "Codex wants to change files."
		}
		request.Options = approvalOptions()
	case "item/permissions/requestApproval":
		request.Kind, request.Title = provider.InteractionPermissionApproval, "Approve permissions"
		request.Summary = bounded(strings.TrimSpace(base.Reason+"\n"+stringify(base.Permissions)), provider.MaxSummaryBytes)
		if request.Summary == "" {
			request.Summary = "Codex requested additional permissions."
		}
		request.Options = []provider.InteractionOption{
			{ID: "grantTurn", Label: "Allow for turn", Description: "Grant the requested permissions for this turn."},
			{ID: "grantSession", Label: "Allow for session", Description: "Grant the requested permissions for this session."},
			{ID: "decline", Label: "Decline", Description: "Continue without granting them."},
		}
		field, permissionChoices, globMaxDepth, parseErr := parsePermissionChoices(base.Permissions)
		if parseErr != nil {
			return provider.InteractionRequest{}, nativeInteraction{}, parseErr
		}
		request.Fields = []provider.InteractionField{field}
		pending.permissionChoices = permissionChoices
		pending.permissionGlobMaxDepth = globMaxDepth
	case "item/tool/requestUserInput":
		request.Kind, request.Title, request.Summary = provider.InteractionUserInput, "Codex needs input", "Answer the questions to continue."
		var native []struct {
			ID       string `json:"id"`
			Header   string `json:"header"`
			Question string `json:"question"`
			IsOther  bool   `json:"isOther"`
			IsSecret bool   `json:"isSecret"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		}
		if json.Unmarshal(base.Questions, &native) != nil {
			return provider.InteractionRequest{}, nativeInteraction{}, errors.New("invalid questions")
		}
		nativeKeys := make(map[string]struct{}, len(native))
		for questionIndex, question := range native {
			if _, duplicate := nativeKeys[question.ID]; duplicate {
				return provider.InteractionRequest{}, nativeInteraction{}, errors.New("duplicate native question id")
			}
			nativeKeys[question.ID] = struct{}{}
			questionID := "question" + strconv.Itoa(questionIndex)
			pending.responseKey[questionID] = question.ID
			pending.choices[questionID] = make(map[string]string, len(question.Options))
			normalized := provider.InteractionQuestion{ID: questionID, Header: question.Header, Prompt: question.Question, AllowOther: question.IsOther, Secret: question.IsSecret}
			for index, option := range question.Options {
				optionID := interactionOptionID(index)
				pending.choices[questionID][optionID] = option.Label
				normalized.Options = append(normalized.Options, provider.InteractionOption{ID: optionID, Label: option.Label, Description: option.Description})
			}
			request.Questions = append(request.Questions, normalized)
		}
	case "mcpServer/elicitation/request":
		request.Kind, request.Title = provider.InteractionMCPElicitation, "MCP server needs input"
		request.Summary = bounded(strings.TrimSpace(base.ServerName+": "+base.Message+" "+base.URL), provider.MaxSummaryBytes)
		request.Options = []provider.InteractionOption{{ID: "accept", Label: "Accept", Description: "Provide the requested input."}, {ID: "decline", Label: "Decline", Description: "Decline this request."}, {ID: "cancel", Label: "Cancel", Description: "Cancel this request."}}
		if base.Mode == "form" {
			var parseErr error
			request.Fields, pending.responseKey, pending.choices, pending.fieldTypes, parseErr = parseMCPFields(base.RequestedSchema)
			if parseErr != nil {
				return provider.InteractionRequest{}, nativeInteraction{}, parseErr
			}
		} else if base.Mode != "url" {
			return provider.InteractionRequest{}, nativeInteraction{}, errors.New("unsupported MCP elicitation mode")
		}
	default:
		return provider.InteractionRequest{}, nativeInteraction{}, errors.New("unsupported interaction method")
	}
	if request.Validate() != nil {
		return provider.InteractionRequest{}, nativeInteraction{}, errors.New("invalid normalized interaction")
	}
	pending.request = request
	return request, pending, nil
}

func validateInteractionParams(method string, params json.RawMessage) error {
	validID := func(value string) bool { return value != "" && len(value) <= provider.MaxNativeReferenceBytes }
	type approval struct {
		ThreadID  string           `json:"threadId"`
		TurnID    string           `json:"turnId"`
		ItemID    string           `json:"itemId"`
		StartedAt *json.RawMessage `json:"startedAtMs"`
		CWD       *json.RawMessage `json:"cwd"`
		Payload   *json.RawMessage `json:"permissions"`
		Questions *json.RawMessage `json:"questions"`
		Server    string           `json:"serverName"`
		Mode      string           `json:"mode"`
		Message   *string          `json:"message"`
		Schema    *json.RawMessage `json:"requestedSchema"`
		URL       *string          `json:"url"`
		ElicitID  *string          `json:"elicitationId"`
	}
	var value approval
	if json.Unmarshal(params, &value) != nil || !validID(value.ThreadID) {
		return errors.New("invalid interaction params")
	}
	requireTurnItem := func() error {
		if !validID(value.TurnID) || !validID(value.ItemID) || value.StartedAt == nil {
			return errors.New("missing interaction identity")
		}
		var timestamp int64
		if json.Unmarshal(*value.StartedAt, &timestamp) != nil {
			return errors.New("invalid interaction timestamp")
		}
		return nil
	}
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return requireTurnItem()
	case "item/permissions/requestApproval":
		if err := requireTurnItem(); err != nil || value.CWD == nil || value.Payload == nil || !isJSONObject(*value.Payload) {
			return errors.New("invalid permission approval params")
		}
	case "item/tool/requestUserInput":
		if !validID(value.TurnID) || !validID(value.ItemID) || value.Questions == nil {
			return errors.New("invalid user input params")
		}
		var questions []json.RawMessage
		if json.Unmarshal(*value.Questions, &questions) != nil || len(questions) == 0 || len(questions) > provider.MaxInteractionQuestions {
			return errors.New("invalid user input questions")
		}
	case "mcpServer/elicitation/request":
		if value.Server == "" || len(value.Server) > provider.MaxTitleBytes || value.Message == nil {
			return errors.New("invalid MCP elicitation params")
		}
		switch value.Mode {
		case "form":
			if value.Schema == nil || !isJSONObject(*value.Schema) {
				return errors.New("invalid MCP form schema")
			}
		case "url":
			if value.URL == nil || *value.URL == "" || value.ElicitID == nil || *value.ElicitID == "" {
				return errors.New("invalid MCP URL elicitation")
			}
		default:
			return errors.New("unsupported MCP elicitation mode")
		}
	default:
		return errors.New("unsupported interaction method")
	}
	return nil
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func approvalOptions() []provider.InteractionOption {
	return []provider.InteractionOption{
		{ID: "accept", Label: "Allow once", Description: "Approve this action."},
		{ID: "acceptForSession", Label: "Allow for session", Description: "Approve this and matching actions for the session."},
		{ID: "decline", Label: "Decline", Description: "Continue without this action."},
		{ID: "cancel", Label: "Cancel turn", Description: "Decline and interrupt the turn."},
	}
}

func parsePermissionChoices(raw json.RawMessage) (provider.InteractionField, map[string]nativePermissionChoice, *uint64, error) {
	var profile struct {
		Network *struct {
			Enabled *bool `json:"enabled"`
		} `json:"network"`
		FileSystem *struct {
			Entries          []json.RawMessage `json:"entries"`
			Read             []string          `json:"read"`
			Write            []string          `json:"write"`
			GlobScanMaxDepth *uint64           `json:"globScanMaxDepth"`
		} `json:"fileSystem"`
	}
	if json.Unmarshal(raw, &profile) != nil {
		return provider.InteractionField{}, nil, nil, errors.New("invalid permission profile")
	}
	field := provider.InteractionField{
		ID: "permissions", Label: "Permissions to grant", Description: "Select only the permissions Codex may use.", Type: provider.InteractionMultiSelect,
	}
	choices := make(map[string]nativePermissionChoice)
	add := func(label, description, kind string, value json.RawMessage) error {
		if len(field.Options) >= provider.MaxInteractionOptions || len(value) == 0 {
			return errors.New("permission profile exceeds supported bounds")
		}
		id := "permission" + strconv.Itoa(len(field.Options))
		field.Options = append(field.Options, provider.InteractionOption{
			ID: id, Label: bounded(cleanText(label), provider.MaxTitleBytes), Description: bounded(cleanText(description), provider.MaxSummaryBytes),
		})
		choices[id] = nativePermissionChoice{kind: kind, value: bytes.Clone(value)}
		return nil
	}
	if profile.Network != nil && profile.Network.Enabled != nil && *profile.Network.Enabled {
		if err := add("Network access", "Allow network access requested by Codex.", "network", json.RawMessage(`{"enabled":true}`)); err != nil {
			return provider.InteractionField{}, nil, nil, err
		}
	}
	if profile.FileSystem != nil {
		for _, entry := range profile.FileSystem.Entries {
			var value struct {
				Access string          `json:"access"`
				Path   json.RawMessage `json:"path"`
			}
			if json.Unmarshal(entry, &value) != nil || (value.Access != "read" && value.Access != "write" && value.Access != "deny") || !isJSONObject(value.Path) {
				return provider.InteractionField{}, nil, nil, errors.New("invalid filesystem permission entry")
			}
			if err := add("Filesystem "+value.Access, stringify(value.Path), "entry", entry); err != nil {
				return provider.InteractionField{}, nil, nil, err
			}
		}
		for _, path := range profile.FileSystem.Read {
			encoded, _ := json.Marshal(path)
			if path == "" || add("Read "+path, "Allow filesystem reads from this path.", "read", encoded) != nil {
				return provider.InteractionField{}, nil, nil, errors.New("invalid filesystem read permission")
			}
		}
		for _, path := range profile.FileSystem.Write {
			encoded, _ := json.Marshal(path)
			if path == "" || add("Write "+path, "Allow filesystem writes to this path.", "write", encoded) != nil {
				return provider.InteractionField{}, nil, nil, errors.New("invalid filesystem write permission")
			}
		}
	}
	if len(field.Options) == 0 {
		return provider.InteractionField{}, nil, nil, errors.New("permission request has no supported permission")
	}
	var globMaxDepth *uint64
	if profile.FileSystem != nil && profile.FileSystem.GlobScanMaxDepth != nil {
		value := *profile.FileSystem.GlobScanMaxDepth
		if value == 0 {
			return provider.InteractionField{}, nil, nil, errors.New("invalid filesystem glob depth")
		}
		globMaxDepth = &value
	}
	return field, choices, globMaxDepth, nil
}

func parseMCPFields(schema json.RawMessage) ([]provider.InteractionField, map[string]string, map[string]map[string]string, map[string]provider.InteractionFieldType, error) {
	var object struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if json.Unmarshal(schema, &object) != nil || object.Type != "object" || len(object.Properties) == 0 || len(object.Properties) > provider.MaxInteractionAnswers {
		return nil, nil, nil, nil, errors.New("invalid MCP form schema")
	}
	required := make(map[string]bool, len(object.Required))
	for _, key := range object.Required {
		required[key] = true
	}
	keys := make([]string, 0, len(object.Properties))
	for key := range object.Properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fields := make([]provider.InteractionField, 0, len(keys))
	responseKeys := make(map[string]string, len(keys))
	choices := make(map[string]map[string]string, len(keys))
	fieldTypes := make(map[string]provider.InteractionFieldType, len(keys))
	for fieldIndex, key := range keys {
		raw := object.Properties[key]
		var value struct {
			Type        string   `json:"type"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
			EnumNames   []string `json:"enumNames"`
			OneOf       []struct {
				Const string `json:"const"`
				Title string `json:"title"`
			} `json:"oneOf"`
			Items struct {
				Enum  []string `json:"enum"`
				OneOf []struct {
					Const string `json:"const"`
					Title string `json:"title"`
				} `json:"oneOf"`
			} `json:"items"`
		}
		if json.Unmarshal(raw, &value) != nil {
			return nil, nil, nil, nil, errors.New("invalid MCP field schema")
		}
		fieldID := "field" + strconv.Itoa(fieldIndex)
		field := provider.InteractionField{ID: fieldID, Label: value.Title, Description: value.Description, Required: required[key]}
		responseKeys[fieldID] = key
		choices[fieldID] = make(map[string]string)
		if field.Label == "" {
			field.Label = key
		}
		switch value.Type {
		case "boolean":
			field.Type = provider.InteractionBoolean
		case "number", "integer":
			field.Type = provider.InteractionNumber
		case "array":
			field.Type = provider.InteractionMultiSelect
			addMCPOptions(&field, choices[fieldID], value.Items.Enum, nil, value.Items.OneOf)
		default:
			field.Type = provider.InteractionText
			if len(value.Enum) != 0 || len(value.OneOf) != 0 {
				field.Type = provider.InteractionSelect
				addMCPOptions(&field, choices[fieldID], value.Enum, value.EnumNames, value.OneOf)
			}
		}
		if (field.Type == provider.InteractionSelect || field.Type == provider.InteractionMultiSelect) && len(field.Options) == 0 {
			return nil, nil, nil, nil, errors.New("MCP selection field has no options")
		}
		fieldTypes[fieldID] = field.Type
		fields = append(fields, field)
	}
	return fields, responseKeys, choices, fieldTypes, nil
}

func addMCPOptions(field *provider.InteractionField, choices map[string]string, values, names []string, titled []struct {
	Const string `json:"const"`
	Title string `json:"title"`
}) {
	for index, value := range values {
		label := value
		if index < len(names) && names[index] != "" {
			label = names[index]
		}
		optionID := interactionOptionID(index)
		choices[optionID] = value
		field.Options = append(field.Options, provider.InteractionOption{ID: optionID, Label: label})
	}
	for _, option := range titled {
		optionID := interactionOptionID(len(field.Options))
		choices[optionID] = option.Const
		field.Options = append(field.Options, provider.InteractionOption{ID: optionID, Label: option.Title})
	}
}

func interactionRPCResult(pending nativeInteraction, response provider.InteractionResponse) (any, error) {
	switch pending.method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return map[string]any{"decision": response.OptionID}, nil
	case "item/permissions/requestApproval":
		scope := "turn"
		if response.OptionID == "grantTurn" || response.OptionID == "grantSession" {
			if response.OptionID == "grantSession" {
				scope = "session"
			}
			permissions, err := selectedPermissions(pending, response.Answers["permissions"])
			if err != nil {
				return nil, err
			}
			return map[string]any{"permissions": permissions, "scope": scope}, nil
		}
		if response.OptionID != "decline" {
			return nil, errors.New("invalid permission decision")
		}
		return map[string]any{"permissions": map[string]any{}, "scope": scope}, nil
	case "item/tool/requestUserInput":
		answers := make(map[string]any, len(response.Answers))
		for key, values := range response.Answers {
			nativeKey := pending.responseKey[key]
			if nativeKey == "" {
				return nil, errors.New("invalid question")
			}
			nativeValues := make([]string, len(values))
			for index, value := range values {
				nativeValues[index] = translateChoice(pending, key, value)
			}
			answers[nativeKey] = map[string]any{"answers": nativeValues}
		}
		return map[string]any{"answers": answers}, nil
	case "mcpServer/elicitation/request":
		result := map[string]any{"action": response.OptionID, "content": nil}
		if response.OptionID == "accept" {
			content := make(map[string]any, len(response.Answers))
			for key, values := range response.Answers {
				nativeKey := pending.responseKey[key]
				if nativeKey == "" {
					return nil, errors.New("invalid elicitation field")
				}
				nativeValues := make([]string, len(values))
				for index, value := range values {
					nativeValues[index] = translateChoice(pending, key, value)
				}
				if pending.fieldTypes[key] == provider.InteractionMultiSelect {
					content[nativeKey] = nativeValues
				} else {
					if len(nativeValues) != 1 {
						return nil, errors.New("scalar elicitation field requires exactly one answer")
					}
					coerced, err := coerceInteractionValue(pending.fieldTypes[key], nativeValues[0])
					if err != nil {
						return nil, err
					}
					content[nativeKey] = coerced
				}
			}
			result["content"] = content
		}
		return result, nil
	default:
		return nil, errors.New("unsupported interaction")
	}
}

func selectedPermissions(pending nativeInteraction, selected []string) (map[string]any, error) {
	if len(selected) == 0 {
		return nil, errors.New("permission grant selected no permissions")
	}
	permissions := make(map[string]any)
	fileSystem := make(map[string]any)
	entries := make([]json.RawMessage, 0, len(selected))
	reads := make([]string, 0, len(selected))
	writes := make([]string, 0, len(selected))
	for _, id := range selected {
		choice, ok := pending.permissionChoices[id]
		if !ok {
			return nil, errors.New("invalid permission selection")
		}
		switch choice.kind {
		case "network":
			permissions["network"] = choice.value
		case "entry":
			entries = append(entries, choice.value)
		case "read", "write":
			var path string
			if json.Unmarshal(choice.value, &path) != nil || path == "" {
				return nil, errors.New("invalid permission path")
			}
			if choice.kind == "read" {
				reads = append(reads, path)
			} else {
				writes = append(writes, path)
			}
		default:
			return nil, errors.New("invalid permission selection")
		}
	}
	if len(entries) != 0 {
		fileSystem["entries"] = entries
	}
	if len(reads) != 0 {
		fileSystem["read"] = reads
	}
	if len(writes) != 0 {
		fileSystem["write"] = writes
	}
	if len(fileSystem) != 0 {
		if pending.permissionGlobMaxDepth != nil {
			fileSystem["globScanMaxDepth"] = *pending.permissionGlobMaxDepth
		}
		permissions["fileSystem"] = fileSystem
	}
	if len(permissions) == 0 {
		return nil, errors.New("permission grant selected no permissions")
	}
	return permissions, nil
}

func translateChoice(pending nativeInteraction, key, value string) string {
	if translated := pending.choices[key][value]; translated != "" {
		return translated
	}
	return value
}

func coerceInteractionValue(fieldType provider.InteractionFieldType, value string) (any, error) {
	switch fieldType {
	case provider.InteractionNumber:
		number, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, errors.New("invalid finite interaction number")
		}
		return number, nil
	case provider.InteractionBoolean:
		return strconv.ParseBool(value)
	default:
		return value, nil
	}
}

func cancellationRPCResult(method string) any {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return map[string]any{"decision": "cancel"}
	case "item/permissions/requestApproval":
		return map[string]any{"permissions": map[string]any{}, "scope": "turn"}
	case "item/tool/requestUserInput":
		return map[string]any{"answers": map[string]any{}}
	case "mcpServer/elicitation/request":
		return map[string]any{"action": "cancel", "content": nil}
	default:
		return map[string]any{}
	}
}

func splitText(text string, limit int) []string {
	if text == "" {
		return nil
	}
	result := make([]string, 0, (len(text)+limit-1)/limit)
	for len(text) > limit {
		end := limit
		for end > 0 && !utf8.ValidString(text[:end]) {
			end--
		}
		if end == 0 {
			break
		}
		result = append(result, text[:end])
		text = text[end:]
	}
	if text != "" {
		result = append(result, text)
	}
	return result
}

func cleanText(value string) string {
	return strings.Map(func(char rune) rune {
		if char < 0x20 && char != '\t' && char != '\n' && char != '\r' {
			return -1
		}
		return char
	}, value)
}

func appendBounded(existing, addition string, limit int) string {
	combined := existing
	if combined != "" && addition != "" {
		combined += "\n"
	}
	combined += addition
	if len(combined) <= limit {
		return combined
	}
	start := len(combined) - limit
	for start < len(combined) && !utf8.ValidString(combined[start:]) {
		start++
	}
	return combined[start:]
}

func interactionOptionID(index int) string { return "option" + strconv.Itoa(index) }
