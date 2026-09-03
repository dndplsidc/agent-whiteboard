---
name: agent-whiteboard-setup
description: Use when an agent needs to install or update Agent Whiteboard, configure publishing servers, set up Page Agent, origin trust, the local broker or macOS daemon, or diagnose setup failures. Use agent-whiteboard to publish resources.
---

# Agent Whiteboard Setup

Set up only the mode the user needs. Preserve existing configuration and provider state.

## Choose the setup mode

| User goal | Required work |
| --- | --- |
| Publish to an existing server | Install the CLI; verify the selected server |
| Run a local publishing server | Install the CLI; start and verify `agent-whiteboard serve` |
| Use Page Agent on a remote whiteboard | Install the CLI; prepare a provider; trust the exact origin; start the local broker |
| Run everything locally | Start a Page-Agent-enabled publishing server; prepare a provider; start the local broker |

Do not configure providers or a broker for publishing-only use. Do not start a publishing server for a reader using an existing remote deployment.

When a general setup request does not make clear whether Page Agent is wanted, ask whether to include it before checking providers, changing origin trust, or starting the broker. Do not ask this for publishing-only requests. When the user explicitly requests Page Agent or a local all-in-one setup, the broker is required; do not ask whether to enable it again. Use supervised foreground operation by default, and install the macOS managed daemon only when the user explicitly requests persistent operation.

## Protect existing state

Before durable changes, follow the active harness's approval requirements.

- Inspect an existing configuration before editing it; merge only required fields.
- Use the same configuration path for the server, trust commands, broker, and daemon.
- Never collect, copy, or edit Pi, Codex, or Cursor credentials or native configuration. Never run provider authentication for the user.
- Prefer supervised foreground processes for initial setup.
- Install a persistent daemon only when the user requests it.
- Do not delete whiteboards, provider histories, or broker state to fix setup.

## Inspect

```sh
uname -s
command -v agent-whiteboard || true
go version || true
```

Check `pi`, `codex`, and exactly `cursor-agent` only when Page Agent is required. Never check a generic `agent` executable for Cursor default discovery.

Before offering to start a Page Agent broker, inspect any existing foreground broker or macOS managed daemon and confirm that its process owns a loopback listening socket on the configured `agent.port` (default `8568`). Read [Page Agent setup](references/page-agent.md) for the exact lifecycle and listener checks. Do not send publishing-server `/healthz` or `/readyz` probes to the broker port: those routes belong to the publishing server, not the broker, and their failure does not mean the broker is stopped.

For publishing, resolve the target in this order: explicit `--server`, `AGENT_WHITEBOARD_SERVER`, YAML `client.server`, then built-in localhost.
If the first three are absent and localhost is not intended, ask for the publishing origin instead of guessing.

## Install or update

Agent Whiteboard supports macOS and Linux with Go 1.25 or 1.26.

```sh
go install github.com/dndplsidc/agent-whiteboard/cmd/agent-whiteboard@latest
command -v agent-whiteboard
agent-whiteboard --help
```

If installation succeeds but the command is missing, inspect `go env GOBIN` and `go env GOPATH`, then report the required `PATH` addition. Do not edit shell startup files unless requested.

If the binary already exists, do not reinstall it unless the user asks to update it.

## Use an existing publishing server

Use the origin supplied by the user or resolved from `AGENT_WHITEBOARD_SERVER` or YAML `client.server`. If none is configured, ask for it.

Verify the actual origin:

```sh
curl -fsS https://whiteboard.example/healthz
curl -fsS https://whiteboard.example/readyz
```

Do not create a test resource unless verification requires it; if created, use non-sensitive content and delete it afterward.

## Run a local publishing server

Start it with a supervised foreground process:

```sh
agent-whiteboard serve --storage "$HOME/.agent-whiteboard"
```

Default origin: `http://127.0.0.1:8567`.

```sh
curl -fsS http://127.0.0.1:8567/healthz
curl -fsS http://127.0.0.1:8567/readyz
```

Process existence is not readiness. If configuration is required, read [Configuration](references/configuration.md).

## Set up Page Agent

Read [Page Agent setup](references/page-agent.md) only when the user needs Page Agent, origin trust, providers, the broker, or daemon management. Reuse a verified existing broker; do not ask to start another broker merely because undocumented HTTP probes against port `8568` fail.

## Verify and report

Verify only the selected mode, then report:

```text
Binary: resolved path
Configuration: selected path or default
Mode: existing server | local server | remote Page Agent | local all-in-one
Publishing server: verified URL or not required
Page Agent broker: foreground | managed daemon | not started | not required
Providers: available Pi/Codex/Cursor providers or not required (paths/availability only; no secrets)
Manual action remaining: exact action or none
```

Do not include credentials, private content, provider internals, or full capability IDs.

## Rollback

Rollback only changes made during this setup.
Stop supervised processes normally.
Remove trust or a daemon only when requested; do not remove the binary, configuration, resources, or provider state without explicit authorization.
