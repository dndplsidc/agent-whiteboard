# CLI JSON contract

`--json` selects machine output with `"schema_version":1`. Successful data is written only to stdout; errors and diagnostics are written only to stderr. Every envelope is one JSON object followed by a newline.

Markdown create and update require `--context FILE`. Both the source and creator-context files must be non-empty UTF-8 Markdown. A runnable lifecycle starts with a temporary context artifact:

```sh
context_dir="$(mktemp -d)"
trap 'rm -rf "$context_dir"' EXIT
context_file="$context_dir/context.md"
cat >"$context_file" <<'EOF'
# Creator context

- Goal: publish the accompanying board.
- Decisions: use Markdown and bundled rendering assets.
- Assumptions: no private data is included.
- Open questions: none.
EOF

agent-whiteboard --json create markdown --context "$context_file" board.md
agent-whiteboard --json update markdown --context "$context_file" -- CAPABILITY_ID board.md
agent-whiteboard --json get markdown -- CAPABILITY_ID
```

Single create/update success:

```json
{"schema_version":1,"resource":{"id":"CAPABILITY_ID","url":"https://whiteboard.example/whiteboards/markdown/CAPABILITY_ID","expires_at":1767229200,"permanent":false}}
```

Markdown retrieval is JSON-only. Calling `get markdown` without `--json` is a usage error. Success includes the exact stored UTF-8 strings:

```json
{"schema_version":1,"resource":{"id":"CAPABILITY_ID","url":"https://whiteboard.example/whiteboards/markdown/CAPABILITY_ID","expires_at":1767229200,"permanent":false},"markdown":"# Board\n","context":"# Creator context\n"}
```

Legacy schema-1 Markdown returns `"context":""` until its first paired update.

Image upload always uses the plural envelope, even for one image, and preserves input order:

```json
{"schema_version":1,"resources":[{"id":"CAPABILITY_ID","url":"https://whiteboard.example/images/CAPABILITY_ID","expires_at":null,"permanent":true}]}
```

Delete and trusted-origin add/remove success is `{"schema_version":1}`. Trusted-origin list preserves insertion order and contains canonical exact HTTPS origins:

```json
{"schema_version":1,"origins":["https://whiteboard.example"]}
```

An empty trusted-origin list is `{"schema_version":1,"origins":[]}`. Trust commands are supported only on macOS and Linux.

Error output is stable:

```json
{"schema_version":1,"error":{"code":"not_found","message":"resource not found"}}
```

`expires_at` is nullable Unix seconds. `null` pairs with `permanent:true`; a timestamp pairs with `permanent:false`. URLs are resolved by the CLI against `--server`, because HTTP mutations return paths.

Timeout produces stderr `{"schema_version":1,"error":{"code":"timeout","message":"request timed out"}}` and exit 4. Cancellation uses code `canceled`.

A whiteboard create can fail after persistence becomes uncertain. In that case the CLI writes the generated resource envelope to stdout before writing the error envelope to stderr and exiting nonzero. Preserve that ID: the resource may exist and should be checked or deleted. Ordinary failed creates do not emit a resource.

| Exit | Meaning |
| ---: | --- |
| 0 | success |
| 1 | unexpected/internal failure |
| 2 | CLI usage or local configuration error |
| 3 | stable remote/domain error |
| 4 | timeout or cancellation |

Human mode prints URLs to stdout, one per line; successful delete and trust mutations print nothing. Scripts should branch on `schema_version`, the top-level `resource`/`resources`/`markdown`/`error` member, and exit status. Do not assume stdout is empty after an uncertain create error. Version 1 will not change the meaning or type of existing fields; additive fields may be introduced. A breaking change requires a new schema version.

Creator context is not a private or hidden channel. Anyone with the capability ID can retrieve it. Do not include hidden reasoning, credentials, personal or sensitive data, private source, or raw tool output. Error envelopes do not echo Markdown or context.

## Local Page Agent and daemon output

`agent serve` is a long-running local broker service rather than a stream of CLI JSON envelopes. It resolves Pi and Codex independently and accepts these provider executable selectors:

```sh
agent-whiteboard agent serve --pi-executable /absolute/path/to/pi
agent-whiteboard agent serve --codex-executable /absolute/path/to/codex

AGENT_WHITEBOARD_PROVIDER_PI_EXECUTABLE=/absolute/path/to/pi \
  AGENT_WHITEBOARD_PROVIDER_CODEX_EXECUTABLE=/absolute/path/to/codex \
  agent-whiteboard agent serve
```

An explicit flag takes precedence over the matching non-empty environment variable; otherwise the service resolves `pi` or `codex` from `PATH`. One unavailable provider does not prevent the other provider or the broker from starting. Codex inherits the effective default user Codex configuration and authentication unchanged. Agent Whiteboard never edits `~/.codex/config.toml` or sets a production `CODEX_HOME`.

On macOS, `agent serve --daemon` install success and `agent daemon restart|stop|uninstall` mutation success use the ordinary delete-success JSON envelope:

```json
{"schema_version":1}
```

`agent daemon status` reports only managed-process state:

```json
{"schema_version":1,"installed":true,"loaded":true,"running":true,"pid":1234}
```

The daemon accepts `--pi-executable` and `--codex-executable`, but rejects the foreground-only `--port`, `--provider-idle-timeout`, and `--shutdown-timeout` flags. Its provider-specific persisted values are resolved executable paths only; it does not copy provider configuration or credentials.

The browser protocol remains separate from CLI JSON. It is versioned and strictly validated, and identifies `pi` or `codex` conversations independently. Provider selection itself does not connect or send whiteboard content. The first contextual turn carries the complete canonical context envelope. Codex tool activity is normalized into bounded `tool_activity` events, while native App Server identifiers and raw JSONL never enter the browser protocol.

Stable command, file-change, and permission approvals and MCP elicitation use typed `interaction_request` and `interaction_resolved` events plus the `interaction_respond` command. The broker accepts exactly one valid response; the first response across attached tabs wins and all tabs receive the resolved state. App Server's `request_user_input` remains experimental, so `experimentalApi` is disabled and that request family is not active in this stable protocol slice.
