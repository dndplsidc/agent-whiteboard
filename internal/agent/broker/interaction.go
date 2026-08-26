package broker

import (
	"context"
	"errors"
	"math"
	"strconv"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

const maxPendingInteractions = provider.MaxInteractionAnswers

type pendingInteraction struct {
	request      provider.InteractionRequest
	requestBytes int
	token        uint64
	resolving    bool
	response     *provider.InteractionResponse
	commandID    string
	clientID     string
	cancel       context.CancelFunc
}

type interactionWorkerResult struct {
	requestID string
	token     uint64
	commandID string
	clientID  string
	response  provider.InteractionResponse
	err       error
	automatic bool
}

func (actor *conversation) acquireInteractionCall() bool {
	if actor.interactionCallBudget == nil {
		actor.interactionCallBudget = make(chan struct{}, maxPendingInteractions)
	}
	select {
	case actor.interactionCallBudget <- struct{}{}:
		return true
	default:
		return false
	}
}

func (actor *conversation) releaseInteractionCall() { <-actor.interactionCallBudget }

func (actor *conversation) commandInteractionRespond(attachments map[*clientAttachment]struct{}, results chan<- interactionWorkerResult, command protocol.Command, payload protocol.InteractionResponsePayload) protocol.BrowserErrorCode {
	pending := actor.pendingInteractions[payload.RequestID]
	interactive, ok := actor.session.session.(provider.InteractiveSession)
	if pending == nil || pending.resolving || !ok || protocol.InteractionKind(pending.request.Kind) != payload.Kind || !validInteractionResponseForRequest(pending.request, payload) {
		return protocol.ErrorInvalidState
	}
	response := provider.InteractionResponse{RequestID: payload.RequestID, Kind: pending.request.Kind, OptionID: payload.OptionID, Answers: cloneInteractionAnswers(payload.Answers)}
	if response.Validate() != nil {
		return protocol.ErrorInvalidCommand
	}
	if !actor.acquireInteractionCall() {
		actor.retireInteraction(attachments, payload.RequestID, pending.token, response.OptionID, true, "")
		return protocol.ErrorInvalidState
	}
	pending.resolving = true
	token := pending.token
	copyOfResponse := response
	pending.response = &copyOfResponse
	pending.commandID = command.CommandID
	pending.clientID = command.ClientID
	responseCtx, cancel := context.WithCancel(actor.lifecycleCtx)
	pending.cancel = cancel
	go func() {
		defer cancel()
		defer actor.releaseInteractionCall()
		err := interactive.Respond(responseCtx, response)
		result := interactionWorkerResult{requestID: payload.RequestID, token: token, commandID: command.CommandID, clientID: command.ClientID, response: response, err: err}
		select {
		case results <- result:
		case <-actor.done:
		}
	}()
	return ""
}

func (actor *conversation) handleInteractionResult(attachments map[*clientAttachment]struct{}, result interactionWorkerResult) {
	pending := actor.pendingInteractions[result.requestID]
	if pending == nil || pending.token != result.token || !pending.resolving {
		return
	}
	if result.err != nil {
		// InteractiveSession owns exactly-once native delivery and consumes a
		// validated response before attempting the App Server write. A write
		// failure is therefore an unknown native outcome, not permission to let
		// another tab choose a second answer.
		actor.forgetInteraction(result.requestID)
		actor.publishShared(attachments, protocol.InteractionResolvedPayload{
			RequestID: result.requestID,
			Kind:      protocol.InteractionKind(pending.request.Kind),
			OptionID:  result.response.OptionID,
		})
		if !result.automatic && result.commandID != "" {
			actor.completePendingCommand(attachments, result.commandID, result.clientID, MapError(result.err).Code())
		}
		return
	}
	actor.forgetInteraction(result.requestID)
	actor.publishShared(attachments, protocol.InteractionResolvedPayload{RequestID: result.requestID, Kind: protocol.InteractionKind(pending.request.Kind), OptionID: result.response.OptionID})
	if !result.automatic && result.commandID != "" {
		actor.completePendingCommand(attachments, result.commandID, result.clientID, "")
	}
}

func (actor *conversation) cancelPendingInteractions(attachments map[*clientAttachment]struct{}, results chan<- interactionWorkerResult) {
	interactive, ok := actor.session.session.(provider.InteractiveSession)
	if !ok {
		return
	}
	for requestID, pending := range actor.pendingInteractions {
		if pending.resolving {
			continue
		}
		if !actor.acquireInteractionCall() {
			actor.retireInteraction(attachments, requestID, pending.token, "", true, "")
			continue
		}
		pending.resolving = true
		token := pending.token
		ctx, cancel := context.WithTimeout(actor.lifecycleCtx, actor.shutdownTimeout)
		pending.cancel = cancel
		go func(id string, token uint64, ctx context.Context, cancel context.CancelFunc) {
			defer cancel()
			defer actor.releaseInteractionCall()
			err := interactive.CancelInteraction(ctx, id)
			select {
			case results <- interactionWorkerResult{requestID: id, token: token, automatic: true, err: err}:
			case <-actor.done:
			}
		}(requestID, token, ctx, cancel)
	}
}

func (actor *conversation) retireInteraction(attachments map[*clientAttachment]struct{}, requestID string, token uint64, optionID string, publishResolution bool, commandCode protocol.BrowserErrorCode) bool {
	pending := actor.pendingInteractions[requestID]
	if pending == nil || pending.token != token {
		return false
	}
	if pending.cancel != nil {
		pending.cancel()
	}
	if publishResolution {
		actor.publishShared(attachments, protocol.InteractionResolvedPayload{
			RequestID: requestID,
			Kind:      protocol.InteractionKind(pending.request.Kind),
			OptionID:  optionID,
		})
	}
	if pending.commandID != "" {
		actor.completePendingCommand(attachments, pending.commandID, pending.clientID, commandCode)
	}
	actor.forgetInteraction(requestID)
	return true
}

// expireTerminatedSessionInteractions closes every response surface owned by
// the terminated session and retires in-flight workers without waiting for
// provider code that may ignore cancellation.
func (actor *conversation) expireTerminatedSessionInteractions(attachments map[*clientAttachment]struct{}) {
	for requestID, pending := range actor.pendingInteractions {
		optionID := ""
		if pending.response != nil {
			optionID = pending.response.OptionID
		}
		actor.retireInteraction(attachments, requestID, pending.token, optionID, true, protocol.ErrorProviderCrashed)
	}
}

func (actor *conversation) expirePendingInteractions(attachments map[*clientAttachment]struct{}) {
	for requestID, pending := range actor.pendingInteractions {
		optionID := ""
		if pending.response != nil {
			optionID = pending.response.OptionID
		}
		actor.retireInteraction(attachments, requestID, pending.token, optionID, true, protocol.ErrorInvalidState)
	}
}

func validInteractionResponseForRequest(request provider.InteractionRequest, response protocol.InteractionResponsePayload) bool {
	if request.ID != response.RequestID || protocol.InteractionKind(request.Kind) != response.Kind || !validInteractionOption(request.Options, response.OptionID) {
		return false
	}
	switch request.Kind {
	case provider.InteractionCommandApproval, provider.InteractionFileApproval:
		return response.OptionID != "" && len(response.Answers) == 0
	case provider.InteractionPermissionApproval:
		switch response.OptionID {
		case "grantTurn", "grantSession":
			selected := response.Answers["permissions"]
			return len(response.Answers) == 1 && len(selected) != 0 && validInteractionAnswers(request, response.Answers, false)
		case "decline":
			return len(response.Answers) == 0
		default:
			return false
		}
	case provider.InteractionUserInput:
		return response.OptionID == "" && len(response.Answers) != 0 && validInteractionAnswers(request, response.Answers, false)
	case provider.InteractionMCPElicitation:
		switch response.OptionID {
		case "accept":
			return validInteractionAnswers(request, response.Answers, true)
		case "decline", "cancel":
			return len(response.Answers) == 0
		default:
			return false
		}
	default:
		return false
	}
}

func validInteractionOption(options []provider.InteractionOption, selected string) bool {
	if len(options) == 0 {
		return selected == ""
	}
	if selected == "" {
		return false
	}
	return interactionOptionOffered(options, selected)
}

func validInteractionAnswers(request provider.InteractionRequest, answers map[string][]string, enforceRequired bool) bool {
	type answerSurface struct {
		question *provider.InteractionQuestion
		field    *provider.InteractionField
	}
	allowed := make(map[string]answerSurface, len(request.Questions)+len(request.Fields))
	for index := range request.Questions {
		question := &request.Questions[index]
		allowed[question.ID] = answerSurface{question: question}
		if len(answers[question.ID]) == 0 {
			return false
		}
	}
	for index := range request.Fields {
		field := &request.Fields[index]
		allowed[field.ID] = answerSurface{field: field}
		if enforceRequired && field.Required && len(answers[field.ID]) == 0 {
			return false
		}
	}
	for key, values := range answers {
		surface, ok := allowed[key]
		if !ok || len(values) == 0 || hasDuplicateInteractionAnswer(values) {
			return false
		}
		if surface.question != nil {
			if !validQuestionAnswers(*surface.question, values) {
				return false
			}
			continue
		}
		if !validFieldAnswers(*surface.field, values) {
			return false
		}
	}
	return true
}

func validQuestionAnswers(question provider.InteractionQuestion, values []string) bool {
	if !question.Multiple && len(values) != 1 {
		return false
	}
	for _, value := range values {
		if value == "" || (!interactionOptionOffered(question.Options, value) && len(question.Options) != 0 && !question.AllowOther) {
			return false
		}
	}
	return true
}

func validFieldAnswers(field provider.InteractionField, values []string) bool {
	for _, value := range values {
		if value == "" {
			return false
		}
	}
	switch field.Type {
	case provider.InteractionText:
		return len(values) == 1
	case provider.InteractionNumber:
		return len(values) == 1 && canonicalFiniteNumber(values[0])
	case provider.InteractionBoolean:
		return len(values) == 1 && (values[0] == "true" || values[0] == "false")
	case provider.InteractionSelect:
		return len(values) == 1 && interactionOptionOffered(field.Options, values[0])
	case provider.InteractionMultiSelect:
		for _, value := range values {
			if !interactionOptionOffered(field.Options, value) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func interactionOptionOffered(options []provider.InteractionOption, selected string) bool {
	for _, option := range options {
		if option.ID == selected {
			return true
		}
	}
	return false
}

func hasDuplicateInteractionAnswer(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func canonicalFiniteNumber(value string) bool {
	number, err := strconv.ParseFloat(value, 64)
	return err == nil && !math.IsInf(number, 0) && !math.IsNaN(number) && strconv.FormatFloat(number, 'g', -1, 64) == value
}

func cloneInteractionAnswers(answers map[string][]string) map[string][]string {
	if answers == nil {
		return nil
	}
	cloned := make(map[string][]string, len(answers))
	for key, values := range answers {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func (actor *conversation) rememberInteraction(request provider.InteractionRequest) error {
	if request.Validate() != nil {
		return errors.New("invalid interaction request")
	}
	if _, duplicate := actor.pendingInteractions[request.ID]; duplicate {
		return errors.New("duplicate interaction request")
	}
	requestBytes := interactionRequestSize(request) + 64
	if len(actor.pendingInteractions) >= maxPendingInteractions || requestBytes > MaxReplayBytes-actor.pendingInteractionBytes {
		return errors.New("pending interactions exceed replay limit")
	}
	if actor.nextInteractionToken == ^uint64(0) {
		return errors.New("interaction ownership exhausted")
	}
	actor.nextInteractionToken++
	copyOfRequest := cloneInteractionRequest(request)
	actor.pendingInteractions[request.ID] = &pendingInteraction{request: copyOfRequest, requestBytes: requestBytes, token: actor.nextInteractionToken}
	actor.pendingInteractionBytes += requestBytes
	return nil
}

func (actor *conversation) forgetInteraction(requestID string) *pendingInteraction {
	pending := actor.pendingInteractions[requestID]
	if pending == nil {
		return nil
	}
	delete(actor.pendingInteractions, requestID)
	actor.pendingInteractionBytes -= pending.requestBytes
	if pending.cancel != nil {
		pending.cancel()
		pending.cancel = nil
	}
	return pending
}

func cloneInteractionRequest(request provider.InteractionRequest) provider.InteractionRequest {
	request.Options = append([]provider.InteractionOption(nil), request.Options...)
	request.Questions = append([]provider.InteractionQuestion(nil), request.Questions...)
	for index := range request.Questions {
		request.Questions[index].Options = append([]provider.InteractionOption(nil), request.Questions[index].Options...)
	}
	request.Fields = append([]provider.InteractionField(nil), request.Fields...)
	for index := range request.Fields {
		request.Fields[index].Options = append([]provider.InteractionOption(nil), request.Fields[index].Options...)
	}
	return request
}

func interactionRequestSize(request provider.InteractionRequest) int {
	size := len(request.ID) + len(request.TurnID) + len(request.Kind) + len(request.Title) + len(request.Summary) + len(request.Command) + len(request.WorkingDirectory)
	for _, option := range request.Options {
		size += len(option.ID) + len(option.Label) + len(option.Description) + 16
	}
	for _, question := range request.Questions {
		size += len(question.ID) + len(question.Header) + len(question.Prompt) + 32
		for _, option := range question.Options {
			size += len(option.ID) + len(option.Label) + len(option.Description) + 16
		}
	}
	for _, field := range request.Fields {
		size += len(field.ID) + len(field.Label) + len(field.Description) + len(field.Type) + 32
		for _, option := range field.Options {
			size += len(option.ID) + len(option.Label) + len(option.Description) + 16
		}
	}
	return size
}
