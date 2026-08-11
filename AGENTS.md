# AGENTS.md

## Project

`agent-whiteboard` is a self-hosted Go server, CLI, and library for publishing Markdown, trusted standalone HTML, and raster images at capability URLs.

The project includes a Go backend and CLI, bundled browser assets, filesystem storage, public Go and HTTP APIs, and an agent-facing skill.

## Structure and packaging

- `cmd/agent-whiteboard` is the thin executable entry point.
- `pkg/agentwb` is the supported public Go API.
- `internal/` contains implementation packages organized by domain or responsibility.
- `tests/integration` covers real-component and process workflows.
- `tests/browser` contains Playwright end-to-end tests.
- `docs/` and `skills/agent-whiteboard/` contain user- and agent-facing guidance.

Place new code in the existing package that owns the behavior. Create a package only when it introduces a distinct, independently testable responsibility or dependency boundary.

Keep business behavior in its domain package, infrastructure behind domain-owned interfaces, and concrete dependency wiring in `internal/app`. Keep APIs internal unless external Go consumers need a stable contract through `pkg/agentwb`.

## Testing

Every behavioral change must add or update tests at all applicable levels:

- **Unit:** isolated logic, validation, edge cases, and errors.
- **Integration:** boundaries between real components, including storage, HTTP, CLI, and processes.
- **End-to-end:** complete user-visible server or browser workflows.

Use the test levels that can meaningfully detect regressions from the change. Bug fixes must include a regression test.

Tests must be hermetic, deterministic, and isolated. They must not depend on public networks, hosted services, credentials, existing machine state, or fixed ports. Prefer temporary directories, ephemeral ports, local servers, injected dependencies, and committed fixtures. Clean up all resources created by tests.

Keep repeated stress runs focused. Run an affected package normally first, then use `-count` only with a narrow `-run` expression for tests that exercise a specific race or nondeterministic interleaving. Do not apply high repeat counts to an entire package—especially one containing filesystem `fsync`, process, timeout, or integration tests. Run the race detector once for the affected package, increasing the count only for focused race-sensitive tests. Run repository-wide normal and race checks once at the milestone boundary.

For example:

```sh
go test ./internal/agent/pi
go test ./internal/agent/pi -run 'TestSessionSubmitNegativeAndAmbiguousAcceptance|TestNativeConcurrentExactFinalization' -count=20
go test -race ./internal/agent/pi
```

Run the checks applicable to the change:

```sh
go test ./...
go test -race ./...
go vet ./...
pnpm test
pnpm run check:assets
pnpm run test:browser
```

## Documentation

Keep documentation synchronized with behavior in the same change.

Update the affected `README.md`, detailed documents under `docs/`, examples, exported API comments, and agent skill instructions. Commands and examples must remain accurate and runnable.

A change is not complete when its tests pass but its documentation is outdated.
