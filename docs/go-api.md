# Go API

Import `github.com/dndplsidc/agent-whiteboard/pkg/agentwb`. This stable public facade forwards to the internal application composition root, which assembles domain services, HTTP handlers, lifecycle, and default filesystem storage.

```go
service, err := agentwb.New(agentwb.Config{
    RootDir:        "/var/lib/agent-whiteboard",
    Host:           "127.0.0.1",
    Port:           8567,
    MaxContextBytes: 1 << 20,
})
if err != nil { return err }
defer service.Close()
return service.ListenAndServe(ctx)
```

The public Go configuration is programmatic and does not load CLI YAML or environment variables. Zero-valued fields use library defaults except where an option records an explicit value, such as `WithPort(0)` or `WithDefaultExpiration(0)`.

## Paired whiteboard contract

`Whiteboard` carries `ID`, `Kind`, `Source`, `Context`, `CreatedAt`, `UpdatedAt`, and `ExpiresAt`. `CreateWhiteboardInput` and `UpdateWhiteboardInput` likewise expose `Source`, `Context`, and expiration metadata. Every Markdown or HTML create and update requires non-empty UTF-8 `Source` and `Context`. Markdown retains its source validation; HTML also retains standalone-document structural validation.

```go
result, err := service.CreateMarkdown(ctx, agentwb.CreateWhiteboardInput{
    Source:  []byte("# Release status\n"),
    Context: []byte("# Creator context\n\nGoal: summarize current release status.\n"),
})
if err != nil { return err }

board, err := service.GetWhiteboard(ctx, result.ID)
if err != nil { return err }
fmt.Printf("%s\n%s", board.Source, board.Context)

htmlResult, err := service.CreateHTML(ctx, agentwb.CreateWhiteboardInput{
    Source:  []byte("<!doctype html><html><head></head><body>status</body></html>"),
    Context: []byte("# Creator context\n\nGoal: present release status as HTML.\n"),
})
if err != nil { return err }
_ = htmlResult
```

Context should summarize goals, decisions, assumptions, and open questions. It is stored and returned as part of the capability resource, not treated as hidden or confidential. Do not include hidden reasoning, credentials, personal or sensitive data, private source, or raw tool output.

`MaxContextBytes` is an independent HTTP transport limit with a 1 MiB default. `MaxWhiteboardBytes` remains the source limit. A zero value selects the default rather than disabling either limit. Domain service calls validate non-empty UTF-8 pairs but do not apply HTTP byte limits; callers that bypass the handler must enforce any application-specific bounds themselves.

Legacy schema-1 Markdown read through `GetWhiteboard` has an empty `Context`. Its first update must supply both the current/new source and a non-empty context; the replacement becomes one paired resource.

## Stores and create uncertainty

`Config` accepts `WhiteboardStore` and `ImageStore` independently. Missing stores use the built-in filesystem. Domain boundaries communicate through services; custom stores implement only their domain-owned interface:

```go
type WhiteboardStore interface {
    Create(context.Context, agentwb.Whiteboard) error
    Get(context.Context, string) (agentwb.Whiteboard, error)
    Replace(context.Context, agentwb.Whiteboard) error
    Delete(context.Context, string) error
    Ready(context.Context) error
    Close() error
}
```

`ImageStore` has the same method shape with `agentwb.Image`. Implementations must honor context cancellation, distinguish invalid/not-found/storage errors, preserve all model metadata including whiteboard `Context`, make paired replacements atomic, report readiness, and make `Close` safe to call more than once. Inject them through `Config`; tests can inject testify/mock implementations.

```go
service, err := agentwb.New(agentwb.Config{
    WhiteboardStore: myWhiteboardStore,
    ImageStore:      myImageStore,
})
```

A custom `WhiteboardStore.Create` error normally promises that no resource was left behind. If rollback cannot establish absence, return an error implementing `agentwb.UncertainCreateError`:

```go
type UncertainCreateError interface {
    error
    ResourceMayExist() bool
}
```

When `ResourceMayExist()` is true, `CreateMarkdown` or `CreateHTML` returns the generated `WhiteboardResult` together with the error. The result retains the capability ID so the caller can check or delete a possibly live resource. Callers must inspect both values rather than discarding the result whenever `err != nil`:

```go
result, err := service.CreateMarkdown(ctx, input)
if err != nil {
    var uncertain agentwb.UncertainCreateError
    if errors.As(err, &uncertain) && uncertain.ResourceMayExist() {
        // result.ID may be live; retain it for a GetWhiteboard/DeleteWhiteboard check.
    }
    return err
}
```

Update errors do not use this interface. A store remains responsible for documenting whether a post-commit durability or cleanup error can accompany an already-visible replacement.

## Service and lifecycle

The service methods are `CreateMarkdown`, `CreateHTML`, `GetWhiteboard`, `UpdateWhiteboard`, `DeleteWhiteboard`, `CreateImages`, `GetImage`, `UpdateImage`, and `DeleteImage`. `Image` carries lifecycle fields plus `Extension`, `MediaType`, and `Content`. Create expiration omission uses the configured default; zero means permanent. Update omission preserves expiration.

Every operation uses the caller's `context.Context`; cancellation/deadlines are propagated to the store and HTTP request. Do not replace a request context with a background context. The internally owned filesystem lifetime and bounded shutdown cleanup are the intentional exceptions.

`Handler()` embeds the application in another HTTP server. `Ready(ctx)` checks the whiteboard and image stores in dependency order with the exact caller context; it does not report whether an externally owned HTTP server is listening or draining. An embedding application should combine `Ready(ctx)` with its own traffic-admission state in its aggregate readiness endpoint.

```go
if !serverAccepting.Load() {
    return errors.New("not accepting requests")
}
if err := service.Ready(ctx); err != nil {
    return err
}
```

The built-in `/readyz` endpoint continues to combine the library-owned server lifecycle with those same dependency checks. `Serve(ctx, listener)` uses the supplied listener, `ListenAndServe(ctx)` uses the configured host/port, `Shutdown(ctx)` performs caller-bounded graceful shutdown, and `Close()` is idempotent and releases owned/custom stores. A listener can also be selected with `agentwb.WithListener(listener)`. `WithClock`, `WithIDGenerator`, `WithPort`, `WithDefaultExpiration`, and `WithViewerAssets` provide narrower dependency injection.

Errors expose stable codes:

```go
var domainErr *agentwb.Error
if errors.As(err, &domainErr) { /* inspect domainErr.Code and domainErr.Message */ }
if agentwb.HasErrorCode(err, agentwb.CodeNotFound) { /* handle absence */ }
```

Exported codes are `CodeInvalidRequest`, `CodeNotFound`, `CodeContentTooLarge`, `CodeUnsupportedMediaType`, `CodeStorageUnavailable`, and `CodeInternal`. Prefer `errors.As`/`errors.Is` and `agentwb.HasErrorCode`; do not parse messages. Public errors are sanitized at HTTP and CLI boundaries, and source/context bytes are not included in error messages.
