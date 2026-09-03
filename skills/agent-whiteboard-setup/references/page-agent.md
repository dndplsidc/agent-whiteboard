# Page Agent setup

Use this only after the user has selected Page Agent for a remote Whiteboard or local all-in-one setup.

## 1. Select the broker lifecycle

Page Agent requires the local broker, so do not ask whether to enable it again. Use supervised foreground operation when the user did not select a lifecycle. On macOS, select the managed daemon only when the user explicitly requests persistent operation. Linux supports foreground operation only.

## 2. Check a provider

```sh
command -v pi || true
command -v codex || true
command -v cursor-agent || true
```

At least one requested provider must be installed and authenticated through its own CLI. For Cursor, report `cursor-agent login` as the native manual login step when needed. Agent Whiteboard has no provider login command: never run or configure authentication, open a login browser, receive credentials, or edit provider credentials, native configuration, or shell state. Report only provider availability and required manual action, never secrets.

Cursor default discovery checks exactly `cursor-agent`; never check or select a generic executable named `agent` by default. A generic `agent` is valid only when the user explicitly supplies its path through the Cursor selector. Agent Whiteboard discovers Cursor's exact complete model variants with the public `cursor-agent --list-models` command and starts a conversation as `cursor-agent --model <slug> acp`. The Page Agent menu is searchable and intentionally has no independent Cursor Effort or Speed controls because those attributes are already part of each CLI variant. Ordinary messages reuse the conversation child. An explicit idle model change safely replaces only that child after the candidate loads the same native session; a failed replacement retains the previous process and settings. If Cursor has not listed a newly created prompt-free session, the broker may instead replace that uncommitted native reference and its selected settings atomically before the first prompt.

If an executable is outside `PATH`, pass its path when starting the broker:

```sh
agent-whiteboard agent serve \
  --pi-executable /path/to/pi \
  --codex-executable /path/to/codex \
  --cursor-executable /path/to/cursor-agent
```

A missing provider does not block other providers. Selection precedence is explicit flag, then the matching non-empty environment variable, then default `PATH` discovery. Cursor's environment selector is `AGENT_WHITEBOARD_PROVIDER_CURSOR_EXECUTABLE`; an explicitly empty executable flag is invalid. Use the same selectors with `agent serve --daemon` during macOS daemon installation so resolved paths are persisted.

## 3. Trust a remote origin

For a remote Whiteboard, derive the exact HTTPS origin from its URL: scheme, hostname, and optional port only.

```sh
agent-whiteboard agent trust add https://whiteboard.example
agent-whiteboard agent trust list
```

Use the same global `--config PATH` selected for the broker. Do not include a path, query, fragment, credentials, or wildcard.

Literal `http://127.0.0.1` origins are admitted automatically. This does not include `localhost`, other loopback spellings, IPv6, or remote HTTP.

## 4. Detect and reuse an existing broker

Resolve the broker port from `agent.port` in the selected configuration, or use the default `8568`. Before offering to start a broker, check whether one is already usable.

On macOS, inspect the managed daemon first:

```sh
agent-whiteboard agent daemon status
```

When it reports `running: true` and a PID, confirm that exact process owns a loopback TCP listener on the resolved port:

```sh
lsof -nP -a -p PID -iTCP -sTCP:LISTEN
```

For an existing foreground broker, use its startup output and process identity, or inspect the resolved port with the platform's listener tool. On Linux, use either available form, then correlate the reported PID with the executable:

```sh
lsof -nP -iTCP:PORT -sTCP:LISTEN
ss -ltnp | grep -F "127.0.0.1:PORT"
ps -p PID -o pid=,command=
```

Confirm the listener belongs to `agent-whiteboard`, not merely that some process occupies the port. If the managed daemon or foreground broker owns the expected loopback listener, reuse it and do not start a second broker.

The broker does not expose the publishing server's `/healthz` or `/readyz` contract. Never probe `http://127.0.0.1:8568/healthz` or `/readyz`, and never interpret those responses as broker readiness. Managed-daemon status alone proves only lifecycle state; combine it with listener ownership, then use the browser verification below for the Page Agent protocol and provider.

If status claims the daemon is running but its PID has no expected listener, report the mismatch before restarting or replacing anything. If no listener inspection tool is available or process ownership is hidden, do not install a package automatically and do not claim the broker is ready or absent. Browser verification may continue as diagnosis, but successful reuse remains unconfirmed until the user performs the smallest process/listener ownership check. Do not start a possible duplicate merely because ownership could not be inspected.

## 5. Start the broker when absent

For foreground operation, run:

```sh
agent-whiteboard agent serve
```

Default broker address: `127.0.0.1:8568`.

For explicitly requested persistent operation on macOS, install the managed daemon:

```sh
agent-whiteboard agent serve --daemon
agent-whiteboard agent daemon status
```

Linux supports foreground operation only.

## 6. Verify without sending a model turn

1. Open the Whiteboard URL.
2. Confirm the Page Agent control is visible.
3. Open it and confirm the broker and intended provider are available.
4. Confirm Page Context can show source and creator context.
5. Connect only if needed to verify authorization.

Do not send a message merely to test setup. A model turn may incur usage and use provider tools according to native policy.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Page Agent control absent | Publishing server must enable `viewer.local_agent.enabled` and restart |
| Broker unavailable | Check managed-daemon or foreground process state, process-owned loopback listener, resolved `agent.port`, and browser Local Network Access before starting another broker |
| Origin rejected | Add the exact HTTPS publishing origin using the same configuration |
| Provider unavailable | Check `pi`, `codex`, or exactly `cursor-agent`; use an explicit selector for a generic `agent`. For Cursor, report `cursor-agent login` if native authentication is missing and verify ACP v1 plus stable `session/list` and `session/load` support. |
| Cursor briefly remains active after an otherwise complete answer | Wait one second. Page Agent suppresses the exact known closed-stream artifact, retires only the stuck call, and retains the existing Cursor process and session. Do not resubmit the message. |
| Cursor shows **Confirming delivery** or another protocol failure | Let Page Agent reconnect and reconcile the existing turn automatically. Do not resubmit the same message or restart the broker unless automatic reconnection itself remains unavailable. |
| Browser cannot reach loopback | Grant browser Local Network Access when prompted |
| Daemon command fails on Linux | Use foreground `agent serve` |
| `/healthz` or `/readyz` fails on port `8568` | Do not use those publishing-server endpoints for broker readiness; inspect the broker listener and verify through the Whiteboard UI |

Do not clear broker or provider state as a generic fix.

## Remove setup only when requested

```sh
agent-whiteboard agent trust remove https://whiteboard.example
agent-whiteboard agent daemon stop
agent-whiteboard agent daemon uninstall
```

Remove only trust or daemon state added for this setup. Preserve Whiteboards, configuration, conversations, and provider histories.
