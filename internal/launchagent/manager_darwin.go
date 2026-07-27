//go:build darwin

package launchagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

type darwinManager struct {
	mu     sync.Mutex
	runner Runner
	home   string
	uid    int
	paths  servicePaths
	ops    fileOps
}

// NewManager constructs the macOS per-user LaunchAgent manager.
func NewManager(runner Runner) (Manager, error) {
	if nilRunner(runner) {
		return nil, errors.New("launchctl runner is required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user home: %w", err)
	}
	home, err = normalizeHome(home)
	if err != nil {
		return nil, err
	}
	return newDarwinManager(runner, home, os.Getuid(), defaultFileOps()), nil
}

func newDarwinManager(runner Runner, home string, uid int, ops fileOps) *darwinManager {
	return &darwinManager{runner: runner, home: home, uid: uid, paths: pathsForHome(home), ops: ops}
}

func nilRunner(runner Runner) bool {
	if runner == nil {
		return true
	}
	value := reflect.ValueOf(runner)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (manager *darwinManager) Install(ctx context.Context, config Config) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	normalized, err := normalizeConfig(config)
	if err != nil {
		return err
	}
	contents, err := marshalPlist(normalized, manager.paths)
	if err != nil {
		return err
	}
	if err := manager.prepareFilesystem(); err != nil {
		return err
	}
	if err := writeAtomic(manager.paths.Plist, contents, manager.ops); err != nil {
		return err
	}

	target := manager.target()
	if _, printErr := manager.runner.Run(ctx, LaunchctlExecutable, "print", target); printErr == nil {
		if output, bootoutErr := manager.runner.Run(ctx, LaunchctlExecutable, "bootout", target); bootoutErr != nil && !notLoaded(output, bootoutErr) {
			return reloadFailure("bootout")
		}
	}
	if _, err := manager.runner.Run(ctx, LaunchctlExecutable, "bootstrap", manager.domain(), manager.paths.Plist); err != nil {
		return reloadFailure("bootstrap")
	}
	if _, err := manager.runner.Run(ctx, LaunchctlExecutable, "kickstart", "-k", target); err != nil {
		return reloadFailure("kickstart")
	}
	return nil
}

func (manager *darwinManager) prepareFilesystem() error {
	if err := ensurePrivateDirectory(manager.home, filepath.Dir(manager.paths.Plist)); err != nil {
		return err
	}
	if err := ensurePrivateDirectory(manager.home, filepath.Dir(manager.paths.StdoutLog)); err != nil {
		return err
	}
	if err := ensurePrivateFile(manager.paths.StdoutLog); err != nil {
		return err
	}
	if err := ensurePrivateFile(manager.paths.StderrLog); err != nil {
		return err
	}
	if err := manager.ops.syncDir(filepath.Dir(manager.paths.StdoutLog)); err != nil {
		return fmt.Errorf("sync log directory: %w", err)
	}
	return nil
}

func (manager *darwinManager) Status(ctx context.Context) (Status, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	installed, err := installedPlist(manager.paths.Plist)
	if err != nil {
		return Status{}, err
	}
	status := Status{Installed: installed}
	output, err := manager.runner.Run(ctx, LaunchctlExecutable, "print", manager.target())
	if err != nil {
		return status, nil
	}
	status.Loaded = true
	status.Running, status.PID = parseLaunchctlStatus(output)
	return status, nil
}

func (manager *darwinManager) Restart(ctx context.Context) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	installed, err := installedPlist(manager.paths.Plist)
	if err != nil {
		return err
	}
	if !installed {
		return ErrNotInstalled
	}
	target := manager.target()
	if output, err := manager.runner.Run(ctx, LaunchctlExecutable, "bootout", target); err != nil && !notLoaded(output, err) {
		return reloadFailure("bootout")
	}
	if _, err := manager.runner.Run(ctx, LaunchctlExecutable, "bootstrap", manager.domain(), manager.paths.Plist); err != nil {
		return reloadFailure("bootstrap")
	}
	if _, err := manager.runner.Run(ctx, LaunchctlExecutable, "kickstart", "-k", target); err != nil {
		return reloadFailure("kickstart")
	}
	return nil
}

func (manager *darwinManager) Stop(ctx context.Context) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	output, err := manager.runner.Run(ctx, LaunchctlExecutable, "bootout", manager.target())
	if err != nil && !notLoaded(output, err) {
		return errors.New("stop LaunchAgent: launchctl bootout failed")
	}
	return nil
}

func (manager *darwinManager) Uninstall(ctx context.Context) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	output, err := manager.runner.Run(ctx, LaunchctlExecutable, "bootout", manager.target())
	if err != nil && !notLoaded(output, err) {
		return errors.New("uninstall LaunchAgent: launchctl bootout failed")
	}
	return removeDurable(manager.paths.Plist, manager.ops)
}

func (manager *darwinManager) domain() string {
	return "gui/" + strconv.Itoa(manager.uid)
}

func (manager *darwinManager) target() string {
	return manager.domain() + "/" + Label
}

func reloadFailure(operation string) error {
	return fmt.Errorf("%w: launchctl %s failed", ErrInstalledNotRunning, operation)
}

func notLoaded(output []byte, err error) bool {
	if errors.Is(err, ErrNotLoaded) {
		return true
	}
	message := strings.ToLower(string(output))
	return strings.Contains(message, "could not find service") || strings.Contains(message, "service not found") || strings.Contains(message, "no such process")
}

func parseLaunchctlStatus(output []byte) (bool, int) {
	state := ""
	pid := 0
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "state":
			state = strings.TrimSpace(value)
		case "pid":
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err == nil && parsed > 0 {
				pid = parsed
			}
		}
	}
	return state == "running", pid
}
