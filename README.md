# Agent Whiteboard

Publish agent work as pages people can inspect, share, and discuss.

Agent Whiteboard is a self-hosted Go server and CLI for publishing Markdown, Mermaid diagrams, trusted standalone HTML, and raster images at capability URLs. Agents can publish from the shell; readers get a polished browser view and can optionally discuss the page with Pi or Codex through **Page Agent**.

- **Built for agent workflows:** publish, update, retrieve, and delete without opening a browser.
- **Self-hosted:** one Go binary, filesystem storage, and no CDN dependency.
- **Made for rich results:** sanitized Markdown, syntax highlighting, Mermaid, trusted active HTML, and images.
- **Page-aware conversations:** send exact source, creator context, selected sections, components, code, or images to a local Pi or Codex session.
- **Explicit lifecycle:** use expiring or permanent capability URLs and replace content in place.

## Quick start

Agent Whiteboard supports macOS and Linux with Go 1.25 or 1.26.

### 1. Install the CLI

```sh
go install github.com/edocsss/agent-whiteboard/cmd/agent-whiteboard@latest
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
- Decisions: use a short flow diagram and Go example.
- Assumptions: the bundled viewer assets are available.
- Open questions: none.
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
  --expires-in 3600 \
  "$board_file"
```

The command prints a capability URL. Open it in a browser to see the rendered whiteboard.

Creator context records the goals, decisions, assumptions, and open questions behind a page. It travels with the whiteboard and is available to readers and Page Agent. Do not include hidden reasoning, credentials, sensitive data, private source, or raw tool output.

## Install the agent skill

Install the bundled skill so supported coding agents can publish and manage whiteboards for you:

```sh
npx skills add dndplsidc/agent-whiteboard --skill agent-whiteboard
```

The installer detects supported agents and installs the skill into the current project. To make it available globally:

```sh
npx skills add dndplsidc/agent-whiteboard --skill agent-whiteboard --global
```

Once installed, ask your agent to publish Markdown, Mermaid, trusted standalone HTML, or images to Agent Whiteboard. The skill covers resource selection, creator context, publication, lifecycle commands, and rendered verification.

Install the separate setup skill when the agent needs to install the binary, run a publishing server, configure Page Agent, manage origin trust, or diagnose setup:

```sh
npx skills add dndplsidc/agent-whiteboard --skill agent-whiteboard-setup
```

The publishing and setup skills stay separate so each invocation loads only the instructions needed for the task.

## Use Page Agent

Page Agent lets a reader discuss the current whiteboard with a locally running Pi or Codex provider. The reader explicitly connects, reviews what will be shared, and keeps the provider's normal model, tools, skills, approval policy, sandbox, and project configuration.

Setup has two sides: the publishing server must expose Page Agent, and each reader must run and authorize their own local broker.

### Enable Page Agent on the publishing server

The server operator enables the viewer integration in `~/.agent-whiteboard/config.yaml` or another selected configuration file:

```yaml
version: 1

viewer:
  local_agent:
    enabled: true
```

Restart `agent-whiteboard serve` after changing the configuration. The Page Agent control will then appear on published Markdown and trusted HTML whiteboards.

See [configuration](docs/configuration.md) for the complete schema and configuration-file rules.

### Prepare a provider on the reader's machine

Each reader needs:

1. The `agent-whiteboard` CLI installed.
2. Pi, Codex, or both available on `PATH`.
3. Authentication completed through each provider's own CLI.

Agent Whiteboard does not accept or store provider credentials. Pi and Codex use their effective native user configuration unchanged.

If provider executables are installed elsewhere, pass one or both paths when starting the broker:

```sh
agent-whiteboard agent serve \
  --pi-executable /path/to/pi \
  --codex-executable /path/to/codex
```

A missing provider does not stop the broker or the other provider from working.

### Trust the publishing origin

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

### Start the reader's local broker

Run the broker in the foreground:

```sh
agent-whiteboard agent serve
```

It listens on literal IPv4 loopback, uses port `8568` by default, and resolves Pi and Codex independently from `PATH`.

On macOS, install and start it as a managed per-user LaunchAgent instead:

```sh
agent-whiteboard agent serve --daemon
agent-whiteboard agent daemon status
```

Other daemon operations are:

```sh
agent-whiteboard agent daemon restart
agent-whiteboard agent daemon stop
agent-whiteboard agent daemon uninstall
```

Managed daemon operations are not available on Linux; keep `agent serve` running in the foreground there.

### Connect from a whiteboard

1. Open an Agent Whiteboard capability URL.
2. Open **Page Agent**.
3. Select Pi or Codex.
4. Review the page context disclosed by the viewer.
5. Choose **Connect**.
6. Write a message or add page content to the composer, then send it.

Opening the pane, checking broker status, or switching providers does not send page content. The first contextual message sends the complete exact Markdown or HTML source, creator context, title, URL, resource metadata, and the reader's message as one envelope to the selected provider.

Readers can add more precise context without copying and pasting:

- Select rendered Markdown text and choose **Add to message**.
- Add a heading-defined Markdown section or the complete page.
- Add supported rendered raster images.
- In trusted HTML, use **+ Add** or the **Components** chooser for eligible sections, images, charts, tables, code, quotes, and explicitly declared components.
- Add private PNG, JPEG, GIF, or WebP attachments from the composer.

Page Agent also exposes the provider's supported model and reasoning controls, native skills through `$`, manual `/compact`, streaming activity, interruption, archives, and supported approval or elicitation requests. Pi and Codex keep independent conversations for the same whiteboard.

### Troubleshoot reader setup

| Symptom | What to check |
| --- | --- |
| Broker unavailable | Start `agent-whiteboard agent serve` and verify the viewer's broker port, normally `8568`. |
| Origin not trusted | Run the exact `agent-whiteboard agent trust add https://…` command for the publishing origin. |
| Provider unavailable | Confirm `pi` or `codex` is on `PATH` and authenticated through its native CLI. |
| Browser cannot reach loopback | Allow Local Network Access when prompted by the browser. |
| Incompatible local API | Update the publishing server and reader CLI together, then restart the broker. |

## Why Agent Whiteboard?

Agents are good at producing reports, diagrams, prototypes, and visual explanations, but their results often end up as terminal output, temporary files, or local pages that are awkward to share. Generic paste services make content viewable, but usually lose lifecycle control, exact source retrieval, creator context, or a path back into the agent workflow.

Agent Whiteboard closes that gap:

1. An agent publishes from the CLI or HTTP API.
2. The server returns a capability URL with an explicit lifetime.
3. A reader opens a bundled, self-contained viewer.
4. If Page Agent is enabled, the reader can continue the work with a local Pi or Codex session using exact page context.

## What you can publish

### Markdown and Mermaid

Markdown is rendered in the browser with bundled markdown-it, DOMPurify, highlight.js, and Mermaid assets. Use ordinary fenced `mermaid` blocks for diagrams.

### Trusted standalone HTML

Publish interactive reports, dashboards, or prototypes as trusted standalone HTML. The stable public URL uses an application-owned wrapper around opaque-origin sandboxed content. Exact submitted bytes remain available from the resource's `/content` route.

Standalone HTML is active content, not sanitized Markdown. Publish only code you trust and read the [security model](docs/security.md) before using it.

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

## How it works

```text
Agent or CLI
    │ publish
    ▼
Agent Whiteboard server ── capability URL ──► Browser viewer
                                                   │
                                                   │ explicit reader consent
                                                   ▼
                                          Local Page Agent broker
                                               │         │
                                               ▼         ▼
                                              Pi       Codex
```

Public resources live on the self-hosted server. The optional Page Agent broker lives only on the reader's machine and accepts authorized browser origins over literal loopback. Published content and creator context remain untrusted provider input; each provider's native tools, approvals, and sandbox remain authoritative.

## Common workflows

### Publish trusted HTML

```sh
agent-whiteboard create html \
  --context "$context_file" \
  --expires-in 3600 \
  docs/examples/standalone.html
```

### Update content

Markdown and HTML updates replace source and creator context together:

```sh
agent-whiteboard update markdown \
  --context "$context_file" \
  --expires-in 7200 \
  -- CAPABILITY_ID board.md

agent-whiteboard update html \
  --context "$context_file" \
  --expires-in 7200 \
  -- CAPABILITY_ID board.html
```

Omitting `--expires-in` on update preserves the current expiration. `--expires-in 0` makes the resource permanent.

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
agent-whiteboard --server https://whiteboard.example --timeout 20s create markdown --context "$context_file" board.md
```

## Security model

Capability URLs are bearer capabilities, not authenticated private links. Anyone holding a Markdown or HTML capability ID can view the resource, retrieve its exact source and creator context, update it, or delete it. `noindex` limits discovery; it is not access control.

Keep these boundaries in mind:

- Never publish credentials, tokens, private source, personal data, or other sensitive information.
- Creator context is visible to anyone holding the capability and is not a hidden channel.
- Markdown is sanitized; standalone HTML is trusted active content with a stricter sandboxed delivery model.
- A local `127.0.0.1` publishing origin is deliberately trusted by the Page Agent broker without an explicit trust-list entry.
- Whiteboard content is untrusted model input. Native provider tools, approval settings, sandbox, project trust, and extensions remain authoritative.
- Agent Whiteboard does not provide a content-only provider sandbox or per-whiteboard filesystem boundary.

Read [Security](docs/security.md) for the complete browser, capability, HTML, Page Agent, and provider threat model.

## Deployment and configuration

Configuration defaults to `~/.agent-whiteboard/config.yaml`. Settings resolve in this order where supported:

1. Explicit flags
2. Non-empty `AGENT_WHITEBOARD_*` environment variables
3. YAML
4. Built-in defaults

The YAML format is versioned and strict. See [Configuration](docs/configuration.md) for the complete client, server, viewer, and agent schema, including validation and file-safety rules.

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

## APIs and integrations

Agent Whiteboard exposes several supported surfaces:

- **CLI:** human-readable output and a stable [versioned JSON format](docs/cli-json.md)
- **HTTP API:** publishing, retrieval, mutation, deletion, and health endpoints under [`/api/v1`](docs/http-api.md)
- **Go API:** embeddable server construction through [`pkg/agentwb`](docs/go-api.md)
- **Agent skills:** publishing guidance under [`skills/agent-whiteboard`](skills/agent-whiteboard/SKILL.md) and setup guidance under [`skills/agent-whiteboard-setup`](skills/agent-whiteboard-setup/SKILL.md)
- **Filesystem storage:** documented layout and durability contracts in [Storage](docs/storage.md)

## Documentation

### Use Agent Whiteboard

- [Agent setup skill](skills/agent-whiteboard-setup/SKILL.md)
- [Agent publishing skill](skills/agent-whiteboard/SKILL.md)
- [Configuration](docs/configuration.md)
- [Security](docs/security.md)
- [CLI JSON](docs/cli-json.md)
- [Markdown and Mermaid example](docs/examples/diagram.md)
- [Standalone HTML example](docs/examples/standalone.html)

### Integrate Agent Whiteboard

- [HTTP API](docs/http-api.md)
- [Go API and dependency injection](docs/go-api.md)
- [Filesystem storage](docs/storage.md)

### Test provider integrations

- [Optional hosted-provider smoke test](docs/hosted-provider-smoke.md)

## Development

Build and test the Go application:

```sh
go build -trimpath -o ./bin/agent-whiteboard ./cmd/agent-whiteboard
go test ./...
go test -race ./...
go vet ./...
```

Browser asset development uses Node 24 and pnpm 11.4:

```sh
pnpm install --frozen-lockfile
pnpm test
pnpm run check:assets
pnpm run test:browser
```

See [Releasing Agent Whiteboard](docs/releasing.md) for the verified release checklist and annotated-tag helper.

## License

Agent Whiteboard is available under the terms in [LICENSE](LICENSE).
