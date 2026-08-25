# Publishing commands

Use `--json` for agent-driven commands. Put global flags before the command and `--` before an ID.

## Create

```sh
agent-whiteboard [--server URL] [--timeout DURATION] [--json] create markdown --context CONTEXT_FILE [--expires-in SECONDS] FILE
agent-whiteboard [global flags] create html --context CONTEXT_FILE [--expires-in SECONDS] FILE
agent-whiteboard [global flags] image upload [--expires-in SECONDS] FILE...
```

Markdown and HTML require non-empty UTF-8 source and context files. Images accept PNG, JPEG, GIF, and WebP.

## Update

```sh
agent-whiteboard [global flags] update markdown --context CONTEXT_FILE [--expires-in SECONDS] -- ID FILE
agent-whiteboard [global flags] update html --context CONTEXT_FILE [--expires-in SECONDS] -- ID FILE
agent-whiteboard [global flags] image update [--expires-in SECONDS] -- ID FILE
```

Markdown and HTML updates replace source and context together while preserving the capability URL. Omitting `--expires-in` preserves the existing expiration.

## Retrieve

```sh
agent-whiteboard [global flags] --json get markdown -- ID
agent-whiteboard [global flags] --json get html -- ID
```

Retrieval returns exact source and context. There is no image retrieval command; use its public URL.

## Delete

```sh
agent-whiteboard [global flags] delete markdown -- ID
agent-whiteboard [global flags] delete html -- ID
agent-whiteboard [global flags] image delete -- ID
```

Deletion revokes the capability. Successful human-mode deletion is silent.

## Server and global options

Server selection is explicit `--server`, non-empty `AGENT_WHITEBOARD_SERVER`, selected YAML `client.server`, then built-in `http://127.0.0.1:8567`.
If no host was supplied and the configured/default target is acceptable, let the CLI resolve it and use the absolute URL in its JSON result.
When the host is explicitly unknown or remote is expected, inspect environment and YAML before creating; ask for the origin rather than falling back to localhost.
Never guess a replacement host after a connection failure.

- `--server URL`: override the configured publishing server.
- `--timeout DURATION`: set the client deadline.
- `--config PATH`: select a configuration file.
- `--json`: request structured output.

Create without `--expires-in` uses the server default.
Update without it preserves the existing absolute expiration.
A positive value resets expiration from the command time; `0` makes the resource permanent.
There is no `--permanent` flag.

## JSON result

Create and update return schema version 1:

```json
{"schema_version":1,"resource":{"id":"CAPABILITY_ID","url":"https://whiteboard.example/whiteboards/markdown/CAPABILITY_ID","expires_at":1780000000,"permanent":false}}
```

Image upload returns `resources` in input order. Retrieval adds exact `markdown` or `html` and `context` fields. Delete returns only `schema_version`. Errors are written to stderr with `error.code` and `error.message`.

## Failure recovery

- Usage errors: fix the command; do not retry with guessed flags.
- Remote errors: report the stable error code and message.
- Timeout or cancellation: report that the result is unknown unless the server response proves otherwise.
- Uncertain create: the CLI may print a generated resource to stdout before exiting nonzero. Preserve the ID privately, then retrieve or delete that resource. Do not assume failed create means nothing was stored.
