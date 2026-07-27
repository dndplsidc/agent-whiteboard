//go:build darwin || linux

package processgroup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const helperEnvironment = "AGENT_WHITEBOARD_PROCESSGROUP_HELPER=1"

func TestLaunchUsesExactSpecification(t *testing.T) {
	request := helperRequest(t, "report", "first", "second")
	request.Environment = []string{helperEnvironment, "EXPLICIT=value"}
	t.Setenv("MUST_NOT_BE_INHERITED", "secret")

	child, err := NewLauncher().Launch(context.Background(), request)
	require.NoError(t, err)
	require.Same(t, child.Input(), child.Input())
	require.Same(t, child.Output(), child.Output())
	require.Same(t, child.Errors(), child.Errors())

	output, err := readAll(child.Output())
	require.NoError(t, err)
	require.NoError(t, child.Wait())
	_, err = child.Input().Write([]byte("closed"))
	require.ErrorIs(t, err, os.ErrClosed)
	_, err = child.Output().Read(make([]byte, 1))
	require.True(t, errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed))
	_, err = child.Errors().Read(make([]byte, 1))
	require.True(t, errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed))

	fields := strings.Split(strings.TrimSpace(output), "\n")
	require.Equal(t, []string{
		request.WorkingDirectory,
		"first\x00second",
		helperEnvironment + "\x00EXPLICIT=value",
	}, fields)
}

func TestLaunchPreservesExplicitEmptyEnvironment(t *testing.T) {
	const sentinel = "AGENT_WHITEBOARD_MUST_NOT_BE_INHERITED"
	t.Setenv(sentinel, "broker-secret")
	request := helperRequest(t, "report-empty-environment", sentinel)
	request.Environment = []string{}

	child, err := NewLauncher().Launch(context.Background(), request)
	require.NoError(t, err)
	require.NotNil(t, child.(*managedChild).command.Env)
	require.Empty(t, child.(*managedChild).command.Env)
	require.NoError(t, child.Wait())
	output, err := io.ReadAll(child.Output())
	require.NoError(t, err)
	require.Equal(t, "false\n0\n", string(output))
}

func TestWaitThenReadDrainsFinalOutput(t *testing.T) {
	child, err := NewLauncher().Launch(context.Background(), helperRequest(t, "final-output"))
	require.NoError(t, err)
	require.NoError(t, child.Wait())

	output, err := io.ReadAll(child.Output())
	require.NoError(t, err)
	errorsOutput, err := io.ReadAll(child.Errors())
	require.NoError(t, err)
	require.Equal(t, "FINAL stdout\n", string(output))
	require.Equal(t, "FINAL stderr\n", string(errorsOutput))
}

func TestLaunchRejectsInvalidSpecification(t *testing.T) {
	valid := helperRequest(t, "wait-stdin")
	tests := []struct {
		name string
		edit func(*provider.LaunchRequest)
		kind ErrorKind
	}{
		{name: "contract", edit: func(r *provider.LaunchRequest) { r.Environment = nil }, kind: ErrorInvalidRequest},
		{name: "relative executable", edit: func(r *provider.LaunchRequest) { r.Executable = "relative" }, kind: ErrorInvalidRequest},
		{name: "missing executable", edit: func(r *provider.LaunchRequest) { r.Executable = filepath.Join(r.WorkingDirectory, "missing") }, kind: ErrorExecutable},
		{name: "directory executable", edit: func(r *provider.LaunchRequest) { r.Executable = r.WorkingDirectory }, kind: ErrorExecutable},
		{name: "non-executable file", edit: func(r *provider.LaunchRequest) {
			path := filepath.Join(r.WorkingDirectory, "non-executable")
			require.NoError(t, os.WriteFile(path, []byte("data"), 0o600))
			r.Executable = path
		}, kind: ErrorExecutable},
		{name: "missing cwd", edit: func(r *provider.LaunchRequest) { r.WorkingDirectory = filepath.Join(r.WorkingDirectory, "missing") }, kind: ErrorWorkingDirectory},
		{name: "permissive cwd", edit: func(r *provider.LaunchRequest) { require.NoError(t, os.Chmod(r.WorkingDirectory, 0o755)) }, kind: ErrorWorkingDirectory},
		{name: "symlink cwd", edit: func(r *provider.LaunchRequest) {
			link := filepath.Join(filepath.Dir(r.WorkingDirectory), "workspace-link")
			require.NoError(t, os.Symlink(r.WorkingDirectory, link))
			r.WorkingDirectory = link
		}, kind: ErrorWorkingDirectory},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := valid
			request.Arguments = append([]string(nil), valid.Arguments...)
			request.Environment = append([]string(nil), valid.Environment...)
			test.edit(&request)
			child, err := NewLauncher().Launch(context.Background(), request)
			require.Nil(t, child)
			requireErrorKind(t, err, test.kind)
		})
	}
}

func TestLaunchClassifiesStartFailureAndClosesPipes(t *testing.T) {
	workspace := realPrivateTempDir(t)
	badExecutable := filepath.Join(workspace, "bad-executable")
	require.NoError(t, os.WriteFile(badExecutable, []byte("not an executable format"), 0o700))
	before := openFileDescriptors(t)

	child, err := NewLauncher().Launch(context.Background(), provider.LaunchRequest{
		Executable: badExecutable, Arguments: []string{}, Environment: []string{}, WorkingDirectory: workspace,
	})
	require.Nil(t, child)
	requireErrorKind(t, err, ErrorStart)
	requireNoNewFileDescriptors(t, before)
}

func TestCanceledLaunchDoesNotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	child, err := NewLauncher().Launch(ctx, helperRequest(t, "wait-stdin"))
	require.Nil(t, child)
	require.ErrorIs(t, err, context.Canceled)
	requireErrorKind(t, err, ErrorCanceled)
}

func TestPGIDVerificationFailureKillsPotentialGroupAndReaps(t *testing.T) {
	request := helperRequest(t, "cancel-tree")
	resultPath := filepath.Join(request.WorkingDirectory, "started-pids")
	request.Arguments = append(request.Arguments, resultPath)
	before := openFileDescriptors(t)

	originalGetProcessGroupID := getProcessGroupID
	getProcessGroupID = func(int) (int, error) {
		require.Eventually(t, func() bool {
			contents, err := os.ReadFile(resultPath)
			return err == nil && len(strings.Fields(string(contents))) == 2
		}, 2*time.Second, time.Millisecond)
		return 0, syscall.EIO
	}
	t.Cleanup(func() { getProcessGroupID = originalGetProcessGroupID })

	child, err := NewLauncher().Launch(context.Background(), request)
	require.Nil(t, child)
	requireErrorKind(t, err, ErrorUnsafeProcessGroup)

	contents, readErr := os.ReadFile(resultPath)
	require.NoError(t, readErr)
	fields := strings.Fields(string(contents))
	require.Len(t, fields, 2)
	rootPID, conversionError := strconv.Atoi(fields[0])
	require.NoError(t, conversionError)
	grandchildPID, conversionError := strconv.Atoi(fields[1])
	require.NoError(t, conversionError)
	requireProcessesGone(t, rootPID, grandchildPID)
	requireNoNewFileDescriptors(t, before)
}

func TestCancellationDuringLaunchReapsEntireStartedGroup(t *testing.T) {
	request := helperRequest(t, "cancel-tree")
	resultPath := filepath.Join(request.WorkingDirectory, "started-pids")
	request.Arguments = append(request.Arguments, resultPath)
	before := openFileDescriptors(t)

	ctx := newCancelWhenProcessTreeReadyContext(resultPath)
	child, err := NewLauncher().Launch(ctx, request)
	require.Nil(t, child)
	require.ErrorIs(t, err, context.Canceled)
	requireErrorKind(t, err, ErrorCanceled)

	contents, readErr := os.ReadFile(resultPath)
	require.NoError(t, readErr)
	fields := strings.Fields(string(contents))
	require.Len(t, fields, 2)
	rootPID, conversionError := strconv.Atoi(fields[0])
	require.NoError(t, conversionError)
	grandchildPID, conversionError := strconv.Atoi(fields[1])
	require.NoError(t, conversionError)
	requireProcessesGone(t, rootPID, grandchildPID)
	requireNoNewFileDescriptors(t, before)
}

type cancelWhenProcessTreeReadyContext struct {
	path  string
	done  chan struct{}
	once  sync.Once
	mu    sync.Mutex
	calls int
}

func newCancelWhenProcessTreeReadyContext(path string) *cancelWhenProcessTreeReadyContext {
	return &cancelWhenProcessTreeReadyContext{path: path, done: make(chan struct{})}
}

func (*cancelWhenProcessTreeReadyContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *cancelWhenProcessTreeReadyContext) Done() <-chan struct{}   { return ctx.done }
func (*cancelWhenProcessTreeReadyContext) Value(any) any               { return nil }
func (ctx *cancelWhenProcessTreeReadyContext) Err() error {
	ctx.mu.Lock()
	ctx.calls++
	calls := ctx.calls
	ctx.mu.Unlock()
	if calls == 1 {
		return nil
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(ctx.path)
		if err == nil && len(strings.Fields(string(contents))) == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	ctx.once.Do(func() { close(ctx.done) })
	return context.Canceled
}

func TestWaitRunsOnceAndReturnsOneStableResult(t *testing.T) {
	request := helperRequest(t, "exit", "17")
	child, err := NewLauncher().Launch(context.Background(), request)
	require.NoError(t, err)

	const callers = 32
	results := make([]error, callers)
	var wg sync.WaitGroup
	for index := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[index] = child.Wait()
		}()
	}
	wg.Wait()
	for _, result := range results {
		require.Same(t, results[0], result)
	}
	var exitError *exec.ExitError
	require.ErrorAs(t, results[0], &exitError)
	require.Equal(t, 17, exitError.ExitCode())
}

func TestTerminateSignalsEntireIsolatedGroup(t *testing.T) {
	testSignalEntireGroup(t, false)
}

func TestKillSignalsEntireIsolatedGroupAndIsIdempotent(t *testing.T) {
	testSignalEntireGroup(t, true)
}

func TestIgnoredTerminateCanBeEscalated(t *testing.T) {
	child, rootPID, grandchildPID := launchTree(t, "tree-ignore-term")
	require.NoError(t, child.Terminate())
	require.NoError(t, child.Terminate())
	requireProcessExists(t, rootPID)
	requireProcessExists(t, grandchildPID)
	require.NoError(t, child.Kill())
	require.NoError(t, child.Kill())
	require.Error(t, child.Wait())
	requireProcessesGone(t, rootPID, grandchildPID)
}

func TestGracefulTerminateDoesNotRequireKill(t *testing.T) {
	child, rootPID, grandchildPID := launchTree(t, "tree-graceful-term")
	require.NoError(t, child.Terminate())
	require.NoError(t, child.Wait())
	requireProcessesGone(t, rootPID, grandchildPID)
}

func TestLeaderExitKillsSurvivingGrandchildBeforeWaitReturns(t *testing.T) {
	child, rootPID, grandchildPID := launchTree(t, "tree-leader-exits")
	require.NoError(t, child.Wait())
	requireProcessesGone(t, rootPID, grandchildPID)
}

func TestMonitorFailureReportsFailedCleanupAndAllowsKillRetry(t *testing.T) {
	monitorReady := make(chan struct{})
	monitorRelease := make(chan struct{})
	originalNewProcessIdentity := newProcessIdentity
	newProcessIdentity = func(int) (processIdentity, error) {
		return fakeProcessIdentity{
			wait: func() error {
				close(monitorReady)
				<-monitorRelease
				return syscall.EIO
			},
		}, nil
	}
	t.Cleanup(func() { newProcessIdentity = originalNewProcessIdentity })

	child, err := NewLauncher().Launch(context.Background(), helperRequest(t, "wait-stdin"))
	require.NoError(t, err)
	managed := child.(*managedChild)
	<-monitorReady

	managed.mu.Lock()
	managed.signalFn = func(int, unix.Signal) error { return syscall.ESRCH }
	managed.mu.Unlock()
	close(monitorRelease)

	waitDone := make(chan error, 1)
	go func() { waitDone <- child.Wait() }()
	select {
	case waitErr := <-waitDone:
		requireErrorKinds(t, waitErr, ErrorStart, ErrorSignal)
	case <-time.After(time.Second):
		t.Fatal("Wait hung after identity monitoring and cleanup failed")
	}

	managed.mu.Lock()
	require.Equal(t, managed.pid, managed.pgid, "failed cleanup must retain verified process-group ownership")
	managed.mu.Unlock()
	requireErrorKind(t, child.Kill(), ErrorSignal)
	managed.mu.Lock()
	require.Equal(t, managed.pid, managed.pgid, "a failed retry must retain verified process-group ownership")
	managed.signalFn = unix.Kill
	managed.mu.Unlock()
	require.NoError(t, child.Kill(), "Kill must retry a previously failed cleanup signal")
	requireProcessesGone(t, managed.pid)
}

type fakeProcessIdentity struct {
	wait func() error
}

func (identity fakeProcessIdentity) waitForExit() error { return identity.wait() }
func (fakeProcessIdentity) close() error                { return nil }

func TestSignalGuardNeverTargetsBrokerGroup(t *testing.T) {
	brokerGroup := unix.Getpgrp()
	child := &managedChild{pid: brokerGroup, pgid: brokerGroup, brokerPGID: brokerGroup}
	requireErrorKind(t, child.Terminate(), ErrorUnsafeProcessGroup)
	requireErrorKind(t, child.Kill(), ErrorUnsafeProcessGroup)
}

func TestSignalsAfterWaitNeverUseEndedProcessGroup(t *testing.T) {
	child, err := NewLauncher().Launch(context.Background(), helperRequest(t, "exit", "0"))
	require.NoError(t, err)
	managed := child.(*managedChild)
	require.NoError(t, child.Wait())

	managed.mu.Lock()
	managed.signalFn = func(int, unix.Signal) error {
		t.Fatal("signal syscall made after Wait completed")
		return nil
	}
	managed.mu.Unlock()

	require.NoError(t, child.Terminate())
	require.NoError(t, child.Kill())
}

func TestKillDominatesLaterTerminate(t *testing.T) {
	child, err := NewLauncher().Launch(context.Background(), helperRequest(t, "wait-stdin"))
	require.NoError(t, err)
	managed := child.(*managedChild)
	var signals []unix.Signal

	managed.mu.Lock()
	managed.signalFn = func(pid int, signal unix.Signal) error {
		signals = append(signals, signal)
		return unix.Kill(pid, signal)
	}
	managed.mu.Unlock()

	require.NoError(t, child.Kill())
	require.NoError(t, child.Terminate())
	require.Error(t, child.Wait())
	require.Equal(t, []unix.Signal{unix.SIGKILL}, signals)
}

func TestConcurrentWaitTerminateKillIsRaceClean(t *testing.T) {
	for iteration := 0; iteration < 5; iteration++ {
		child, rootPID, grandchildPID := launchTree(t, "tree-ignore-term")
		const callers = 24
		var waitGroup sync.WaitGroup
		for caller := 0; caller < callers; caller++ {
			waitGroup.Add(1)
			go func(operation int) {
				defer waitGroup.Done()
				switch operation % 3 {
				case 0:
					_ = child.Wait()
				case 1:
					require.NoError(t, child.Terminate())
				case 2:
					require.NoError(t, child.Kill())
				}
			}(caller)
		}
		waitGroup.Wait()
		require.Error(t, child.Wait())
		requireProcessesGone(t, rootPID, grandchildPID)
	}
}

func testSignalEntireGroup(t *testing.T, force bool) {
	t.Helper()
	child, rootPID, grandchildPID := launchTree(t, "tree-wait")
	if force {
		require.NoError(t, child.Kill())
		require.NoError(t, child.Kill())
	} else {
		require.NoError(t, child.Terminate())
		require.NoError(t, child.Terminate())
	}
	require.Error(t, child.Wait())
	requireProcessesGone(t, rootPID, grandchildPID)
}

func launchTree(t *testing.T, mode string) (provider.ManagedChild, int, int) {
	t.Helper()
	var arguments []string
	if mode == "tree-leader-exits" {
		arguments = append(arguments, filepath.Join(realPrivateTempDir(t), "grandchild-ready"))
	}
	child, err := NewLauncher().Launch(context.Background(), helperRequest(t, mode, arguments...))
	require.NoError(t, err)
	scanner := bufio.NewScanner(child.Output())
	wantedPIDs := 2
	if mode == "tree-leader-exits" {
		wantedPIDs = 3
	}
	pids := make(map[string]int, wantedPIDs)
	for scanner.Scan() {
		label, value, ok := strings.Cut(scanner.Text(), ":")
		require.True(t, ok)
		pid, conversionError := strconv.Atoi(value)
		require.NoError(t, conversionError)
		pids[label] = pid
		if len(pids) == wantedPIDs {
			break
		}
	}
	require.NoError(t, scanner.Err())
	require.Contains(t, pids, "root")
	require.Contains(t, pids, "grandchild")
	rootGroup := pids["root"]
	if mode == "tree-leader-exits" {
		require.Equal(t, rootGroup, pids["grandchild-group"])
	} else {
		actualRootGroup, err := unix.Getpgid(pids["root"])
		require.NoError(t, err)
		require.Equal(t, rootGroup, actualRootGroup)
		grandchildGroup, err := unix.Getpgid(pids["grandchild"])
		require.NoError(t, err)
		require.Equal(t, rootGroup, grandchildGroup)
	}
	require.NotEqual(t, unix.Getpgrp(), rootGroup)
	return child, pids["root"], pids["grandchild"]
}

func helperRequest(t *testing.T, mode string, arguments ...string) provider.LaunchRequest {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	executable, err = filepath.EvalSymlinks(executable)
	require.NoError(t, err)
	return provider.LaunchRequest{
		Executable:       executable,
		Arguments:        append([]string{"-test.run=^TestProcessGroupHelper$", "--", mode}, arguments...),
		Environment:      []string{helperEnvironment},
		WorkingDirectory: realPrivateTempDir(t),
	}
}

func realPrivateTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	directory, err := filepath.EvalSymlinks(directory)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(directory, 0o700))
	return directory
}

func requireErrorKind(t *testing.T, err error, kind ErrorKind) {
	t.Helper()
	var processError *Error
	require.ErrorAs(t, err, &processError)
	require.Equal(t, kind, processError.Kind)
}

func requireErrorKinds(t *testing.T, err error, kinds ...ErrorKind) {
	t.Helper()
	found := make(map[ErrorKind]bool, len(kinds))
	var visit func(error)
	visit = func(current error) {
		if current == nil {
			return
		}
		if processErr, ok := current.(*Error); ok {
			found[processErr.Kind] = true
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			for _, nested := range joined.Unwrap() {
				visit(nested)
			}
			return
		}
		if nested := errors.Unwrap(current); nested != nil {
			visit(nested)
		}
	}
	visit(err)
	for _, kind := range kinds {
		require.True(t, found[kind], "missing process error kind %q in %v", kind, err)
	}
}

func readAll(reader interface{ Read([]byte) (int, error) }) (string, error) {
	result, err := io.ReadAll(reader)
	return string(result), err
}

type fileDescriptorIdentity struct {
	device uint64
	inode  uint64
	mode   uint32
	rdev   uint64
}

func openFileDescriptors(t *testing.T) map[fileDescriptorIdentity]int {
	t.Helper()
	result := make(map[fileDescriptorIdentity]int)
	for descriptor := 0; descriptor < 256; descriptor++ {
		var stat unix.Stat_t
		if err := unix.Fstat(descriptor, &stat); err != nil {
			continue
		}
		identity := fileDescriptorIdentity{
			device: uint64(stat.Dev),
			inode:  uint64(stat.Ino),
			mode:   uint32(stat.Mode),
			rdev:   uint64(stat.Rdev),
		}
		result[identity]++
	}
	return result
}

func requireNoNewFileDescriptors(t *testing.T, before map[fileDescriptorIdentity]int) {
	t.Helper()
	require.Eventually(t, func() bool {
		after := openFileDescriptors(t)
		for identity, count := range after {
			if count > before[identity] {
				return false
			}
		}
		return true
	}, time.Second, 5*time.Millisecond)
}

func requireProcessExists(t *testing.T, pid int) {
	t.Helper()
	require.NoError(t, unix.Kill(pid, 0))
}

func requireProcessesGone(t *testing.T, pids ...int) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, pid := range pids {
			if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
				return false
			}
		}
		return true
	}, 2*time.Second, 5*time.Millisecond)
}

func TestProcessGroupHelper(t *testing.T) {
	separator := -1
	helperRun := false
	for index, argument := range os.Args {
		if argument == "-test.run=^TestProcessGroupHelper$" {
			helperRun = true
		}
		if argument == "--" {
			separator = index
			break
		}
	}
	if !helperRun || separator < 0 {
		return
	}
	if separator+1 >= len(os.Args) {
		os.Exit(90)
	}
	mode := os.Args[separator+1]
	arguments := os.Args[separator+2:]
	switch mode {
	case "report":
		cwd, err := os.Getwd()
		if err != nil {
			os.Exit(91)
		}
		fmt.Printf("%s\n%s\n%s\n", cwd, strings.Join(arguments, "\x00"), strings.Join(os.Environ(), "\x00"))
	case "report-empty-environment":
		_, present := os.LookupEnv(arguments[0])
		fmt.Printf("%t\n%d\n", present, len(os.Environ()))
	case "final-output":
		fmt.Fprintln(os.Stdout, "FINAL stdout")
		fmt.Fprintln(os.Stderr, "FINAL stderr")
	case "wait-stdin":
		_, _ = os.Stdin.Read(make([]byte, 1))
	case "exit":
		code, _ := strconv.Atoi(arguments[0])
		os.Exit(code)
	case "tree-wait", "tree-ignore-term", "tree-graceful-term", "tree-leader-exits":
		runTreeHelper(mode, arguments)
	case "cancel-tree":
		runCancellationTree(arguments[0])
	default:
		os.Exit(92)
	}
	os.Exit(0)
}

func runCancellationTree(resultPath string) {
	if os.Getenv("PROCESSGROUP_GRANDCHILD") == "1" {
		_ = os.WriteFile(resultPath+".ready", []byte(strconv.Itoa(os.Getpid())), 0o600)
		select {}
	}
	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$", "--", "cancel-tree", resultPath)
	command.Env = []string{helperEnvironment, "PROCESSGROUP_GRANDCHILD=1"}
	if err := command.Start(); err != nil {
		os.Exit(94)
	}
	var grandchildPID []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		grandchildPID, _ = os.ReadFile(resultPath + ".ready")
		if len(grandchildPID) != 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(grandchildPID) == 0 {
		os.Exit(95)
	}
	contents := fmt.Sprintf("%d %s", os.Getpid(), grandchildPID)
	if err := os.WriteFile(resultPath, []byte(contents), 0o600); err != nil {
		os.Exit(96)
	}
	select {}
}

func runTreeHelper(mode string, arguments []string) {
	if os.Getenv("PROCESSGROUP_GRANDCHILD") == "1" {
		if mode == "tree-ignore-term" {
			signal.Ignore(syscall.SIGTERM)
		}
		if mode == "tree-graceful-term" {
			channel := make(chan os.Signal, 1)
			signal.Notify(channel, syscall.SIGTERM)
			fmt.Printf("grandchild:%d\n", os.Getpid())
			<-channel
			signal.Stop(channel)
			return
		}
		fmt.Printf("grandchild:%d\n", os.Getpid())
		if mode == "tree-leader-exits" {
			group, err := unix.Getpgid(0)
			if err != nil {
				os.Exit(97)
			}
			fmt.Printf("grandchild-group:%d\n", group)
			if err := os.WriteFile(arguments[0], []byte("ready"), 0o600); err != nil {
				os.Exit(97)
				os.Exit(97)
			}
		}
		select {}
	}

	commandArguments := append([]string{"-test.run=^TestProcessGroupHelper$", "--", mode}, arguments...)
	command := exec.Command(os.Args[0], commandArguments...)
	command.Env = []string{helperEnvironment, "PROCESSGROUP_GRANDCHILD=1"}
	command.Stdin = nil
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		os.Exit(93)
	}
	if mode == "tree-leader-exits" {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if contents, err := os.ReadFile(arguments[0]); err == nil && string(contents) == "ready" {
				fmt.Printf("root:%d\n", os.Getpid())
				return
			}
			time.Sleep(time.Millisecond)
		}
		os.Exit(98)
	}
	if mode == "tree-ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	if mode == "tree-graceful-term" {
		channel := make(chan os.Signal, 1)
		signal.Notify(channel, syscall.SIGTERM)
		fmt.Printf("root:%d\n", os.Getpid())
		<-channel
		signal.Stop(channel)
		_ = command.Wait()
		return
	}
	fmt.Printf("root:%d\n", os.Getpid())
	_ = command.Wait()
}
