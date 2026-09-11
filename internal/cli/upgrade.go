package cli

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const (
	upgradeExecutableName = "agent-whiteboard"
	upgradeModule         = "github.com/dndplsidc/agent-whiteboard"
	upgradePackage        = upgradeModule + "/cmd/agent-whiteboard"
)

type upgradeResolutionOutput struct {
	Path     string   `json:"Path"`
	Version  string   `json:"Version"`
	Query    string   `json:"Query"`
	Versions []string `json:"Versions"`
}

type upgradeError struct{ message string }

func (err upgradeError) Error() string { return err.message }

func (factory commandFactory) newUpgradeCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "upgrade",
		Short:       "Upgrade Agent Whiteboard to the latest release",
		Args:        usageArgs(cobra.NoArgs),
		Annotations: map[string]string{handlesConfigurationAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return factory.runUpgrade(cmd.Context())
		},
	}
}

func (factory commandFactory) runUpgrade(ctx context.Context) error {
	executable, err := factory.resolveExecutablePath()
	if err != nil {
		return err
	}
	if name := filepath.Base(executable); name != upgradeExecutableName {
		return upgradeError{message: `cannot upgrade executable named "` + name + `"; expected "` + upgradeExecutableName + `"`}
	}

	goExecutable, err := factory.deps.LookPath("go")
	if err != nil {
		return upgradeError{message: "cannot upgrade Agent Whiteboard: Go is not available on PATH"}
	}
	currentVersion, err := factory.deps.ReadBuildVersion(executable)
	if err != nil {
		return upgradeError{message: "cannot determine current Agent Whiteboard version: " + err.Error()}
	}

	resolution, err := factory.resolveLatestUpgrade(ctx, goExecutable)
	if err != nil {
		return err
	}
	currentIndex, targetIndex := versionIndex(resolution.Versions, currentVersion), versionIndex(resolution.Versions, resolution.Version)
	if currentIndex < 0 {
		return upgradeError{message: "cannot verify current Agent Whiteboard version " + currentVersion + " against available releases"}
	}
	if targetIndex < 0 {
		return upgradeError{message: "resolve latest Agent Whiteboard release: unexpected module metadata"}
	}
	if currentIndex > targetIndex {
		return upgradeError{message: "refusing to downgrade Agent Whiteboard from " + currentVersion + " to " + resolution.Version}
	}
	if currentIndex == targetIndex {
		return writeUpgradeSuccess(factory.deps.Stdout, factory.root.json, resolution.Version, true)
	}

	stagingDirectory, err := os.MkdirTemp(filepath.Dir(executable), ".agent-whiteboard-upgrade-")
	if err != nil {
		return upgradeError{message: "stage Agent Whiteboard upgrade: " + err.Error()}
	}
	defer os.RemoveAll(stagingDirectory)

	installEnvironment := withEnvironmentValue(factory.deps.Environ(), "GOBIN", stagingDirectory)
	stdout, stderr := factory.deps.Stderr, factory.deps.Stderr
	var installerOutput bytes.Buffer
	if factory.root.json {
		stdout, stderr = &installerOutput, &installerOutput
	}
	if err := factory.deps.RunCommand(ctx, goExecutable, []string{"install", upgradePackage + "@" + resolution.Version}, installEnvironment, stdout, stderr); err != nil {
		return upgradeCommandError("upgrade Agent Whiteboard", err, installerOutput.String())
	}

	stagedExecutable := filepath.Join(stagingDirectory, upgradeExecutableName)
	stagedVersion, err := factory.deps.ReadBuildVersion(stagedExecutable)
	if err != nil {
		return upgradeError{message: "verify downloaded Agent Whiteboard: " + err.Error()}
	}
	if stagedVersion != resolution.Version {
		return upgradeError{message: "downloaded Agent Whiteboard version " + stagedVersion + "; expected " + resolution.Version}
	}
	if err := os.Rename(stagedExecutable, executable); err != nil {
		return upgradeError{message: "replace Agent Whiteboard executable: " + err.Error()}
	}
	return writeUpgradeSuccess(factory.deps.Stdout, factory.root.json, resolution.Version, false)
}

func (factory commandFactory) resolveLatestUpgrade(ctx context.Context, goExecutable string) (upgradeResolutionOutput, error) {
	var stdout, stderr bytes.Buffer
	environment := withEnvironmentValue(factory.deps.Environ(), "GOPROXY", "direct")
	err := factory.deps.RunCommand(ctx, goExecutable, []string{"list", "-m", "-versions", "-json", upgradeModule + "@latest"}, environment, &stdout, &stderr)
	if err != nil {
		return upgradeResolutionOutput{}, upgradeCommandError("resolve latest Agent Whiteboard release", err, stderr.String())
	}
	var resolution upgradeResolutionOutput
	decoder := json.NewDecoder(&stdout)
	if err := decoder.Decode(&resolution); err != nil {
		return upgradeResolutionOutput{}, upgradeError{message: "resolve latest Agent Whiteboard release: unexpected module metadata"}
	}
	if resolution.Path != upgradeModule || resolution.Query != "latest" || resolution.Version == "" || len(resolution.Versions) == 0 {
		return upgradeResolutionOutput{}, upgradeError{message: "resolve latest Agent Whiteboard release: unexpected module metadata"}
	}
	return resolution, nil
}

func versionIndex(versions []string, target string) int {
	for index, version := range versions {
		if version == target {
			return index
		}
	}
	return -1
}

func upgradeCommandError(action string, err error, output string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if detail := strings.Join(strings.Fields(output), " "); detail != "" {
		return upgradeError{message: action + ": " + detail}
	}
	return upgradeError{message: action + ": " + err.Error()}
}

func readBuildVersion(path string) (string, error) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", err
	}
	if info.Path != upgradePackage || info.Main.Version == "" {
		return "", fmt.Errorf("unexpected build metadata")
	}
	return info.Main.Version, nil
}

func withEnvironmentValue(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func writeUpgradeSuccess(writer io.Writer, jsonMode bool, version string, alreadyCurrent bool) error {
	if jsonMode {
		return writeDeleteSuccess(writer, true)
	}
	message := "Agent Whiteboard upgraded to " + version + ".\n"
	if alreadyCurrent {
		message = "Agent Whiteboard is already up to date at " + version + ".\n"
	}
	_, err := io.WriteString(writer, message)
	return err
}
