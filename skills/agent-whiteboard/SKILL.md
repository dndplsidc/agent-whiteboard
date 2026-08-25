---
name: agent-whiteboard
description: Publish, update, retrieve, or delete Markdown, Mermaid, trusted HTML, or raster images with Agent Whiteboard. Use for capability URLs; use agent-whiteboard-setup for installation, servers, Page Agent, trust, or daemons.
---

# Agent Whiteboard

Use the CLI to publish and manage Agent Whiteboard resources. Return the resulting public URL and expiration.

## Workflow

1. Run `command -v agent-whiteboard`. If it is missing or no publishing server is available, use the `agent-whiteboard-setup` skill.
2. Choose the resource type below.
3. Remove secrets, credentials, private source, personal data, and other sensitive content. Capability URLs are public bearer access, not private links.
4. For Markdown or HTML, create a fresh non-empty creator-context file.
5. Run the CLI with `--json`; put global flags before the command.
6. Verify the result proportionally.
7. Return the URL, expiration, and verification performed.

## Resolve the publishing server

Do not assume localhost. Server selection follows this order:

1. explicit `--server URL`;
2. non-empty `AGENT_WHITEBOARD_SERVER`;
3. `client.server` in the selected YAML configuration;
4. built-in `http://127.0.0.1:8567`.

When no host is supplied and the configured/default target is acceptable, run the CLI without `--server` and let it apply this precedence.
Treat the absolute URL in the JSON result as authoritative.

When the host is explicitly unknown or a remote deployment is expected, inspect the environment and selected or default configuration before creating anything.
If neither identifies a server, ask for the publishing origin instead of falling back to localhost.
Do not guess another host or start a local server automatically.

## Choose the resource

| Need | Resource |
| --- | --- |
| Prose, code, tables, reports, Mermaid | Markdown |
| Trusted interactive or highly custom presentation | HTML |
| PNG, JPEG, GIF, or WebP visual | Image |

Prefer Markdown unless the result needs active HTML. Never upload SVG.

For Markdown with local images, upload the images first and replace local paths with their returned absolute URLs. Read [Markdown publishing](references/markdown.md).

## Creator context

Every Markdown or HTML create and update requires a separate UTF-8 Markdown context file.
Include only information useful to a reader or Page Agent: the goal, relevant decisions, assumptions, and open questions.
Omit empty categories and unrelated process notes.

```sh
context_dir="$(mktemp -d)"
context_file="$context_dir/context.md"
cat >"$context_file" <<'EOF'
# Creator context

Goal: Explain the purpose of this page in one or two sentences.
Assumptions: List only assumptions needed to interpret it.
EOF
```

Context is retrievable by anyone holding the capability URL.
Do not include hidden reasoning, raw tool output, credentials, private source, or sensitive information.
Remove the temporary directory after the command completes.

## Publish

Use JSON output so the URL, ID, and expiration are unambiguous:

```sh
agent-whiteboard --json create markdown --context "$context_file" FILE.md
agent-whiteboard --json create html --context "$context_file" FILE.html
agent-whiteboard --json image upload FILE.png
```

Use `--server URL` before the command only to override configured server resolution. Add `--expires-in SECONDS` only when the user requests a specific lifetime; `0` means permanent.

Read [publishing commands](references/publish.md) for update, retrieve, delete, expiration, output, and failure recovery.

## Verify

When you authored or changed the content and browser automation is available:

- open the returned URL;
- confirm the intended content renders;
- check obvious layout, console, and network failures;
- exercise primary interactions in authored HTML.

For unchanged user-supplied content, a successful command and retrieval check are sufficient unless the user requests visual review. Do not turn ordinary publishing into Page Agent product acceptance testing.

If verification finds a content problem, update the source and creator context together, republish, and recheck.

## Return

Report:

- the absolute capability URL;
- expiration or `permanent`;
- the verification performed;
- any unresolved limitation.

Do not print the capability ID separately unless it is needed for a requested update, retrieval, or deletion.

## Read only when needed

| Task | Reference |
| --- | --- |
| Exact lifecycle commands and JSON | [Publishing commands](references/publish.md) |
| Markdown, local images, or supported syntax | [Markdown publishing](references/markdown.md) |
| Authoring or publishing standalone HTML | [HTML publishing](references/html.md) |
| Mermaid diagram | [Mermaid guidance](references/mermaid.md) |
| Publication safety question | [Security](references/security.md) |
| Install, configure, or run the product | Use the `agent-whiteboard-setup` skill |
