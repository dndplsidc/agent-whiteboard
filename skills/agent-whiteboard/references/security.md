# Security model

Treat each ID and public URL as a bearer capability. Anyone who has the URL can read the resource, recover the ID used to update or delete it, and retrieve paired Markdown source and creator context. There is no login, authorization layer, private mode, or separate edit token.

Public responses ask crawlers not to index them, but `noindex` is not access control. Links can leak through chat, logs, browser history, screenshots, referrers, and forwarding. Delete a resource to revoke its URL; expiration limits its lifetime but does not make it private while live.

Never publish credentials, tokens, secrets, personal or sensitive data, or private source. Avoid putting full capability IDs into logs or unrelated documents.

Markdown context is not hidden reasoning or a private agent channel. It is stored beside source, shares the same capability and lifecycle, and is returned by `agent-whiteboard --json get markdown ID` and the machine HTTP API. Keep it to concise goals, decisions, assumptions, and open questions. Exclude hidden reasoning, raw tool output, unrelated personal information, credentials, and all sensitive material.

Markdown is rendered in the browser and sanitized. Standalone HTML is different: it is trusted, same-origin active content and may execute inline JavaScript. Publish HTML only when its entire source is trusted. Do not use the origin for authentication cookies or sensitive application state. Never upload SVG; use PNG, JPEG, GIF, or WebP.

The CLI and server sanitize stable errors and do not echo source, context, request bodies, internal causes, or filesystem paths. A create error may still return a capability if rollback is uncertain; retain it privately and check or delete the possibly live resource.

Trusted-origin configuration accepts exact HTTPS origins only. The foreground local broker reloads this allowlist for each new admission; existing accepted connections keep their original trust decision. Trust commands do not make whiteboard content private or authenticated.

Managed daemon lifecycle is macOS-only. `agent serve --daemon` records absolute agent/configuration paths, the Pi path when resolved during installation, and no ambient environment or provider credentials; `stop` retains the plist while `uninstall` removes it. Linux requires foreground `agent serve`. Authenticate through Pi's provider-native login; agent-whiteboard does not collect, store, or expose provider credentials.

The Pi adapter sends the exact reader message and approved Markdown context envelope to the model provider. It disables tools, extensions, skills, templates, project context files, themes, approval paths, retries, and online discovery. It uses Pi's provider-native login file and deliberately excludes ambient API-key and auth-token variables. Provider-native session files remain under the owner-only agent state directory; broker state does not copy transcripts or model output.
