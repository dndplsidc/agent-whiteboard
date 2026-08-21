package provider

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	MaxSkillsCatalogBytes    = 512 << 10
	MaxSkills                = 512
	MaxSkillNameBytes        = 512
	MaxSkillDisplayNameBytes = 512
	MaxSkillDescriptionBytes = 2 << 10
)

type SkillScope string

const (
	SkillScopeUser   SkillScope = "user"
	SkillScopeRepo   SkillScope = "repo"
	SkillScopeSystem SkillScope = "system"
	SkillScopeAdmin  SkillScope = "admin"
)

func (scope SkillScope) Valid() bool {
	switch scope {
	case SkillScopeUser, SkillScopeRepo, SkillScopeSystem, SkillScopeAdmin:
		return true
	default:
		return false
	}
}

type SkillsState string

const (
	SkillsReady       SkillsState = "ready"
	SkillsUnavailable SkillsState = "unavailable"
)

func (state SkillsState) Valid() bool { return state == SkillsReady || state == SkillsUnavailable }

type SkillDescriptor struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	DisplayName string     `json:"display_name,omitempty"`
	Description string     `json:"description,omitempty"`
	Scope       SkillScope `json:"scope"`
}

func (skill SkillDescriptor) Validate() error {
	if !validID(skill.ID) || !validBoundedText(skill.Name, MaxSkillNameBytes, true) ||
		!validBoundedText(skill.DisplayName, MaxSkillDisplayNameBytes, false) ||
		!validBoundedText(skill.Description, MaxSkillDescriptionBytes, false) || !skill.Scope.Valid() {
		return errors.New("invalid skill descriptor")
	}
	return nil
}

type SkillCatalog struct {
	State             SkillsState
	Skills            []SkillDescriptor
	MaxSelectedSkills int
}

func (catalog SkillCatalog) Validate() error {
	validLimit := catalog.State == SkillsReady && catalog.MaxSelectedSkills > 0 && catalog.MaxSelectedSkills <= MaxMessageSkills ||
		catalog.State == SkillsUnavailable && catalog.MaxSelectedSkills == 0
	if !catalog.State.Valid() || !validLimit || catalog.Skills == nil || len(catalog.Skills) > MaxSkills || catalog.State == SkillsUnavailable && len(catalog.Skills) != 0 {
		return errors.New("invalid skill catalog")
	}
	seenIDs := make(map[string]struct{}, len(catalog.Skills))
	seenNames := make(map[string]struct{}, len(catalog.Skills))
	for _, skill := range catalog.Skills {
		if skill.Validate() != nil {
			return errors.New("invalid skill catalog")
		}
		if _, duplicate := seenIDs[skill.ID]; duplicate {
			return errors.New("duplicate skill catalog id")
		}
		if _, duplicate := seenNames[skill.Name]; duplicate {
			return errors.New("duplicate skill catalog name")
		}
		seenIDs[skill.ID] = struct{}{}
		seenNames[skill.Name] = struct{}{}
	}
	encoded, err := json.Marshal(catalog.Skills)
	if err != nil || len(encoded) > MaxSkillsCatalogBytes {
		return errors.New("skill catalog exceeds byte limit")
	}
	return nil
}

func (catalog SkillCatalog) Clone() SkillCatalog {
	skills := make([]SkillDescriptor, len(catalog.Skills))
	copy(skills, catalog.Skills)
	return SkillCatalog{State: catalog.State, Skills: skills, MaxSelectedSkills: catalog.MaxSelectedSkills}
}

// SkillCatalogSession is implemented only by sessions that can expose a safe,
// memory-only catalog. Native paths and load details remain adapter-private.
type SkillCatalogSession interface {
	Skills(context.Context) SkillCatalog
}

type BusyTurnPolicy string

const (
	BusyTurnQueue         BusyTurnPolicy = "queue"
	BusyTurnPreserveDraft BusyTurnPolicy = "preserve_draft"
)

func (policy BusyTurnPolicy) Valid() bool {
	return policy == BusyTurnQueue || policy == BusyTurnPreserveDraft
}

// BusyTurnSession exposes the provider's bounded static busy-turn policy.
type BusyTurnSession interface {
	BusyTurnPolicy() BusyTurnPolicy
}

type CompactRequest struct{ WorkID string }

func (request CompactRequest) Validate() error {
	if !validID(request.WorkID) {
		return errors.New("invalid compact request")
	}
	return nil
}

type AcceptedCompact struct {
	WorkID     string
	AcceptedAt time.Time
}

func (accepted AcceptedCompact) Validate() error {
	if !validID(accepted.WorkID) || accepted.AcceptedAt.IsZero() {
		return errors.New("invalid accepted compact work")
	}
	return nil
}

// ManualCompactSession is implemented only by sessions that support native
// manual compaction. Compact and its interruption are correlated solely by the
// broker-safe work identity at this boundary.
type ManualCompactSession interface {
	SupportsCompact() bool
	Compact(context.Context, CompactRequest) (AcceptedCompact, error)
	InterruptCompact(context.Context, AcceptedCompact) error
}

type CompactStatus string

const (
	CompactCompleted   CompactStatus = "completed"
	CompactInterrupted CompactStatus = "interrupted"
	CompactFailed      CompactStatus = "failed"
)

func (status CompactStatus) Valid() bool {
	return status == CompactCompleted || status == CompactInterrupted || status == CompactFailed
}

type CompactResult struct {
	WorkID string
	Status CompactStatus
}

func (result CompactResult) Validate() error {
	if !validID(result.WorkID) || !result.Status.Valid() {
		return errors.New("invalid compact result")
	}
	return nil
}
