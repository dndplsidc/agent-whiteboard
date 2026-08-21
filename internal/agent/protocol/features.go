package protocol

import "errors"

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

func (skill SkillDescriptor) validate() error {
	if !validID(skill.ID) || !validBoundedText(skill.Name, MaxSkillNameBytes, true) ||
		!validBoundedText(skill.DisplayName, MaxSkillDisplayNameBytes, false) ||
		!validBoundedText(skill.Description, MaxSkillDescriptionBytes, false) || !skill.Scope.Valid() {
		return invalid(nil)
	}
	return nil
}

func ValidateSkillCatalog(state SkillsState, skills []SkillDescriptor) error {
	var limit *int
	if state == SkillsReady {
		value := MaxMessageSkills
		limit = &value
	}
	return validateSkillCatalog(state, skills, limit)
}

func ValidateSkillCatalogWithLimit(state SkillsState, skills []SkillDescriptor, maxSelectedSkills *int) error {
	return validateSkillCatalog(state, skills, maxSelectedSkills)
}

func validateSkillCatalog(state SkillsState, skills []SkillDescriptor, maxSelectedSkills *int) error {
	validLimit := state == SkillsReady && maxSelectedSkills != nil && *maxSelectedSkills > 0 && *maxSelectedSkills <= MaxMessageSkills ||
		state == SkillsUnavailable && maxSelectedSkills == nil
	if !state.Valid() || !validLimit || skills == nil || len(skills) > MaxSkills || state == SkillsUnavailable && len(skills) != 0 {
		return invalid(nil)
	}
	seenIDs := make(map[string]struct{}, len(skills))
	seenNames := make(map[string]struct{}, len(skills))
	for _, skill := range skills {
		if skill.validate() != nil {
			return invalid(nil)
		}
		if _, duplicate := seenIDs[skill.ID]; duplicate {
			return invalid(nil)
		}
		if _, duplicate := seenNames[skill.Name]; duplicate {
			return invalid(nil)
		}
		seenIDs[skill.ID] = struct{}{}
		seenNames[skill.Name] = struct{}{}
	}
	encoded, err := marshalApplicationJSON(skills)
	if err != nil {
		return invalid(err)
	}
	if len(encoded) > MaxSkillsCatalogBytes {
		return ErrMessageTooLarge
	}
	return nil
}

type BusyTurnPolicy string

const (
	BusyTurnQueue         BusyTurnPolicy = "queue"
	BusyTurnPreserveDraft BusyTurnPolicy = "preserve_draft"
)

func (policy BusyTurnPolicy) Valid() bool {
	return policy == BusyTurnQueue || policy == BusyTurnPreserveDraft
}

type ComposerAdmission string

const (
	ComposerSubmit        ComposerAdmission = "submit"
	ComposerQueue         ComposerAdmission = "queue"
	ComposerPreserveDraft ComposerAdmission = "preserve_draft"
	ComposerBlocked       ComposerAdmission = "blocked"
)

func (admission ComposerAdmission) Valid() bool {
	switch admission {
	case ComposerSubmit, ComposerQueue, ComposerPreserveDraft, ComposerBlocked:
		return true
	default:
		return false
	}
}

func validComposerAdmission(policy BusyTurnPolicy, admission ComposerAdmission) bool {
	if !policy.Valid() || !admission.Valid() {
		return false
	}
	return admission == ComposerSubmit || admission == ComposerBlocked ||
		admission == ComposerQueue && policy == BusyTurnQueue ||
		admission == ComposerPreserveDraft && policy == BusyTurnPreserveDraft
}

type ActiveWorkKind string

const (
	ActiveWorkTurn    ActiveWorkKind = "turn"
	ActiveWorkCompact ActiveWorkKind = "compact"
)

type ActiveWorkState string

const (
	ActiveWorkRunning  ActiveWorkState = "running"
	ActiveWorkStopping ActiveWorkState = "stopping"
)

type ActiveWork struct {
	WorkID string          `json:"work_id"`
	Kind   ActiveWorkKind  `json:"kind"`
	State  ActiveWorkState `json:"state"`
}

func (work ActiveWork) validate() error {
	if !validID(work.WorkID) || (work.Kind != ActiveWorkTurn && work.Kind != ActiveWorkCompact) || (work.State != ActiveWorkRunning && work.State != ActiveWorkStopping) {
		return errors.New("invalid active work")
	}
	return nil
}

func validActiveWork(lifecycle LifecycleState, work *ActiveWork) bool {
	switch lifecycle {
	case LifecycleResponding:
		return work != nil && work.validate() == nil && work.Kind == ActiveWorkTurn
	case LifecycleCompacting:
		return work != nil && work.validate() == nil && work.Kind == ActiveWorkCompact
	default:
		return work == nil
	}
}

type CompactionStatus string

const (
	CompactionRunning     CompactionStatus = "running"
	CompactionStopping    CompactionStatus = "stopping"
	CompactionCompleted   CompactionStatus = "completed"
	CompactionInterrupted CompactionStatus = "interrupted"
	CompactionFailed      CompactionStatus = "failed"
)

func (status CompactionStatus) valid() bool {
	switch status {
	case CompactionRunning, CompactionStopping, CompactionCompleted, CompactionInterrupted, CompactionFailed:
		return true
	default:
		return false
	}
}
