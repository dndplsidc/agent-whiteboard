package broker

import (
	"context"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/dndplsidc/agent-whiteboard/internal/agent/state"
)

func (broker *Broker) promoteAccepted(identity statepkg.Identity, before statepkg.Mapping) (statepkg.Mapping, error) {
	if before.Validate(identity) != nil || before.Current == nil || before.Current.PreparedCommit == nil || before.Current.PreparedCommit.Phase != statepkg.CommitAccepted {
		return statepkg.Mapping{}, NewBrokerError(protocol.ErrorStateRepairFailed)
	}
	turnID := before.Current.PreparedCommit.TurnID
	at, ok := broker.mutationTime()
	if !ok {
		return statepkg.Mapping{}, NewBrokerError(protocol.ErrorStateRepairFailed)
	}
	expected := promotedMapping(before, at)
	outcome, mutationErr := broker.state.PromotePrepared(identity, turnID, at)
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		return expected, nil
	}
	loaded, loadedClass := broker.classifyLoaded(identity, before, expected)
	if !knownCommitOutcome(outcome) {
		return statepkg.Mapping{}, NewBrokerError(protocol.ErrorStateRepairFailed)
	}
	if loadedClass == mappingTarget {
		return loaded, nil
	}
	if loadedClass != mappingPrecondition {
		return statepkg.Mapping{}, NewBrokerError(protocol.ErrorStateRepairFailed)
	}

	// A promotion is retryable only after Load proves the exact durable
	// CommitAccepted precondition. No other mutation is retried generically.
	at, ok = broker.mutationTime()
	if !ok {
		return statepkg.Mapping{}, NewBrokerError(protocol.ErrorStateRepairFailed)
	}
	expected = promotedMapping(loaded, at)
	outcome, mutationErr = broker.state.PromotePrepared(identity, turnID, at)
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		return expected, nil
	}
	loaded, loadedClass = broker.classifyLoaded(identity, loaded, expected)
	if knownCommitOutcome(outcome) && loadedClass == mappingTarget {
		return loaded, nil
	}
	return statepkg.Mapping{}, NewBrokerError(protocol.ErrorStateRepairFailed)
}

func (broker *Broker) reconcilePrepared(ctx context.Context, identity statepkg.Identity, before statepkg.Mapping, handle *sessionHandle) (*conversation, error) {
	prepared := before.Current.PreparedCommit
	if prepared == nil || prepared.Phase != statepkg.CommitPrepared {
		broker.retainStop(identity, handle)
		return nil, NewBrokerError(protocol.ErrorStateRepairFailed)
	}
	drainer := startTemporaryDrainer(handle.events)
	state, reconcileErr := handle.session.Reconcile(ctx, provider.TurnReference{TurnID: prepared.TurnID})
	if reconcileErr != nil {
		drainer.stop()
		broker.retainStop(identity, handle)
		if ctx.Err() != nil {
			return nil, NewBrokerError(protocol.ErrorBrokerShuttingDown)
		}
		return nil, MapError(reconcileErr)
	}
	if state == provider.TurnUnknown {
		drainer.stop()
		broker.retainStop(identity, handle)
		return nil, NewBrokerError(protocol.ErrorAcceptanceOutcomeUnknown)
	}
	if !state.Valid() || !state.Definitive() {
		drainer.stop()
		broker.retainStop(identity, handle)
		return nil, NewBrokerError(protocol.ErrorStateRepairFailed)
	}
	accepted := state != provider.TurnNotAccepted
	at, ok := broker.mutationTime()
	if !ok {
		drainer.stop()
		broker.retainStop(identity, handle)
		return nil, NewBrokerError(protocol.ErrorStateRepairFailed)
	}
	expected := reconciledMapping(before, accepted, at)
	outcome, mutationErr := broker.state.ReconcilePrepared(identity, prepared.TurnID, accepted, at)
	var repaired statepkg.Mapping
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		repaired = expected
	} else {
		loaded, class := broker.classifyLoaded(identity, before, expected)
		if class != mappingTarget || !knownCommitOutcome(outcome) {
			drainer.stop()
			broker.retainStop(identity, handle)
			return nil, NewBrokerError(protocol.ErrorStateRepairFailed)
		}
		repaired = loaded
	}
	drainer.stop()
	return broker.newConversation(identity, repaired, handle)
}

type temporaryDrainer struct {
	stopSignal chan struct{}
	done       chan struct{}
}

func startTemporaryDrainer(events <-chan provider.Event) *temporaryDrainer {
	drainer := &temporaryDrainer{stopSignal: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(drainer.done)
		for {
			select {
			case <-drainer.stopSignal:
				return
			default:
			}
			select {
			case _, open := <-events:
				if !open {
					events = nil
				}
			case <-drainer.stopSignal:
				return
			}
		}
	}()
	return drainer
}

func (drainer *temporaryDrainer) stop() {
	close(drainer.stopSignal)
	<-drainer.done
}

type mappingClassification uint8

const (
	mappingInvalid mappingClassification = iota
	mappingPrecondition
	mappingTarget
	mappingOther
)

func (broker *Broker) classifyLoaded(identity statepkg.Identity, before, target statepkg.Mapping) (statepkg.Mapping, mappingClassification) {
	return classifyLoadedState(broker.state, identity, before, target)
}

func knownCommitOutcome(outcome statepkg.CommitOutcome) bool {
	return outcome == statepkg.CommitApplied || outcome == statepkg.CommitNotApplied || outcome == statepkg.CommitUncertain
}

func (broker *Broker) mutationTime() (time.Time, bool) {
	at := broker.clock.Now().UTC()
	return at, !at.IsZero()
}

func preparedMapping(mapping statepkg.Mapping, revision statepkg.Revision, turnID string, at time.Time) statepkg.Mapping {
	result := cloneMapping(mapping)
	observed := revision
	prepared := statepkg.PreparedCommit{Revision: revision, TurnID: turnID, Phase: statepkg.CommitPrepared}
	result.Current.Observed = &observed
	result.Current.PreparedCommit = &prepared
	result.Current.UpdatedAt = maxTime(result.Current.UpdatedAt, at)
	result.UpdatedAt = maxTime(result.UpdatedAt, at)
	return result
}

func acceptedMapping(mapping statepkg.Mapping, turnID string, at time.Time) statepkg.Mapping {
	result := cloneMapping(mapping)
	if result.Current != nil && result.Current.PreparedCommit != nil && result.Current.PreparedCommit.TurnID == turnID {
		result.Current.PreparedCommit.Phase = statepkg.CommitAccepted
		result.Current.UpdatedAt = maxTime(result.Current.UpdatedAt, at)
		result.UpdatedAt = maxTime(result.UpdatedAt, at)
	}
	return result
}

func promotedMapping(mapping statepkg.Mapping, at time.Time) statepkg.Mapping {
	result := cloneMapping(mapping)
	prepared := result.Current.PreparedCommit
	committed := prepared.Revision
	result.Current.Committed = &committed
	result.Current.Observed = nil
	result.Current.PreparedCommit = nil
	result.Current.UpdatedAt = maxTime(result.Current.UpdatedAt, at)
	result.UpdatedAt = maxTime(result.UpdatedAt, at)
	return result
}

func reconciledMapping(mapping statepkg.Mapping, accepted bool, at time.Time) statepkg.Mapping {
	if accepted {
		return promotedMapping(mapping, at)
	}
	result := cloneMapping(mapping)
	prepared := result.Current.PreparedCommit
	observed := prepared.Revision
	result.Current.Observed = &observed
	result.Current.PreparedCommit = nil
	result.Current.UpdatedAt = maxTime(result.Current.UpdatedAt, at)
	result.UpdatedAt = maxTime(result.UpdatedAt, at)
	return result
}

func acknowledgedMapping(mapping statepkg.Mapping, revision statepkg.Revision, at time.Time) statepkg.Mapping {
	result := cloneMapping(mapping)
	committed := revision
	result.Current.Committed = &committed
	result.Current.Observed = nil
	result.Current.UpdatedAt = maxTime(result.Current.UpdatedAt, at)
	result.UpdatedAt = maxTime(result.UpdatedAt, at)
	return result
}

func observedMapping(mapping statepkg.Mapping, revision statepkg.Revision, at time.Time) statepkg.Mapping {
	result := cloneMapping(mapping)
	observed := revision
	result.Current.Observed = &observed
	result.Current.UpdatedAt = maxTime(result.Current.UpdatedAt, at)
	result.UpdatedAt = maxTime(result.UpdatedAt, at)
	return result
}

func cloneMapping(mapping statepkg.Mapping) statepkg.Mapping {
	result := mapping
	if mapping.Current != nil {
		current := *mapping.Current
		current.Committed = cloneRevision(mapping.Current.Committed)
		current.Observed = cloneRevision(mapping.Current.Observed)
		if mapping.Current.PreparedCommit != nil {
			prepared := *mapping.Current.PreparedCommit
			current.PreparedCommit = &prepared
		}
		result.Current = &current
	}
	if mapping.Archives != nil {
		result.Archives = append([]statepkg.Session{}, mapping.Archives...)
	}
	for index := range result.Archives {
		result.Archives[index].Committed = cloneRevision(result.Archives[index].Committed)
		result.Archives[index].Observed = cloneRevision(result.Archives[index].Observed)
		if mapping.Archives[index].PreparedCommit != nil {
			prepared := *mapping.Archives[index].PreparedCommit
			result.Archives[index].PreparedCommit = &prepared
		}
	}
	return result
}

func cloneRevision(revision *statepkg.Revision) *statepkg.Revision {
	if revision == nil {
		return nil
	}
	copyOfRevision := *revision
	return &copyOfRevision
}

func maxTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}
