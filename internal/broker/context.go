package broker

import (
	"reflect"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
)

func (actor *conversation) observeAttach(resource agentprotocol.Resource, digest string) (agentprotocol.ContextState, bool, agentprotocol.Event, error) {
	if validateProtocolResource(resource) != nil || !validDigest(digest) || actor.mapping.Validate(actor.identity) != nil || actor.mapping.Current == nil {
		return "", false, agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorBoardRevisionMalformed)
	}
	current := actor.mapping.Current
	if current.PreparedCommit != nil {
		return "", false, agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	if current.Observed != nil {
		if current.Observed.Digest == digest {
			actor.updateMatchingResource(resource, digest)
			return agentprotocol.ContextPending, false, agentprotocol.Event{}, nil
		}
		if current.Committed != nil && current.Committed.Digest == digest {
			switch {
			case resource.UpdatedAt.Before(current.Observed.SourceUpdatedAt):
				return "", false, agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorBoardRevisionUnavailable)
			case resource.UpdatedAt.Equal(current.Observed.SourceUpdatedAt):
				return "", false, agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorBoardRevisionMalformed)
			default:
				return actor.acknowledgeCommitted(resource, digest)
			}
		}
	} else if current.Committed != nil && current.Committed.Digest == digest {
		actor.updateMatchingResource(resource, digest)
		return agentprotocol.ContextUnchanged, false, agentprotocol.Event{}, nil
	}

	latest := latestRevision(current)
	if latest != nil {
		switch {
		case resource.UpdatedAt.Before(latest.SourceUpdatedAt):
			return "", false, agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorBoardRevisionUnavailable)
		case resource.UpdatedAt.Equal(latest.SourceUpdatedAt):
			return "", false, agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorBoardRevisionMalformed)
		}
	}
	kind := agentstate.RevisionInitial
	if current.Committed != nil {
		kind = agentstate.RevisionReplacement
	}
	revision := agentstate.Revision{Digest: digest, Revision: kind, SourceUpdatedAt: resource.UpdatedAt}
	contextEvent, err := actor.factory.New(agentprotocol.ContextPayload{Digest: digest, State: agentprotocol.ContextPending})
	if err != nil {
		return "", false, agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorBrokerUnavailable)
	}
	at := actor.clock.Now().UTC()
	if at.IsZero() {
		return "", false, agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	before := cloneMapping(actor.mapping)
	expected := observedMapping(before, revision, at)
	outcome, mutationErr := actor.state.ObserveRevision(actor.identity, revision, at)
	var proven agentstate.Mapping
	if outcome == agentstate.CommitApplied && mutationErr == nil {
		proven = expected
	} else {
		loaded, class := classifyLoadedState(actor.state, actor.identity, before, expected)
		if class != mappingTarget || !knownCommitOutcome(outcome) {
			return "", false, agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
		}
		proven = loaded
	}

	actor.mapping = proven
	actor.resource = cloneProtocolResource(resource)
	actor.contextDigest = digest
	actor.contextState = agentprotocol.ContextPending
	return agentprotocol.ContextPending, true, contextEvent, nil
}

func (actor *conversation) acknowledgeCommitted(resource agentprotocol.Resource, digest string) (agentprotocol.ContextState, bool, agentprotocol.Event, error) {
	revision := agentstate.Revision{Digest: digest, Revision: agentstate.RevisionReplacement, SourceUpdatedAt: resource.UpdatedAt}
	contextEvent, err := actor.factory.New(agentprotocol.ContextPayload{Digest: digest, State: agentprotocol.ContextUnchanged})
	if err != nil {
		return "", false, agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorBrokerUnavailable)
	}
	at := actor.clock.Now().UTC()
	if at.IsZero() {
		return "", false, agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	before := cloneMapping(actor.mapping)
	expected := acknowledgedMapping(before, revision, at)
	outcome, mutationErr := actor.state.AcknowledgeCommittedRevision(actor.identity, revision, at)
	var proven agentstate.Mapping
	if outcome == agentstate.CommitApplied && mutationErr == nil {
		proven = expected
	} else {
		loaded, class := classifyLoadedState(actor.state, actor.identity, before, expected)
		if class != mappingTarget || !knownCommitOutcome(outcome) {
			return "", false, agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
		}
		proven = loaded
	}
	actor.mapping = proven
	actor.resource = cloneProtocolResource(resource)
	actor.contextDigest = digest
	actor.contextState = agentprotocol.ContextUnchanged
	return agentprotocol.ContextUnchanged, true, contextEvent, nil
}

func classifyLoadedState(state StateStore, identity agentstate.Identity, before, target agentstate.Mapping) (agentstate.Mapping, mappingClassification) {
	loaded, err := state.Load(identity)
	if err != nil || loaded.Validate(identity) != nil {
		return agentstate.Mapping{}, mappingInvalid
	}
	switch {
	case reflect.DeepEqual(loaded, target):
		return loaded, mappingTarget
	case reflect.DeepEqual(loaded, before):
		return loaded, mappingPrecondition
	default:
		return loaded, mappingOther
	}
}

func latestRevision(session *agentstate.Session) *agentstate.Revision {
	var latest *agentstate.Revision
	if session.Committed != nil {
		latest = session.Committed
	}
	if session.Observed != nil && (latest == nil || session.Observed.SourceUpdatedAt.After(latest.SourceUpdatedAt)) {
		latest = session.Observed
	}
	return latest
}

func (actor *conversation) updateMatchingResource(resource agentprotocol.Resource, digest string) {
	if actor.contextDigest != "" && actor.contextDigest != digest {
		return
	}
	if actor.mapping.Current != nil {
		if latest := latestRevision(actor.mapping.Current); latest != nil && resource.UpdatedAt.Before(latest.SourceUpdatedAt) {
			return
		}
	}
	if actor.resource.ID == "" || resource.UpdatedAt.After(actor.resource.UpdatedAt) {
		actor.resource = cloneProtocolResource(resource)
		actor.contextDigest = digest
	}
}

func cloneProtocolResource(resource agentprotocol.Resource) agentprotocol.Resource {
	result := resource
	result.ExpiresAt = cloneTimePointer(resource.ExpiresAt)
	return result
}
