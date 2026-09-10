# Page Agent

Page Agent lets a reader discuss the current whiteboard with a locally running Pi, Codex, or Cursor provider. The reader explicitly connects, reviews what will be shared, and uses the selected provider model with the provider's normal tools, approval policy, sandbox, and project configuration. Provider-specific features remain provider-specific; Cursor does not expose a native skill catalog or manual compaction.

Setup has two sides: the publishing server must expose Page Agent, and each reader must run and authorize their own local broker.

## Enable Page Agent on the publishing server

The server operator enables the viewer integration in `~/.agent-whiteboard/config.yaml` or another selected configuration file:

```yaml
version: 1

viewer:
  local_agent:
    enabled: true
```

Restart `agent-whiteboard serve` after changing the configuration. The Page Agent control will then appear on published Markdown and trusted HTML whiteboards.

See [configuration](configuration.md) for the complete schema and configuration-file rules.

## Prepare a provider on the reader's machine

Each reader needs:

1. The `agent-whiteboard` CLI installed.
2. Any requested providers available on `PATH`: `pi`, `codex`, and/or `cursor-agent`.
3. Authentication completed through each provider's own CLI. For Cursor, run `cursor-agent login`.

Agent Whiteboard does not accept or store provider credentials. Providers use their effective native user configuration unchanged. For Cursor, Agent Whiteboard never invokes ACP authentication, opens a login browser, receives credentials, or copies or edits Cursor authentication, configuration, or shell state.

If provider executables are installed elsewhere, pass their paths when starting the broker:

```sh
agent-whiteboard agent serve \
  --pi-executable /path/to/pi \
  --codex-executable /path/to/codex \
  --cursor-executable /path/to/cursor-agent
```

Each selector uses its explicit flag first, then its matching non-empty environment variable, then default `PATH` discovery. Cursor's default executable is exactly `cursor-agent`; a generic executable named `agent` is accepted only through `--cursor-executable` or `AGENT_WHITEBOARD_PROVIDER_CURSOR_EXECUTABLE`. Cursor executable selection canonicalizes a discovered symlink before the adapter validates and launches the direct regular executable. An explicitly supplied empty executable flag is invalid. A missing provider does not stop the broker or other providers from working.

## Trust the publishing origin

For a remotely hosted whiteboard, every reader must trust its exact HTTPS origin locally:

```sh
agent-whiteboard agent trust add https://whiteboard.example
agent-whiteboard agent trust list
```

Trust only the origin—scheme, hostname, and optional port. Do not include a path, query, fragment, credentials, or wildcard.

Pages served from literal `http://127.0.0.1` are admitted automatically and do not need a trust entry. This local exception does not include `localhost`, other loopback spellings, IPv6, or remote HTTP origins.

Remove an origin when it is no longer needed:

```sh
agent-whiteboard agent trust remove https://whiteboard.example
```

## Reuse or start the reader's local broker

Before starting a broker, check for an existing foreground process or macOS managed daemon. Resolve `agent.port` from the selected configuration, or use its default `8568`. On macOS:

```sh
agent-whiteboard agent daemon status
lsof -nP -a -p PID -iTCP -sTCP:LISTEN
```

When the reported daemon PID owns the expected `127.0.0.1` listener, reuse it. Do not probe `/healthz` or `/readyz` on port `8568`; those routes belong to the publishing server and do not report broker readiness.

If no broker exists, run it in the foreground:

```sh
agent-whiteboard agent serve
```

It listens on literal IPv4 loopback and independently resolves `pi`, `codex`, and exactly `cursor-agent` from `PATH`.

On macOS, install and start it as a managed per-user LaunchAgent instead only when persistent operation is wanted:

```sh
agent-whiteboard agent serve --daemon
agent-whiteboard agent daemon status
```

The installer records resolved Pi, Codex, and Cursor executable paths plus a standalone runtime `PATH` in the LaunchAgent; it does not persist provider credentials or configuration. The `PATH` preserves safe absolute entries from the current shell and adds common system, Homebrew, Bun, asdf, mise, Volta, and Nix locations. It does not source `.zshrc` or another shell startup file. When using NVM, `nix develop`, or another version-specific environment, activate the intended runtime before installation. Rerun `agent-whiteboard agent serve --daemon` after changing or removing that runtime so the plist is regenerated and reloaded.

Other daemon operations are:

```sh
agent-whiteboard agent daemon restart
agent-whiteboard agent daemon stop
agent-whiteboard agent daemon uninstall
```

Managed daemon operations are not available on Linux; keep `agent serve` running in the foreground there.

## Connect from a whiteboard

1. Open an Agent Whiteboard capability URL.
2. Open **Page Agent**.
3. Select Pi, Codex, or Cursor.
4. Review the page context disclosed by the viewer.
5. Choose **Connect**.
6. Write a message or add page content to the composer, then send it.

Opening the pane, checking broker status, or switching providers does not send page content. The first contextual message sends the complete exact Markdown or HTML source, creator context, title, URL, resource metadata, and the reader's message as one envelope to the selected provider.

Readers can add more precise context without copying and pasting:

- Select rendered Markdown text and choose **Add to message**.
- Add a heading-defined Markdown section or the complete page.
- Add the exact fenced source for a rendered Mermaid diagram.
- Add supported rendered raster images.
- In trusted HTML, use **+ Add** or the **Components** chooser for eligible sections, images, charts, tables, code, quotes, and explicitly declared components.
- Add private PNG, JPEG, GIF, or WebP attachments from the composer.

Page Agent exposes each provider's supported subset of model and reasoning controls, streaming activity, interruption, archives, and approval or elicitation requests. Pi and Codex may also expose native skills and manual `/compact`; Cursor does not. Cursor reads the public `cursor-agent --list-models` catalog and presents each exact CLI entry as a complete model variant, including any reasoning or Fast attribute already embedded in its native name. The searchable menu sorts variants naturally by model name, then from lower to higher effort. It does not fabricate separate Effort or Speed controls. Each Cursor conversation launches ACP as `cursor-agent --model <slug> acp`; an explicit model change replaces only that idle conversation's child and reloads the same native session, while ordinary messages retain the process. If Cursor has not yet listed a newly created prompt-free session, Page Agent atomically replaces that uncommitted native reference and its settings before sending the first prompt. Cursor derives image availability from ACP. Cursor archives can be listed and restored, but native archive deletion is unavailable. All three providers keep independent conversations for the same whiteboard.

Refreshing resumes the saved current conversation for that page and provider; there is no shared default thread ID. New Codex conversations are given the title **Page Agent** and their empty history is persisted and checked before the reference is saved, without sending a model message. If a saved Pi or Codex thread is missing, the pane shows **Conversation unavailable** and offers **Start new conversation**. Confirming keeps the old reference in Archives and makes the new conversation current. It does not reconstruct unavailable messages or silently replace the old thread.

## Troubleshoot reader setup

| Symptom | What to check |
| --- | --- |
| Broker unavailable | Check existing daemon/foreground state and verify that the `agent-whiteboard` process owns the configured loopback listener before starting another broker. Do not use publishing `/healthz` or `/readyz` routes on port `8568`. |
| Origin not trusted | Run the exact `agent-whiteboard agent trust add https://…` command for the publishing origin. |
| Provider unavailable | Confirm `pi`, `codex`, or exactly `cursor-agent` is on `PATH` and authenticated through its native CLI (`cursor-agent login` for Cursor). Cursor also requires negotiated ACP v1 with stable `session/list` and `session/load`; missing or incompatible capabilities fail closed. For a generic `agent` executable, configure `--cursor-executable` explicitly. For a managed daemon, activate the intended NVM/Nix environment and rerun `agent-whiteboard agent serve --daemon`. |
| Browser cannot reach loopback | Allow Local Network Access when prompted by the browser. |
| Incompatible local API | Update the publishing server and reader CLI together, then restart the broker. |

