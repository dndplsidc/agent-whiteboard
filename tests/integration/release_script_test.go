package integration

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReleaseScriptCreatesOneAnnotatedSemanticVersionTag(t *testing.T) {
	repository := newReleaseRepository(t)
	script := releaseScriptPath(t)

	invalidVersions := []struct {
		version string
		message string
	}{
		{version: "1.2.3", message: "semantic version prefixed with v"},
		{version: "v1.2.3-01", message: "numeric prerelease identifiers must not contain leading zeroes"},
		{version: "v1.2.3-alpha.01", message: "numeric prerelease identifiers must not contain leading zeroes"},
	}
	for _, test := range invalidVersions {
		invalid := exec.Command(script, test.version)
		invalid.Dir = repository
		invalidOutput, err := invalid.CombinedOutput()
		require.Error(t, err, test.version)
		require.Contains(t, string(invalidOutput), test.message, test.version)
	}

	validPrerelease := exec.Command(script, "v1.2.3-alpha--beta.1")
	validPrerelease.Dir = repository
	validPrereleaseOutput, err := validPrerelease.CombinedOutput()
	require.NoError(t, err, string(validPrereleaseOutput))
	require.Equal(t, "tag\n", releaseGit(t, repository, "cat-file", "-t", "refs/tags/v1.2.3-alpha--beta.1"))

	require.NoError(t, os.WriteFile(filepath.Join(repository, "dirty.txt"), []byte("dirty"), 0o600))
	dirty := exec.Command(script, "v0.1.0")
	dirty.Dir = repository
	dirtyOutput, err := dirty.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(dirtyOutput), "clean Git worktree")
	require.NoError(t, os.Remove(filepath.Join(repository, "dirty.txt")))

	create := exec.Command(script, "v0.1.0")
	create.Dir = repository
	createOutput, err := create.CombinedOutput()
	require.NoError(t, err, string(createOutput))
	require.Contains(t, string(createOutput), "Created annotated tag v0.1.0")
	require.Equal(t, "tag\n", releaseGit(t, repository, "cat-file", "-t", "refs/tags/v0.1.0"))
	require.Equal(t, releaseGit(t, repository, "rev-parse", "HEAD"), releaseGit(t, repository, "rev-list", "-n", "1", "v0.1.0"))

	repeated := exec.Command(script, "v0.1.0")
	repeated.Dir = repository
	repeatedOutput, err := repeated.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(repeatedOutput), "already exists")
}

func newReleaseRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	releaseGit(t, repository, "init", "--initial-branch=main")
	releaseGit(t, repository, "config", "user.name", "Release Test")
	releaseGit(t, repository, "config", "user.email", "release@example.test")
	require.NoError(t, os.WriteFile(filepath.Join(repository, "README.md"), []byte("release fixture\n"), 0o600))
	releaseGit(t, repository, "add", "README.md")
	releaseGit(t, repository, "commit", "--message", "initial")
	return repository
}

func releaseScriptPath(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", "scripts", "release.sh"))
}

func releaseGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	require.NoError(t, command.Run(), stderr.String())
	return stdout.String()
}
