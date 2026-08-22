---
name: agent-whiteboard
description: Use when an agent needs to publish Markdown, Mermaid diagrams, trusted standalone HTML, or images as shareable agent-whiteboard URLs, or update, retrieve, and delete previously published resources.
---

# Agent Whiteboard

Publish through the CLI whenever shell execution is available. Use direct HTTP only when the CLI cannot run. Return the final public URLs to the user after completing the verification below.

## Choose the resource

- Choose Markdown for ordinary boards, prose, code, tables, and Mermaid diagrams. The browser renders and sanitizes Markdown.
- Choose standalone HTML only for trusted active documents. It runs as opaque-origin sandboxed active content without same-origin authority, not sanitized Markdown; trusted code can still disclose its capability through permitted child self-navigation.
- Choose images for PNG, JPEG, GIF, or WebP binary visuals. Never upload SVG.

Respect the configured limits; defaults are 10 MiB per whiteboard source, 1 MiB per creator context, 25 MiB per image, and 100 MiB for the complete image request.

When Markdown uses local images, publish every image first, capture each returned absolute URL, and insert those URLs into the Markdown before publishing it. The service does not bundle local dependencies.

## Create the whiteboard context artifact

Every Markdown or HTML create and update requires a separate, non-empty UTF-8 Markdown context file. Always create it in a temporary directory and pass it with `--context`; never omit it or reuse stale context accidentally.

```sh
context_dir="$(mktemp -d)"
trap 'rm -rf "$context_dir"' EXIT
context_file="$context_dir/context.md"
cat >"$context_file" <<'EOF'
# Creator context

- Goal: explain what the published board is intended to communicate.
- Decisions: record relevant presentation and scope choices.
- Assumptions: list facts a reader or local agent may rely on.
- Open questions: list unresolved items, or state that there are none.
EOF

agent-whiteboard create markdown --context "$context_file" board.md
```

Write concise goals, decisions, assumptions, and open questions. Context is machine-retrievable by anyone with the capability ID and is not a hidden or private channel. Never include hidden reasoning, credentials, tokens, personal or sensitive data, private source, unrelated information, or raw tool output. Update the context file whenever the whiteboard source changes, then replace both artifacts together:

```sh
agent-whiteboard update markdown --context "$context_file" -- CAPABILITY_ID board.md
```

## Author and verify rendered content

Before creating standalone HTML, check for installed UI/UX or frontend-design skills. When a relevant skill is available, invoke it and use its guidance when designing the content.

After publishing Markdown, Mermaid, or standalone HTML, check for available headless-browser or browser-automation skills and tools. When available, use them to open the returned URL and verify that the content is readable, correctly laid out, and functioning as intended.

Author selectable HTML with semantic, accessible markup and unique stable IDs. Labeled `section`, `article`, and region elements; meaningful images/figures; labeled SVG/canvas charts; tables; `pre > code`; and blockquote/note/alert content are discovered automatically. Ensure each intended component has an accessible label: prefer native captions/headings/alt text, otherwise use `aria-labelledby` or `aria-label`. Do not annotate every semantic component. Use `data-agent-select="section|image|chart|table|code|quote|component"` or `data-agent-section` only when the intended boundary or kind is ambiguous; custom `component` is explicit-only. Use `data-agent-select="none"` or `data-agent-section-ignore` deliberately to exclude a subtree. Invalid explicit values, conflicting overrides, duplicate/missing IDs, and missing labels reject create/update.

Never embed, copy, or call Agent Whiteboard bridge code in publisher HTML. Do not describe video, audio, runtime DOM, live canvas/SVG pixels, or live application state as selectable or captured. An eligible image component may optionally send one exact-source embedded PNG/JPEG/GIF/WebP visual to an image-capable model; all other component references are semantic source context.

When Page Agent is enabled, verify HTML's compact parent-owned **+ Add** hover action and its type/label accessible name, add a component, and verify the app-bar **Components** chooser by keyboard or touch/coarse-pointer emulation. The chooser must remain available when the bridge is missing after child self-navigation. Add multiple components with surrounding text and verify their order and revision-bound navigation. These controls belong to the wrapper, not publisher HTML.

If verification finds an issue, update the source, update the creator context, republish it, and verify again. Do not return the final URL until verification succeeds.

## Publish safely

1. Inspect source and creator context. Remove credentials, tokens, personal or sensitive data, private source, hidden reasoning, and raw tool output. Never publish any of them.
2. Select `--server`, `--timeout`, `--json`, `--config`, and `--expires-in` deliberately. Omit `--expires-in` for the server default; use `--expires-in 0` for a permanent resource. There is no `--permanent` flag.
3. For every Markdown or HTML whiteboard, create the temporary context artifact above and pass `--context "$context_file"` on every create or update.
4. Publish with an approved command:
   - `agent-whiteboard serve`
   - `agent-whiteboard create markdown --context "$context_file" FILE`
   - `agent-whiteboard create html --context "$context_file" FILE`
   - `agent-whiteboard update markdown --context "$context_file" -- ID FILE`
   - `agent-whiteboard update html --context "$context_file" -- ID FILE`
   - `agent-whiteboard --json get markdown -- ID`
   - `agent-whiteboard --json get html -- ID`
   - `agent-whiteboard delete markdown -- ID`
   - `agent-whiteboard delete html -- ID`
   - `agent-whiteboard image upload FILE...`
   - `agent-whiteboard image update -- ID FILE`
   - `agent-whiteboard image delete -- ID`
5. Read stdout or the JSON result and capture the public URL and capability ID. Complete any available rendering verification before returning the final URL.
6. Use the capability ID to retrieve, update, or delete. There is no separate edit token.

Public URLs are bearer capabilities: anyone with one can read the resource, derive the ID used for mutation, and retrieve both source and context. `noindex` limits discovery but is not authorization. Do not assume authentication, secrecy, or revocation beyond deletion.

A failed create can still print a generated resource before returning an error when persistence is uncertain. Preserve the ID, check or delete the possibly live resource, and do not publish or log the full ID unnecessarily.

The CLI supports trust-list configuration and foreground `agent serve` with independent Pi and Codex providers. Use `--pi-executable` or `AGENT_WHITEBOARD_PROVIDER_PI_EXECUTABLE` for Pi, and `--codex-executable` or `AGENT_WHITEBOARD_PROVIDER_CODEX_EXECUTABLE` for Codex; otherwise each executable resolves from `PATH`. Page Agent selection is silent, sends no content, and preserves separate provider conversations and archives. Codex receives the complete canonical context envelope as user-message content and shows bounded tool activity plus stable command, file, permission, and MCP elicitation requests. The first valid response across tabs wins. Experimental `request_user_input` is not active because `experimentalApi` remains disabled.

Page Agent readers can select multiple PNG, JPEG, GIF, or WebP images or paste images into the composer, preview them, remove or retry failed staging, and send an image-only turn. These private attachments travel only to the loopback broker and selected provider; they are not the public Whiteboard image resources created by `agent-whiteboard image upload`. Fixed limits are 8 images, 10 MiB each, 20 MiB per turn, and 512 MiB retained per conversation workspace. Provider capability is authoritative. Never instruct readers to publish a private Page Agent attachment merely to send it to Pi or Codex.

On Markdown, Page Agent readers can add rendered context directly into the message: select text for the contextual popup, use a heading action for its section, or use a rendered raster's action for a private inline image. HTML readers can hover an indexed component and use the parent-owned **+ Add** action or choose it from the app-bar **Components** list. These are ordered message parts rather than attachments, so surrounding prose and multiple references retain their positions. HTML source excerpts supplement the complete exact source/context envelope and remain untrusted reader content. The child supplies only untrusted candidate geometry; the parent owns the canonical inert-source index, activation, and submission.

Selected Markdown images remain loopback-private and accept only embedded or same-origin raster sources. One unambiguous exact-source embedded PNG/JPEG/GIF/WebP in an HTML image component may add an optional private visual when the connected model supports images; otherwise the reference remains semantic. Browser/local API v5 viewers require a v5 `agent serve` broker using `agent-whiteboard.v5`; new provider turns use native envelope v4, historical native v1-v3 histories remain supported, and mixed browser/broker versions fail closed.

The shared composer pill selects live-catalog models and advertised reasoning efforts for Pi and Codex; Speed appears only for capable Codex models and is omitted for Pi. Typing `$` selects enabled native skills as atomic tokens (one per Pi message, the advertised multiple-skill limit for Codex), and exact `/compact` starts native manual compaction. No skill path or body reaches the browser. Pi advertises queue admission while Codex preserves an editable busy draft without submitting it. Each accepted turn retains its exact tuple; Pi applies changes to the current Page Agent session and Pi's future default, not already-running sessions. Provider-scoped same-origin preference changes only after native acceptance, and stale catalogs preserve the draft.

On macOS, the CLI also supports `agent serve --daemon` (install/update and start), plus `agent daemon status`, `restart`, `stop`, and `uninstall`; `stop` retains the LaunchAgent plist and `uninstall` removes it. Linux does not support managed daemon operations; use foreground `agent serve`. The daemon records absolute agent/configuration paths and resolved Pi/Codex executable paths, while the child reads Agent Whiteboard configuration at startup. It never copies provider configuration or credentials. `agent serve --daemon` accepts `--pi-executable` and `--codex-executable` among the foreground settings.

Pi and Codex use their effective default user provider home, authentication, tools, extensions, MCP, apps, hooks, skills, approval policy, sandbox, project trust, and other native configuration unchanged; Agent Whiteboard never edits provider configuration files or creates a production `CODEX_HOME`. Model and effort, plus Codex Speed when advertised, may be applied only through the bounded native controls described above. Tool allowlists, content-only execution, per-whiteboard filesystem roots, and a stronger cross-agent sandbox are deferred, so treat whiteboard content as untrusted model input and do not claim a content-only execution boundary. Never invent Agent Whiteboard authentication commands or treat daemon management as provider authentication.

Read [CLI commands](references/cli.md) for exact syntax and output, [Mermaid guidance](references/mermaid.md) when authoring diagrams, and [security guidance](references/security.md) before publishing HTML or non-public material.
