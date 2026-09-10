# CLI JSON contract

`--json` selects machine output with `"schema_version":1`. Successful data is written only to stdout; errors and warning diagnostics are written only to stderr. Each envelope is one JSON object followed by a newline. A successful remote operation can therefore produce a resource on stdout, a warning on stderr, and exit status 0.

Markdown and HTML create require non-empty UTF-8 `--title` and `--summary` values plus `--context FILE`. Updates require the context file and accept optional title/summary replacements. Source and creator-context files must be non-empty UTF-8; creator context is Markdown. A runnable lifecycle starts with a temporary context artifact:

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

agent-whiteboard --json create markdown --context "$context_file" --title "Architecture" --summary "Current recharge architecture" board.md
agent-whiteboard --json update markdown --context "$context_file" --title "Updated architecture" -- CAPABILITY_ID board.md
agent-whiteboard --json get markdown -- CAPABILITY_ID
agent-whiteboard --json create html --context "$context_file" --title "Interactive dashboard" --summary "Standalone operational dashboard" board.html
agent-whiteboard --json update html --context "$context_file" --summary "Updated operational dashboard" -- CAPABILITY_ID board.html
agent-whiteboard --json get html -- CAPABILITY_ID
```

Title and summary are trimmed local catalog metadata. They are not inserted into or sent as source or creator context, and they do not change Markdown headings, HTML titles, or viewer title selection. An update replaces only supplied metadata for a board already tracked on this laptop; omitted metadata is preserved.

Single create/update success:

```json
{"schema_version":1,"resource":{"id":"CAPABILITY_ID","url":"https://whiteboard.example/whiteboards/markdown/CAPABILITY_ID","expires_at":1767229200,"permanent":false}}
```

Whiteboard retrieval is JSON-only. Calling `get markdown` or `get html` without `--json` is a usage error. Success includes the exact stored UTF-8 strings:

```json
{"schema_version":1,"resource":{"id":"CAPABILITY_ID","url":"https://whiteboard.example/whiteboards/markdown/CAPABILITY_ID","expires_at":1767229200,"permanent":false},"markdown":"# Board\n","context":"# Creator context\n"}
```

HTML success uses exact `html` and `context` strings:

```json
{"schema_version":1,"resource":{"id":"CAPABILITY_ID","url":"https://whiteboard.example/whiteboards/html/CAPABILITY_ID","expires_at":null,"permanent":true},"html":"<!doctype html><html><head></head><body></body></html>","context":"# Creator context\n"}
```

Legacy schema-1 Markdown returns `"context":""` until its first paired update.

Image upload always uses the plural envelope, even for one image, and preserves input order:

```json
{"schema_version":1,"resources":[{"id":"CAPABILITY_ID","url":"https://whiteboard.example/images/CAPABILITY_ID","expires_at":null,"permanent":true}]}
```

## Local catalog discovery

The CLI records new Markdown and HTML creations under `~/.agent-whiteboard/catalog`, including results from remote publishing origins. Images, direct HTTP/Go API creations, older boards, other devices, and unknown boards that are merely retrieved, updated, or deleted are not enrolled.

List and search run offline across all recorded origins:

```sh
agent-whiteboard --json catalog list
agent-whiteboard --json catalog list --kind markdown --query "recharge architecture" --limit 20 --offset 0
```

`--kind` is `markdown` or `html`; `--limit` defaults to 20 and must be positive; `--offset` must be nonnegative. Search is case-insensitive, and every whitespace-separated query term must occur as a substring in either the title or summary. Filtering happens before pagination. Results are newest first, with server, kind, and ID as deterministic tie-breakers.

Catalog output is schema version 1. `records` is always an array, and `total` is the complete matching count before pagination:

```json
{"schema_version":1,"records":[{"schema_version":1,"server":"https://whiteboard.example","kind":"markdown","id":"CAPABILITY_ID","url":"https://whiteboard.example/whiteboards/markdown/CAPABILITY_ID","title":"Architecture","summary":"Current recharge architecture","source_filename":"board.md","created_at":1767220000,"updated_at":1767221000,"expires_at":1767306400,"permanent":false,"state":"created","deleted_at":null,"expired":false}],"total":1,"limit":20,"offset":0}
```

`created_at` and `updated_at` are UTC Unix seconds observed by this local CLI, not server timestamps. `state` is `created`, `creation_uncertain`, or `deleted`; `expired` is derived when listing. Expired, uncertain, and locally deleted records remain searchable. A plain `created` and unexpired result is not a remote availability guarantee.

The catalog belongs only to the effective user on this laptop. An empty result means no matching local record, not that no remote board exists. To check content or availability, use the selected record's `server` with `get` rather than the currently configured default. `catalog list` ignores `--server` for filtering, does not load publishing configuration, and never contacts a server.

Delete and trusted-origin add/remove success is `{"schema_version":1}`. Trusted-origin list preserves insertion order and contains canonical exact HTTPS origins:

```json
{"schema_version":1,"origins":["https://whiteboard.example"]}
```

An empty trusted-origin list is `{"schema_version":1,"origins":[]}`. Trust commands are supported only on macOS and Linux.

Error output is stable:

```json
{"schema_version":1,"error":{"code":"not_found","message":"resource not found"}}
```

Warnings preserve the remote result and exit classification. In JSON mode they are newline-delimited objects on stderr:

```json
{"schema_version":1,"warning":{"code":"catalog_write_failed","message":"Whiteboard was published, but its local catalog record could not be saved."}}
{"schema_version":1,"warning":{"code":"catalog_record_missing","message":"Whiteboard was updated, but title and summary metadata were not recorded because this board is not in the local catalog."}}
```

Update and delete use operation-specific `catalog_write_failed` messages. Do not treat stderr as necessarily empty when exit status is 0. Diagnostics never contain source, creator context, or raw filesystem errors.

`expires_at` is nullable Unix seconds. `null` pairs with `permanent:true`; a timestamp pairs with `permanent:false`. URLs are resolved by the CLI against `--server`, because HTTP mutations return paths.

Timeout produces stderr `{"schema_version":1,"error":{"code":"timeout","message":"request timed out"}}` and exit 4. Cancellation uses code `canceled`.

A whiteboard create can fail after remote persistence becomes uncertain. If the server returns a validated resource, the CLI records it as `creation_uncertain`, writes the resource envelope to stdout, then writes the original error envelope to stderr and exits nonzero. Preserve that ID and use the recorded server to retrieve or delete it; never repeat `create` as recovery because that can create a duplicate. A successfully recorded uncertain result has no additional catalog warning. If catalog recording also fails, the catalog warning precedes the original error envelope. Ordinary failed creates without a returned ID do not emit or invent a resource or catalog record.

Creation preflights catalog writability before sending the HTTP request. A preflight error uses `catalog_unavailable` and guarantees that the command did not publish. Remote and local persistence cannot be atomic: after a response, catalog recording can still fail. In that case the resource output and remote outcome remain authoritative and `catalog_write_failed` warns that this laptop's history is incomplete. Preserve the returned URL and do not repeat creation to repair local recording.

| Exit | Meaning |
| ---: | --- |
| 0 | success |
| 1 | unexpected/internal failure |
| 2 | CLI usage or local configuration error |
| 3 | stable remote/domain error |
| 4 | timeout or cancellation |

Human mode prints URLs to stdout, one per line; successful delete and trust mutations print nothing. Scripts should branch on `schema_version`, the top-level `resource`/`resources`/`markdown`/`html`/`error` member, and exit status. Do not assume stdout is empty after an uncertain create error. Version 1 will not change the meaning or type of existing fields; additive fields may be introduced. A breaking change requires a new schema version.

Creator context is not a private or hidden channel. Anyone with the capability ID can retrieve it. Do not include hidden reasoning, credentials, personal or sensitive data, private source, or raw tool output. Error envelopes do not echo source or context.

## Local Page Agent and daemon output

`agent serve` is a long-running local broker service rather than a stream of CLI JSON envelopes. It resolves Pi, Codex, and Cursor independently and accepts these provider executable selectors:

```sh
agent-whiteboard agent serve --pi-executable /absolute/path/to/pi
agent-whiteboard agent serve --codex-executable /absolute/path/to/codex
agent-whiteboard agent serve --cursor-executable /absolute/path/to/cursor-agent

AGENT_WHITEBOARD_PROVIDER_PI_EXECUTABLE=/absolute/path/to/pi \
  AGENT_WHITEBOARD_PROVIDER_CODEX_EXECUTABLE=/absolute/path/to/codex \
  AGENT_WHITEBOARD_PROVIDER_CURSOR_EXECUTABLE=/absolute/path/to/cursor-agent \
  agent-whiteboard agent serve
```

For each provider, an explicit flag takes precedence over the matching non-empty environment variable; otherwise the service resolves exactly `pi`, `codex`, or `cursor-agent` from `PATH`. An explicitly supplied empty executable flag is invalid. A generic `agent` executable is accepted for Cursor only through the explicit flag or environment selector; it is never discovered by default. One unavailable provider does not prevent other providers or the broker from starting.

Providers inherit their effective native environment and configuration unchanged. Authenticate Cursor with `cursor-agent login`. Agent Whiteboard never invokes ACP authentication, opens a login browser, receives credentials, edits `~/.codex/config.toml`, sets a production `CODEX_HOME`, or copies or edits Cursor authentication, configuration, or shell state.

On macOS, `agent serve --daemon` install success and `agent daemon restart|stop|uninstall` mutation success use the ordinary delete-success JSON envelope:

```json
{"schema_version":1}
```

`agent daemon status` reports only managed-process state:

```json
{"schema_version":1,"installed":true,"loaded":true,"running":true,"pid":1234}
```

This status does not prove broker reachability. Confirm that the reported process owns the configured loopback listener, then verify Page Agent through the Whiteboard UI. The broker does not implement the publishing server's `/healthz` or `/readyz` routes, so requests to those paths on the broker port are not readiness checks.

The daemon accepts `--pi-executable`, `--codex-executable`, and `--cursor-executable`, but rejects the foreground-only `--port`, `--provider-idle-timeout`, and `--shutdown-timeout` flags. Its provider-specific persisted values are resolved Pi, Codex, and Cursor executable paths only; it does not copy provider configuration or credentials. The generated LaunchAgent also receives a standalone runtime `PATH` built from safe absolute entries in the installing process plus common system and package-manager locations. It never sources shell startup files. Activate an NVM or Nix development environment before installation and rerun `agent-whiteboard agent serve --daemon` after changing that runtime.

The browser protocol remains separate from CLI JSON. It is versioned and strictly validated, and identifies `pi`, `codex`, or `cursor` conversations independently. Provider selection itself does not connect or send whiteboard content. The first contextual turn carries the complete canonical context envelope. Codex and standard Cursor tool activity are normalized into bounded provider-neutral events; native session, request, tool, and message IDs and raw App Server or ACP frames never enter the browser protocol or log evidence.

Cursor uses the public CLI catalog and launch interfaces: a bounded `cursor-agent --list-models` probe discovers exact complete variants, and each loaded conversation runs `cursor-agent --model <slug> acp`. Standard ACP v1 carries session traffic; readiness may use a transient non-conversation ACP probe. Negotiation requires ACP v1 and stable `session/list` and `session/load`, and fails closed otherwise. An explicit idle model change replaces only that conversation's child after the candidate successfully loads the same native session; failed replacement retains the previous process and settings. Cursor-private `cursor/*` methods, private storage, ACP client tools, filesystem or terminal capabilities, and arbitrary configuration options are outside the supported surface.

Stable command, file-change, and permission approvals and MCP elicitation, including standard Cursor command/file permission requests, use typed `interaction_request` and `interaction_resolved` events plus the `interaction_respond` command. The broker accepts exactly one valid response; the first response across attached tabs wins and all tabs receive the resolved state. For Cursor, interaction success means a complete local ACP response write, not Cursor consumption. Only `NotWritten` is retry-safe; `Indeterminate` fails closed without automatic replay. App Server's `request_user_input` remains experimental, so `experimentalApi` is disabled and that request family is not active in this stable protocol slice.
