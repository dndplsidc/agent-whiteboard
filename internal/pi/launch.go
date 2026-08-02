package pi

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

const contentOnlySystemPrompt = "Answer only from content supplied in user messages. No tools or external resources are available."

type launchConfig struct {
	Executable  string
	Environment []string
}

func buildLaunchRequest(config launchConfig, workspace, sessionsDirectory, sessionFile string) (provider.LaunchRequest, error) {
	base := filepath.Base(sessionFile)
	ref := strings.TrimSuffix(base, ".jsonl")
	if config.Environment == nil || !validLaunchPath(config.Executable) || !validLaunchPath(workspace) || !validLaunchPath(sessionsDirectory) || !validLaunchPath(sessionFile) || filepath.Dir(sessionFile) != sessionsDirectory || ref == base || common.ValidateID(ref) != nil {
		return provider.LaunchRequest{}, errors.New("invalid Pi launch configuration")
	}
	request := provider.LaunchRequest{
		Executable: config.Executable,
		Arguments: []string{
			"--mode", "rpc",
			"--system-prompt", contentOnlySystemPrompt,
			"--no-tools", "--no-extensions", "--no-skills", "--no-prompt-templates",
			"--no-context-files", "--no-themes", "--no-approve", "--offline",
			"--session-dir", sessionsDirectory,
			"--session", sessionFile,
		},
		Environment:      slices.Clone(config.Environment),
		WorkingDirectory: workspace,
	}
	if err := request.Validate(); err != nil {
		return provider.LaunchRequest{}, errors.New("invalid Pi launch request")
	}
	return request, nil
}

func validLaunchPath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && len(path) <= provider.MaxNativeReferenceBytes
}
