package broker

import (
	"reflect"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	statepkg "github.com/dndplsidc/agent-whiteboard/internal/agent/state"
)

func (actor *conversation) observeAttach(resource protocol.Resource, digest string) (protocol.ContextState, bool, protocol.Event, error) {
	if validateProtocolResource(resource) != nil || !validDigest(digest) || actor.mapping.Validate(actor.identity) != nil || actor.mapping.Current == nil {
		return "", false, protocol.Event{}, NewBrokerError(protocol.ErrorBoardRevisionMalformed)
	}
	current := actor.mapping.Current
	if current.PreparedCommit != nil {
		return "", false, protocol.Event{}, NewBrokerError(protocol.ErrorStateRepairFailed)
	}
	if current.Observed != nil {
		if current.Observed.Digest == digest {
			actor.updateMatchingResource(resource, digest)
			return protocol.ContextPending, false, protocol.Event{}, nil
		}
		if current.Committed != nil && current.Committed.Digest == digest {
			switch {
			case resource.UpdatedAt.Before(current.Observed.SourceUpdatedAt):
				return "", false, protocol.Event{}, NewBrokerError(protocol.ErrorBoardRevisionUnavailable)
			case resource.UpdatedAt.Equal(current.Observed.SourceUpdatedAt):
				return "", false, protocol.Event{}, NewBrokerError(protocol.ErrorBoardRevisionMalformed)
			default:
				return actor.acknowledgeCommitted(resource, digest)
			}
		}
	} else if current.Committed != nil && current.Committed.Digest == digest {
		actor.updateMatchingResource(resource, digest)
		return protocol.ContextUnchanged, false, protocol.Event{}, nil
	}

	latest := latestRevision(current)
	if latest != nil {
		switch {
		case resource.UpdatedAt.Before(latest.SourceUpdatedAt):
			return "", false, protocol.Event{}, NewBrokerError(protocol.ErrorBoardRevisionUnavailable)
		case resource.UpdatedAt.Equal(latest.SourceUpdatedAt):
			return "", false, protocol.Event{}, NewBrokerError(protocol.ErrorBoardRevisionMalformed)
		}
	}
	kind := statepkg.RevisionInitial
	if current.Committed != nil {
		kind = statepkg.RevisionReplacement
	}
	revision := statepkg.Revision{Digest: digest, Revision: kind, SourceUpdatedAt: resource.UpdatedAt}
	contextEvent, err := actor.factory.New(protocol.ContextPayload{Digest: digest, State: protocol.ContextPending})
	if err != nil {
		return "", false, protocol.Event{}, NewBrokerError(protocol.ErrorBrokerUnavailable)
	}
	at := actor.clock.Now().UTC()
	if at.IsZero() {
		return "", false, protocol.Event{}, NewBrokerError(protocol.ErrorStateRepairFailed)
	}
	before := cloneMapping(actor.mapping)
	expected := observedMapping(before, revision, at)
	outcome, mutationErr := actor.state.ObserveRevision(actor.identity, revision, at)
	var proven statepkg.Mapping
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		proven = expected
	} else {
		loaded, class := classifyLoadedState(actor.state, actor.identity, before, expected)
		if class != mappingTarget || !knownCommitOutcome(outcome) {
			return "", false, protocol.Event{}, NewBrokerError(protocol.ErrorStateRepairFailed)
		}
		proven = loaded
	}

	actor.mapping = proven
	actor.resource = cloneProtocolResource(resource)
	actor.contextDigest = digest
	actor.contextState = protocol.ContextPending
	return protocol.ContextPending, true, contextEvent, nil
}

func (actor *conversation) acknowledgeCommitted(resource protocol.Resource, digest string) (protocol.ContextState, bool, protocol.Event, error) {
	revision := statepkg.Revision{Digest: digest, Revision: statepkg.RevisionReplacement, SourceUpdatedAt: resource.UpdatedAt}
	contextEvent, err := actor.factory.New(protocol.ContextPayload{Digest: digest, State: protocol.ContextUnchanged})
	if err != nil {
		return "", false, protocol.Event{}, NewBrokerError(protocol.ErrorBrokerUnavailable)
	}
	at := actor.clock.Now().UTC()
	if at.IsZero() {
		return "", false, protocol.Event{}, NewBrokerError(protocol.ErrorStateRepairFailed)
	}
	before := cloneMapping(actor.mapping)
	expected := acknowledgedMapping(before, revision, at)
	outcome, mutationErr := actor.state.AcknowledgeCommittedRevision(actor.identity, revision, at)
	var proven statepkg.Mapping
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		proven = expected
	} else {
		loaded, class := classifyLoadedState(actor.state, actor.identity, before, expected)
		if class != mappingTarget || !knownCommitOutcome(outcome) {
			return "", false, protocol.Event{}, NewBrokerError(protocol.ErrorStateRepairFailed)
		}
		proven = loaded
	}
	actor.mapping = proven
	actor.resource = cloneProtocolResource(resource)
	actor.contextDigest = digest
	actor.contextState = protocol.ContextUnchanged
	return protocol.ContextUnchanged, true, contextEvent, nil
}

func classifyLoadedState(state StateStore, identity statepkg.Identity, before, target statepkg.Mapping) (statepkg.Mapping, mappingClassification) {
	loaded, err := state.Load(identity)
	if err != nil || loaded.Validate(identity) != nil {
		return statepkg.Mapping{}, mappingInvalid
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

func latestRevision(session *statepkg.Session) *statepkg.Revision {
	var latest *statepkg.Revision
	if session.Committed != nil {
		latest = session.Committed
	}
	if session.Observed != nil && (latest == nil || session.Observed.SourceUpdatedAt.After(latest.SourceUpdatedAt)) {
		latest = session.Observed
	}
	return latest
}

func (actor *conversation) updateMatchingResource(resource protocol.Resource, digest string) {
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

func cloneProtocolResource(resource protocol.Resource) protocol.Resource {
	result := resource
	result.ExpiresAt = cloneTimePointer(resource.ExpiresAt)
	return result
}
