# Publication safety

## Capability URLs

A resource URL is a bearer capability, not a private link.
Anyone holding it can view the resource, recover its ID, and use supported retrieval, update, or deletion operations.
`noindex` reduces discovery but is not access control.

Do not publish credentials, tokens, cookies, private source, personal data, regulated data, or anything that must remain confidential. Avoid copying full capability IDs into logs or unrelated documents.

## Creator context

Markdown and HTML context has the same capability exposure and lifecycle as its source.
Keep it concise and useful.
Never put hidden reasoning, raw tool output, credentials, sensitive data, or unrelated information in it.

## Content types

- Markdown is rendered and sanitized, but sanitization does not make secrets safe to publish.
- Standalone HTML is active content. Publish only HTML you trust and keep it self-contained.
- Images are limited to validated PNG, JPEG, GIF, and WebP. SVG is rejected.

## Page Agent

Published source, creator context, and reader messages are untrusted model input.
Page Agent uses the selected Pi, Codex, or Cursor provider's existing tools, rules, hooks, MCP servers, approval policy, sandbox, project trust, and configuration.
Untrusted content can activate those native capabilities under the provider's policy. Origin trust is not a sandbox, and Page Agent does not create a content-only execution boundary.

For deployment threat-model or implementation details, read the repository's `docs/security.md`; those details are not needed for ordinary publishing.
