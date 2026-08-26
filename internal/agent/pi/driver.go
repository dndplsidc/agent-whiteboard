//go:build unix

package pi

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
)

type OneShotTimer interface {
	Stop() bool
}

type TimerFactory interface {
	AfterFunc(time.Duration, func()) OneShotTimer
}

// Config contains every dependency and environment value used by the Pi adapter.
// Environment is passed verbatim; the ambient process environment is never read.
type Config struct {
	Executable   string
	Environment  []string
	ProviderRoot string
	Launcher     provider.Launcher
	IDs          common.IDGenerator
	Clock        common.Clock
	Timers       TimerFactory
}

type Driver struct {
	config      Config
	native      *nativeManager
	mu          sync.Mutex
	active      map[string]provider.Session
	unfinalized map[string]nativeAllocation
	lastModel   string
	idMu        sync.Mutex
}

func NewDriver(config Config) (*Driver, error) {
	if config.Environment == nil || common.IsNil(config.Launcher) || common.IsNil(config.IDs) || common.IsNil(config.Clock) || !validLaunchPath(config.Executable) || !validCanonicalPath(config.ProviderRoot) {
		return nil, errors.New("invalid Pi driver configuration")
	}
	request := provider.LaunchRequest{Executable: config.Executable, Arguments: []string{}, Environment: config.Environment, WorkingDirectory: config.ProviderRoot}
	if request.Validate() != nil {
		return nil, errors.New("invalid Pi driver configuration")
	}
	native, err := newNativeManager(config.ProviderRoot, config.IDs, config.Clock)
	if err != nil {
		return nil, err
	}
	config.Environment = slices.Clone(config.Environment)
	if common.IsNil(config.Timers) {
		config.Timers = realTimerFactory{}
	}
	return &Driver{config: config, native: native, active: make(map[string]provider.Session), unfinalized: make(map[string]nativeAllocation)}, nil
}

func (d *Driver) Readiness(context.Context) provider.Readiness {
	model := "Pi default"
	if d != nil {
		d.mu.Lock()
		if d.lastModel != "" {
			model = d.lastModel
		}
		d.mu.Unlock()
	}
	if d == nil || d.native == nil || d.native.verify() != nil {
		return provider.Readiness{State: provider.StartupFailed, Provider: provider.NamePi}
	}
	info, err := os.Stat(d.config.Executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return provider.Readiness{State: provider.MissingExecutable, Provider: provider.NamePi}
	}
	return provider.Readiness{State: provider.Ready, Provider: provider.NamePi, Model: model}
}

func (d *Driver) Create(ctx context.Context, request provider.CreateRequest) (provider.Session, error) {
	if d == nil || request.Validate() != nil {
		return nil, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	allocation, err := d.native.allocate(request.Workspace)
	if err != nil {
		return nil, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	launch, err := buildLaunchRequest(launchConfig{Executable: d.config.Executable, Environment: d.config.Environment}, request.Workspace, d.native.sessions, allocation.path)
	if err != nil {
		_ = d.native.rollbackAllocation(allocation)
		return nil, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	child, err := d.config.Launcher.Launch(ctx, launch)
	if common.IsNil(child) {
		_ = d.native.rollbackAllocation(allocation)
		return nil, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	if err != nil {
		return d.retainFailedAllocation(allocation, child, nil), provider.NewProviderError(provider.ErrorStartupFailed)
	}
	client, err := newRPCClient(child)
	if err != nil {
		return d.retainFailedAllocation(allocation, child, nil), provider.NewProviderError(provider.ErrorStartupFailed)
	}
	state, err := startup(ctx, client, allocation.path, request.Workspace)
	if err != nil {
		client.finish(err)
		return d.retainFailedAllocation(allocation, child, client), err
	}
	native, err := d.native.finalizeAllocation(allocation, state)
	if err != nil {
		client.finish(err)
		// A published exact sidecar is durable provider state; otherwise retain
		// the allocation identity so broker-owned cleanup can roll it back only
		// after the child has stopped.
		if discovered, inspectErr := d.native.inspect(allocation.Ref); inspectErr == nil {
			return d.retainFailedNative(discovered, child, client), err
		}
		return d.retainFailedAllocation(allocation, child, client), err
	}
	session, err := d.activate(native, state, child, client)
	if err != nil {
		client.finish(err)
		return d.retainFailedNative(native, child, client), err
	}
	if request.Settings != nil {
		if _, _, applyErr := session.ApplySettings(ctx, *request.Settings); applyErr != nil {
			return session, applyErr
		}
	}
	return session, nil
}

func (d *Driver) Resume(ctx context.Context, request provider.ResumeRequest) (provider.Session, error) {
	if d == nil || request.Validate() != nil {
		return nil, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	key := request.NativeSession.Value()
	if !d.reserve(key) {
		return nil, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	reserved := true
	defer func() {
		if reserved {
			d.release(key, nil)
		}
	}()
	metadata, err := d.native.readMetadata(request.NativeSession)
	inspected, inspectErr := d.native.inspect(request.NativeSession)
	if err != nil || inspectErr != nil || metadata.Workspace != request.Workspace || inspected.Validate() != nil {
		return nil, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	path := d.native.sessionPath(request.NativeSession)
	launch, err := buildLaunchRequest(launchConfig{Executable: d.config.Executable, Environment: d.config.Environment}, request.Workspace, d.native.sessions, path)
	if err != nil {
		return nil, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	child, err := d.config.Launcher.Launch(ctx, launch)
	if common.IsNil(child) {
		return nil, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	if err != nil {
		session := d.retainFailedNative(inspected, child, nil)
		reserved = false
		return session, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	client, err := newRPCClient(child)
	if err != nil {
		session := d.retainFailedNative(inspected, child, nil)
		reserved = false
		return session, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	state, err := startup(ctx, client, path, request.Workspace)
	if err != nil {
		client.finish(err)
		session := d.retainFailedNative(inspected, child, client)
		reserved = false
		return session, err
	}
	if state.Model != metadata.Model {
		failure := provider.NewProviderError(provider.ErrorProtocolIncompatible)
		client.finish(failure)
		session := d.retainFailedNative(inspected, child, client)
		reserved = false
		return session, failure
	}
	native := metadata.nativeForState(request.NativeSession, state)
	if err := d.native.updateSettings(native); err != nil {
		client.finish(err)
		session := d.retainFailedNative(native, child, client)
		reserved = false
		return session, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	session := newSession(d, native, state, child, client)
	d.mu.Lock()
	d.active[key] = session
	d.lastModel = state.Model
	d.mu.Unlock()
	session.start()
	reserved = false
	return session, nil
}

func (d *Driver) Inspect(_ context.Context, request provider.InspectRequest) (provider.NativeSession, error) {
	if d == nil || request.Validate() != nil {
		return provider.NativeSession{}, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	return d.native.inspect(request.NativeSession)
}

func (d *Driver) Delete(_ context.Context, request provider.DeleteRequest) error {
	if d == nil || request.Validate() != nil {
		return provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	d.mu.Lock()
	_, active := d.active[request.NativeSession.Value()]
	allocation, unfinalized := d.unfinalized[request.NativeSession.Value()]
	d.mu.Unlock()
	if active {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	if unfinalized {
		if err := d.native.rollbackAllocation(allocation); err != nil {
			return provider.NewProviderError(provider.ErrorNativeSessionMissing)
		}
		d.mu.Lock()
		delete(d.unfinalized, request.NativeSession.Value())
		d.mu.Unlock()
		return nil
	}
	return d.native.delete(request.NativeSession)
}

func (d *Driver) activate(native provider.NativeSession, state startupState, child provider.ManagedChild, client *rpcClient) (*Session, error) {
	key := native.Ref.Value()
	if !d.reserve(key) {
		return nil, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s := newSession(d, native, state, child, client)
	d.mu.Lock()
	d.active[key] = s
	d.lastModel = state.Model
	d.mu.Unlock()
	s.start()
	return s, nil
}
func (d *Driver) reserve(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.active[key]; ok {
		return false
	}
	d.active[key] = nil
	return true
}
func (d *Driver) release(key string, expected provider.Session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if current, ok := d.active[key]; ok && (expected == nil || current == expected) {
		delete(d.active, key)
	}
}

func (d *Driver) newID() (string, error) {
	d.idMu.Lock()
	defer d.idMu.Unlock()
	return d.config.IDs.NewID()
}

func (d *Driver) retainFailedAllocation(allocation nativeAllocation, child provider.ManagedChild, client *rpcClient) provider.Session {
	settings := provider.ExecutionSettings{Model: "Pi unavailable", Effort: "off", Speed: provider.SpeedStandard}
	presentation := provider.ModelPresentation{ModelDisplayName: "Pi unavailable", Selectable: false}
	native := provider.NativeSession{Ref: allocation.Ref, Provider: provider.NamePi, Model: settings.Model, Settings: &settings, Presentation: &presentation, CreatedAt: allocation.createdAt, UpdatedAt: allocation.createdAt}
	d.mu.Lock()
	d.unfinalized[allocation.Ref.Value()] = allocation
	d.mu.Unlock()
	return d.retainFailedNative(native, child, client)
}

func (d *Driver) retainFailedNative(native provider.NativeSession, child provider.ManagedChild, client *rpcClient) provider.Session {
	var session provider.Session
	if client != nil {
		state := startupState{Model: native.Model, ModelProvider: "Pi", ModelID: "unavailable", ContextWindow: 1, MaxTokens: 1}
		active := newSession(d, native, state, child, client)
		session = active
		d.mu.Lock()
		d.active[native.Ref.Value()] = active
		d.mu.Unlock()
		active.start()
		return active
	}
	failed := newFailedSession(d, native, child)
	session = failed
	d.mu.Lock()
	d.active[native.Ref.Value()] = failed
	d.mu.Unlock()
	return session
}

var _ provider.Driver = (*Driver)(nil)
