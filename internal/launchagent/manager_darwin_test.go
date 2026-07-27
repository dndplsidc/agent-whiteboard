//go:build darwin

package launchagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type runnerCall struct {
	command string
	args    []string
}

type runnerResult struct {
	output []byte
	err    error
}

type fakeRunner struct {
	calls   []runnerCall
	results []runnerResult
}

func (runner *fakeRunner) Run(_ context.Context, command string, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, runnerCall{command: command, args: append([]string(nil), args...)})
	if len(runner.results) == 0 {
		return nil, nil
	}
	result := runner.results[0]
	runner.results = runner.results[1:]
	return result.output, result.err
}

func TestInstallWritesPrivateFilesAndUsesExactLaunchctlSequence(t *testing.T) {
	home := testHome(t)
	runner := &fakeRunner{results: []runnerResult{{err: ErrNotLoaded}, {}, {}}}
	manager := newDarwinManager(runner, home, 501, defaultFileOps())
	config := validTestConfig(t, home, "first")

	if err := manager.Install(context.Background(), config); err != nil {
		t.Fatalf("install: %v", err)
	}
	paths := pathsForHome(home)
	assertMode(t, filepath.Dir(paths.Plist), 0o700)
	assertMode(t, filepath.Dir(paths.StdoutLog), 0o700)
	assertMode(t, paths.Plist, 0o600)
	assertMode(t, paths.StdoutLog, 0o600)
	assertMode(t, paths.StderrLog, 0o600)
	plist, err := os.ReadFile(paths.Plist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plist), "<key>AGENT_WHITEBOARD_PROVIDER_PI_EXECUTABLE</key>") {
		t.Fatal("install did not record the safely resolved Pi executable")
	}
	assertCalls(t, runner.calls, []runnerCall{
		{command: LaunchctlExecutable, args: []string{"print", "gui/501/" + Label}},
		{command: LaunchctlExecutable, args: []string{"bootstrap", "gui/501", paths.Plist}},
		{command: LaunchctlExecutable, args: []string{"kickstart", "-k", "gui/501/" + Label}},
	})
}

func TestInstallOmitsMissingRegisteredProvider(t *testing.T) {
	home := testHome(t)
	runner := &fakeRunner{results: []runnerResult{{err: ErrNotLoaded}, {}, {}}}
	manager := newDarwinManager(runner, home, 501, defaultFileOps())
	config := validTestConfig(t, home, "missing-provider")
	config.ExecutableResolver = testExecutableResolver{}
	if err := manager.Install(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	plist, err := os.ReadFile(pathsForHome(home).Plist)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plist), "EnvironmentVariables") || strings.Contains(string(plist), "AGENT_WHITEBOARD_PROVIDER_PI_EXECUTABLE") {
		t.Fatal("missing registered provider was recorded in plist")
	}
}

func TestInstallPreservesExistingLibraryMode(t *testing.T) {
	home := testHome(t)
	if err := os.Chmod(filepath.Join(home, "Library"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := newDarwinManager(&fakeRunner{results: []runnerResult{{err: ErrNotLoaded}, {}, {}}}, home, 501, defaultFileOps())
	if err := manager.Install(context.Background(), validTestConfig(t, home, "library-mode")); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(home, "Library"), 0o755)
}

func TestInstallUpdateBootsOutLoadedAgentBeforeReload(t *testing.T) {
	home := testHome(t)
	runner := &fakeRunner{results: []runnerResult{{output: []byte("state = running\npid = 42\n")}, {}, {}, {}}}
	manager := newDarwinManager(runner, home, 502, defaultFileOps())
	config := validTestConfig(t, home, "updated")

	if err := manager.Install(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	paths := pathsForHome(home)
	assertCalls(t, runner.calls, []runnerCall{
		{command: LaunchctlExecutable, args: []string{"print", "gui/502/" + Label}},
		{command: LaunchctlExecutable, args: []string{"bootout", "gui/502/" + Label}},
		{command: LaunchctlExecutable, args: []string{"bootstrap", "gui/502", paths.Plist}},
		{command: LaunchctlExecutable, args: []string{"kickstart", "-k", "gui/502/" + Label}},
	})
}

func TestInstallRejectsSymlinkedLaunchAgentsDirectoryWithoutInvokingRunner(t *testing.T) {
	home := testHome(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "Library", "LaunchAgents")); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	manager := newDarwinManager(runner, home, 503, defaultFileOps())

	if err := manager.Install(context.Background(), validTestConfig(t, home, "symlink")); err == nil {
		t.Fatal("expected symlinked LaunchAgents directory rejection")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner invoked after unsafe filesystem rejection: %#v", runner.calls)
	}
	if _, err := os.Stat(filepath.Join(outside, Label+".plist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("install wrote through directory symlink: %v", err)
	}
}

func TestInstallFailureBeforeRenamePreservesDurablePlistAndDoesNotRunLaunchctl(t *testing.T) {
	home := testHome(t)
	runner := &fakeRunner{results: []runnerResult{{err: ErrNotLoaded}, {}, {}}}
	manager := newDarwinManager(runner, home, 503, defaultFileOps())
	if err := manager.Install(context.Background(), validTestConfig(t, home, "old")); err != nil {
		t.Fatal(err)
	}
	paths := pathsForHome(home)
	oldContents, err := os.ReadFile(paths.Plist)
	if err != nil {
		t.Fatal(err)
	}

	manager.ops.syncFile = func(*os.File) error { return errors.New("injected sync failure") }
	runner.calls = nil
	if err := manager.Install(context.Background(), validTestConfig(t, home, "new")); err == nil {
		t.Fatal("expected install failure")
	}
	newContents, err := os.ReadFile(paths.Plist)
	if err != nil {
		t.Fatal(err)
	}
	if string(newContents) != string(oldContents) {
		t.Fatal("failed atomic update changed prior durable plist")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("launchctl invoked after failed plist write: %#v", runner.calls)
	}
}

func TestInstallReloadFailureIsClassifiedAndKeepsDurablePlist(t *testing.T) {
	home := testHome(t)
	runner := &fakeRunner{results: []runnerResult{{err: ErrNotLoaded}, {err: errors.New("bootstrap failed")}}}
	manager := newDarwinManager(runner, home, 504, defaultFileOps())
	config := validTestConfig(t, home, "reload-failure")

	err := manager.Install(context.Background(), config)
	if !errors.Is(err, ErrInstalledNotRunning) {
		t.Fatalf("install error = %v, want installed-not-running classification", err)
	}
	contents, readErr := os.ReadFile(pathsForHome(home).Plist)
	if readErr != nil {
		t.Fatalf("durable plist missing after reload failure: %v", readErr)
	}
	if !strings.Contains(string(contents), config.ConfigPath) {
		t.Fatal("durable plist does not contain updated configuration")
	}
}

func TestStatusCombinesPlistExistenceAndRedactedLaunchctlState(t *testing.T) {
	home := testHome(t)
	paths := pathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.Plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Plist, []byte("plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{results: []runnerResult{{output: []byte(`gui/505/com.agent-whiteboard.local-agent = {
	path = /secret/path/agent-whiteboard
	arguments = {
		--credential=do-not-expose
	}
	state = running
	pid = 9182
}`)}}}
	manager := newDarwinManager(runner, home, 505, defaultFileOps())

	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Status{Installed: true, Loaded: true, Running: true, PID: 9182}
	if status != want {
		t.Fatalf("status = %#v, want %#v", status, want)
	}
	if strings.Contains(fmt.Sprintf("%#v", status), "secret") || strings.Contains(fmt.Sprintf("%#v", status), "credential") {
		t.Fatal("status exposed launch arguments")
	}
	assertCalls(t, runner.calls, []runnerCall{{command: LaunchctlExecutable, args: []string{"print", "gui/505/" + Label}}})
}

func TestStatusDistinguishesNotLoadedFromArbitraryFailure(t *testing.T) {
	home := testHome(t)
	paths := pathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.Plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Plist, []byte("plist"), 0o600); err != nil {
		t.Fatal(err)
	}

	notLoadedRunner := &fakeRunner{results: []runnerResult{{err: ErrNotLoaded}}}
	manager := newDarwinManager(notLoadedRunner, home, 506, defaultFileOps())
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != (Status{Installed: true}) {
		t.Fatalf("status = %#v", status)
	}

	arbitrary := errors.New("permission denied")
	arbitraryRunner := &fakeRunner{results: []runnerResult{{err: arbitrary}}}
	manager = newDarwinManager(arbitraryRunner, home, 506, defaultFileOps())
	if _, err := manager.Status(context.Background()); !errors.Is(err, arbitrary) {
		t.Fatalf("status error = %v, want propagated arbitrary error", err)
	}

	canceledRunner := &fakeRunner{results: []runnerResult{{output: []byte("Could not find service"), err: context.Canceled}}}
	manager = newDarwinManager(canceledRunner, home, 506, defaultFileOps())
	if _, err := manager.Status(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("status cancellation = %v, want context.Canceled", err)
	}
}

func TestInstallPropagatesArbitraryProbeFailure(t *testing.T) {
	home := testHome(t)
	failure := errors.New("launchctl permission denied")
	runner := &fakeRunner{results: []runnerResult{{err: failure}}}
	manager := newDarwinManager(runner, home, 506, defaultFileOps())
	if err := manager.Install(context.Background(), validTestConfig(t, home, "probe-error")); !errors.Is(err, failure) {
		t.Fatalf("install error = %v, want propagated probe failure", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].args[0] != "print" {
		t.Fatalf("calls after failed probe = %#v", runner.calls)
	}
}

func TestParseLaunchctlStatusPIDEdges(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		running bool
		pid     int
		wantErr bool
	}{
		{name: "running positive pid", output: "state = running\npid = 42", running: true, pid: 42},
		{name: "stopped stale pid", output: "state = stopped\npid = 42"},
		{name: "running missing pid", output: "state = running", wantErr: true},
		{name: "running zero pid", output: "state = running\npid = 0", wantErr: true},
		{name: "running negative pid", output: "state = running\npid = -2", wantErr: true},
		{name: "running malformed pid", output: "state = running\npid = nope", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			running, pid, err := parseLaunchctlStatus([]byte(test.output))
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
			if running != test.running || pid != test.pid {
				t.Fatalf("status = (%v, %d), want (%v, %d)", running, pid, test.running, test.pid)
			}
		})
	}
}

func TestRestartRequiresInstalledPlistAndUsesExactSequence(t *testing.T) {
	home := testHome(t)
	runner := &fakeRunner{}
	manager := newDarwinManager(runner, home, 507, defaultFileOps())
	if err := manager.Restart(context.Background()); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("restart without plist = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("restart invoked launchctl without installation: %#v", runner.calls)
	}

	paths := pathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.Plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Plist, []byte("plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, runner.calls, []runnerCall{
		{command: LaunchctlExecutable, args: []string{"bootout", "gui/507/" + Label}},
		{command: LaunchctlExecutable, args: []string{"bootstrap", "gui/507", paths.Plist}},
		{command: LaunchctlExecutable, args: []string{"kickstart", "-k", "gui/507/" + Label}},
	})
}

func TestStopAndUninstallAreIdempotent(t *testing.T) {
	home := testHome(t)
	runner := &fakeRunner{results: []runnerResult{
		{output: []byte("Could not find service"), err: errors.New("exit 3")},
		{output: []byte("Could not find service"), err: errors.New("exit 3")},
		{output: []byte("Could not find service"), err: errors.New("exit 3")},
	}}
	manager := newDarwinManager(runner, home, 508, defaultFileOps())
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if err := manager.Uninstall(context.Background()); err != nil {
		t.Fatalf("uninstall absent plist: %v", err)
	}
	wantCall := runnerCall{command: LaunchctlExecutable, args: []string{"bootout", "gui/508/" + Label}}
	assertCalls(t, runner.calls, []runnerCall{wantCall, wantCall, wantCall})
}

func TestStopLeavesPlistAndUninstallDurablyRemovesIt(t *testing.T) {
	home := testHome(t)
	paths := pathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.Plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Plist, []byte("plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	manager := newDarwinManager(runner, home, 509, defaultFileOps())

	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.Plist); err != nil {
		t.Fatalf("stop removed plist: %v", err)
	}
	if err := manager.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.Plist); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uninstall did not remove plist: %v", err)
	}
}

func TestDurableRemovalFailureRestoresInstalledPlist(t *testing.T) {
	home := testHome(t)
	paths := pathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.Plist), 0o700); err != nil {
		t.Fatal(err)
	}
	old := []byte("prior durable plist")
	if err := os.WriteFile(paths.Plist, old, 0o600); err != nil {
		t.Fatal(err)
	}
	ops := defaultFileOps()
	calls := 0
	originalSyncDir := ops.syncDir
	ops.syncDir = func(directory *os.File) error {
		calls++
		if calls == 1 {
			return errors.New("injected directory sync failure")
		}
		return originalSyncDir(directory)
	}
	manager := newDarwinManager(&fakeRunner{}, home, 510, ops)

	if err := manager.Uninstall(context.Background()); err == nil {
		t.Fatal("expected durable removal failure")
	}
	contents, err := os.ReadFile(paths.Plist)
	if err != nil {
		t.Fatalf("failed removal did not restore plist: %v", err)
	}
	if string(contents) != string(old) {
		t.Fatalf("restored plist = %q, want %q", contents, old)
	}
}

func TestAtomicPublicationDetectsLaunchAgentsPathSwap(t *testing.T) {
	home := testHome(t)
	paths := pathsForHome(home)
	if err := os.Mkdir(filepath.Dir(paths.Plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.StdoutLog), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.StdoutLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.StderrLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ops := defaultFileOps()
	originalRename := ops.renameAt
	moved := filepath.Join(home, "Library", "LaunchAgents-old")
	ops.renameAt = func(fromFD int, from string, toFD int, to string) error {
		if err := os.Rename(filepath.Dir(paths.Plist), moved); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Dir(paths.Plist), 0o700); err != nil {
			return err
		}
		return originalRename(fromFD, from, toFD, to)
	}
	runner := &fakeRunner{}
	manager := newDarwinManager(runner, home, 510, ops)
	if err := manager.Install(context.Background(), validTestConfig(t, home, "swap")); err == nil || !strings.Contains(err.Error(), "changed during operation") {
		t.Fatalf("install path-swap error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("launchctl invoked after path swap: %#v", runner.calls)
	}
	if _, err := os.Stat(paths.Plist); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement path received plist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, filepath.Base(paths.Plist))); err != nil {
		t.Fatalf("verified directory handle did not receive atomic plist: %v", err)
	}
}

func TestOwnershipMismatchAndUncertainRecoveryAreRejected(t *testing.T) {
	home := testHome(t)
	paths := pathsForHome(home)
	if err := os.Mkdir(filepath.Dir(paths.Plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Plist, []byte("plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := openLaunchAgents(home, false)
	if err != nil {
		t.Fatal(err)
	}
	directory.uid++
	if _, err := installedPlist(directory, filepath.Base(paths.Plist)); err == nil || !strings.Contains(err.Error(), "owned by the current user") {
		t.Fatalf("ownership mismatch error = %v", err)
	}
	directory.close()

	runner := &fakeRunner{results: []runnerResult{{err: ErrNotLoaded}, {}, {}}}
	manager := newDarwinManager(runner, home, 510, defaultFileOps())
	if err := manager.Install(context.Background(), validTestConfig(t, home, "old")); err != nil {
		t.Fatal(err)
	}
	ops := defaultFileOps()
	ops.syncDir = func(*os.File) error { return errors.New("persistent directory sync failure") }
	manager.ops = ops
	runner.calls = nil
	err = manager.Install(context.Background(), validTestConfig(t, home, "new"))
	var uncertain *CommitUncertainError
	if !errors.As(err, &uncertain) {
		t.Fatalf("error = %v, want CommitUncertainError", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("launchctl invoked after uncertain commit: %#v", runner.calls)
	}
}

func TestNewManagerUsesTemporaryHOMEAndNeverRealLaunchAgents(t *testing.T) {
	home := testHome(t)
	t.Setenv("HOME", home)
	runner := &fakeRunner{results: []runnerResult{{err: ErrNotLoaded}, {}, {}}}
	manager, err := NewManager(runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(context.Background(), validTestConfig(t, home, "isolated")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pathsForHome(home).Plist); err != nil {
		t.Fatalf("temporary HOME plist missing: %v", err)
	}
}

func validTestConfig(t *testing.T, home, suffix string) Config {
	t.Helper()
	provider := testRegularFile(t, filepath.Join(home, "providers", "pi"), 0o700)
	return Config{
		Executable:         testRegularFile(t, filepath.Join(home, "bin", "agent-whiteboard"), 0o700),
		ConfigPath:         testRegularFile(t, filepath.Join(home, "config-"+suffix+".yaml"), 0o600),
		Providers:          []ProviderDescriptor{testProviderDescriptor{name: "pi", executable: "pi"}},
		ExecutableResolver: testExecutableResolver{paths: map[string]string{"pi": provider}},
	}
}

func testHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "Library"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode for %s = %#o, want %#o", path, got, want)
	}
}

func assertCalls(t *testing.T, got, want []runnerCall) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}
