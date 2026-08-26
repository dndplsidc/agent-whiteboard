package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestBuildTurnInputUsesTextThenOrderedLocalImages(t *testing.T) {
	workspace := t.TempDir()
	imagesDirectory := filepath.Join(workspace, ".agent-images")
	require.NoError(t, os.Mkdir(imagesDirectory, 0o700))
	first := filepath.Join(imagesDirectory, strings.Repeat("A", 32)+".png")
	second := filepath.Join(imagesDirectory, strings.Repeat("B", 32)+".webp")
	require.NoError(t, os.WriteFile(first, []byte("first"), 0o600))
	require.NoError(t, os.WriteFile(second, []byte("second"), 0o600))

	input, err := buildTurnInput(workspace, []byte("envelope"), nil, []provider.ImageInput{
		{ID: strings.Repeat("A", 32), Name: "first.png", MediaType: "image/png", Bytes: 5, Path: first},
		{ID: strings.Repeat("B", 32), Name: "second.webp", MediaType: "image/webp", Bytes: 6, Path: second},
	})
	require.NoError(t, err)
	require.Equal(t, []turnInput{
		{Type: "text", Text: "envelope"},
		{Type: "localImage", Path: first},
		{Type: "localImage", Path: second},
	}, input)
}

func TestBuildTurnInputRejectsPathsOutsideWorkspaceAndSymlinks(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.png")
	require.NoError(t, os.WriteFile(outside, []byte("image"), 0o600))
	insideLink := filepath.Join(workspace, "link.png")
	require.NoError(t, os.Symlink(outside, insideLink))

	for _, path := range []string{outside, insideLink} {
		_, err := buildTurnInput(workspace, []byte("envelope"), nil, []provider.ImageInput{{ID: strings.Repeat("A", 32), Name: "image.png", MediaType: "image/png", Bytes: 5, Path: path}})
		require.Error(t, err)
	}
}

func TestModelCatalogDefaultsMissingModalitiesAndHonorsTextOnly(t *testing.T) {
	models, cursor, err := parseModelCatalogPage([]byte(`{"data":[
		{"id":"default-id","model":"default-model","displayName":"Default","description":"default","hidden":false,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}]},
		{"id":"text-id","model":"text-model","displayName":"Text","description":"text","hidden":false,"isDefault":false,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}],"inputModalities":["text"]},
		{"id":"vision-id","model":"vision-model","displayName":"Vision","description":"vision","hidden":false,"isDefault":false,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}],"inputModalities":["text","image"]}
	],"nextCursor":null}`))
	require.NoError(t, err)
	require.Empty(t, cursor)
	require.True(t, models[0].Capabilities.Images)
	require.False(t, models[1].Capabilities.Images)
	require.True(t, models[2].Capabilities.Images)
}

func TestModelCatalogRejectsMalformedModalities(t *testing.T) {
	for _, raw := range []string{
		`{"data":null}`,
		`{"data":[{"id":"model","model":"model","displayName":"Model","description":"desc","hidden":false,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}],"inputModalities":null}]}`,
		`{"data":[{"id":"model","model":"model","displayName":"Model","description":"desc","hidden":false,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}],"inputModalities":["bogus"]}]}`,
	} {
		_, _, err := parseModelCatalogPage([]byte(raw))
		require.Error(t, err)
	}
}

func TestSessionSubmitSendsStableLocalImageInputs(t *testing.T) {
	workspace := t.TempDir()
	imagesDirectory := filepath.Join(workspace, ".agent-images")
	require.NoError(t, os.Mkdir(imagesDirectory, 0o700))
	path := filepath.Join(imagesDirectory, "image.png")
	require.NoError(t, os.WriteFile(path, []byte("image"), 0o600))
	runtime, requests, child := pipeRuntime(t)
	catalog := testNativeCatalog(t)
	runtime.catalog = catalog
	go runtime.readLoop(child.Output())
	driver := &Driver{config: Config{Clock: fixedClock{time.Unix(10, 0).UTC()}, IDs: &sequenceIDs{}, IdleTimeout: time.Hour}, runtime: runtime}
	session := &Session{
		driver: driver, runtime: runtime, threadID: "native-thread", workspace: workspace,
		capabilities: provider.Capabilities{Images: true}, catalog: catalog, events: make(chan provider.Event, 8), view: newSessionChild(),
		activities: make(map[string]string), toolStates: make(map[string]provider.ToolActivity), interactions: make(map[string]nativeInteraction),
	}
	runtime.sessions[session.threadID] = session
	settings := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	request := provider.TurnRequest{
		TurnID: testID(900), MessageID: testID(901), Content: provider.TextMessage("compare"), Settings: &settings,
		Images: []provider.ImageInput{{ID: strings.Repeat("A", 32), Name: "image.png", MediaType: "image/png", Bytes: 5, Path: path}},
	}
	result := make(chan error, 1)
	go func() { _, submitErr := session.Submit(context.Background(), request); result <- submitErr }()
	native := <-requests
	var method string
	require.NoError(t, json.Unmarshal(native["method"], &method))
	require.Equal(t, "turn/start", method)
	var params struct {
		Input []turnInput `json:"input"`
	}
	require.NoError(t, json.Unmarshal(native["params"], &params))
	require.Len(t, params.Input, 2)
	require.Equal(t, "text", params.Input[0].Type)
	require.Equal(t, turnInput{Type: "localImage", Path: path}, params.Input[1])
	child.send(t, map[string]any{"id": native["id"], "result": map[string]any{"turn": map[string]any{"id": "native-turn"}}})
	require.NoError(t, <-result)
}
