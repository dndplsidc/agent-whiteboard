# agent-whiteboard

`agent-whiteboard` is a small Go server and CLI for publishing Markdown or trusted standalone HTML with creator context, plus raster images, at capability URLs. It is designed for agents that need to return a viewable result without opening a browser or depending on a CDN.

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

Markdown and HTML create and update require a second, non-empty UTF-8 Markdown file containing creator context. Create it in a temporary directory and remove it when finished:

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
agent-whiteboard create html --context "$context_file" --expires-in 3600 docs/examples/standalone.html
```

Creator context should summarize goals, decisions, assumptions, and open questions. Do not put hidden reasoning, credentials, sensitive data, unrelated personal data, private source, or raw tool output in it. The context shares the board's bearer capability and lifecycle and is returned by machine retrieval.

Markdown is rendered in the browser by bundled markdown-it, DOMPurify, highlight.js, and Mermaid assets. Add diagrams with ordinary fenced `mermaid` blocks. Standalone HTML remains trusted active content: its stable public URL returns an application-owned wrapper around an opaque-origin sandbox, while exact stored bytes are served only to that wrapper at `/content`. When Page Agent is enabled, the wrapper adds trusted theme and agent chrome above the child without injecting styles, data, or authority into submitted HTML. Trusted scripts may still disclose the capability through permitted child self-navigation, so never use standalone HTML for untrusted code.

Images are validated from their bytes. PNG, JPEG, GIF, and WebP are supported; SVG is rejected. Publish images before publishing Markdown that references their returned URLs.

```sh
agent-whiteboard image upload --expires-in 3600 chart.png photo.webp
```

Use the returned capability ID to replace, retrieve, or delete a resource. Markdown and HTML updates replace source and context together; neither half can be updated independently.

```sh
agent-whiteboard update markdown --context "$context_file" --expires-in 7200 -- CAPABILITY_ID docs/examples/diagram.md
agent-whiteboard --json get markdown -- CAPABILITY_ID
agent-whiteboard update html --context "$context_file" --expires-in 7200 -- CAPABILITY_ID docs/examples/standalone.html
agent-whiteboard --json get html -- CAPABILITY_ID
agent-whiteboard delete markdown -- CAPABILITY_ID
agent-whiteboard delete html -- CAPABILITY_ID
agent-whiteboard image update --expires-in 7200 -- CAPABILITY_ID chart.png
agent-whiteboard image delete -- CAPABILITY_ID
```

`get markdown` and `get html` require `--json` and return the exact source and creator context. For a remote server, put global flags before the command (or set `AGENT_WHITEBOARD_SERVER`):

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
# Authenticate with Pi and/or Codex through the provider's own CLI first.
pi
agent-whiteboard agent serve
```

`agent serve` resolves `pi` and `codex` independently from `PATH`. Use `--pi-executable PATH` or `AGENT_WHITEBOARD_PROVIDER_PI_EXECUTABLE` for Pi, and `--codex-executable PATH` or `AGENT_WHITEBOARD_PROVIDER_CODEX_EXECUTABLE` for Codex. If either executable is unavailable, the other provider and the local broker still work.

Pi and Codex use the effective default user provider configuration. Pi runs in RPC mode with the normal Pi home, authentication, model, tools, extensions, skills, project resources, retry behavior, and other native settings. Codex runs through one lazily started `codex app-server` process shared by the loaded Codex conversations and inherits the normal Codex home, authentication, model, tools, MCP servers, apps, hooks, skills, approval policy, sandbox, and other configuration unchanged. Agent Whiteboard never edits provider configuration files or creates a production `CODEX_HOME`. Page Agent builds shared controls from each live native capability catalog: Pi advertises Model and Effort, while Codex also advertises Speed when supported. It does not override approval, sandbox, tools, or other native settings. The broker binds only to literal IPv4 loopback and admits exact configured HTTPS origins.

When `viewer.local_agent.enabled` is true, Markdown and HTML viewers show the same **Page agent** control with a Pi/Codex selector. HTML keeps that control in the trusted outer app bar and leaves submitted HTML in its opaque child. Selection is immediate and silent: it does not connect, send content, or interrupt the other provider. Pi and Codex keep independent conversations, lifecycle state, and provider-specific archives for the same whiteboard; Pi alone keeps a follow-up queue, and an inactive provider may continue responding. The drawer presents distinct unavailable and ready states and lets the reader review the complete page context before consent. Current Chrome checks the configured loopback port (default `8568`) and may require Local Network Access permission. Opening the pane, checking status, or changing providers sends no page content. The reader must connect the selected provider; the first contextual message then sends the complete exact Markdown or HTML source, creator context, title, URL, resource metadata, and reader message as one canonical envelope. Codex receives that envelope as user-message content rather than as replacement system instructions. Reconnects resume broker state but never resubmit an interrupted or uncertain turn.

The broker automatically admits canonical HTTP page origins on literal `127.0.0.1`, with no explicit port or a valid non-default port, so locally served proofs of concept do not need TLS or a trust-list entry. It does not extend this exception to `localhost`, other loopback addresses, IPv6, or remote HTTP origins. This deliberately trusts every browser page served from literal `127.0.0.1`: a local page can call the broker directly without using the viewer's Connect button, so open only local pages and processes you trust. Non-loopback viewers still require an exact configured HTTPS origin.

On desktop, Page agent is a docked right pane that reflows the whiteboard and can be resized from its left edge; its validated width preference is restored on reload and temporarily clamped for narrower windows. Below `64rem` it is a modal overlay, becoming full-width at `40rem`. Its compact chat workspace keeps the header and rounded composer fixed while only the transcript scrolls, and starts the transcript with a page-context summary. Enter sends text or ready images, Shift+Enter adds a newline, and the visible Send button remains available. The composer uses its own styled focus treatment instead of the browser's default textarea outline.

Rendered Markdown can be inserted directly into a message as positional context. HTML supports ordinary text, image attachments, skills, compaction, and all shared conversation controls, but exposes no page-reference actions or child-DOM bridge. Drag across text and use the **Add to message** popup, hover a heading and choose **Add section** (or **Add page** for the top-level heading), or hover a raster image and choose **Add image**. A section starts at its heading and ends immediately before the next heading at the same or a higher level. Every choice becomes an atomic inline token at the current composer caret, so readers can combine multiple references with ordinary text before, between, and after them. Sent and queued messages preserve that order. Selecting a token in transcript history navigates back to its source when the page revision still matches.

Use **Add images** to select one or several files, or paste images directly into the composer. PNG, JPEG, GIF, and WebP are accepted; SVG and other files are rejected. Each draft supports at most 8 images, 10 MiB per image, and 20 MiB total. Images show preparing, ready, failed/retry, and removable preview states; an image-only turn is valid once every retained image is ready. Sent messages, queued follow-ups, reconnects, and restored history keep their previews. If the selected provider model does not report image support, the image control is disabled with an explanation while text chat remains available.

Page Agent images—including images selected from rendered Markdown—go only to the loopback broker and the selected native provider. They are not uploaded to the publishing server or converted into public Whiteboard image capabilities. Rendered-image selection accepts embedded data images and same-origin PNG, JPEG, GIF, or WebP resources; cross-origin and redirected sources are rejected. Pi receives native base64 image content and Codex receives validated private local-image paths. Image availability follows the selected Codex draft model and native capability. The browser and `agent serve` must both support local API version 4 (`agent-whiteboard.v4`); after upgrading, restart a foreground broker or managed daemon before reconnecting an updated viewer.

During a Pi response, Stop and queued follow-up submission remain available together. During Codex work, the composer remains editable for the next draft, but Enter, Send, image addition, and skill selection do not submit or queue anything; the rightmost Send control becomes Stop and then `Stopping…`. While either provider has active work, Escape performs the same interruption as Stop instead of closing Page Agent. A three-dot responding indicator appears only after the broker reports the authoritative responding lifecycle, then real streamed deltas replace it progressively. Context and provider-specific archives are alternate pane views.

For either capable provider, typing `$` at a token boundary opens the enabled native skill catalog. Selected skills become non-editable tokens and are sent as native skill inputs; Pi accepts one skill per message and Codex accepts the advertised multiple-skill limit. The browser receives only opaque IDs, names, bounded descriptions, and scopes—never skill paths or bodies. Catalog drift marks selected tokens unavailable and preserves the draft until they are removed or reselected. Typing `/` offers only `/compact`; the exact command starts native manual compaction, shows `Compacting context…`, and can be stopped. Compaction activity and skill catalogs are memory-only and actor-lifetime.

The compact composer pill selects a live-catalog model and one of that model's advertised reasoning efforts. Codex also shows Standard/Fast Speed when supported; Pi omits Speed because it has no native service-tier choice. Every submit or queued Pi follow-up captures its exact tuple. Pi settings apply to the current Page Agent session and update Pi's future native default, without changing already-running Pi sessions. Codex admits no follow-up while busy and preserves the editable draft; Pi advertises queue admission. Defaults change only after native acceptance, and rejection or catalog drift preserves the draft for retry. Provider-scoped last-accepted tuples are stored only for genuinely new conversations on the same origin; unsent edits are tab-local and disappear on reload.

For Codex, bounded activity cards show commands, file changes, MCP calls, web searches, image views, collaboration, plans, compaction, and other supported work without exposing native App Server identifiers or hidden reasoning. Stable command, file-change, and permission approvals and MCP elicitation are shown as interactive cards. The first valid response across attached tabs wins; other tabs receive the resolved state. Agent Whiteboard does not invent an approval when the effective Codex policy does not request one. App Server's `request_user_input` remains experimental, so `experimentalApi` is disabled and structured `request_user_input` cards are not active in this stable slice.

Browser preferences are limited to theme, pane-open state, decimal loopback port, validated pane width, the selected provider (`pi` or `codex`), and each provider's last native-accepted model/effort/speed tuple for that publishing origin. Messages, context, capabilities, IDs, consent, conversation state, approval requests and decisions, provider output, credentials, and hidden reasoning are never stored there.

Whiteboard content is untrusted model input and the user's effective provider tools, approval policy, sandbox, project trust, and extensions remain authoritative. In particular, permissive native settings may allow tool actions without a Page Agent prompt. Tool allowlists, per-whiteboard filesystem roots, and a stronger cross-agent sandbox are deferred for this non-public deployment; do not treat Page Agent as a content-only execution boundary.

On macOS, install and start the managed per-user LaunchAgent with `agent-whiteboard agent serve --daemon`. It records the absolute agent and configuration paths plus the resolved Pi and Codex executable paths when available during installation; provider configuration and credentials are never copied. The child reads Agent Whiteboard configuration at startup and providers still use the effective default user configuration unchanged. Inspect the daemon with `agent-whiteboard agent daemon status`, reload it with `restart`, stop it with `stop` (the plist remains installed), or remove it with `uninstall`. Daemon operations are unsupported on Linux; run `agent-whiteboard agent serve` in the foreground instead. Authenticate with each provider itself; Agent Whiteboard never stores or accepts provider credentials.

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
| Creator context limit | 1 MiB |
| Image limit | 25 MiB each |
| Image request limit | 100 MiB |

Run `agent-whiteboard serve --help` for the complete server flag list.

## Security and detailed contracts

Capability URLs are public but marked non-indexable. Non-indexing is not access control: do not publish credentials, tokens, private source, personal data, or sensitive information in source or creator context. Anyone with a Markdown or HTML capability ID can retrieve both source and creator context through the machine API. See [security](docs/security.md).

- [configuration](docs/configuration.md)
- [HTTP API](docs/http-api.md)
- [Go API and dependency injection](docs/go-api.md)
- [filesystem storage](docs/storage.md)
- [versioned CLI JSON](docs/cli-json.md)
- [optional hosted-provider smoke test](docs/hosted-provider-smoke.md)
- examples: [Markdown/Mermaid](docs/examples/diagram.md) and [standalone HTML](docs/examples/standalone.html)

Asset development uses Node 24 and pnpm 11.4:

```sh
pnpm install --frozen-lockfile
pnpm test
pnpm run check:assets
pnpm run test:browser
```
