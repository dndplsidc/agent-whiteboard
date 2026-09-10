package cli

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const (
	upgradeExecutableName = "agent-whiteboard"
	upgradePackage        = "github.com/dndplsidc/agent-whiteboard/cmd/agent-whiteboard@latest"
)

type upgradeError struct{ message string }

func (err upgradeError) Error() string { return err.message }

func (factory commandFactory) newUpgradeCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "upgrade",
		Short:       "Upgrade Agent Whiteboard to the latest release",
		Args:        usageArgs(cobra.NoArgs),
		Annotations: map[string]string{handlesConfigurationAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
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

			environment := withEnvironmentValue(factory.deps.Environ(), "GOBIN", filepath.Dir(executable))
			stdout, stderr := factory.deps.Stderr, factory.deps.Stderr
			var installerOutput bytes.Buffer
			if factory.root.json {
				stdout, stderr = &installerOutput, &installerOutput
			}
			if err := factory.deps.RunCommand(cmd.Context(), goExecutable, []string{"install", upgradePackage}, environment, stdout, stderr); err != nil {
				if detail := strings.Join(strings.Fields(installerOutput.String()), " "); detail != "" {
					return upgradeError{message: "upgrade Agent Whiteboard: " + detail}
				}
				return upgradeError{message: "upgrade Agent Whiteboard: " + err.Error()}
			}
			return writeUpgradeSuccess(factory.deps.Stdout, factory.root.json)
		},
	}
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

func writeUpgradeSuccess(writer io.Writer, jsonMode bool) error {
	if jsonMode {
		return writeDeleteSuccess(writer, true)
	}
	_, err := io.WriteString(writer, "Agent Whiteboard upgraded successfully.\n")
	return err
}
