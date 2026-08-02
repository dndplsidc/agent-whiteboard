package pi

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildLaunchRequestIsExactAndClonesEnvironment(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "pi")
	workspace := filepath.Join(root, "workspace")
	sessions := filepath.Join(root, "sessions")
	session := filepath.Join(sessions, "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4.jsonl")
	environment := []string{"HOME=/private/home", "API_TOKEN=secret"}
	request, err := buildLaunchRequest(launchConfig{Executable: executable, Environment: environment}, workspace, sessions, session)
	require.NoError(t, err)
	require.Equal(t, executable, request.Executable)
	require.Equal(t, workspace, request.WorkingDirectory)
	require.Equal(t, []string{
		"--mode", "rpc", "--system-prompt", contentOnlySystemPrompt,
		"--no-tools", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-themes", "--no-approve", "--offline",
		"--session-dir", sessions, "--session", session,
	}, request.Arguments)
	require.Equal(t, environment, request.Environment)
	environment[0] = "CHANGED=yes"
	require.Equal(t, "HOME=/private/home", request.Environment[0])
	require.NoError(t, request.Validate())
}

func TestBuildLaunchRequestRejectsNilEnvironmentAndMismatchedSessionDirectory(t *testing.T) {
	root := t.TempDir()
	_, err := buildLaunchRequest(launchConfig{Executable: filepath.Join(root, "pi")}, filepath.Join(root, "workspace"), filepath.Join(root, "sessions"), filepath.Join(root, "sessions", "session.jsonl"))
	require.Error(t, err)
	_, err = buildLaunchRequest(launchConfig{Executable: filepath.Join(root, "pi"), Environment: []string{}}, filepath.Join(root, "workspace"), filepath.Join(root, "sessions"), filepath.Join(root, "other", "session.jsonl"))
	require.Error(t, err)
}
