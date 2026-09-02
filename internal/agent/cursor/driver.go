package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
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
	config          Config
	probeMu         sync.Mutex
	lastModel       string
	cachedReadiness provider.Readiness
	cacheUntil      time.Time
}

type inboundRouter struct {
	mu      sync.RWMutex
	session *Session
}

func (r *inboundRouter) handle(ctx context.Context, request acp.Request) {
	r.mu.RLock()
	session := r.session
	r.mu.RUnlock()
	if session == nil {
		if request.Responder != nil {
			_, _ = request.Responder.Respond(ctx, nil, &acp.RPCError{Code: -32601, Message: "method not found"})
		}
		return
	}
	session.handle(ctx, request)
}

func (r *inboundRouter) publish(session *Session) {
	r.mu.Lock()
	r.session = session
	r.mu.Unlock()
}

func NewDriver(config Config) (*Driver, error) {
	if config.Environment == nil || common.IsNil(config.Launcher) || common.IsNil(config.IDs) || common.IsNil(config.Clock) || !absolute(config.Executable) || !absolute(config.ProviderRoot) {
		return nil, errors.New("invalid Cursor driver configuration")
	}
	if config.IdleTimeout <= 0 {
		return nil, errors.New("invalid Cursor driver configuration")
	}
	launch := provider.LaunchRequest{Executable: config.Executable, Arguments: []string{"acp"}, Environment: config.Environment, WorkingDirectory: config.ProviderRoot}
	if launch.Validate() != nil {
		return nil, errors.New("invalid Cursor driver configuration")
	}
	config.Environment = slices.Clone(config.Environment)
	return &Driver{config: config}, nil
}
func absolute(p string) bool { return p != "" && filepath.IsAbs(p) && filepath.Clean(p) == p }

func (d *Driver) Readiness(ctx context.Context) provider.Readiness {
	if d == nil {
		return provider.Readiness{State: provider.StartupFailed, Provider: provider.NameCursor}
	}
	if err := validateCursorExecutable(d.config.Executable); err != nil {
		return provider.Readiness{State: provider.MissingExecutable, Provider: provider.NameCursor}
	}
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	now := d.config.Clock.Now().UTC()
	if d.cachedReadiness.State.Valid() && now.Before(d.cacheUntil) {
		return d.cachedReadiness
	}
	rt, err := d.launch(ctx, d.config.ProviderRoot, nil)
	if err != nil {
		return readinessError(err)
	}
	defer rt.close(context.Background())
	if _, err = walkSessionList(ctx, rt, ""); err != nil {
		return readinessError(err)
	}
	model := rt.defaultModel()
	if model == "" {
		model = "Cursor default"
	}
	d.lastModel = model
	result := provider.Readiness{State: provider.Ready, Provider: provider.NameCursor, Model: model}
	d.cachedReadiness = result
	d.cacheUntil = now.Add(d.config.IdleTimeout)
	return result
}
func walkSessionList(ctx context.Context, rt *runtime, target string) (*listItem, error) {
	seenSessions := make(map[string]struct{})
	seenCursors := make(map[string]struct{})
	var found *listItem
	totalSessions := 0
	totalBytes := 0
	cursor := ""
	for pageNumber := 0; pageNumber < maxListPages; pageNumber++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var raw json.RawMessage
		if _, err := rt.client.Call(ctx, "session/list", params, &raw); err != nil {
			return nil, classifyRPC(err)
		}
		var page listResult
		if json.Unmarshal(raw, &page) != nil || page.validate() != nil {
			return nil, provider.NewProviderError(provider.ErrorProtocolIncompatible)
		}
		totalSessions += len(page.Sessions)
		pageBytes := page.semanticBytes()
		if totalSessions > maxListSessions || pageBytes > maxListSemanticBytes || totalBytes > maxListSemanticBytes-pageBytes {
			return nil, provider.NewProviderError(provider.ErrorProtocolIncompatible)
		}
		totalBytes += pageBytes
		for i := range page.Sessions {
			item := &page.Sessions[i]
			if _, duplicate := seenSessions[item.SessionID]; duplicate {
				return nil, provider.NewProviderError(provider.ErrorProtocolIncompatible)
			}
			seenSessions[item.SessionID] = struct{}{}
			if found == nil && target != "" && item.SessionID == target {
				retained := *item
				found = &retained
			}
		}
		if page.NextCursor == "" {
			return found, nil
		}
		if _, duplicate := seenCursors[page.NextCursor]; duplicate {
			return nil, provider.NewProviderError(provider.ErrorProtocolIncompatible)
		}
		seenCursors[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	return nil, provider.NewProviderError(provider.ErrorProtocolIncompatible)
}

func readinessError(err error) provider.Readiness {
	var pe provider.ProviderError
	if errors.As(err, &pe) {
		switch pe.Code() {
		case provider.ErrorAuthenticationRequired:
			return provider.Readiness{State: provider.AuthenticationRequired, Provider: provider.NameCursor}
		case provider.ErrorProtocolIncompatible:
			return provider.Readiness{State: provider.ProtocolIncompatible, Provider: provider.NameCursor}
		}
	}
	return provider.Readiness{State: provider.StartupFailed, Provider: provider.NameCursor}
}

func (d *Driver) Create(ctx context.Context, req provider.CreateRequest) (provider.Session, error) {
	if d == nil || req.Provider != provider.NameCursor || req.Validate() != nil {
		return nil, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	return d.open(ctx, req.Workspace, "session/new", map[string]any{"cwd": req.Workspace, "mcpServers": []any{}}, req.Settings)
}
func (d *Driver) Resume(ctx context.Context, req provider.ResumeRequest) (provider.Session, error) {
	if d == nil || req.Provider != provider.NameCursor || req.Validate() != nil {
		return nil, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	return d.open(ctx, req.Workspace, "session/load", map[string]any{"sessionId": req.NativeSession.Value(), "cwd": req.Workspace, "mcpServers": []any{}}, nil)
}
func (d *Driver) open(ctx context.Context, workspace, method string, params map[string]any, wanted *provider.ExecutionSettings) (provider.Session, error) {
	router := &inboundRouter{}
	rt, err := d.launch(ctx, workspace, router.handle)
	if err != nil {
		return nil, err
	}
	session := newSession(d, rt, workspace)
	if method == "session/load" {
		session.beginReplay()
	}
	router.publish(session)
	go func() { <-rt.client.Done(); session.transportEnded() }()
	loadedSessionID := ""
	if method == "session/load" {
		target, validTarget := params["sessionId"].(string)
		if !validTarget || target == "" {
			_ = session.Shutdown(context.Background())
			return session, provider.NewProviderError(provider.ErrorProtocolFailure)
		}
		listed, listErr := walkSessionList(ctx, rt, target)
		if listErr != nil {
			_ = session.Shutdown(context.Background())
			return session, listErr
		}
		if listed == nil {
			_ = session.Shutdown(context.Background())
			return session, provider.NewProviderError(provider.ErrorNativeSessionMissing)
		}
		loadedSessionID = target
	}
	var opened openResult
	if _, err = rt.client.Call(ctx, method, params, &opened); err != nil {
		_ = session.Shutdown(context.Background())
		return session, classifyRPC(err)
	}
	loaded := loadedSessionID != ""
	if loaded {
		if opened.SessionID != "" && opened.SessionID != loadedSessionID {
			_ = session.Shutdown(context.Background())
			return session, provider.NewProviderError(provider.ErrorProtocolIncompatible)
		}
		opened.SessionID = loadedSessionID
	}
	if err = session.finishOpen(opened, loaded); err != nil {
		_ = session.Shutdown(context.Background())
		return session, err
	}
	if wanted != nil {
		if _, _, err = session.ApplySettings(ctx, *wanted); err != nil {
			return session, err
		}
	}
	return session, nil
}
func (d *Driver) Inspect(ctx context.Context, req provider.InspectRequest) (provider.NativeSession, error) {
	if d == nil || req.Provider != provider.NameCursor || req.Validate() != nil {
		return provider.NativeSession{}, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	rt, err := d.launch(ctx, d.config.ProviderRoot, nil)
	if err != nil {
		return provider.NativeSession{}, err
	}
	defer rt.close(context.Background())
	item, err := walkSessionList(ctx, rt, req.NativeSession.Value())
	if err != nil {
		return provider.NativeSession{}, err
	}
	if item == nil {
		return provider.NativeSession{}, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	catalog, settings, presentation, err := catalogFromOptions(item.ConfigOptions, rt.caps.Image)
	if err != nil {
		return provider.NativeSession{}, err
	}
	_ = catalog
	now := d.config.Clock.Now().UTC()
	return provider.NativeSession{Ref: req.NativeSession, Provider: provider.NameCursor, Model: settings.Model, Settings: &settings, Presentation: &presentation, CreatedAt: now, UpdatedAt: now}, nil
}
func (d *Driver) launch(ctx context.Context, cwd string, handler acp.RequestHandler) (*runtime, error) {
	request := provider.LaunchRequest{Executable: d.config.Executable, Arguments: []string{"acp"}, Environment: slices.Clone(d.config.Environment), WorkingDirectory: cwd}
	// Revalidate immediately before handing the path to the launcher. The
	// launcher contract accepts a pathname, so this check must not be moved
	// earlier where a substitution could occur in the intervening setup.
	if err := validateCursorExecutable(request.Executable); err != nil {
		return nil, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	child, err := d.config.Launcher.Launch(ctx, request)
	if err != nil || common.IsNil(child) {
		return nil, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	client, err := acp.New(child, acp.Options{
		Handler:               handler,
		MaxInboundFrameBytes:  acp.SupportedMaxFrameBytes,
		MaxOutboundFrameBytes: acp.SupportedMaxFrameBytes,
		MaxRetainedBytes:      acp.SupportedMaxRetainedBytes,
		HandlerTimeout:        30 * time.Second,
		MaxHandlerConcurrency: 1,
	})
	if err != nil {
		return nil, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	rt := &runtime{child: child, client: client}
	var initialized initializeResult
	params := map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]string{"name": "agent-whiteboard", "version": "1"}}
	if _, err = client.Call(ctx, "initialize", params, &initialized); err != nil {
		_ = rt.close(context.Background())
		return nil, classifyRPC(err)
	}
	caps, err := parseCapabilities(initialized)
	if err != nil {
		_ = rt.close(context.Background())
		return nil, err
	}
	rt.caps = caps
	return rt, nil
}
func classifyRPC(err error) error {
	var rpc *acp.RPCError
	if errors.As(err, &rpc) {
		if rpc.Code == -32000 || rpc.Code == -32001 || rpc.Code == -32002 || rpc.Code == -32003 {
			return provider.NewProviderError(provider.ErrorAuthenticationRequired)
		}
		if rpc.Code == -32601 || rpc.Code == -32602 {
			return provider.NewProviderError(provider.ErrorProtocolIncompatible)
		}
	}
	if errors.Is(err, acp.ErrMalformed) || errors.Is(err, acp.ErrFrameTooLarge) {
		return provider.NewProviderError(provider.ErrorMalformedStream)
	}
	if errors.Is(err, acp.ErrChildExited) {
		return provider.NewProviderError(provider.ErrorChildExited)
	}
	return provider.NewProviderError(provider.ErrorProtocolFailure)
}

var _ provider.Driver = (*Driver)(nil)
