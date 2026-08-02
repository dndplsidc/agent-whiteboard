# Security model

Resource IDs are bearer capabilities and are embedded in public URLs. Anyone who learns a URL therefore has the ID needed to read, update, or delete that resource; there is no separate edit token. IDs must not appear in logs in full.

Resources are public but non-indexed. `X-Robots-Tag` and Markdown robots meta ask cooperative crawlers not to index or archive content. This is not authentication, authorization, revocation distribution, or a confidentiality boundary. Never publish credentials, API tokens, cookies, private source, personal data, regulated data, or other sensitive material.

## Markdown and creator context

Every new or updated Markdown resource contains two artifacts: rendered source and creator context. The context is intended to summarize goals, decisions, assumptions, and open questions. It must not contain hidden reasoning, credentials, unrelated personal data, sensitive data, private source, or raw tool output.

The main browser document renders only the Markdown source. When the local-agent viewer is enabled, its Page context disclosure also lets the reader inspect the exact Markdown and creator context before sending a message. `GET /api/v1/whiteboards/markdown/{id}` and `agent-whiteboard --json get markdown ID` likewise return both exact strings. Anyone holding the public URL can recover its ID and use that retrieval route. Context therefore has exactly the same bearer-capability exposure and expiration/deletion lifecycle as source; it is not private merely because it is absent from the main rendered document.

Markdown is rendered client-side by bundled JavaScript. Raw Markdown HTML is disabled, links and generated SVG are sanitized with DOMPurify, Mermaid uses strict security settings, and no CDN is needed. The JSON source envelope escapes script-closing sequences. These controls reduce injection risk but do not make publication of secrets safe.

## Standalone HTML and images

Standalone HTML remains trusted active content, but its public capability URL now returns an application-controlled wrapper rather than submitted bytes. The wrapper contains exactly one `credentialless` iframe with `sandbox="allow-scripts"` and `referrerpolicy="no-referrer"`. Exact submitted bytes are available only at the capability's `/content` route. That response independently applies a CSP sandbox without `allow-same-origin`, plus `connect-src 'none'`, `frame-src 'none'`, `form-action 'none'`, and restrictive resource directives. Inline scripts can run and use `postMessage`, but execute with an opaque origin: they cannot read parent content, publishing-origin cookies or storage, load child frames or remote subresources, submit forms, open popups, initiate downloads, navigate the top page, or make fetch, XHR, WebSocket, or authorized loopback-broker connections.

Sandboxing does not generically prevent a script-capable child from navigating itself. Under the supported wrapper, the outer `frame-src 'self'` policy limits that navigation to the publishing origin, while `credentialless` and `no-referrer` prevent publishing-origin cookies and the capability Referer from accompanying it. Submitted code can still deliberately encode its `/content` capability URL into a permitted same-origin navigation, exposing it to publishing-origin request handlers or logs. Cross-origin child navigation is additionally blocked by the wrapper's current CSP, but direct top-level navigation to `/content` is not the supported entry point and does not gain the wrapper's destination restriction. Treat self-navigation capability disclosure as an accepted risk and publish only HTML you trust.

Image uploads accept PNG, JPEG, GIF, and WebP only after signature detection and format-specific configuration validation. SVG is rejected because it can contain active content. Responses use `nosniff`, a detected media type, inline filename, no-store caching, and `noimageindex`.

## Local browser agent

The optional Markdown drawer talks only to the configured decimal port on literal `127.0.0.1`; it never scans hosts or ports. Before explicit Connect consent, it may issue only the bounded status request. Connect carries resource metadata and the context digest but no Markdown or creator context. The first contextual message sends both complete artifacts together; later messages omit them until an observed resource revision requires a complete replacement. A disconnect or uncertain delivery never automatically resubmits a model turn.

The browser accepts only the versioned, strictly validated broker event schema. Provider payloads, native IDs, paths, credentials, hidden reasoning, and raw tool output are not browser events. Assistant Markdown is sanitized and automatic-fetch media is removed. A generic responding indicator exposes lifecycle state, not model reasoning. Only broker-normalized `visible_summary` activity may appear as a collapsed work summary. Blocked tool and permission activity states only that content-only mode prevented execution; it never exposes attempted tool names, arguments, payloads, or output.

Browser storage is limited to theme, pane-open state, the decimal loopback port, and a canonical integer pane width from `360` through `720`. The effective width may be temporarily clamped for the viewport without replacing the saved preference. Capabilities, resource metadata, messages, context, conversation IDs, provider output, credentials, and hidden reasoning are never browser preferences.

Chrome Local Network Access is an additional browser permission, not broker authorization. The broker binds only to IPv4 loopback, requires an exact Host and canonical Origin, reloads configured HTTPS trust for admission, omits credentials and referrers from browser requests, and applies exact-origin CORS. Standalone HTML has an opaque origin and cannot obtain this authority.

Canonical HTTP origins on literal `127.0.0.1`, with no port or a valid non-default port, are automatically admitted without a configured trust entry. `localhost`, other IPv4 loopback addresses or spellings, IPv6 loopback, explicit default port 80, and remote HTTP origins remain rejected. This is an intentional local-development trust boundary: any browser page served by any local process from literal `127.0.0.1` can call the broker directly and is not forced to use the viewer's Connect control. Run and open only trusted local servers and content. Configured non-loopback trust remains exact-origin HTTPS only.

## Configuration and origin allowlist

Trusted origins must be exact HTTPS origins. The CLI rejects paths, queries, fragments, user information, wildcards, and HTTP. Exact origin configuration does not add authentication to whiteboard resources.

On macOS, the managed daemon records only the absolute agent executable, selected/default configuration path, and—when Pi resolves during installation—the absolute Pi executable path in the per-user LaunchAgent. It does not record ambient environment variables, provider credentials, or raw launchctl output. `agent serve --daemon` installs/updates and starts the service; `stop` unloads it but retains the plist, and `uninstall` removes it. Linux rejects managed daemon operations with foreground guidance. Authenticate with Pi's provider-native login; agent-whiteboard never captures or persists provider credentials.

Configuration files must be regular non-symlinks and must not be writable by group or others. On macOS and Linux, trust operations additionally require the immediate parent to be a non-symlink directory that is not writable by group or others; edits are locked, written to `0600` temporary files, synced, and atomically renamed. See [configuration](configuration.md) for the complete permission and platform contract.

## Non-leakage and filesystem boundary

The service validates multipart fields, independent exact size limits, capability IDs, filesystem containment, regular files, and symlink safety. Logs avoid request bodies, Markdown, creator context, and full capability IDs. Operational logs and metrics should keep the same rule. Stable HTTP/CLI errors sanitize internal causes and do not echo source, context, raw multipart data, or filesystem paths.

A failed create can exceptionally return a resource capability alongside a sanitized error when rollback cannot prove absence. Preserve and explicitly check/delete that ID rather than logging it broadly. Ordinary failed creates expose no generated capability.

Use TLS and appropriate network access controls when serving beyond localhost. TLS protects capabilities in transit but does not turn a shared capability URL into identity-based authorization.
