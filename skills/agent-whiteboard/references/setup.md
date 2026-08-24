# Agent Whiteboard setup runbook

Use this runbook when the user asks to install Agent Whiteboard, configure a local publishing server, prepare Page Agent for a reader, or verify an existing setup. Complete only the setup mode the user needs.

## Define the target state

Classify the request before changing the machine:

| Setup mode | Required work |
| --- | --- |
| Publish only | Install the binary; configure and start a publishing server |
| Read a remote whiteboard with Page Agent | Install the binary; prepare a native provider; trust the exact remote origin; start the local broker |
| Local all-in-one | Install the binary; enable Page Agent in the local viewer; start the publishing server; prepare a provider; start the local broker |

Do not configure a provider or broker for publish-only use. Do not start a publishing server when the user only needs Page Agent for an existing remote deployment.

Before acting, establish:

- macOS or Linux;
- the required setup mode;
- the configuration path, defaulting to `~/.agent-whiteboard/config.yaml`;
- whether Pi, Codex, or both are needed;
- the exact publishing origin for a remote viewer;
- foreground operation or, on macOS, an explicitly requested managed daemon.

## Protect existing state

- Follow the active harness's approval requirements for durable machine changes.
- Inspect an existing configuration before editing it. Merge only the required fields; never replace an unknown file with a minimal example.
- Keep using the same selected configuration path for the publishing server, trust commands, broker, and daemon.
- Never collect, copy, print, or store Pi or Codex credentials. Authentication belongs to each provider's native CLI.
- Never edit provider homes, model settings, tools, skills, approval policies, sandboxes, or project trust as part of Agent Whiteboard setup.
- Trust only an exact origin the user intends to open. Never add a wildcard or broaden HTTPS trust to HTTP.
- Prefer foreground processes for initial setup and verification. Do not install a persistent daemon unless the user requests persistent operation.
- Use only non-sensitive content for setup verification. Do not expose full capability IDs in logs or reports.
- Do not delete existing whiteboards, provider histories, configuration, or broker state merely to obtain a clean setup.

Read [security guidance](security.md) before configuring a remote origin or Page Agent.

## 1. Inspect prerequisites

Run non-mutating checks first:

```sh
uname -s
go version
command -v agent-whiteboard || true
command -v pi || true
command -v codex || true
```

Agent Whiteboard supports macOS and Linux with Go 1.25 or 1.26. If Go is absent or unsupported, report the prerequisite instead of silently installing or replacing the user's toolchain.

If `agent-whiteboard` already exists, resolve and report its path. Continue with installation only when the binary is missing or the user asked to update it.

## 2. Install or update the binary

Use the supported Go installation path:

```sh
go install github.com/edocsss/agent-whiteboard/cmd/agent-whiteboard@latest
```

Verify the installed command and relevant surfaces:

```sh
command -v agent-whiteboard
agent-whiteboard --help
agent-whiteboard agent serve --help
```

If installation succeeds but the command is not found, inspect the Go binary directory:

```sh
go env GOBIN
go env GOPATH
```

An empty `GOBIN` means binaries normally land in the first `GOPATH` entry under `bin`. Report the required `PATH` addition. Do not edit shell startup files unless the user requested that durable change.

## 3. Configure a publishing server when required

Skip this step for a reader using an existing remote deployment.

The default configuration is `~/.agent-whiteboard/config.yaml`. A different file may be selected with the global `--config PATH` flag. The format is strict version-1 YAML; unknown fields, duplicate keys, aliases, merge keys, invalid types, and unsupported versions are rejected.

For publish-only use, the built-in server defaults may require no configuration. For local all-in-one use, enable Page Agent in the viewer:

```yaml
version: 1

viewer:
  local_agent:
    enabled: true
```

This is a minimal example for a new file, not a replacement template. If the file exists, preserve every unrelated section and set only `viewer.local_agent.enabled` to `true`.

Configuration files must be regular non-symlinks and must not be writable by group or others. Prefer `0600` for a newly created file and `0700` for a newly created configuration directory.

Restart the publishing server after changing viewer configuration. A reader cannot enable Page Agent from their own machine for a remote deployment; if the Page Agent control is absent remotely, report that the publishing server operator must enable it.

## 4. Start and verify the publishing server

Start the server in the foreground by default:

```sh
agent-whiteboard serve
```

When using a selected configuration:

```sh
agent-whiteboard --config /path/to/config.yaml serve
```

Use the harness's supervised-process facility for a long-running foreground server. Do not use detached `nohup` or an unmanaged background process.

The built-in server origin is `http://127.0.0.1:8567`. Verify both endpoints:

```sh
curl -fsS http://127.0.0.1:8567/healthz
curl -fsS http://127.0.0.1:8567/readyz
```

If the configured host or port differs, verify the effective local address instead. Do not claim readiness from process existence alone.

## 5. Prepare Pi or Codex when Page Agent is required

Skip this step for publish-only use.

Confirm that at least one requested provider is available:

```sh
command -v pi || true
command -v codex || true
```

If the provider is installed but not authenticated, ask the user to complete authentication through that provider's own CLI. There are no Agent Whiteboard login or credential commands.

The broker resolves `pi` and `codex` independently from `PATH`. If either executable lives elsewhere, pass one or both paths when starting the broker:

```sh
agent-whiteboard agent serve \
  --pi-executable /path/to/pi \
  --codex-executable /path/to/codex
```

A missing provider does not block the broker or the other provider. Do not fail a Pi-only setup because Codex is absent, or the reverse.

## 6. Trust a remote publishing origin

Skip this step for publish-only use and for pages served from canonical literal `http://127.0.0.1` origins.

Derive the exact origin from the whiteboard URL: scheme, host, and optional non-default port only. Remove the capability path, query, and fragment. Then add and inspect it:

```sh
agent-whiteboard agent trust add https://whiteboard.example
agent-whiteboard agent trust list
```

When using a selected configuration, pass the same path:

```sh
agent-whiteboard --config /path/to/config.yaml agent trust add https://whiteboard.example
agent-whiteboard --config /path/to/config.yaml agent trust list
```

Configured trust accepts exact HTTPS origins only. User information, paths, queries, fragments, wildcards, and HTTP origins are rejected.

The broker automatically admits canonical browser origins on literal `http://127.0.0.1`, with no port or a valid non-default port. This exception does not cover `localhost`, other loopback addresses or spellings, IPv6 loopback, an explicit default port 80, or remote HTTP origins. Treat the exception as trust in every browser page served by a local process on literal `127.0.0.1`; open only local servers and content the user trusts.

## 7. Start the Page Agent broker

Start the broker in the foreground for setup and verification:

```sh
agent-whiteboard agent serve
```

With a selected configuration:

```sh
agent-whiteboard --config /path/to/config.yaml agent serve
```

The broker binds to literal IPv4 loopback and uses port `8568` by default. Keep the foreground process supervised while the user uses Page Agent.

On macOS, when the user explicitly requests persistent background operation, install and start the managed per-user LaunchAgent:

```sh
agent-whiteboard agent serve --daemon
agent-whiteboard agent daemon status
```

Pass the selected `--config` and any provider executable overrides during daemon installation. Linux does not support managed daemon operations; use foreground `agent serve`.

The daemon records the Agent Whiteboard executable, selected configuration path, and resolved provider executable paths. It does not copy provider credentials or configuration.

## 8. Verify Page Agent without silently invoking a model

Prefer an existing non-sensitive whiteboard. For local all-in-one verification, publish a short-lived test board only when needed and clean it up after verification.

Verify in this order:

1. The publishing URL renders successfully.
2. The **Page Agent** control is visible.
3. Opening Page Agent reports the local broker as available.
4. The intended provider is available.
5. The displayed broker port matches the configured port, normally `8568`.
6. The context disclosure is available before the user connects.
7. The user can choose **Connect** without an untrusted-origin or browser-permission error.

Opening the pane, checking status, selecting a provider, and connecting do not send whiteboard source or creator context. The complete context is sent with the first contextual message.

Do not submit that first message merely to prove setup unless the user authorizes the provider call. A message may incur usage and may allow tools according to the provider's native approval and sandbox configuration.

When browser automation is available and authorized, use it to verify the visible states. Browser Local Network Access may require a user-granted permission; it is additional browser permission, not broker authorization.

## 9. Troubleshoot from evidence

| Symptom | Check |
| --- | --- |
| `agent-whiteboard` is not found | Inspect `go env GOBIN` and `go env GOPATH`; report the required `PATH` addition |
| Configuration is rejected | Check strict YAML structure, file type, permissions, and symlink restrictions |
| Publishing server is unavailable | Inspect foreground output; verify effective host and port; query `/healthz` and `/readyz` |
| Page Agent control is absent | Enable `viewer.local_agent.enabled` on the publishing server and restart it |
| Broker is unavailable | Start `agent-whiteboard agent serve`; verify the configured loopback port |
| Origin is rejected | Add the exact HTTPS publishing origin to the same selected configuration |
| Provider is unavailable | Check its executable path and native authentication |
| Browser cannot reach loopback | Ask the user to grant Local Network Access and confirm the displayed port |
| Managed daemon command fails on Linux | Use foreground `agent serve` |
| Browser and broker API are incompatible | Update the publishing server and reader binary together, then restart the broker |

Do not clear broker state as a generic troubleshooting step. A coordinated Page Agent upgrade may require removing only documented disposable broker conversation/workspace state, but that is destructive and must follow current version guidance and the active approval rules. Never delete public whiteboards or provider-native histories as part of that reset.

Read [CLI commands](cli.md) for exact command syntax and output behavior.

## 10. Report completion

Return a concise setup report:

```text
Binary: /resolved/path/agent-whiteboard
Configuration: /path/to/config.yaml
Setup mode: publish only | remote reader | local all-in-one
Publishing server: URL | existing remote | not required
Page Agent enabled: yes | no | server-operator action required | not applicable
Trusted origins: exact origins added or already present
Broker: foreground | macOS daemon | not required
Broker port: effective port
Providers available: Pi | Codex | both | neither
Verification: checks completed
Manual actions remaining: provider authentication, browser permission, or none
```

Do not include credentials, hidden provider details, private content, or full capability IDs.

## Roll back only what setup added

When the user requests rollback, remove only changes made for this setup:

```sh
agent-whiteboard agent trust remove https://whiteboard.example
agent-whiteboard agent daemon stop
agent-whiteboard agent daemon uninstall
```

Disable `viewer.local_agent.enabled` only if setup enabled it and the user no longer wants Page Agent. Preserve every unrelated configuration field. Stop supervised foreground processes normally.

Remove the binary, shell configuration, stored whiteboards, or broker/provider state only when the user explicitly requests those destructive changes.
