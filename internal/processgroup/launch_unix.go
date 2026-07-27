//go:build darwin || linux

package processgroup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/edocsss/agent-whiteboard/internal/provider"
	"golang.org/x/sys/unix"
)

var getProcessGroupID = unix.Getpgid

type processIdentity interface {
	waitForExit() error
	close() error
}

var newProcessIdentity = func(pid int) (processIdentity, error) {
	return openProcessIdentity(pid)
}

type lifecyclePhase uint8

const (
	lifecycleActive lifecyclePhase = iota
	lifecycleTerminated
	lifecycleKilled
	lifecycleEnded
)

type managedChild struct {
	command *exec.Cmd
	input   *os.File
	output  *outputSpool
	errors  *outputSpool
	pid     int

	mu                 sync.Mutex
	phase              lifecyclePhase
	pgid               int
	brokerPGID         int
	termDone           bool
	killDone           bool
	termErr            error
	killErr            error
	waitErr            error
	done               chan struct{}
	signalFn           func(int, unix.Signal) error
	cleanupRetry       bool
	leaderExitObserved bool
	waitStarted        bool
}

type childPipes struct {
	input       *os.File
	childInput  *os.File
	output      *os.File
	childOutput *os.File
	errors      *os.File
	childErrors *os.File
}

// Launch validates and starts exactly the requested executable, arguments,
// environment, and working directory in a new process group.
func (*Launcher) Launch(ctx context.Context, request provider.LaunchRequest) (provider.ManagedChild, error) {
	if err := ctx.Err(); err != nil {
		return nil, processError(ErrorCanceled, "launch", err)
	}
	if err := request.Validate(); err != nil {
		return nil, processError(ErrorInvalidRequest, "validate", err)
	}
	if err := validateExecutable(request.Executable); err != nil {
		return nil, processError(ErrorExecutable, "validate executable", err)
	}
	if err := validateWorkingDirectory(request.WorkingDirectory); err != nil {
		return nil, processError(ErrorWorkingDirectory, "validate working directory", err)
	}

	pipes, err := openChildPipes()
	if err != nil {
		return nil, processError(ErrorStart, "create pipes", err)
	}
	closeAll := true
	defer func() {
		if closeAll {
			pipes.closeAll()
		}
	}()

	command := exec.Command(request.Executable, request.Arguments...)
	command.Args = append([]string{request.Executable}, request.Arguments...)
	command.Env = make([]string, len(request.Environment))
	copy(command.Env, request.Environment)
	command.Dir = request.WorkingDirectory
	command.Stdin = pipes.childInput
	command.Stdout = pipes.childOutput
	command.Stderr = pipes.childErrors
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := command.Start(); err != nil {
		return nil, processError(ErrorStart, "start", err)
	}
	pipes.closeChildEnds()

	pid := command.Process.Pid
	pgid, err := getProcessGroupID(pid)
	if err != nil || pgid != pid {
		cleanupStartedProcess(command, pid)
		if err == nil {
			err = fmt.Errorf("child PID %d has process group %d", pid, pgid)
		}
		return nil, processError(ErrorUnsafeProcessGroup, "verify child process group", err)
	}
	brokerPGID := unix.Getpgrp()
	if pgid == brokerPGID {
		cleanupStartedProcess(command, pid)
		return nil, processError(ErrorUnsafeProcessGroup, "verify child process group", errors.New("child process group matches broker process group"))
	}

	identity, err := newProcessIdentity(pid)
	if err != nil {
		cleanupStartedProcess(command, pid)
		return nil, processError(ErrorUnsafeProcessGroup, "track child process", err)
	}
	child := &managedChild{
		command:    command,
		input:      pipes.input,
		output:     newOutputSpool(pipes.output),
		errors:     newOutputSpool(pipes.errors),
		pid:        pid,
		phase:      lifecycleActive,
		pgid:       pgid,
		brokerPGID: brokerPGID,
		done:       make(chan struct{}),
		signalFn:   unix.Kill,
	}
	go child.reap(identity)

	if err := ctx.Err(); err != nil {
		cleanupErr := child.cleanupCanceledLaunch()
		if cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		return nil, processError(ErrorCanceled, "launch", err)
	}

	closeAll = false
	return child, nil
}

func validateExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("executable is not a regular file")
	}
	if err := unix.Access(path, unix.X_OK); err != nil {
		return fmt.Errorf("executable is not executable by the current user: %w", err)
	}
	return nil
}

func validateWorkingDirectory(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if resolved != path {
		return errors.New("working directory is not a real path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("working directory is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("working directory grants group or other permissions")
	}
	if info.Mode().Perm()&0o100 == 0 {
		return errors.New("working directory is not searchable by its owner")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("working directory ownership is unavailable")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return errors.New("working directory is not owned by the current user")
	}
	return nil
}

func openChildPipes() (*childPipes, error) {
	childInput, input, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	output, childOutput, err := os.Pipe()
	if err != nil {
		_ = childInput.Close()
		_ = input.Close()
		return nil, err
	}
	errorsPipe, childErrors, err := os.Pipe()
	if err != nil {
		_ = childInput.Close()
		_ = input.Close()
		_ = output.Close()
		_ = childOutput.Close()
		return nil, err
	}
	return &childPipes{
		input: input, childInput: childInput,
		output: output, childOutput: childOutput,
		errors: errorsPipe, childErrors: childErrors,
	}, nil
}

func (pipes *childPipes) closeChildEnds() {
	_ = pipes.childInput.Close()
	_ = pipes.childOutput.Close()
	_ = pipes.childErrors.Close()
}

func (pipes *childPipes) closeParentEnds() {
	_ = pipes.input.Close()
	_ = pipes.output.Close()
	_ = pipes.errors.Close()
}

func (pipes *childPipes) closeAll() {
	pipes.closeChildEnds()
	pipes.closeParentEnds()
}

// cleanupStartedProcess is used only before ownership is published. Setpgid was
// requested, so -pid is the only possible new group and cannot be the broker's
// group. Kill that potential tree before killing the leader and reaping it.
func cleanupStartedProcess(command *exec.Cmd, pid int) {
	if pid > 0 {
		_ = unix.Kill(-pid, unix.SIGKILL)
	}
	if command.Process != nil {
		_ = command.Process.Kill()
	}
	_ = command.Wait()
}

func (child *managedChild) cleanupCanceledLaunch() error {
	signalErr := child.Kill()
	waitErr := child.Wait()
	if signalErr != nil {
		return signalErr
	}
	var exitError *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitError) {
		return waitErr
	}
	return nil
}

func (child *managedChild) reap(identity processIdentity) {
	// pidfd/kqueue reports this specific leader's exit without reaping it. Keep
	// the leader unreaped while the lifecycle lock protects the verified PGID,
	// and kill any surviving members before numeric ownership is retired.
	identityErr := identity.waitForExit()
	exitObserved := identityErr == nil
	closeErr := identity.close()
	if closeErr != nil {
		identityErr = errors.Join(identityErr, closeErr)
	}

	child.mu.Lock()
	child.leaderExitObserved = exitObserved
	var cleanupErr error
	if child.killDone && child.killErr == nil {
		// A successful caller-issued SIGKILL already covered the whole group.
	} else {
		child.phase = lifecycleKilled
		child.killDone = true
		child.killErr = child.signalGroupLocked(unix.SIGKILL, "cleanup child process group", exitObserved)
		if exitObserved && isExitedLeaderOnlyGroupError(child.killErr) {
			child.killErr = nil
		}
		cleanupErr = child.killErr
	}
	if cleanupErr != nil {
		// Do not reap the leader or clear the PGID: retaining the unreaped leader
		// keeps the numeric identity safe so Kill can retry the failed cleanup.
		child.cleanupRetry = true
		child.waitErr = errors.Join(monitorProcessError(identityErr), cleanupErr)
		close(child.done)
		child.mu.Unlock()
		return
	}
	child.phase = lifecycleEnded
	child.pgid = 0
	child.waitStarted = true
	child.mu.Unlock()

	child.finishWait(identityErr, true)
}

func monitorProcessError(err error) error {
	if err == nil {
		return nil
	}
	return processError(ErrorStart, "monitor child", err)
}

func (child *managedChild) finishWait(identityErr error, publish bool) {
	waitErr := child.command.Wait()
	_ = child.input.Close()
	child.output.wait()
	child.errors.wait()
	if !publish {
		return
	}
	waitErr = errors.Join(waitErr, monitorProcessError(identityErr))

	child.mu.Lock()
	child.waitErr = waitErr
	close(child.done)
	child.mu.Unlock()
}

func (child *managedChild) Input() io.WriteCloser { return child.input }
func (child *managedChild) Output() io.Reader     { return child.output }
func (child *managedChild) Errors() io.Reader     { return child.errors }

func (child *managedChild) Wait() error {
	if child.done == nil {
		return processError(ErrorStart, "wait", errors.New("child was not started"))
	}
	child.output.forceBoundedDrain()
	child.errors.forceBoundedDrain()
	<-child.done
	child.mu.Lock()
	defer child.mu.Unlock()
	return child.waitErr
}

func (child *managedChild) Terminate() error {
	child.mu.Lock()
	defer child.mu.Unlock()

	if child.termDone {
		return child.termErr
	}
	child.termDone = true
	if child.phase == lifecycleEnded || child.phase == lifecycleKilled {
		return nil
	}
	child.phase = lifecycleTerminated
	child.termErr = child.signalGroupLocked(unix.SIGTERM, "terminate", true)
	return child.termErr
}

func (child *managedChild) Kill() error {
	child.mu.Lock()
	if child.killDone && child.killErr == nil {
		child.mu.Unlock()
		return nil
	}
	if child.phase == lifecycleEnded {
		child.mu.Unlock()
		return nil
	}
	child.killDone = true
	child.phase = lifecycleKilled
	missingIsSuccess := !child.cleanupRetry || child.leaderExitObserved
	child.killErr = child.signalGroupLocked(unix.SIGKILL, "kill", missingIsSuccess)
	if child.cleanupRetry && child.leaderExitObserved && isExitedLeaderOnlyGroupError(child.killErr) {
		child.killErr = nil
	}
	if child.killErr != nil {
		err := child.killErr
		child.mu.Unlock()
		return err
	}

	resumeCleanup := child.cleanupRetry && !child.waitStarted
	if resumeCleanup {
		// The successful retry makes it safe to retire numeric ownership before
		// command.Wait releases the leader PID for reuse.
		child.cleanupRetry = false
		child.phase = lifecycleEnded
		child.pgid = 0
		child.waitStarted = true
	}
	child.mu.Unlock()
	if resumeCleanup {
		go child.finishWait(nil, false)
	}
	return nil
}

func (child *managedChild) signalGroupLocked(value unix.Signal, operation string, missingIsSuccess bool) error {
	if child.pid <= 0 || child.pgid <= 0 || child.pgid != child.pid || child.pgid == child.brokerPGID || child.pgid == unix.Getpgrp() {
		return processError(ErrorUnsafeProcessGroup, operation, errors.New("refusing to signal an unverified or broker process group"))
	}
	if err := child.signalFn(-child.pgid, value); err != nil {
		if missingIsSuccess && errors.Is(err, unix.ESRCH) {
			return nil
		}
		return processError(ErrorSignal, operation, err)
	}
	return nil
}

var _ provider.ManagedChild = (*managedChild)(nil)
