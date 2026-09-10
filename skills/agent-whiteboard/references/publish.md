# Publishing commands

Use `--json` for agent-driven commands. Put global flags before the command and `--` before an ID when a filename could be mistaken for a flag.

## Discover this laptop's records

Search the local catalog before creating a replacement or asking for a known URL:

```sh
agent-whiteboard --json catalog list
agent-whiteboard --json catalog list --query "recharge architecture"
agent-whiteboard --json catalog list --kind markdown --query "recharge architecture" --limit 20 --offset 0
```

The catalog searches records created by this CLI on this laptop, across all recorded publishing origins, without contacting a server. `--kind` is `markdown` or `html`. Search is case-insensitive, and every whitespace-separated query term must appear in the title or summary. Filter before pagination; use the returned `total`, `limit`, and `offset` to request later pages.

This history is not every board on a server or account. Images, older creations, other devices, direct HTTP/Go API creations, and unknown boards that are merely retrieved, updated, or deleted are absent. An empty result means no local match. Expired, locally deleted, and `creation_uncertain` records remain visible; an ordinary-looking record is still not proof of current remote availability.

Use a selected result's recorded server for current `get`, `update`, or `delete` evidence. Do not substitute the currently configured default. Global `--server` and publishing configuration neither filter nor prevent `catalog list`.

## Create

```sh
agent-whiteboard [--server URL] [--timeout DURATION] --json create markdown \
  --context CONTEXT_FILE --title "TITLE" --summary "SUMMARY" [--expires-in SECONDS] FILE

agent-whiteboard [global flags] --json create html \
  --context CONTEXT_FILE --title "TITLE" --summary "SUMMARY" [--expires-in SECONDS] FILE

agent-whiteboard [global flags] --json image upload [--expires-in SECONDS] FILE...
```

Markdown and HTML require non-empty UTF-8 source and context files plus non-empty UTF-8 title and summary flags. Title and summary are trimmed local catalog metadata. They are never inserted into or sent as source or creator context and do not rename document headings, HTML titles, or browser tabs. Images accept PNG, JPEG, GIF, and WebP and are not cataloged.

## Update

```sh
agent-whiteboard [global flags] --json update markdown \
  --context CONTEXT_FILE [--title "TITLE"] [--summary "SUMMARY"] [--expires-in SECONDS] -- ID FILE

agent-whiteboard [global flags] --json update html \
  --context CONTEXT_FILE [--title "TITLE"] [--summary "SUMMARY"] [--expires-in SECONDS] -- ID FILE

agent-whiteboard [global flags] --json image update [--expires-in SECONDS] -- ID FILE
```

Markdown and HTML updates replace source and context together while preserving the capability URL. Omitting `--expires-in` preserves expiration. For a tracked record, optional title and summary replace only the supplied metadata; omitted metadata is preserved.

Updating an unknown board does not enroll it. If title or summary is supplied, stderr reports `catalog_record_missing` even though the remote update and resource stdout remain successful. Use an existing locally recorded board when catalog metadata management is required.

## Retrieve

```sh
agent-whiteboard --server RECORDED_SERVER --json get markdown -- ID
agent-whiteboard --server RECORDED_SERVER --json get html -- ID
```

Retrieval returns exact source and context and does not change or enroll a catalog record. There is no image retrieval command; use its public URL.

## Delete

```sh
agent-whiteboard --server RECORDED_SERVER --json delete markdown -- ID
agent-whiteboard --server RECORDED_SERVER --json delete html -- ID
agent-whiteboard [global flags] --json image delete -- ID
```

Deletion revokes the remote capability. A successful local CLI deletion marks an existing catalog record `deleted` but retains it for discovery. Deleting an unknown board does not enroll it. Successful human-mode deletion is silent.

## Server, expiration, and global options

Server selection is explicit `--server`, non-empty `AGENT_WHITEBOARD_SERVER`, selected YAML `client.server`, then built-in `http://127.0.0.1:8567`. If no host was supplied and the configured/default target is acceptable, let the CLI resolve it and use the absolute URL in its JSON result. When the host is explicitly unknown or remote is expected, inspect environment and YAML before creating; ask for the origin rather than falling back to localhost. Never guess a replacement host after a connection failure.

- `--server URL`: override the configured publishing server.
- `--timeout DURATION`: set the client deadline.
- `--config PATH`: select a configuration file.
- `--json`: request structured output.

Create without `--expires-in` uses the server default. Update without it preserves the existing absolute expiration. A positive value resets expiration from the command time; `0` makes the resource permanent. There is no `--permanent` flag.

## JSON results

Create and update return schema version 1:

```json
{"schema_version":1,"resource":{"id":"CAPABILITY_ID","url":"https://whiteboard.example/whiteboards/markdown/CAPABILITY_ID","expires_at":1780000000,"permanent":false}}
```

Catalog list also uses schema version 1. Records contain `server`, `kind`, `id`, `url`, title, summary, source basename, local observation timestamps, expiration, permanence, lifecycle state, optional deletion time, and derived `expired`. `records` is always an array, and `total` counts every match before pagination:

```json
{"schema_version":1,"records":[],"total":0,"limit":20,"offset":0}
```

Image upload returns `resources` in input order. Retrieval adds exact `markdown` or `html` and `context` fields. Delete returns only `schema_version`. Errors are written to stderr with `error.code` and `error.message`.

## Warnings and failure recovery

Remote success can still emit a warning on stderr and exit 0. Parse each newline-delimited warning envelope instead of assuming successful stderr is empty:

- `catalog_write_failed`: the remote create, update, or delete succeeded, but this laptop's local record could not be saved. Preserve the returned URL. Do not repeat create to repair the catalog because that may create a duplicate.
- `catalog_record_missing`: an unknown board was updated successfully, but supplied title/summary metadata was not recorded.

Creation checks catalog writability before HTTP publication. A `catalog_unavailable` preflight error means no create request was sent. The preflight cannot make remote and local persistence atomic, so a later catalog warning remains possible.

For a create error with a validated returned resource, stdout still contains the resource, the local record state is `creation_uncertain`, and stderr contains the original error. Preserve the ID privately and use the returned URL and recorded server with existing `get` or `delete`. Do not repeat create. A successfully recorded uncertain result adds no catalog warning; if local recording also fails, `catalog_write_failed` appears before the original error envelope. A failure without a returned ID produces no catalog record.

Warnings never contain source, creator context, or raw filesystem errors. A warning-write failure does not replace the remote outcome. Timeout or cancellation remains uncertain unless a returned resource proves otherwise.
