# agent-whiteboard

`agent-whiteboard` is a small Go server and CLI for publishing Markdown, trusted standalone HTML, and raster images at capability URLs. It is designed for agents that need to return a viewable result without opening a browser or depending on a CDN.

## Install and build

Go 1.25 or 1.26 is supported on macOS and Linux.

```sh
go install github.com/edocsss/agent-whiteboard/cmd/agent-whiteboard@latest
```

For development:

```sh
go build -trimpath -o ./bin/agent-whiteboard ./cmd/agent-whiteboard
go test ./...
go test -race ./...
```

## Five-minute local start

Start the server in one terminal:

```sh
agent-whiteboard serve --storage "$HOME/.agent-whiteboard"
```

Publish the executable examples in another terminal. Each create/update command prints a public URL; it never opens it.

```sh
agent-whiteboard create markdown --expires-in 3600 docs/examples/diagram.md
agent-whiteboard create html --expires-in 3600 docs/examples/standalone.html
```

Markdown is rendered in the browser by bundled markdown-it, DOMPurify, highlight.js, and Mermaid assets. Add diagrams with ordinary fenced `mermaid` blocks. Standalone HTML is served unchanged and must be treated as trusted active content.

Images are validated from their bytes. PNG, JPEG, GIF, and WebP are supported; SVG is rejected. Publish images before publishing Markdown that references their returned URLs.

```sh
agent-whiteboard image upload --expires-in 3600 chart.png photo.webp
```

Use the returned capability ID to replace or delete a resource:

```sh
agent-whiteboard update markdown --expires-in 7200 CAPABILITY_ID docs/examples/diagram.md
agent-whiteboard update html --expires-in 7200 CAPABILITY_ID docs/examples/standalone.html
agent-whiteboard delete markdown CAPABILITY_ID
agent-whiteboard delete html CAPABILITY_ID
agent-whiteboard image update --expires-in 7200 CAPABILITY_ID chart.png
agent-whiteboard image delete CAPABILITY_ID
```

For a remote server, put global flags before the command (or set `AGENT_WHITEBOARD_SERVER`):

```sh
agent-whiteboard --server https://whiteboard.example --timeout 20s --json create markdown --expires-in 3600 docs/examples/diagram.md
```

Omitting `--expires-in` uses the server default. `--expires-in 0` makes a resource permanent. Expiration is recalculated from update time when the flag is supplied; omission on update retains the current expiration.

## Trusted agent origins

Manage exact HTTPS origins in `~/.agent-whiteboard/config.yaml`:

```sh
agent-whiteboard agent trust add https://whiteboard.example
agent-whiteboard agent trust list
agent-whiteboard agent trust remove https://whiteboard.example
```

Use the global `--config PATH` flag before or after subcommands to select another configuration file. Explicitly selected files must already exist; adding to a missing default configuration creates its parent and file with owner-only permissions. Add and remove are idempotent, and human list output contains one canonical origin per line.

## Defaults

| Setting | Default |
| --- | ---: |
| Bind address | `127.0.0.1:8567` |
| Storage | `$HOME/.agent-whiteboard` |
| Client timeout | `30s` |
| Resource expiration | `86400` seconds |
| Cleanup interval | `15m` |
| Shutdown timeout | `10s` |
| Whiteboard limit | 10 MiB |
| Image limit | 25 MiB each |
| Image request limit | 100 MiB |

Flag values override `AGENT_WHITEBOARD_*` environment variables, which override defaults. Run `agent-whiteboard serve --help` for the complete flag list.

## Security and detailed contracts

Capability URLs are public but marked non-indexable. Non-indexing is not access control: do not publish credentials, tokens, private source, personal data, or sensitive information. See [security](docs/security.md).

- [HTTP API](docs/http-api.md)
- [Go API and dependency injection](docs/go-api.md)
- [filesystem storage](docs/storage.md)
- [versioned CLI JSON](docs/cli-json.md)
- examples: [Markdown/Mermaid](docs/examples/diagram.md) and [standalone HTML](docs/examples/standalone.html)

Asset development uses Node 24 and pnpm 11.4:

```sh
pnpm install --frozen-lockfile
pnpm test
pnpm run check:assets
pnpm run test:browser
```
