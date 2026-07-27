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

type managedChild struct {
	command    *exec.Cmd
	input      *os.File
	output     *os.File
	errors     *os.File
	pid        int
	pgid       int
	brokerPGID int

	waitOnce sync.Once
	waitErr  error
	term     signalResult
	kill     signalResult
}

type signalResult struct {
	once sync.Once
	err  error
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
	command.Env = append([]string(nil), request.Environment...)
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
	pgid, err := unix.Getpgid(pid)
	if err != nil || pgid != pid {
		cleanupStartedProcess(command, pid, pgid)
		if err == nil {
			err = fmt.Errorf("child PID %d has process group %d", pid, pgid)
		}
		return nil, processError(ErrorUnsafeProcessGroup, "verify child process group", err)
	}
	brokerPGID := unix.Getpgrp()
	if pgid == brokerPGID {
		cleanupStartedProcess(command, pid, 0)
		return nil, processError(ErrorUnsafeProcessGroup, "verify child process group", errors.New("child process group matches broker process group"))
	}

	child := &managedChild{
		command: command, input: pipes.input, output: pipes.output, errors: pipes.errors,
		pid: pid, pgid: pgid, brokerPGID: brokerPGID,
	}
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

func cleanupStartedProcess(command *exec.Cmd, pid, verifiedPGID int) {
	if verifiedPGID == pid && verifiedPGID > 0 && verifiedPGID != unix.Getpgrp() {
		_ = unix.Kill(-verifiedPGID, unix.SIGKILL)
	} else if command.Process != nil && pid > 0 {
		_ = command.Process.Kill()
	}
	_ = command.Wait()
}

func (child *managedChild) cleanupCanceledLaunch() error {
	signalErr := child.signal(unix.SIGKILL, "cancel launch")
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

func (child *managedChild) Input() io.WriteCloser { return child.input }
func (child *managedChild) Output() io.Reader     { return child.output }
func (child *managedChild) Errors() io.Reader     { return child.errors }

func (child *managedChild) Wait() error {
	child.waitOnce.Do(func() {
		if child.command == nil {
			child.waitErr = processError(ErrorStart, "wait", errors.New("child was not started"))
			return
		}
		child.waitErr = child.command.Wait()
		_ = child.input.Close()
		_ = child.output.Close()
		_ = child.errors.Close()
	})
	return child.waitErr
}

func (child *managedChild) Terminate() error {
	child.term.once.Do(func() {
		child.term.err = child.signal(unix.SIGTERM, "terminate")
	})
	return child.term.err
}

func (child *managedChild) Kill() error {
	child.kill.once.Do(func() {
		child.kill.err = child.signal(unix.SIGKILL, "kill")
	})
	return child.kill.err
}

func (child *managedChild) signal(value unix.Signal, operation string) error {
	if child.pid <= 0 || child.pgid <= 0 || child.pgid != child.pid || child.pgid == child.brokerPGID || child.pgid == unix.Getpgrp() {
		return processError(ErrorUnsafeProcessGroup, operation, errors.New("refusing to signal an unverified or broker process group"))
	}
	if err := unix.Kill(-child.pgid, value); err != nil && !errors.Is(err, unix.ESRCH) {
		return processError(ErrorSignal, operation, err)
	}
	return nil
}

var _ provider.ManagedChild = (*managedChild)(nil)
