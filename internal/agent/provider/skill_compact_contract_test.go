package provider_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

const (
	skillIDOne = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	skillIDTwo = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	workID     = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
)

func TestSkillPartsAreSafeOrderedContent(t *testing.T) {
	content := provider.MessageContent{Parts: []provider.MessagePart{
		{Kind: provider.MessagePartSkill, Skill: &provider.SkillInvocation{ID: skillIDOne, Name: "review-helper"}},
		{Kind: provider.MessagePartText, Text: "check this page"},
		{Kind: provider.MessagePartSkill, Skill: &provider.SkillInvocation{ID: skillIDTwo, Name: "summarize"}},
	}}
	require.NoError(t, content.Validate())
	require.False(t, content.Empty())
	require.Equal(t, len(skillIDOne)+len("review-helper")+len("check this page")+len(skillIDTwo)+len("summarize"), content.SemanticBytes())

	clone := content.Clone()
	clone.Parts[0].Skill.Name = "changed"
	require.Equal(t, "review-helper", content.Parts[0].Skill.Name)

	encoded, err := json.Marshal(content)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "path")

	duplicate := content.Clone()
	duplicate.Parts[2].Skill.ID = skillIDOne
	require.Error(t, duplicate.Validate())

	mismatchedDuplicate := content.Clone()
	mismatchedDuplicate.Parts[2].Skill = &provider.SkillInvocation{ID: skillIDTwo, Name: "review-helper"}
	require.Error(t, mismatchedDuplicate.Validate(), "native names must identify one catalog entry per turn")

	tooMany := provider.MessageContent{Parts: make([]provider.MessagePart, provider.MaxMessageSkills+1)}
	for index := range tooMany.Parts {
		id := strings.Repeat(string(rune('A'+index)), 32)
		tooMany.Parts[index] = provider.MessagePart{Kind: provider.MessagePartSkill, Skill: &provider.SkillInvocation{ID: id, Name: "skill-" + string(rune('a'+index))}}
	}
	require.Error(t, tooMany.Validate())
}

func TestCanonicalEnvelopeAndHistoryPreserveSafeSkillIdentity(t *testing.T) {
	request := validTurnRequest()
	request.Content = provider.MessageContent{Parts: []provider.MessagePart{
		{Kind: provider.MessagePartSkill, Skill: &provider.SkillInvocation{ID: skillIDOne, Name: "review-helper"}},
		{Kind: provider.MessagePartText, Text: "check this page"},
	}}
	encoded, err := provider.Build(request, provider.PolicyConfigured)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "path")
	parsed, err := provider.Parse(encoded)
	require.NoError(t, err)
	require.Equal(t, request.Content, parsed.ReaderContent)
	require.Empty(t, parsed.ReaderMessage)

	history := provider.HistoryPage{Items: []provider.HistoryItem{{
		TurnID: request.TurnID, MessageID: request.MessageID, Role: provider.HistoryUser, Content: request.Content, CreatedAt: time.Now().UTC(),
	}}}
	require.NoError(t, history.Validate())
}

func TestSkillCatalogValidationCloningAndBounds(t *testing.T) {
	catalog := provider.SkillCatalog{State: provider.SkillsReady, MaxSelectedSkills: provider.MaxMessageSkills, Skills: []provider.SkillDescriptor{{
		ID: skillIDOne, Name: "review-helper", DisplayName: "Review helper", Description: "Review the current page.", Scope: provider.SkillScopeRepo,
	}}}
	require.NoError(t, catalog.Validate())
	clone := catalog.Clone()
	clone.Skills[0].Name = "changed"
	require.Equal(t, "review-helper", catalog.Skills[0].Name)

	unavailable := provider.SkillCatalog{State: provider.SkillsUnavailable, Skills: []provider.SkillDescriptor{}}
	require.NoError(t, unavailable.Validate())
	unavailable.Skills = append(unavailable.Skills, catalog.Skills[0])
	require.Error(t, unavailable.Validate())

	duplicate := catalog.Clone()
	duplicate.Skills = append(duplicate.Skills, duplicate.Skills[0])
	require.Error(t, duplicate.Validate())

	overflow := catalog.Clone()
	overflow.Skills[0].Description = strings.Repeat("x", provider.MaxSkillDescriptionBytes+1)
	require.Error(t, overflow.Validate())
}

func TestSkillAndCompactProviderContractsAreClosedAndMemoryOnly(t *testing.T) {
	require.True(t, provider.NewProviderError(provider.ErrorSkillUnavailable).Valid())
	require.True(t, provider.NewProviderError(provider.ErrorCompactUnsupported).Valid())

	request := provider.CompactRequest{WorkID: workID}
	require.NoError(t, request.Validate())
	accepted := provider.AcceptedCompact{WorkID: workID, AcceptedAt: time.Now().UTC()}
	require.NoError(t, accepted.Validate())

	catalog := provider.SkillCatalog{State: provider.SkillsReady, MaxSelectedSkills: provider.MaxMessageSkills, Skills: []provider.SkillDescriptor{}}
	catalogEvent := provider.NewSkillCatalogEvent(catalog)
	require.NoError(t, catalogEvent.Validate())
	cloneCatalog := catalogEvent.SkillCatalog.Clone()
	require.Equal(t, catalog, cloneCatalog)

	for _, status := range []provider.CompactStatus{provider.CompactCompleted, provider.CompactInterrupted, provider.CompactFailed} {
		event := provider.NewCompactEvent(workID, status)
		require.NoError(t, event.Validate())
	}

	_, catalogCapable := any((*catalogSessionContract)(nil)).(provider.SkillCatalogSession)
	_, compactCapable := any((*compactSessionContract)(nil)).(provider.ManualCompactSession)
	require.True(t, catalogCapable)
	require.True(t, compactCapable)
}

type catalogSessionContract struct{}

func (*catalogSessionContract) Skills(context.Context) provider.SkillCatalog {
	return provider.SkillCatalog{}
}

type compactSessionContract struct{}

func (*compactSessionContract) SupportsCompact() bool { return true }
func (*compactSessionContract) Compact(context.Context, provider.CompactRequest) (provider.AcceptedCompact, error) {
	return provider.AcceptedCompact{}, nil
}
func (*compactSessionContract) InterruptCompact(context.Context, provider.AcceptedCompact) error {
	return nil
}
