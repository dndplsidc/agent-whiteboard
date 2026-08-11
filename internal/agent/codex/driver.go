package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/edocsss/agent-whiteboard/internal/common"
)

type Config struct {
	Executable   string
	Environment  []string
	ProviderRoot string
	Launcher     provider.Launcher
	IDs          common.IDGenerator
	Clock        common.Clock
	IdleTimeout  time.Duration
}

type Driver struct {
	config Config

	mu        sync.Mutex
	runtime   *runtime
	starting  *runtimeStart
	idleTimer *time.Timer
	idleToken uint64
	lastModel string

	idMu sync.Mutex
}

type runtimeStart struct {
	done    chan struct{}
	runtime *runtime
	err     error
}

func NewDriver(config Config) (*Driver, error) {
	if config.Environment == nil || common.IsNil(config.Launcher) || common.IsNil(config.IDs) || common.IsNil(config.Clock) || config.IdleTimeout <= 0 || !validAbsolutePath(config.Executable) || !validAbsolutePath(config.ProviderRoot) {
		return nil, errors.New("invalid Codex driver configuration")
	}
	request := provider.LaunchRequest{Executable: config.Executable, Arguments: []string{"app-server"}, Environment: config.Environment, WorkingDirectory: config.ProviderRoot}
	if request.Validate() != nil {
		return nil, errors.New("invalid Codex driver configuration")
	}
	config.Environment = slices.Clone(config.Environment)
	return &Driver{config: config}, nil
}

func (driver *Driver) Readiness(ctx context.Context) provider.Readiness {
	if driver == nil {
		return provider.Readiness{State: provider.StartupFailed, Provider: provider.NameCodex}
	}
	info, err := os.Stat(driver.config.Executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return provider.Readiness{State: provider.MissingExecutable, Provider: provider.NameCodex}
	}
	_, release, err := driver.acquireRuntime(ctx)
	if err != nil {
		var typed provider.ProviderError
		if errors.As(err, &typed) && typed.Code() == provider.ErrorAuthenticationRequired {
			return provider.Readiness{State: provider.AuthenticationRequired, Provider: provider.NameCodex}
		}
		return provider.Readiness{State: provider.StartupFailed, Provider: provider.NameCodex}
	}
	release()
	driver.mu.Lock()
	model := driver.lastModel
	driver.mu.Unlock()
	if model == "" {
		model = "Codex default"
	}
	return provider.Readiness{State: provider.Ready, Provider: provider.NameCodex, Model: model}
}

func (driver *Driver) Create(ctx context.Context, request provider.CreateRequest) (provider.Session, error) {
	if driver == nil || request.Provider != provider.NameCodex || request.Validate() != nil {
		return nil, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	runtime, release, err := driver.acquireRuntime(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	result, err := runtime.call(ctx, "thread/start", map[string]any{"cwd": request.Workspace})
	if err != nil {
		return nil, err
	}
	thread, err := parseThreadResponse(result)
	if err != nil {
		return nil, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	return driver.activate(runtime, thread.ID, thread.Model, request.Workspace)
}

func (driver *Driver) Resume(ctx context.Context, request provider.ResumeRequest) (provider.Session, error) {
	if driver == nil || request.Provider != provider.NameCodex || request.Validate() != nil {
		return nil, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	runtime, release, err := driver.acquireRuntime(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	result, err := runtime.call(ctx, "thread/resume", map[string]any{"threadId": request.NativeSession.Value()})
	if err != nil {
		return nil, err
	}
	thread, err := parseThreadResponse(result)
	if err != nil || thread.ID != request.NativeSession.Value() {
		return nil, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	return driver.activate(runtime, thread.ID, thread.Model, request.Workspace)
}

func (driver *Driver) Inspect(ctx context.Context, request provider.InspectRequest) (provider.NativeSession, error) {
	if driver == nil || request.Provider != provider.NameCodex || request.Validate() != nil {
		return provider.NativeSession{}, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	runtime, release, err := driver.acquireRuntime(ctx)
	if err != nil {
		return provider.NativeSession{}, err
	}
	defer release()
	result, err := runtime.call(ctx, "thread/read", map[string]any{"threadId": request.NativeSession.Value(), "includeTurns": false})
	if err != nil {
		return provider.NativeSession{}, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	thread, err := parseThreadResponse(result)
	if err != nil || thread.ID != request.NativeSession.Value() {
		return provider.NativeSession{}, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	return driver.native(thread.ID, thread.Model)
}

func (driver *Driver) Delete(ctx context.Context, request provider.DeleteRequest) error {
	if driver == nil || request.Provider != provider.NameCodex || request.Validate() != nil {
		return provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	runtime, release, err := driver.acquireRuntime(ctx)
	if err != nil {
		return err
	}
	defer release()
	_, err = runtime.call(ctx, "thread/delete", map[string]any{"threadId": request.NativeSession.Value()})
	var typed provider.ProviderError
	if errors.As(err, &typed) && typed.Code() == provider.ErrorNativeSessionMissing {
		return nil
	}
	return err
}

func (driver *Driver) ensureRuntime(ctx context.Context) (*runtime, error) {
	for {
		driver.mu.Lock()
		driver.stopIdleLocked()
		if driver.runtime != nil {
			select {
			case <-driver.runtime.done:
				driver.runtime = nil
			default:
				runtime := driver.runtime
				driver.mu.Unlock()
				return runtime, nil
			}
		}
		if driver.starting != nil {
			start := driver.starting
			driver.mu.Unlock()
			select {
			case <-start.done:
				return start.runtime, start.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		start := &runtimeStart{done: make(chan struct{})}
		driver.starting = start
		driver.mu.Unlock()

		created, err := startRuntime(ctx, driver)
		driver.mu.Lock()
		if err == nil {
			driver.runtime = created
		}
		start.runtime = created
		start.err = err
		driver.starting = nil
		close(start.done)
		driver.mu.Unlock()
		return created, err
	}
}

func (driver *Driver) acquireRuntime(ctx context.Context) (*runtime, func(), error) {
	for {
		runtime, err := driver.ensureRuntime(ctx)
		if err != nil {
			return nil, nil, err
		}
		driver.mu.Lock()
		if driver.runtime != runtime {
			driver.mu.Unlock()
			continue
		}
		driver.stopIdleLocked()
		runtime.mu.Lock()
		runtime.leases++
		runtime.mu.Unlock()
		driver.mu.Unlock()
		var once sync.Once
		return runtime, func() { once.Do(func() { driver.releaseRuntime(runtime) }) }, nil
	}
}

func (driver *Driver) releaseRuntime(runtime *runtime) {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.runtime != runtime {
		return
	}
	runtime.mu.Lock()
	if runtime.leases > 0 {
		runtime.leases--
	}
	idleEligible := runtime.leases == 0 && len(runtime.sessions) == 0
	runtime.mu.Unlock()
	if idleEligible {
		driver.scheduleIdleLocked(runtime)
	}
}

func (driver *Driver) activate(runtime *runtime, threadID, model, workspace string) (*Session, error) {
	if threadID == "" || model == "" {
		return nil, provider.NewProviderError(provider.ErrorNoUsableModel)
	}
	native, err := driver.native(threadID, model)
	if err != nil {
		return nil, err
	}
	capabilities, known := runtime.models[model]
	if !known {
		capabilities = provider.Capabilities{}
	}
	session := newSession(driver, runtime, native, workspace, capabilities)
	if err := runtime.register(session); err != nil {
		return nil, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	driver.mu.Lock()
	driver.stopIdleLocked()
	driver.lastModel = model
	driver.mu.Unlock()
	return session, nil
}

func (driver *Driver) native(threadID, model string) (provider.NativeSession, error) {
	ref, err := provider.NewNativeSessionRef(threadID)
	if err != nil {
		return provider.NativeSession{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	now := driver.config.Clock.Now().UTC()
	native := provider.NativeSession{Ref: ref, Provider: provider.NameCodex, Model: model, CreatedAt: now, UpdatedAt: now}
	if native.Validate() != nil {
		return provider.NativeSession{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	return native, nil
}

func (driver *Driver) detach(session *Session) {
	driver.mu.Lock()
	runtime := driver.runtime
	if runtime != nil {
		runtime.unregister(session)
		runtime.mu.Lock()
		empty := len(runtime.sessions) == 0
		idleEligible := empty && runtime.leases == 0
		runtime.mu.Unlock()
		if idleEligible && driver.idleTimer == nil {
			driver.scheduleIdleLocked(runtime)
		}
	}
	driver.mu.Unlock()
}

func (driver *Driver) scheduleIdleLocked(runtime *runtime) {
	if runtime == nil || driver.runtime != runtime || driver.idleTimer != nil {
		return
	}
	driver.idleToken++
	token := driver.idleToken
	driver.idleTimer = time.AfterFunc(driver.config.IdleTimeout, func() { driver.expireIdle(runtime, token) })
}

func (driver *Driver) stopIdleLocked() {
	if driver.idleTimer == nil {
		return
	}
	driver.idleToken++
	driver.idleTimer.Stop()
	driver.idleTimer = nil
}

func (driver *Driver) expireIdle(runtime *runtime, token uint64) {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.idleToken != token {
		return
	}
	driver.idleTimer = nil
	if driver.runtime != runtime {
		return
	}
	runtime.mu.Lock()
	idle := runtime.leases == 0 && len(runtime.sessions) == 0 && len(runtime.pending) == 0 && len(runtime.inbound) == 0
	empty := len(runtime.sessions) == 0
	runtime.mu.Unlock()
	if idle {
		driver.runtime = nil
		runtime.close()
		return
	}
	if empty {
		driver.scheduleIdleLocked(runtime)
	}
}

func (driver *Driver) newID() (string, error) {
	driver.idMu.Lock()
	defer driver.idMu.Unlock()
	return driver.config.IDs.NewID()
}

type threadResponse struct {
	ID    string
	Model string
}

func parseThreadResponse(raw json.RawMessage) (threadResponse, error) {
	var response struct {
		Model  string `json:"model"`
		Thread struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Thread.ID == "" {
		return threadResponse{}, errors.New("invalid thread response")
	}
	model := response.Model
	if model == "" {
		model = response.Thread.Model
	}
	if model == "" {
		return threadResponse{}, errors.New("thread has no model")
	}
	return threadResponse{ID: response.Thread.ID, Model: model}, nil
}

func validAbsolutePath(value string) bool {
	return value != "" && filepath.IsAbs(value) && filepath.Clean(value) == value && len(value) <= provider.MaxNativeReferenceBytes
}

var _ provider.Driver = (*Driver)(nil)
