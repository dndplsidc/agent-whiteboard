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
- `docs/` and `skills/` contain user- and agent-facing guidance.

Place new code in the existing package that owns the behavior. Create a package only when it introduces a distinct, independently testable responsibility or dependency boundary.

Keep business behavior in its domain package, infrastructure behind domain-owned interfaces, and concrete dependency wiring in `internal/app`. Keep APIs internal unless external Go consumers need a stable contract through `pkg/agentwb`.

## User interface consistency

New and changed browser UI must follow the established visual language of the surrounding viewer and Page Agent surfaces. Reuse existing typography, spacing, colors, radii, controls, status patterns, and interaction behavior before introducing a new presentation. Do not add a component that looks or behaves like a separate design system.

Before completing a user-visible UI change, compare it with adjacent existing components in the real rendered interface at applicable desktop, narrow, light, and dark states. Verify pointer, keyboard, loading, success, error, disabled, and interruption states where relevant. Automated assertions are necessary but do not replace real-browser visual inspection.

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

## Pull requests and CI

After creating a pull request or pushing commits to a branch with an open pull request, monitor the pull request's CI checks asynchronously and continue other useful work while they run. Use an asynchronous tool from the agent's active tool list that reports completion back into the current session, such as an async subagent task with a completion event or an equivalent callback-capable tool. Do not rely on detached shell processes, `nohup`, background polling, or status files alone, because they do not notify the agent when CI finishes. If no notification-capable asynchronous tool is available, state that limitation explicitly and arrange an explicit follow-up check instead of claiming autonomous monitoring.

Before reporting the work as complete, receive or retrieve the asynchronous result and confirm that all required pull request checks finished successfully. If a check fails, inspect its logs, identify and correct the root cause, push the fix, and start a new notification-capable asynchronous monitor for the replacement checks.

## Documentation

Keep documentation synchronized with behavior in the same change.

Update the affected `README.md`, detailed documents under `docs/`, examples, exported API comments, and agent skill instructions. Commands and examples must remain accurate and runnable.

A change is not complete when its tests pass but its documentation is outdated.
