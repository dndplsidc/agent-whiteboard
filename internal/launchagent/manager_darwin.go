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
	launchAgents, logs, err := manager.prepareFilesystem()
	if err != nil {
		return err
	}
	defer launchAgents.close()
	defer logs.close()
	if err := writeAtomic(launchAgents, filepath.Base(manager.paths.Plist), contents, manager.ops); err != nil {
		return err
	}
	if err := verifyLaunchAgentsBinding(manager.home, launchAgents); err != nil {
		return fmt.Errorf("verify LaunchAgents directory after publication: %w", err)
	}
	if err := verifyLogDirectoryBinding(manager.home, logs); err != nil {
		return fmt.Errorf("verify log directory after publication: %w", err)
	}

	target := manager.target()
	output, printErr := manager.runner.Run(ctx, LaunchctlExecutable, "print", target)
	if printErr == nil {
		if output, bootoutErr := manager.runner.Run(ctx, LaunchctlExecutable, "bootout", target); bootoutErr != nil && !notLoaded(output, bootoutErr) {
			return reloadFailure("bootout", bootoutErr)
		}
	} else if !notLoaded(output, printErr) {
		return fmt.Errorf("inspect installed LaunchAgent: %w", printErr)
	}
	if _, err := manager.runner.Run(ctx, LaunchctlExecutable, "bootstrap", manager.domain(), manager.paths.Plist); err != nil {
		return reloadFailure("bootstrap", err)
	}
	if _, err := manager.runner.Run(ctx, LaunchctlExecutable, "kickstart", "-k", target); err != nil {
		return reloadFailure("kickstart", err)
	}
	return nil
}

func (manager *darwinManager) prepareFilesystem() (*secureDir, *secureDir, error) {
	launchAgents, err := openLaunchAgents(manager.home, true)
	if err != nil {
		return nil, nil, err
	}
	logs, err := openLogDirectory(manager.home, true)
	if err != nil {
		launchAgents.close()
		return nil, nil, err
	}
	if err := ensurePrivateFile(logs, filepath.Base(manager.paths.StdoutLog), manager.ops); err != nil {
		launchAgents.close()
		logs.close()
		return nil, nil, err
	}
	if err := ensurePrivateFile(logs, filepath.Base(manager.paths.StderrLog), manager.ops); err != nil {
		launchAgents.close()
		logs.close()
		return nil, nil, err
	}
	return launchAgents, logs, nil
}

func (manager *darwinManager) installed() (bool, error) {
	launchAgents, err := openLaunchAgents(manager.home, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer launchAgents.close()
	return installedPlist(launchAgents, filepath.Base(manager.paths.Plist))
}

func (manager *darwinManager) Status(ctx context.Context) (Status, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	installed, err := manager.installed()
	if err != nil {
		return Status{}, err
	}
	status := Status{Installed: installed}
	output, err := manager.runner.Run(ctx, LaunchctlExecutable, "print", manager.target())
	if err != nil {
		if notLoaded(output, err) {
			return status, nil
		}
		return status, fmt.Errorf("inspect LaunchAgent status: %w", err)
	}
	status.Loaded = true
	status.Running, status.PID, err = parseLaunchctlStatus(output)
	if err != nil {
		return status, err
	}
	return status, nil
}

func (manager *darwinManager) Restart(ctx context.Context) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	installed, err := manager.installed()
	if err != nil {
		return err
	}
	if !installed {
		return ErrNotInstalled
	}
	target := manager.target()
	if output, err := manager.runner.Run(ctx, LaunchctlExecutable, "bootout", target); err != nil && !notLoaded(output, err) {
		return reloadFailure("bootout", err)
	}
	if _, err := manager.runner.Run(ctx, LaunchctlExecutable, "bootstrap", manager.domain(), manager.paths.Plist); err != nil {
		return reloadFailure("bootstrap", err)
	}
	if _, err := manager.runner.Run(ctx, LaunchctlExecutable, "kickstart", "-k", target); err != nil {
		return reloadFailure("kickstart", err)
	}
	return nil
}

func (manager *darwinManager) Stop(ctx context.Context) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	output, err := manager.runner.Run(ctx, LaunchctlExecutable, "bootout", manager.target())
	if err != nil && !notLoaded(output, err) {
		return fmt.Errorf("stop LaunchAgent: launchctl bootout failed: %w", err)
	}
	return nil
}

func (manager *darwinManager) Uninstall(ctx context.Context) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	output, err := manager.runner.Run(ctx, LaunchctlExecutable, "bootout", manager.target())
	if err != nil && !notLoaded(output, err) {
		return fmt.Errorf("uninstall LaunchAgent: launchctl bootout failed: %w", err)
	}
	launchAgents, err := openLaunchAgents(manager.home, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer launchAgents.close()
	if err := removeDurable(launchAgents, filepath.Base(manager.paths.Plist), manager.ops); err != nil {
		return err
	}
	return verifyLaunchAgentsBinding(manager.home, launchAgents)
}

func (manager *darwinManager) domain() string {
	return "gui/" + strconv.Itoa(manager.uid)
}

func (manager *darwinManager) target() string {
	return manager.domain() + "/" + Label
}

func reloadFailure(operation string, cause error) error {
	return errors.Join(ErrInstalledNotRunning, fmt.Errorf("launchctl %s failed: %w", operation, cause))
}

func notLoaded(output []byte, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrNotLoaded) {
		return true
	}
	message := strings.ToLower(string(output))
	return strings.Contains(message, "could not find service") || strings.Contains(message, "service not found") || strings.Contains(message, "no such process")
}

func parseLaunchctlStatus(output []byte) (bool, int, error) {
	state := ""
	pidText := ""
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
			pidText = strings.TrimSpace(value)
		}
	}
	if state != "running" {
		return false, 0, nil
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return false, 0, errors.New("launchctl reported a running service without a positive PID")
	}
	return true, pid, nil
}
