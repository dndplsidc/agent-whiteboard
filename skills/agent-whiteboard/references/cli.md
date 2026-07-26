# CLI commands

Put global flags before the command. Use `--` before an ID because valid capability IDs may begin with `-`.

```text
agent-whiteboard [--config PATH] [--server URL] [--timeout DURATION] [--json] serve [flags]
agent-whiteboard [global flags] create markdown --context CONTEXT_FILE [--expires-in SECONDS] FILE
agent-whiteboard [global flags] create html [--expires-in SECONDS] FILE
agent-whiteboard [global flags] update markdown --context CONTEXT_FILE [--expires-in SECONDS] -- ID FILE
agent-whiteboard [global flags] update html [--expires-in SECONDS] -- ID FILE
agent-whiteboard [global flags] --json get markdown -- ID
agent-whiteboard [global flags] delete markdown -- ID
agent-whiteboard [global flags] delete html -- ID
agent-whiteboard [global flags] image upload [--expires-in SECONDS] FILE...
agent-whiteboard [global flags] image update [--expires-in SECONDS] -- ID FILE
agent-whiteboard [global flags] image delete -- ID
agent-whiteboard [global flags] agent trust add ORIGIN
agent-whiteboard [global flags] agent trust remove ORIGIN
agent-whiteboard [global flags] agent trust list
```

## Markdown pair workflow

Every Markdown create and update requires non-empty UTF-8 source and context files. Create a fresh temporary context artifact, write concise goals, decisions, assumptions, and open questions, and pass `--context`:

```sh
context_dir="$(mktemp -d)"
trap 'rm -rf "$context_dir"' EXIT
context_file="$context_dir/context.md"
cat >"$context_file" <<'EOF'
# Creator context

- Goal: publish the accompanying Markdown accurately.
- Decisions: use the sanitized Markdown viewer.
- Assumptions: the document contains no private data.
- Open questions: none.
EOF

agent-whiteboard --json create markdown --context "$context_file" board.md
agent-whiteboard --json update markdown --context "$context_file" -- CAPABILITY_ID board.md
agent-whiteboard --json get markdown -- CAPABILITY_ID
```

Markdown source and context are replaced together. `get markdown` requires `--json` and returns exact `markdown` and `context` strings. Legacy Markdown returns an empty context until its first update, which must provide both artifacts.

Do not put hidden reasoning, credentials, tokens, personal or sensitive information, private source, unrelated data, or raw tool output in context. Anyone with the capability ID can retrieve it.

## Settings, expiration, and output

Use `--config` to select a strict version-1 YAML file, `--server` for a non-default service, `--timeout` for the client deadline, and `--json` for machine-readable output. Runtime settings resolve as explicit flags, non-empty environment variables, YAML, then built-ins. The default config is `~/.agent-whiteboard/config.yaml`; an explicit file must exist.

Create without `--expires-in` uses the server default. Update without it preserves the existing absolute expiration. A positive value resets expiration from the update time; zero makes the resource permanent.

Successful create and update commands print one absolute public URL per resource to stdout. Image upload can print multiple URLs in input order. Successful delete is silent. The CLI never opens a browser.

JSON mode uses schema version 1:

```json
{"schema_version":1,"resource":{"id":"CAPABILITY_ID","url":"https://board.example/whiteboards/markdown/CAPABILITY_ID","expires_at":1780000000,"permanent":false}}
```

Markdown retrieval adds exact `markdown` and `context` fields to the single-resource envelope. Multiple uploads use `resources`; permanent resources have `"expires_at":null` and `"permanent":true`; delete success is `{"schema_version":1}`. Errors go to stderr as `{"schema_version":1,"error":{"code":"...","message":"..."}}`. Human errors also use stderr. Exit codes are 0 success, 1 internal, 2 usage, 3 remote/domain, and 4 timeout/cancellation.

If a create error leaves persistence uncertain, the CLI prints the generated resource envelope to stdout before the error on stderr and exits nonzero. Retain that ID and check or delete it. Do not assume every failed create leaves stdout empty.

## Trusted origins

Trust commands add, remove, and list canonical exact HTTPS origins in the selected YAML. Add/remove are idempotent and silent in human mode; list prints one origin per line in insertion order. JSON mutations return `{"schema_version":1}` and list returns `{"schema_version":1,"origins":[...]}`. Trust operations are available only on macOS and Linux and enforce regular-file, non-writable parent/file, no-symlink, and atomic-edit safeguards.

The current CLI has no local broker, sidebar, `agent serve`, or daemon lifecycle commands. Do not invent `publish`, authentication, asset bundling, broker commands, or a `--permanent` flag. The supported operations above are the complete current publication and trust surface.
