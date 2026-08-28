# Markdown publishing

Use Markdown for reports, prose, code, tables, diagrams, and other primarily textual pages.

## Supported content

Supported: headings, paragraphs, emphasis, strikethrough, links, lists, blockquotes, tables, inline code, fenced code, images, and fenced Mermaid diagrams.

Raw HTML is not a substitute for standalone HTML. Do not add external scripts or Mermaid CDN tags.

## Local images

The service does not bundle local Markdown dependencies. Upload each local raster image first:

```sh
agent-whiteboard --json image upload chart.png screenshot.webp
```

Replace local references with the returned absolute URLs before publishing the Markdown:

```markdown
![Recharge trend](https://whiteboard.example/images/CAPABILITY_ID)
```

Only PNG, JPEG, GIF, and WebP are supported. Never upload SVG. Use the same intended expiration for related images and Markdown so images do not expire before the page.

## Mermaid

Use an ordinary fenced `mermaid` block. Read [Mermaid guidance](mermaid.md) before publishing a diagram. When Page Agent is enabled, readers can use **Add diagram** to include the exact fenced Mermaid source in their message without sending rendered pixels.

## Verification

When browser tooling is available, confirm that headings, tables, code, images, and Mermaid diagrams render. Fix source errors rather than adding browser-side dependencies.
