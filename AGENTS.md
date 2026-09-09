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

UI consistency is a completion requirement, not optional polish. New and changed browser UI must look and behave like the surrounding viewer and Page Agent components. This applies equally to errors, recovery notices, empty states, and infrequently used actions.

- Before editing, identify the closest existing component and inspect its markup and styles. Reuse its shared classes or grouped selectors and existing theme variables; do not approximate it with a separate set of values.
- Match the component's typography, control height, padding, spacing, icon size and alignment, borders, radii, colors, and action hierarchy. A generic button class or `font: inherit` alone does not establish consistency: check the actual computed font size and dimensions in the rendered context.
- Place notice actions in a properly spaced action row aligned with the copy. Do not append an oversized or unstyled button directly against explanatory text. Keep compact recovery actions consistent with neighboring controls.
- Keep layouts readable at narrow widths and with wrapping text. Controls must not overflow, crowd the copy, or change scale unexpectedly between states. Reuse established hover, keyboard-focus, disabled, and loading treatments.
- Do not introduce a new visual pattern or redesign surrounding components unless the user requests it or approves the departure.

Before completing a user-visible UI change, inspect the actual changed state alongside adjacent components in a real browser at desktop and narrow widths, in both light and dark themes. Capture and inspect screenshots of those states. Verify pointer, keyboard, loading, success, error, disabled, and interruption states where relevant. Add computed-style or layout regression assertions for sizing, alignment, spacing, and overflow when those properties caused the defect. Passing click/visibility tests or merely generating screenshots is not visual verification. If rendered inspection cannot be completed, report that gap rather than calling the UI finished.

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
