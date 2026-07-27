package broker

import (
	"context"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

func (broker *Broker) promoteAccepted(identity agentstate.Identity, before agentstate.Mapping) (agentstate.Mapping, error) {
	if before.Validate(identity) != nil || before.Current == nil || before.Current.PreparedCommit == nil || before.Current.PreparedCommit.Phase != agentstate.CommitAccepted {
		return agentstate.Mapping{}, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	turnID := before.Current.PreparedCommit.TurnID
	at, ok := broker.mutationTime()
	if !ok {
		return agentstate.Mapping{}, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	expected := promotedMapping(before, at)
	outcome, mutationErr := broker.state.PromotePrepared(identity, turnID, at)
	if outcome == agentstate.CommitApplied && mutationErr == nil {
		return expected, nil
	}
	loaded, loadedClass := broker.classifyLoaded(identity, before, expected)
	if !knownCommitOutcome(outcome) {
		return agentstate.Mapping{}, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	if loadedClass == mappingTarget {
		return loaded, nil
	}
	if loadedClass != mappingPrecondition {
		return agentstate.Mapping{}, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}

	// A promotion is retryable only after Load proves the exact durable
	// CommitAccepted precondition. No other mutation is retried generically.
	at, ok = broker.mutationTime()
	if !ok {
		return agentstate.Mapping{}, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	expected = promotedMapping(loaded, at)
	outcome, mutationErr = broker.state.PromotePrepared(identity, turnID, at)
	if outcome == agentstate.CommitApplied && mutationErr == nil {
		return expected, nil
	}
	loaded, loadedClass = broker.classifyLoaded(identity, loaded, expected)
	if knownCommitOutcome(outcome) && loadedClass == mappingTarget {
		return loaded, nil
	}
	return agentstate.Mapping{}, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
}

func (broker *Broker) reconcilePrepared(ctx context.Context, identity agentstate.Identity, before agentstate.Mapping, session provider.Session) (*conversation, error) {
	prepared := before.Current.PreparedCommit
	if prepared == nil || prepared.Phase != agentstate.CommitPrepared {
		broker.retainStop(identity, session)
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	drainer := startTemporaryDrainer(session.Events())
	state, reconcileErr := session.Reconcile(ctx, provider.TurnReference{TurnID: prepared.TurnID})
	if reconcileErr != nil {
		drainer.stop()
		broker.retainStop(identity, session)
		if ctx.Err() != nil {
			return nil, NewBrokerError(agentprotocol.ErrorBrokerShuttingDown)
		}
		return nil, MapError(reconcileErr)
	}
	if state == provider.TurnUnknown {
		drainer.stop()
		broker.retainStop(identity, session)
		return nil, NewBrokerError(agentprotocol.ErrorAcceptanceOutcomeUnknown)
	}
	if !state.Valid() || !state.Definitive() {
		drainer.stop()
		broker.retainStop(identity, session)
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	accepted := state != provider.TurnNotAccepted
	at, ok := broker.mutationTime()
	if !ok {
		drainer.stop()
		broker.retainStop(identity, session)
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	expected := reconciledMapping(before, accepted, at)
	outcome, mutationErr := broker.state.ReconcilePrepared(identity, prepared.TurnID, accepted, at)
	var repaired agentstate.Mapping
	if outcome == agentstate.CommitApplied && mutationErr == nil {
		repaired = expected
	} else {
		loaded, class := broker.classifyLoaded(identity, before, expected)
		if class != mappingTarget || !knownCommitOutcome(outcome) {
			drainer.stop()
			broker.retainStop(identity, session)
			return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
		}
		repaired = loaded
	}
	drainer.stop()
	return broker.newConversation(identity, repaired, session)
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

func (broker *Broker) classifyLoaded(identity agentstate.Identity, before, target agentstate.Mapping) (agentstate.Mapping, mappingClassification) {
	return classifyLoadedState(broker.state, identity, before, target)
}

func knownCommitOutcome(outcome agentstate.CommitOutcome) bool {
	return outcome == agentstate.CommitApplied || outcome == agentstate.CommitNotApplied || outcome == agentstate.CommitUncertain
}

func (broker *Broker) mutationTime() (time.Time, bool) {
	at := broker.clock.Now().UTC()
	return at, !at.IsZero()
}

func preparedMapping(mapping agentstate.Mapping, revision agentstate.Revision, turnID string, at time.Time) agentstate.Mapping {
	result := cloneMapping(mapping)
	observed := revision
	prepared := agentstate.PreparedCommit{Revision: revision, TurnID: turnID, Phase: agentstate.CommitPrepared}
	result.Current.Observed = &observed
	result.Current.PreparedCommit = &prepared
	result.Current.UpdatedAt = maxTime(result.Current.UpdatedAt, at)
	result.UpdatedAt = maxTime(result.UpdatedAt, at)
	return result
}

func acceptedMapping(mapping agentstate.Mapping, turnID string, at time.Time) agentstate.Mapping {
	result := cloneMapping(mapping)
	if result.Current != nil && result.Current.PreparedCommit != nil && result.Current.PreparedCommit.TurnID == turnID {
		result.Current.PreparedCommit.Phase = agentstate.CommitAccepted
		result.Current.UpdatedAt = maxTime(result.Current.UpdatedAt, at)
		result.UpdatedAt = maxTime(result.UpdatedAt, at)
	}
	return result
}

func promotedMapping(mapping agentstate.Mapping, at time.Time) agentstate.Mapping {
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

func reconciledMapping(mapping agentstate.Mapping, accepted bool, at time.Time) agentstate.Mapping {
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

func acknowledgedMapping(mapping agentstate.Mapping, revision agentstate.Revision, at time.Time) agentstate.Mapping {
	result := cloneMapping(mapping)
	committed := revision
	result.Current.Committed = &committed
	result.Current.Observed = nil
	result.Current.UpdatedAt = maxTime(result.Current.UpdatedAt, at)
	result.UpdatedAt = maxTime(result.UpdatedAt, at)
	return result
}

func observedMapping(mapping agentstate.Mapping, revision agentstate.Revision, at time.Time) agentstate.Mapping {
	result := cloneMapping(mapping)
	observed := revision
	result.Current.Observed = &observed
	result.Current.UpdatedAt = maxTime(result.Current.UpdatedAt, at)
	result.UpdatedAt = maxTime(result.UpdatedAt, at)
	return result
}

func cloneMapping(mapping agentstate.Mapping) agentstate.Mapping {
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
		result.Archives = append([]agentstate.Session{}, mapping.Archives...)
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

func cloneRevision(revision *agentstate.Revision) *agentstate.Revision {
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
