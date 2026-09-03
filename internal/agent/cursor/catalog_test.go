package cursor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/common"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestParseModelCatalogExactRowsAndDefaultPrecedence(t *testing.T) {
	output := "Available models\n\n" +
		"auto - Auto (default)\nclaude-4 - Claude 4 (current)\ngpt-5 - GPT 5\n\n" +
		"Tip: use --model <id> (or /model <id> in interactive mode) to switch.\n"
	catalog, err := parseModelCatalog([]byte(output), true)
	require.NoError(t, err)
	require.Equal(t, []string{"auto", "claude-4", "gpt-5"}, []string{catalog.Models[0].Model, catalog.Models[1].Model, catalog.Models[2].Model})
	require.Equal(t, "Claude 4", catalog.Models[1].DisplayName)
	require.True(t, catalog.Models[1].Default)
	require.False(t, catalog.Models[0].Default)
	for _, model := range catalog.Models {
		require.Equal(t, "default", model.DefaultEffort)
		require.Equal(t, []provider.ReasoningEffort{{Value: "default"}}, model.SupportedReasoningEfforts)
		require.True(t, model.SupportsImages)
		require.False(t, model.SupportsFast)
	}
}

func TestParseModelCatalogAcceptsExactMaximumRowsWithRealFraming(t *testing.T) {
	rows := make([]string, provider.MaxCatalogModels)
	for index := range rows {
		rows[index] = fmt.Sprintf("model-%03d - Model %03d", index, index)
	}
	output := "Available models\n\n" + strings.Join(rows, "\n") + "\n\nTip: use --model <id> to select.\n"
	catalog, err := parseModelCatalog([]byte(output), false)
	require.NoError(t, err)
	require.Len(t, catalog.Models, provider.MaxCatalogModels)
}

func TestParseModelCatalogRejectsMalformedDuplicateControlOversizeAndTooMany(t *testing.T) {
	cases := [][]byte{
		[]byte("missing delimiter\n"),
		[]byte("a - A\na - Again\n"),
		[]byte("a - A (default)\nb - B (default)\n"),
		[]byte("a - A\x00\n"),
		[]byte("a - A\rb - B\n"),
		[]byte(strings.Repeat("x", provider.MaxModelValueBytes+1) + " - X\n"),
	}
	rows := make([]string, provider.MaxCatalogModels+1)
	for index := range rows {
		rows[index] = "m" + strings.Repeat("x", index%3) + string(rune(0x1000+index)) + " - Model"
	}
	cases = append(cases, []byte(strings.Join(rows, "\n")))
	for _, input := range cases {
		_, err := parseModelCatalog(input, false)
		require.Error(t, err)
	}
}

func TestReadinessUsesExactListModelsAndDefaultACPLaunch(t *testing.T) {
	driver, launcher, _ := testDriver(t)
	driver.catalog = provider.ModelCatalog{}
	readiness := driver.Readiness(context.Background())
	require.Equal(t, provider.Ready, readiness.State)
	require.Equal(t, "Model A", readiness.Model)
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	require.Len(t, launcher.requests, 2)
	require.Equal(t, driver.config.Executable, launcher.requests[0].Executable)
	require.Equal(t, driver.config.Environment, launcher.requests[0].Environment)
	require.Equal(t, []string{"--list-models"}, launcher.requests[0].Arguments)
	require.Equal(t, driver.config.ProviderRoot, launcher.requests[0].WorkingDirectory)
	require.Equal(t, []string{"--model", "model-a", "acp"}, launcher.requests[1].Arguments)
}

type blockingProbeReader struct{ released <-chan struct{} }

func (reader blockingProbeReader) Read([]byte) (int, error) {
	<-reader.released
	return 0, io.EOF
}

type blockingProbeChild struct {
	release    chan struct{}
	waited     chan struct{}
	once       sync.Once
	mu         sync.Mutex
	terminated int
	killed     int
}

func (child *blockingProbeChild) Input() io.WriteCloser { return discardCloser{io.Discard} }
func (child *blockingProbeChild) Output() io.Reader {
	return blockingProbeReader{released: child.release}
}
func (child *blockingProbeChild) Errors() io.Reader { return strings.NewReader("") }
func (child *blockingProbeChild) Wait() error {
	<-child.release
	child.once.Do(func() { close(child.waited) })
	return nil
}
func (child *blockingProbeChild) Terminate() error {
	child.mu.Lock()
	child.terminated++
	child.mu.Unlock()
	return nil
}
func (child *blockingProbeChild) Kill() error {
	child.mu.Lock()
	child.killed++
	child.mu.Unlock()
	select {
	case <-child.release:
	default:
		close(child.release)
	}
	return nil
}

type blockingProbeLauncher struct{ child *blockingProbeChild }

func (launcher blockingProbeLauncher) Launch(context.Context, common.ProcessRequest) (common.ManagedProcess, error) {
	return launcher.child, nil
}

func TestProbeDeadlineStillBoundsReadersAfterWait(t *testing.T) {
	driver, _, _ := testDriver(t)
	child := &blockingProbeChild{release: make(chan struct{}), waited: make(chan struct{})}
	driver.config.Launcher = blockingProbeLauncher{child: child}
	driver.probeTimeout = 20 * time.Millisecond
	started := time.Now()
	_, err := driver.probeCatalog(context.Background())
	require.Error(t, err)
	require.Less(t, time.Since(started), time.Second)
	select {
	case <-child.waited:
	default:
		t.Fatal("probe cleanup did not join the child")
	}
	child.mu.Lock()
	terminated, killed := child.terminated, child.killed
	child.mu.Unlock()
	require.Equal(t, 1, terminated)
	require.Equal(t, 1, killed)
}

type ownedTestChild struct {
	mu         sync.Mutex
	terminated int
}

func (*ownedTestChild) Input() io.WriteCloser { return discardCloser{io.Discard} }
func (*ownedTestChild) Output() io.Reader     { return strings.NewReader("") }
func (*ownedTestChild) Errors() io.Reader     { return strings.NewReader("") }
func (*ownedTestChild) Wait() error           { return nil }
func (child *ownedTestChild) Terminate() error {
	child.mu.Lock()
	child.terminated++
	child.mu.Unlock()
	return nil
}
func (*ownedTestChild) Kill() error { return nil }

func TestStableChildRetainsRetiredRuntimeForEscalationAndIsNilSafe(t *testing.T) {
	oldChild, currentChild := &ownedTestChild{}, &ownedTestChild{}
	session := &Session{rt: &runtime{child: currentChild}, retired: []*runtime{{child: oldChild}}}
	stable := &managedChild{session: session}
	require.NoError(t, stable.Terminate())
	oldChild.mu.Lock()
	oldCalls := oldChild.terminated
	oldChild.mu.Unlock()
	currentChild.mu.Lock()
	currentCalls := currentChild.terminated
	currentChild.mu.Unlock()
	require.Equal(t, 1, oldCalls)
	require.Equal(t, 1, currentCalls)
	var nilStable *managedChild
	require.NoError(t, nilStable.Terminate())
	require.NoError(t, nilStable.Kill())
	require.NoError(t, nilStable.Wait())
	require.NoError(t, nilStable.Input().Close())
}

func TestInspectUsesPersistedSelectionInsteadOfCatalogDefault(t *testing.T) {
	driver, launcher, _ := testDriver(t)
	configurePages(launcher, map[string]scriptedListPage{"": listPage("", listedSession("native-b", testOptions("model-a")))})
	ref, err := provider.NewNativeSessionRef("native-b")
	require.NoError(t, err)
	settings := provider.ExecutionSettings{Model: "model-b", Effort: "default", Speed: provider.SpeedStandard}
	inspected, err := driver.Inspect(context.Background(), provider.InspectRequest{Provider: provider.NameCursor, NativeSession: ref, Settings: &settings})
	require.NoError(t, err)
	require.Equal(t, "model-b", inspected.Model)
	require.Equal(t, settings, *inspected.Settings)
	require.Equal(t, "Model B", inspected.Presentation.ModelDisplayName)
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	require.Equal(t, []string{"--model", "model-b", "acp"}, launcher.requests[len(launcher.requests)-1].Arguments)
}

func TestRetiredRouterIgnoresLateMalformedFrameAfterSwap(t *testing.T) {
	old, current := &runtime{}, &runtime{}
	session := &Session{rt: old, runtimeGeneration: 1, phase: sessionRunning, permissions: map[string]*pendingPermission{}, permissionOutcomes: map[string]acp.Delivery{}}
	turn := &activePrompt{rejected: make(chan struct{}), accepted: make(chan struct{}), phase: turnRunning}
	session.active = turn
	router := &inboundRouter{}
	old.router = router
	router.publish(session, old, uint64(1))
	router.retire()
	session.mu.Lock()
	session.rt = current
	session.runtimeGeneration = 2
	session.mu.Unlock()
	router.handle(context.Background(), acp.Request{Method: "session/update", Params: []byte("{")})
	session.mu.Lock()
	defer session.mu.Unlock()
	require.Same(t, turn, session.active)
	require.False(t, turn.poisoned)
	require.Equal(t, sessionRunning, session.phase)
}

func TestCandidateDoneIsLinearizedAtCommitBoundary(t *testing.T) {
	for _, ordering := range []string{"exit-before-commit", "exit-after-commit"} {
		t.Run(ordering, func(t *testing.T) {
			driver, launcher, root := testDriver(t)
			launcher.scenarios = []scriptScenario{
				{newResult: map[string]any{"sessionId": "native-linear", "configOptions": testOptions("model-a")}},
				{loadResult: map[string]any{"sessionId": "native-linear", "configOptions": testOptions("model-b")}},
			}
			opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
			require.NoError(t, err)
			session := opened.(*Session)
			old := session.rt
			atBoundary := make(chan struct{})
			releaseBoundary := make(chan struct{})
			session.beforeCandidateCommit = func() {
				close(atBoundary)
				<-releaseBoundary
			}
			wanted := provider.ExecutionSettings{Model: "model-b", Effort: "default", Speed: provider.SpeedStandard}
			applied := make(chan error, 1)
			go func() { _, _, applyErr := session.ApplySettings(context.Background(), wanted); applied <- applyErr }()
			<-atBoundary
			launcher.mu.Lock()
			candidateChild := launcher.children[1]
			launcher.mu.Unlock()
			if ordering == "exit-before-commit" {
				candidateChild.stop()
				require.Eventually(t, func() bool {
					session.mu.Lock()
					defer session.mu.Unlock()
					for rt := range session.owned {
						if rt.child == candidateChild {
							return rt.candidateState == candidateEnded
						}
					}
					return false
				}, time.Second, time.Millisecond)
			}
			close(releaseBoundary)
			err = <-applied
			if ordering == "exit-before-commit" {
				require.Error(t, err)
				require.Same(t, old, session.rt)
				require.Equal(t, "model-a", session.Model())
			} else {
				require.NoError(t, err)
				candidateChild.stop()
				require.Eventually(t, func() bool {
					session.mu.Lock()
					defer session.mu.Unlock()
					return session.phase != sessionRunning
				}, time.Second, time.Millisecond)
				require.Equal(t, "model-b", session.Model())
			}
			_ = session.Shutdown(context.Background())
		})
	}
}

func TestOldRuntimeCleanupFailureRemainsStableOwned(t *testing.T) {
	driver, launcher, root := testDriver(t)
	launcher.scenarios = []scriptScenario{
		{newResult: map[string]any{"sessionId": "native-retain", "configOptions": testOptions("model-a")}},
		{loadResult: map[string]any{"sessionId": "native-retain", "configOptions": testOptions("model-b")}},
	}
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	require.NoError(t, err)
	session := opened.(*Session)
	launcher.mu.Lock()
	oldChild := launcher.children[0]
	oldChild.waitErr = errors.New("fixture cleanup failure")
	launcher.mu.Unlock()
	wanted := provider.ExecutionSettings{Model: "model-b", Effort: "default", Speed: provider.SpeedStandard}
	_, _, err = session.ApplySettings(context.Background(), wanted)
	require.NoError(t, err, "successful swap is independent of retired cleanup")
	snapshot := session.child.snapshot()
	require.Len(t, snapshot, 2)
	require.Contains(t, snapshot, oldChild, "failed old cleanup must remain owned for escalation")
	_ = session.Shutdown(context.Background())
}

func TestCandidateIsOwnedDuringBlockedInitializeAndShutdownEscalation(t *testing.T) {
	driver, launcher, root := testDriver(t)
	launcher.scenarios = []scriptScenario{{newResult: map[string]any{"sessionId": "native-owned", "configOptions": testOptions("model-a")}}}
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	require.NoError(t, err)
	session := opened.(*Session)
	gate, started := make(chan struct{}), make(chan struct{})
	launcher.mu.Lock()
	launcher.blockIndex, launcher.blockGate, launcher.blockStarted = 1, gate, started
	launcher.mu.Unlock()
	wanted := provider.ExecutionSettings{Model: "model-b", Effort: "default", Speed: provider.SpeedStandard}
	applied := make(chan error, 1)
	go func() { _, _, applyErr := session.ApplySettings(context.Background(), wanted); applied <- applyErr }()
	<-started
	require.Len(t, session.child.snapshot(), 2, "candidate must be stable-owned before initialize")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	require.Error(t, session.Shutdown(shutdownCtx))
	cancel()
	require.NoError(t, session.Child().Terminate())
	require.NoError(t, session.Child().Kill())
	close(gate)
	require.Error(t, <-applied)
	require.NoError(t, session.Child().Wait())
}

func TestSubmitModelChangeSwapsRuntimeAndPromptsOnce(t *testing.T) {
	driver, launcher, root := testDriver(t)
	launcher.scenarios = []scriptScenario{
		{newResult: map[string]any{"sessionId": "native-swap", "configOptions": testOptions("model-a")}},
		{loadResult: map[string]any{"sessionId": "native-swap", "configOptions": testOptions("model-b")}},
	}
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	require.NoError(t, err)
	session := opened.(*Session)
	stableChild := session.Child()
	wanted := provider.ExecutionSettings{Model: "model-b", Effort: "default", Speed: provider.SpeedStandard}
	accepted, err := session.Submit(context.Background(), provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("once"), Settings: &wanted})
	require.NoError(t, err)
	require.Equal(t, wanted, *accepted.Settings)
	require.Same(t, stableChild, session.Child())
	require.Equal(t, "native-swap", session.NativeSession().Ref.Value())
	require.Equal(t, wanted, *session.NativeSession().Settings)
	settingsEvent := <-session.Events()
	require.Equal(t, provider.EventSettings, settingsEvent.Kind)
	require.Empty(t, settingsEvent.TurnID, "idle settings must not imply prompt admission")
	userEvent := <-session.Events()
	require.Equal(t, provider.EventUserMessage, userEvent.Kind)
	launcher.mu.Lock()
	require.Len(t, launcher.children, 2)
	old, current := launcher.children[0], launcher.children[1]
	require.Equal(t, []string{"--model", "model-a", "acp"}, launcher.requests[0].Arguments)
	require.Equal(t, []string{"--model", "model-b", "acp"}, launcher.requests[1].Arguments)
	launcher.mu.Unlock()
	old.mu.Lock()
	oldPrompts := len(old.promptParams)
	old.mu.Unlock()
	current.mu.Lock()
	currentPrompts := len(current.promptParams)
	current.mu.Unlock()
	require.Zero(t, oldPrompts)
	require.Equal(t, 1, currentPrompts)
	select {
	case _, ok := <-session.Events():
		require.True(t, ok, "intentional old generation exit closed the session")
	default:
	}
	require.NoError(t, session.Shutdown(context.Background()))
}
