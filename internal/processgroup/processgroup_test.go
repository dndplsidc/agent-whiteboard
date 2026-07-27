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
	require.ErrorIs(t, err, os.ErrClosed)
	_, err = child.Errors().Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrClosed)

	fields := strings.Split(strings.TrimSpace(output), "\n")
	require.Equal(t, []string{
		request.WorkingDirectory,
		"first\x00second",
		helperEnvironment + "\x00EXPLICIT=value",
	}, fields)
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
	before := openFileDescriptorCount(t)

	child, err := NewLauncher().Launch(context.Background(), provider.LaunchRequest{
		Executable: badExecutable, Arguments: []string{}, Environment: []string{}, WorkingDirectory: workspace,
	})
	require.Nil(t, child)
	requireErrorKind(t, err, ErrorStart)
	require.Eventually(t, func() bool { return openFileDescriptorCount(t) == before }, time.Second, 5*time.Millisecond)
}

func TestCanceledLaunchDoesNotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	child, err := NewLauncher().Launch(ctx, helperRequest(t, "wait-stdin"))
	require.Nil(t, child)
	require.ErrorIs(t, err, context.Canceled)
	requireErrorKind(t, err, ErrorCanceled)
}

func TestCancellationDuringLaunchReapsEntireStartedGroup(t *testing.T) {
	request := helperRequest(t, "cancel-tree")
	resultPath := filepath.Join(request.WorkingDirectory, "started-pids")
	request.Arguments = append(request.Arguments, resultPath)
	before := openFileDescriptorCount(t)

	ctx := newCancelWhenFileExistsContext(resultPath)
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
	require.Eventually(t, func() bool { return openFileDescriptorCount(t) == before }, time.Second, 5*time.Millisecond)
}

type cancelWhenFileExistsContext struct {
	path  string
	done  chan struct{}
	once  sync.Once
	mu    sync.Mutex
	calls int
}

func newCancelWhenFileExistsContext(path string) *cancelWhenFileExistsContext {
	return &cancelWhenFileExistsContext{path: path, done: make(chan struct{})}
}

func (*cancelWhenFileExistsContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *cancelWhenFileExistsContext) Done() <-chan struct{}   { return ctx.done }
func (*cancelWhenFileExistsContext) Value(any) any               { return nil }
func (ctx *cancelWhenFileExistsContext) Err() error {
	ctx.mu.Lock()
	ctx.calls++
	calls := ctx.calls
	ctx.mu.Unlock()
	if calls == 1 {
		return nil
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ctx.path); err == nil {
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

func TestSignalGuardNeverTargetsBrokerGroup(t *testing.T) {
	brokerGroup := unix.Getpgrp()
	child := &managedChild{pid: brokerGroup, pgid: brokerGroup, brokerPGID: brokerGroup}
	requireErrorKind(t, child.Terminate(), ErrorUnsafeProcessGroup)
	requireErrorKind(t, child.Kill(), ErrorUnsafeProcessGroup)
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
	child, err := NewLauncher().Launch(context.Background(), helperRequest(t, mode))
	require.NoError(t, err)
	scanner := bufio.NewScanner(child.Output())
	pids := make(map[string]int, 2)
	for scanner.Scan() {
		label, value, ok := strings.Cut(scanner.Text(), ":")
		require.True(t, ok)
		pid, conversionError := strconv.Atoi(value)
		require.NoError(t, conversionError)
		pids[label] = pid
		if len(pids) == 2 {
			break
		}
	}
	require.NoError(t, scanner.Err())
	require.Contains(t, pids, "root")
	require.Contains(t, pids, "grandchild")
	rootGroup, err := unix.Getpgid(pids["root"])
	require.NoError(t, err)
	grandchildGroup, err := unix.Getpgid(pids["grandchild"])
	require.NoError(t, err)
	require.Equal(t, pids["root"], rootGroup)
	require.Equal(t, rootGroup, grandchildGroup)
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

func readAll(reader interface{ Read([]byte) (int, error) }) (string, error) {
	result, err := io.ReadAll(reader)
	return string(result), err
}

func openFileDescriptorCount(t *testing.T) int {
	t.Helper()
	count := 0
	for descriptor := 0; descriptor < 256; descriptor++ {
		if _, err := unix.FcntlInt(uintptr(descriptor), unix.F_GETFD, 0); err == nil {
			count++
		}
	}
	return count
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
	if os.Getenv("AGENT_WHITEBOARD_PROCESSGROUP_HELPER") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
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
	case "wait-stdin":
		_, _ = os.Stdin.Read(make([]byte, 1))
	case "exit":
		code, _ := strconv.Atoi(arguments[0])
		os.Exit(code)
	case "tree-wait", "tree-ignore-term", "tree-graceful-term":
		runTreeHelper(mode)
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

func runTreeHelper(mode string) {
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
		select {}
	}

	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$", "--", mode)
	command.Env = []string{helperEnvironment, "PROCESSGROUP_GRANDCHILD=1"}
	command.Stdin = nil
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		os.Exit(93)
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
