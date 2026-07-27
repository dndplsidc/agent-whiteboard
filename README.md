# agent-whiteboard

`agent-whiteboard` is a small Go server and CLI for publishing Markdown with creator context, trusted standalone HTML, and raster images at capability URLs. It is designed for agents that need to return a viewable result without opening a browser or depending on a CDN.

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

Markdown create and update require a second, non-empty UTF-8 Markdown file containing creator context. Create it in a temporary directory and remove it when finished:

```sh
context_dir="$(mktemp -d)"
trap 'rm -rf "$context_dir"' EXIT
context_file="$context_dir/context.md"
cat >"$context_file" <<'EOF'
# Creator context

- Goal: demonstrate Markdown, Mermaid, and syntax highlighting.
- Assumptions: the bundled viewer assets are available.
- Open questions: none.
EOF

agent-whiteboard create markdown --context "$context_file" --expires-in 3600 docs/examples/diagram.md
agent-whiteboard create html --expires-in 3600 docs/examples/standalone.html
```

Creator context should summarize goals, decisions, assumptions, and open questions. Do not put hidden reasoning, credentials, sensitive data, unrelated personal data, private source, or raw tool output in it. The context shares the board's bearer capability and lifecycle and is returned by machine retrieval.

Markdown is rendered in the browser by bundled markdown-it, DOMPurify, highlight.js, and Mermaid assets. Add diagrams with ordinary fenced `mermaid` blocks. Standalone HTML remains trusted active content: its stable public URL now returns an opaque-origin sandbox wrapper, while exact stored bytes are served only to that wrapper at `/content`. Trusted scripts may still disclose the capability through permitted child self-navigation, so never use standalone HTML for untrusted code.

Images are validated from their bytes. PNG, JPEG, GIF, and WebP are supported; SVG is rejected. Publish images before publishing Markdown that references their returned URLs.

```sh
agent-whiteboard image upload --expires-in 3600 chart.png photo.webp
```

Use the returned capability ID to replace, retrieve, or delete a resource. Markdown updates replace source and context together; neither half can be updated independently.

```sh
agent-whiteboard update markdown --context "$context_file" --expires-in 7200 -- CAPABILITY_ID docs/examples/diagram.md
agent-whiteboard --json get markdown -- CAPABILITY_ID
agent-whiteboard update html --expires-in 7200 -- CAPABILITY_ID docs/examples/standalone.html
agent-whiteboard delete markdown -- CAPABILITY_ID
agent-whiteboard delete html -- CAPABILITY_ID
agent-whiteboard image update --expires-in 7200 -- CAPABILITY_ID chart.png
agent-whiteboard image delete -- CAPABILITY_ID
```

`get markdown` requires `--json` and returns the exact Markdown and creator context. For a remote server, put global flags before the command (or set `AGENT_WHITEBOARD_SERVER`):

```sh
agent-whiteboard --server https://whiteboard.example --timeout 20s --json create markdown --context "$context_file" --expires-in 3600 docs/examples/diagram.md
```

Omitting `--expires-in` uses the server default. `--expires-in 0` makes a resource permanent. Expiration is recalculated from update time when the flag is supplied; omission on update retains the current expiration.

## Configuration and trusted origins

Configuration defaults to `~/.agent-whiteboard/config.yaml`. Global `--config PATH` selects another existing file. The YAML is versioned and strict: unknown fields, duplicate keys, aliases, merge keys, invalid types, and unsupported versions are rejected. Settings resolve as explicit flags, then non-empty environment variables, then YAML, then built-ins. A relative YAML storage path is resolved from the configuration file's directory.

See [configuration](docs/configuration.md) for the complete client, server, viewer, and agent schema, environment mapping, validation, path, permission, and symlink policies.

Manage the configured exact HTTPS origin list with:

```sh
agent-whiteboard agent trust add https://whiteboard.example
agent-whiteboard agent trust list
agent-whiteboard agent trust remove https://whiteboard.example
```

Add and remove are idempotent; human list output contains one canonical origin per line, and `--json` uses schema version 1. These trust operations are supported only on macOS and Linux.

Run the local agent API and broker in the foreground with:

```sh
# Authenticate with Pi first; Pi stores its provider-native login state.
pi
agent-whiteboard agent serve
```

`agent serve` resolves `pi` from `PATH`; use `--pi-executable PATH` or `AGENT_WHITEBOARD_PROVIDER_PI_EXECUTABLE` to override it. The adapter uses Pi's `~/.pi/agent/auth.json` and deliberately excludes ambient provider API-key and auth-token variables from the Pi process environment. It binds only to literal IPv4 loopback and admits exact configured HTTPS origins.

On macOS, install and start the managed per-user LaunchAgent with `agent-whiteboard agent serve --daemon`. It records absolute agent and configuration paths plus the resolved Pi path when Pi is available during installation; the child reads the configuration when it starts. Inspect it with `agent-whiteboard agent daemon status`, reload it with `restart`, stop it with `stop` (the plist remains installed), or remove it with `uninstall`. Daemon operations are unsupported on Linux; run `agent-whiteboard agent serve` in the foreground instead. Authenticate with Pi itself; agent-whiteboard never stores or accepts provider credentials.

## Defaults

| Setting | Default |
| --- | ---: |
| Bind address | `127.0.0.1:8567` |
| Storage | `$HOME/.agent-whiteboard` |
| Client timeout | `30s` |
| Resource expiration | `86400` seconds |
| Cleanup interval | `15m` |
| Shutdown timeout | `10s` |
| Whiteboard source limit | 10 MiB |
| Markdown context limit | 1 MiB |
| Image limit | 25 MiB each |
| Image request limit | 100 MiB |

Run `agent-whiteboard serve --help` for the complete server flag list.

## Security and detailed contracts

Capability URLs are public but marked non-indexable. Non-indexing is not access control: do not publish credentials, tokens, private source, personal data, or sensitive information in source or creator context. Anyone with a Markdown capability ID can retrieve both through the machine API. See [security](docs/security.md).

- [configuration](docs/configuration.md)
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
