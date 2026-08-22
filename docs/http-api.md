# HTTP API

The API is versioned under `/api/v1`. Successful mutation responses contain paths, never server-generated absolute URLs. Build public URLs using the origin used for the request.

## Routes

| Route | Body and result |
| --- | --- |
| `GET /healthz` | `200 {"status":"ok"}`; `Cache-Control: no-store` |
| `GET /readyz` | `200 {"status":"ready"}` or `503 {"status":"unavailable"}` |
| `POST /api/v1/whiteboards/markdown` | multipart `file` and `context`, optional `expires_in_seconds`; `201` resource |
| `GET /api/v1/whiteboards/markdown/{id}` | `200` resource plus exact `markdown` and `context` strings |
| `PUT /api/v1/whiteboards/markdown/{id}` | same paired multipart fields; `200` resource |
| `DELETE /api/v1/whiteboards/markdown/{id}` | `204` |
| `POST /api/v1/whiteboards/html` | multipart `file` and `context`, optional `expires_in_seconds`; `201` resource |
| `GET /api/v1/whiteboards/html/{id}` | `200` resource plus exact `html` and `context` strings |
| `PUT /api/v1/whiteboards/html/{id}` | same paired multipart fields; `200` resource |
| `DELETE /api/v1/whiteboards/html/{id}` | `204` |
| `GET /whiteboards/markdown/{id}` | browser-rendering HTML shell for Markdown |
| `GET /whiteboards/html/{id}` | application-controlled sandbox wrapper for trusted HTML |
| `GET /whiteboards/html/{id}/content` | exact trusted HTML bytes with an independent opaque-origin CSP sandbox |
| `GET /whiteboards/html/{id}/rendered` | Page-Agent child HTML with the bundled bridge injected after the source `<head>` start; legacy unsafe source falls back to exact bytes |
| `POST /api/v1/images` | one or more multipart `images`, optional `expires_in_seconds`; `201` images |
| `PUT /api/v1/images/{id}` | exactly one multipart `file`, optional `expires_in_seconds`; `200` resource |
| `DELETE /api/v1/images/{id}` | `204` |
| `GET /images/{id}` | validated raster bytes with detected media type |

`HEAD` is supported by Go's GET routing for public and API retrieval routes. Health endpoints require `GET`. Unsupported methods return `405` with `Allow`.

The stable HTML capability URL is the supported browser entry point. It returns one `credentialless` iframe with exactly `sandbox="allow-scripts"` and `referrerpolicy="no-referrer"`. With Page Agent disabled, the script-free full-viewport wrapper points to the exact relative `/content` route. With Page Agent enabled, the application-owned outer response adds the bundled theme and complete shared Page Agent UI and points the child to `/rendered`. That route inserts one bundled bridge script immediately after the exact source `<head>` start; it does not alter `/content`. Historical source that cannot be injected safely because publisher content precedes `<head>` is returned atomically and exactly by `/rendered`, without a bridge, so the parent chooser remains the only component interaction. New create/update requests reject that source shape.

Source and creator notes are JSON-escaped in a non-executable payload for the trusted parent and are not exposed to child scripts. Both `/content` and `/rendered` receive the same opaque-origin CSP sandbox headers, which block origin storage, parent access, connections, forms, popups, downloads, child frames, top navigation, and remote subresources. `/content` always preserves exact stored bytes. Malformed, missing, expired, and wrong-kind requests to either child route retain the same security headers while returning `404`; no `/raw`, `/source`, or other nested route serves submitted bytes.

A script-capable sandbox may navigate its own child. The wrapper's outer `frame-src 'self'` currently limits that navigation to the publishing origin, and the iframe's credentialless/no-referrer attributes strip publishing-origin cookies and Referer. Trusted HTML can nevertheless encode its capability URL into such a permitted request. Direct top-level `/content` navigation remains independently opaque and network/storage restricted, but bypasses the wrapper's destination restriction and is not the supported entry point.

## Paired whiteboard multipart

Markdown and HTML create and update require exactly one file part named `file` and exactly one file part named `context`. Both must be non-empty UTF-8. The context is Markdown summarizing creator goals, decisions, assumptions, and open questions. Source and context are validated and replaced together; there is no API to preserve or update only one half.

Only the documented file fields and at most one scalar `expires_in_seconds` field are accepted. `expires_in_seconds` is a signed decimal at the transport layer; valid service values are nonnegative. Omit it to use the create default or preserve update expiration. Zero means permanent.

The source and context have independent limits: 10 MiB and 1 MiB by default. Each whiteboard multipart request is bounded to the configured source limit plus context limit plus 64 KiB of multipart overhead. The context limit is independently configurable through YAML `server.max_context_bytes`, environment `AGENT_WHITEBOARD_MAX_CONTEXT_BYTES`, and `serve --max-context-bytes`, with flags taking precedence over non-empty environment, YAML, and the built-in. A configured zero selects the 1 MiB built-in; it does not disable the limit.

HTML retains its standalone-document structural and external-script/stylesheet validation and must start `<head>` before publisher content. Create and update also validate explicit component declarations. `data-agent-select` accepts only `section`, `image`, `chart`, `table`, `code`, `quote`, `component`, or the exclusion value `none`; `data-agent-section` is section shorthand and `data-agent-section-ignore` excludes a subtree. Conflicting attributes, unsupported/empty values, non-unique or invalid IDs, excluded explicit components, and explicit components without a source-resolvable accessible label return `invalid_request`. Explicit IDs and labels are non-empty, control-safe, and at most 256 UTF-8 bytes. `aria-labelledby` is resolved first, then `aria-label`, then the kind-specific semantic fallback. These publication errors apply only to explicit declarations; an automatic semantic candidate that does not qualify is omitted.

Images default to 25 MiB each and 100 MiB for the complete image request. Image type is detected from content and verified with format-specific configuration parsing, not trusted from the filename: PNG, JPEG, GIF, and WebP are accepted; SVG and malformed files return `unsupported_media_type`.

## Schemas

A whiteboard or single-image mutation returns:

```json
{"resource":{"id":"CAPABILITY_ID","type":"markdown","path":"/whiteboards/markdown/CAPABILITY_ID","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","expires_at":1767229200,"permanent":false}}
```

Image resources use `filename`, `extension`, and `media_type` instead of `type`. Multi-image create returns `{"images":[...]}` in upload order. `expires_at` is a nullable Unix-seconds integer; when it is `null`, `permanent` is `true`.

Markdown retrieval returns the same resource metadata plus the exact stored pair:

```json
{"resource":{"id":"CAPABILITY_ID","type":"markdown","path":"/whiteboards/markdown/CAPABILITY_ID","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","expires_at":1767229200,"permanent":false},"markdown":"# Board\n","context":"# Creator context\n"}
```

HTML retrieval is symmetric and uses exact `html` and `context` fields:

```json
{"resource":{"id":"CAPABILITY_ID","type":"html","path":"/whiteboards/html/CAPABILITY_ID","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","expires_at":null,"permanent":true},"html":"<!doctype html><html><head></head><body></body></html>","context":"# Creator context\n"}
```

A legacy schema-1 Markdown resource remains retrievable and returns `"context":""`. Its first `PUT` must provide both a non-empty `file` and `context`, after which retrieval returns the pair.

Errors are stable JSON:

```json
{"error":{"code":"invalid_request","message":"invalid multipart form"}}
```

| Code | Status |
| --- | ---: |
| `invalid_request` | 400 |
| `not_found` | 404 |
| `content_too_large` | 413 |
| `unsupported_media_type` | 415 |
| `storage_unavailable` | 503 |
| `internal_error` | 500 |

Unknown/internal causes are sanitized. Error bodies do not echo source, creator context, request bodies, filesystem paths, or internal causes.

If a whiteboard create fails after the server can no longer prove rollback, the error response also includes the generated `resource` next to `error`. Treat that capability as possibly live and use retrieval or deletion to resolve it. Ordinary failed creates omit `resource`.

Public GET responses set `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, and `X-Robots-Tag: noindex, nofollow, noarchive`; images append `noimageindex` and set `Content-Disposition: inline` with filename `<id><detected-extension>`. Markdown HTML also contains the corresponding robots meta tag.

Creator context is available in the opt-in Page Agent disclosure and from the machine retrieval route to anyone with the capability ID. It is not a private, hidden, or authorization-protected channel. Never include hidden reasoning, credentials, personal or sensitive data, private source, or raw tool output.

HTML uses the same Page Agent conversations, providers, ordinary image attachments, skills, compaction, queue/busy behavior, Stop, archives, interactions, and failure handling as Markdown. The parent indexes at most 128 eligible HTML components from inert exact source. Automatic kinds are labeled section/article/region, meaningful image/figure, labeled SVG/canvas chart, table, `pre > code`, and blockquote/note/alert; custom components are explicit-only. Each candidate has one unique stable source ID, a label, a source-order ordinal, and an exact-source excerpt of at most 48 KiB.

A component reference is a strict `kind:"component"` message part with a bounded label; the exact current HTML resource identity, update time, and context digest; an HTML anchor containing `element_id` (at most 256 bytes), lowercase tag (at most 32 bytes), and ordinal 1–128; and a component object containing one allowed type and a non-empty source excerpt of at most 48 KiB. The ordinary message limits still apply: at most 64 parts, 16 references, and 64 KiB of semantic content. Multiple tokens remain in composer order and navigation is revision-bound. A component reference may carry one optional private visual only for an `image` component whose exact source contains one unambiguous embedded PNG/JPEG/GIF/WebP image; model image capability and ordinary private-image limits remain authoritative. SVG/canvas, remote/blob/runtime pixels, video/audio, live state, and other component kinds remain semantic references.

## Private loopback Page Agent images

`agent serve` exposes a separate browser-only endpoint at `POST /api/v1/agent/images` and `GET|DELETE /api/v1/agent/images/{image_id}`. This is not part of the public publishing API and must not be used to create shareable image URLs. Requests require local API version `5`, the exact authorized Origin, attached client and conversation IDs, and—on upload—the selected provider plus `X-Agent-Whiteboard-Image-Purpose: attachment|inline_reference`. Upload bodies are one raw image; successful JSON returns only an opaque image ID, detected media type, and byte length. Submit commands later claim ordinary attachments and ordered inline image references atomically without allowing their purposes to be exchanged.

The browser protocol uses WebSocket subprotocol `agent-whiteboard.v5`. Connect, submit, and New commands always carry a nullable `settings` field. Settings-capable Pi and Codex sessions use the complete semantic tuple `{model, effort, speed}`. Pi fixes the semantic speed to `"standard"` and does not render a Speed control; Codex renders Speed when its catalog advertises the choice. Snapshots and settings events carry a strict visible model catalog plus either verified effective settings or an explicit unverified state. Snapshots and lifecycle events use nullable `active_work` with `turn|compact` kind and `running|stopping` state. Every strict snapshot also carries provider-neutral `skills_state`, a bounded safe `skills` catalog, nullable `max_selected_skills`, `supports_compact`, `busy_policy`, and broker-derived `composer_admission`. Pi advertises at most one selected skill and queue admission; Codex advertises its own skill limit and preserve-draft busy behavior. Queue items carry their immutable captured settings, and an accepted-turn settings event is the only browser-preference update authority.

Submit, queue, history, and user-message payloads carry `content.parts`: ordered text parts interleaved with revision-bound references and safe skill invocations. Native skill paths, bodies, errors, dependencies, icons, and URLs never cross this boundary. `compact` commands carry only an opaque `work_id`; generalized interrupt commands target that advertised work ID. Skill-catalog and compaction-status events are replayable only for the existing broker actor lifetime. Image commands and events contain only bounded opaque references/descriptors—never bytes or private filesystem paths. New provider submissions use native envelope v4, including ordered HTML component references. Historical native v1, v2, and v3 envelopes remain parseable and are normalized, so provider-native histories are preserved. Browser/local protocol v5 is intentionally incompatible with older viewers and brokers: mixed versions fail closed rather than adapting. Deploy viewer and broker together, clear only disposable local broker conversation/workspace state, and restart `agent serve` before reconnecting. Public whiteboards and provider-native Pi/Codex histories are outside that reset.

## curl

Create a temporary context file before publishing Markdown or HTML:

```sh
context_dir="$(mktemp -d)"
trap 'rm -rf "$context_dir"' EXIT
context_file="$context_dir/context.md"
cat >"$context_file" <<'EOF'
# Creator context

- Goal: demonstrate the HTTP pair contract.
- Open questions: none.
EOF

curl -fsS -F file=@docs/examples/diagram.md -F context=@"$context_file" -F expires_in_seconds=3600 http://127.0.0.1:8567/api/v1/whiteboards/markdown
curl -fsS http://127.0.0.1:8567/api/v1/whiteboards/markdown/CAPABILITY_ID
curl -fsS -F file=@docs/examples/standalone.html -F context=@"$context_file" -F expires_in_seconds=3600 http://127.0.0.1:8567/api/v1/whiteboards/html
curl -fsS http://127.0.0.1:8567/api/v1/whiteboards/html/CAPABILITY_ID
curl -fsS -F images=@chart.png -F images=@photo.webp -F expires_in_seconds=0 http://127.0.0.1:8567/api/v1/images
curl -fsS -X DELETE http://127.0.0.1:8567/api/v1/images/CAPABILITY_ID
```
