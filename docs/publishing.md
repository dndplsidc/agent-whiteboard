# Publishing and CLI guide

Publish Markdown, Mermaid diagrams, trusted standalone HTML, and raster images from your shell. For agent-led setup, start with the [README](../README.md#quick-start).

## CLI command reference

All commands start with `agent-whiteboard`. In the tables below, `KIND` means either `markdown` or `html`; `FILE`, `ID`, and `ORIGIN` are placeholders for your source file, capability ID, and exact server origin. Put global flags before the command. Use `--` before positional IDs to prevent IDs starting with a dash from being treated as flags.

### Publish and manage pages

| Command | Use case |
| --- | --- |
| `create KIND --context CONTEXT.md --title "Title" --summary "Summary" FILE` | Publish a new document or trusted HTML page and receive its URL. Context, title, and summary are required. |
| `update KIND --context CONTEXT.md -- ID FILE` | Replace a page and its creator context while keeping its URL. |
| `--json get KIND -- ID` | Retrieve exact source and creator context for inspection or editing. Requires JSON output. |
| `delete KIND -- ID` | Remove a published page from the server. |
| `catalog list` | Find Markdown and HTML boards recorded by this CLI on this machine, across publishing servers, without network access. |

Use `--expires-in SECONDS` on create or update to set a lifetime; `0` means permanent. Updates preserve expiration when this flag is omitted. Optional `--title` and `--summary` on update change the local catalog metadata. Narrow catalog results with `--query "words"`, `--kind markdown`, `--limit 20`, and `--offset 0`. The catalog does not list every resource on a server.

### Manage images

| Command | Use case |
| --- | --- |
| `image upload FILE...` | Upload one or more PNG, JPEG, GIF, or WebP images for sharing or embedding in a page. |
| `image update -- ID FILE` | Replace an image while keeping its URL. |
| `image delete -- ID` | Remove a published image. |

Image upload and update also accept `--expires-in SECONDS`. Images do not require creator context and are not included in the local catalog. There is no image retrieval subcommand; open the returned image URL.

### Run services and authorize Page Agent

| Command | Use case |
| --- | --- |
| `serve` | Run the publishing server; use `--storage PATH` to choose storage. |
| `agent serve` | Run the reader's local Page Agent broker in the foreground. |
| `agent trust add ORIGIN` | Allow Page Agent from an exact remote HTTPS origin to connect to the broker. |
| `agent trust list` | Inspect configured trusted origins. |
| `agent trust remove ORIGIN` | Remove an origin from the trust configuration. |

The publishing server and broker are separate processes. Provider paths can be supplied to `agent serve` with `--pi-executable`, `--codex-executable`, or `--cursor-executable`. See [Page Agent setup](page-agent.md) for provider login, server-side enablement, trust rules, and connection verification.

### Manage the macOS broker daemon

| Command | Use case |
| --- | --- |
| `agent serve --daemon` | Install and start the broker as a persistent per-user macOS service. |
| `agent daemon status` | Inspect the managed service's state. |
| `agent daemon restart` | Restart the managed broker. |
| `agent daemon stop` | Stop the managed broker. |
| `agent daemon uninstall` | Remove the managed service installation. |

Linux supports foreground `agent serve` only. Daemon status alone does not establish broker readiness; follow the listener and browser checks in the [Page Agent guide](page-agent.md#reuse-or-start-the-readers-local-broker).

### Help and global options

Run `agent-whiteboard --help` to list command groups, or append `--help` to any command to see its arguments and flags. `agent-whiteboard help COMMAND` also opens command help.

| Global flag | Use case |
| --- | --- |
| `--server URL` | Select the publishing origin for resource operations. |
| `--config PATH` | Use a particular YAML configuration file. |
| `--timeout DURATION` | Set the publishing client's request timeout, for example `20s`. |
| `--json` | Produce structured output for agents and scripts; required for `get`. |

For example, `agent-whiteboard --json --server https://whiteboard.example get markdown -- ID` retrieves a page from an explicitly selected server. See [CLI JSON](cli-json.md) for output contracts and errors.

## Manual quick start

Agent Whiteboard supports macOS and Linux with Go 1.25 or 1.26. This path installs the CLI and publishes a local page. To add Page Agent, continue with [Use Page Agent](page-agent.md).

### 1. Install the CLI

```sh
go install github.com/dndplsidc/agent-whiteboard/cmd/agent-whiteboard@latest
```

### 2. Start the server

```sh
agent-whiteboard serve --storage "$HOME/.agent-whiteboard"
```

The local server listens on `http://127.0.0.1:8567` by default.

### 3. Publish your first whiteboard

In another terminal, create a small Markdown board and its creator context:

```sh
context_dir="$(mktemp -d)"
trap 'rm -rf "$context_dir"' EXIT
context_file="$context_dir/context.md"
board_file="$context_dir/board.md"

cat >"$context_file" <<'EOF'
# Creator context

- Goal: demonstrate Markdown, Mermaid, and syntax highlighting.
EOF

cat >"$board_file" <<'EOF'
# Agent Whiteboard quick start

A Mermaid diagram rendered from Markdown:

~~~mermaid
flowchart LR
    Agent --> Whiteboard --> Reader
~~~

And a highlighted code block:

~~~go
fmt.Println("Hello from Agent Whiteboard")
~~~
EOF

agent-whiteboard create markdown \
  --context "$context_file" \
  --title "Agent Whiteboard quick start" \
  --summary "Markdown, Mermaid, and syntax-highlighting demonstration" \
  --expires-in 3600 \
  "$board_file"
```

The command prints a capability URL. Open it in a browser to see the rendered whiteboard.

Creator context records the goals, decisions, assumptions, and open questions behind a page. It travels with the whiteboard and is available to readers and Page Agent. Do not include hidden reasoning, credentials, sensitive data, private source, or raw tool output.

`--title` and `--summary` are required descriptive metadata for the local catalog on this laptop. They are not published as source or creator context and do not change the Markdown heading, HTML `<title>`, or browser title. The catalog records Markdown and HTML boards created by this CLI, including boards sent to remote servers.

## What you can publish

### Markdown and Mermaid

Markdown is rendered in the browser with bundled markdown-it, DOMPurify, highlight.js, and Mermaid assets. Use ordinary fenced `mermaid` blocks for diagrams. When Page Agent is enabled, readers can choose **Add diagram** to place the exact fenced Mermaid source in their message.

### Trusted standalone HTML

Publish interactive reports, dashboards, or prototypes as trusted standalone HTML. The stable public URL uses an application-owned wrapper around opaque-origin sandboxed content. Exact submitted bytes remain available from the resource's `/content` route.

Standalone HTML is active content, not sanitized Markdown. Publish only code you trust and read the [security model](security.md) before using it.

### Raster images

Upload PNG, JPEG, GIF, and WebP images. Agent Whiteboard detects and validates formats from their bytes; SVG is rejected.

Publish images before Markdown that references their returned URLs:

```sh
agent-whiteboard image upload --expires-in 3600 chart.png photo.webp
```

Replace an uploaded image in place while keeping its capability URL:

```sh
agent-whiteboard image update --expires-in 7200 -- CAPABILITY_ID chart.png
```

## Common workflows

For Markdown and HTML commands below, set `context_file` to a fresh UTF-8 Markdown file containing the relevant goals, decisions, and assumptions for that page. Replace sample source paths and `CAPABILITY_ID` with your own values. Paths under `docs/examples/` assume you are in a repository checkout.

### Discover locally recorded whiteboards

Search this laptop's catalog before asking for a URL or creating a replacement:

```sh
agent-whiteboard --json catalog list --query "recharge architecture" --limit 20 --offset 0
agent-whiteboard --json catalog list --kind markdown
```

Search is offline across every publishing server recorded by this local CLI. All whitespace-separated terms must appear in the title or summary. Results are newest first and include `total`, `limit`, and `offset` for pagination. Expired, deleted, and uncertain records remain visible. An empty result means only that this local catalog has no match; older boards, boards created on another device, and direct HTTP or Go API creations are not enrolled.

Use the selected record's `server` value for `get`, `update`, or `delete`; the current configured server may be different. Listing does not contact that server, and a record without an expired, deleted, or uncertain label is not proof that the remote resource remains available.

### Publish trusted HTML

```sh
agent-whiteboard create html \
  --context "$context_file" \
  --title "Standalone HTML example" \
  --summary "Interactive example published as trusted HTML" \
  --expires-in 3600 \
  docs/examples/standalone.html
```

### Update content

Markdown and HTML updates replace source and creator context together:

```sh
agent-whiteboard update markdown \
  --context "$context_file" \
  --title "Updated architecture" \
  --expires-in 7200 \
  -- CAPABILITY_ID board.md

agent-whiteboard update html \
  --context "$context_file" \
  --expires-in 7200 \
  -- CAPABILITY_ID board.html
```

Omitting `--expires-in` on update preserves the current expiration. `--expires-in 0` makes the resource permanent. Optional `--title` and `--summary` replace only the supplied local catalog metadata for a tracked board; omitting either preserves it.

### Retrieve exact source and context

```sh
agent-whiteboard --json get markdown -- CAPABILITY_ID
agent-whiteboard --json get html -- CAPABILITY_ID
```

Retrieval requires `--json` and returns the exact source together with creator context.

### Delete resources

```sh
agent-whiteboard delete markdown -- CAPABILITY_ID
agent-whiteboard delete html -- CAPABILITY_ID
agent-whiteboard image delete -- CAPABILITY_ID
```

### Publish to a remote server

Put global flags before the command, or set `AGENT_WHITEBOARD_SERVER`:

```sh
agent-whiteboard --server https://whiteboard.example --timeout 20s create markdown \
  --context "$context_file" \
  --title "Remote board" \
  --summary "Published remotely and recorded on this laptop" \
  board.md
```

## Deployment and configuration

Configuration defaults to `~/.agent-whiteboard/config.yaml`. Settings resolve in this order where supported:

1. Explicit flags
2. Non-empty `AGENT_WHITEBOARD_*` environment variables
3. YAML
4. Built-in defaults

The YAML format is versioned and strict. See [Configuration](configuration.md) for the complete client, server, viewer, and agent schema, including validation and file-safety rules.

The local catalog path is fixed at `~/.agent-whiteboard/catalog` for the effective user. It is not affected by `--config`, `server.storage`, the current directory, or the selected publishing server.

| Setting | Default |
| --- | ---: |
| Publishing server | `http://127.0.0.1:8567` |
| Page Agent broker | `127.0.0.1:8568` |
| Storage | `$HOME/.agent-whiteboard` |
| Client timeout | `30s` |
| Resource expiration | `86400` seconds |
| Whiteboard source limit | 10 MiB |
| Creator context limit | 1 MiB |
| Image limit | 25 MiB each |

Run `agent-whiteboard serve --help` or `agent-whiteboard agent serve --help` for the complete flag lists.

