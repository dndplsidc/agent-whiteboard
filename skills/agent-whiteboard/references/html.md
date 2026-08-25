# HTML publishing

Use standalone HTML only when Markdown cannot provide the required layout or interaction. Publish only source you trust.

## Authoring requirements

- Provide one complete, head-first HTML document.
- Keep it self-contained. Do not depend on external scripts, stylesheets, fonts, images, frames, or runtime network calls.
- Use semantic, accessible markup and unique stable IDs.
- Label meaningful sections, regions, figures, charts, tables, code blocks, and quotes with native headings, captions, alt text, `aria-labelledby`, or `aria-label`.
- Never embed, copy, or call Agent Whiteboard bridge code.

When authoring or materially redesigning the page, use one relevant frontend-design or UI skill if available. Do not load design guidance merely to upload unchanged HTML.

## Page Agent components

The viewer automatically discovers clearly labeled semantic sections, regions, images, figures, SVG/canvas charts, tables, `pre > code`, and quote/note/alert content.

Use explicit attributes only when the intended boundary is ambiguous:

```html
<section id="risk-summary" data-agent-select="section" aria-labelledby="risk-title">
  <h2 id="risk-title">Risk summary</h2>
</section>
```

Valid explicit kinds are `section`, `image`, `chart`, `table`, `code`, `quote`, and `component`.
Use `data-agent-select="none"` to exclude a subtree.
Invalid kinds, conflicts, duplicate or missing IDs, and missing labels reject create or update.

Page Agent references exact source semantics, not arbitrary live DOM state. Do not claim that runtime canvas pixels, video, audio, or dynamically mutated state will be captured.

## Verification

For HTML you authored or changed:

1. Open the published URL with available browser automation.
2. Check desktop and narrow layouts when responsiveness matters.
3. Check console errors, blocked resources, overflow, clipping, and focus visibility.
4. Exercise primary controls and verify their resulting state.
5. Confirm the Page Agent button is present only when the publishing server is configured to expose it.

Do not run exhaustive wrapper, broker, provider, or component-selection acceptance tests during an ordinary publish.
