# Internal Composition and Nil Handling Design

## Goal

Remove the CLI's dependency on the public `pkg/agentwb` facade, centralize typed-nil detection, and consistently treat nil contexts as programmer errors.

The refactor must preserve the supported public Go API, CLI behavior, server behavior, and all non-nil context propagation and cancellation semantics.

## Architecture

`internal/app` will own application configuration, production defaults, dependency options, service composition, and lifecycle ownership. This makes it the shared composition root for both the executable and the public facade.

`internal/cli` will construct internal application configuration and call the internal application factory directly. It will not import `pkg/agentwb`.

`pkg/agentwb` will remain the supported external import path. Its existing exported configuration, option, and service API will be preserved through type aliases where exact type identity is appropriate and thin forwarding functions where constructors or options require functions. It will contain no independent composition or validation logic.

No new composition package will be introduced because the existing architecture already assigns that responsibility to `internal/app`.

## Shared Typed-Nil Detection

Add `common.IsNil(value any) bool` to `internal/common`. It will return true for a nil interface and for typed-nil values whose reflection kind can be nil: channels, functions, interfaces, maps, pointers, and slices. It will return false for non-nil values and non-nilable kinds.

All reflection-based typed-nil helpers will be replaced with `common.IsNil`, including the current helpers in:

- `internal/app`
- `internal/cli`
- `internal/webapi`
- `internal/image`
- `internal/store`
- `internal/whiteboard`
- `pkg/agentwb`

The package-specific wrappers and their `reflect` imports will be removed. Call sites will retain their existing domain-specific error messages and behavior.

## Context Contract

Every API that accepts `context.Context` will require a non-nil context by contract. Nil is a programmer error, consistent with Go's context package guidance; callers that have no context should pass `context.TODO()` or `context.Background()`.

Explicit nil-context checks and conditional guards will be removed throughout the repository, including server lifecycle methods, storage construction and operations, and client error normalization. Code will continue to check `ctx.Err()` and propagate cancellation or deadlines for valid contexts.

Tests that currently require stable errors for nil contexts will be removed or rewritten to cover cancellation and propagation with valid contexts. No replacement panic contract will be added: a nil context may panic naturally when used.

## Public API Compatibility

Existing external source-level names in `pkg/agentwb` will remain available:

- `Config`, `Option`, `LogMode`, and log-mode constants
- `New` and all existing option constructors
- `Service` and its domain, handler, and lifecycle methods
- public model, store, clock, ID, and error aliases

Default resolution, validation messages/codes, injected stores, listener behavior, asset injection, logger selection, and cleanup ownership will remain unchanged. Moving their implementation must not create an import cycle.

## Testing

Development will proceed test-first. Coverage will include:

1. A dependency-boundary test proving `internal/cli` does not import `pkg/agentwb`.
2. `common.IsNil` unit tests for nil interfaces, typed-nil values of every supported nilable kind, non-nil values, and non-nilable values.
3. Existing constructor and typed-nil regression tests, updated to exercise the shared helper through real call sites.
4. Existing public external API tests to prove the facade remains source-compatible and behavior-compatible.
5. Existing CLI serve, application composition, storage, HTTP client, and server lifecycle tests.
6. Repository-wide formatting, unit/integration tests, race tests, vet, asset checks, and browser tests as applicable under `AGENTS.md`.

## Documentation

Architecture documentation that describes `pkg/agentwb` as the composition root will be corrected to describe `internal/app` as the implementation owner and `pkg/agentwb` as the stable public facade. User-facing commands and public API examples should remain unchanged.

## Out of Scope

- Changing CLI flags, environment variables, output, or exit codes
- Changing HTTP routes or payloads
- Changing storage formats or lifecycle semantics
- Adding new public API
- Introducing a new internal composition package
