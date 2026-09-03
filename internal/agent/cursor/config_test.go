package cursor

import (
	"context"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

func TestAuthoritativeConfigGenerationAdvancesWhenSettingsDoNotChange(t *testing.T) {
	d, _, root := testDriver(t)
	s := newSession(d, &runtime{caps: capabilities{}}, root)
	if err := s.finishOpen(openResult{SessionID: "native", ConfigOptions: testOptions("model-a")}, false); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	before := s.configGeneration
	_, _, _, err := s.updateAuthoritativeLocked(testOptions("model-a"))
	after := s.configGeneration
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatalf("generation %d -> %d", before, after)
	}
}

func TestSameSettingsGenerationDriftDuringPromptBuildRejectsActivation(t *testing.T) {
	driver, _, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*Session)
	entered, release := make(chan struct{}), make(chan struct{})
	s.mu.Lock()
	base := s.promptBlocks
	s.promptBlocks = func(workspace string, envelope []byte, images []provider.ImageInput) ([]any, error) {
		close(entered)
		<-release
		return base(workspace, envelope, images)
	}
	s.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		_, submitErr := s.Submit(context.Background(), provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")})
		result <- submitErr
	}()
	<-entered
	s.mu.Lock()
	_, _, _, updateErr := s.updateAuthoritativeLocked(testOptions("model-a"))
	s.mu.Unlock()
	if updateErr != nil {
		t.Fatal(updateErr)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("same-settings generation drift activated stale prompt")
	}
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active != nil {
		t.Fatal("stale prompt installed")
	}
	_ = s.Shutdown(context.Background())
}
