import MarkdownIt from "markdown-it";
import { describe, expect, test, vi } from "vitest";
import { createMarkdownContextController, diagramReference, indexMarkdownTokens, referenceFromSelection, sectionReference } from "./markdown-context.js";

const identity = { resource: { id: "R".repeat(32), updated_at: "2026-08-10T00:00:00Z" }, digest: "a".repeat(64) };

function render(source) {
  const markdown = new MarkdownIt();
  const defaultFence = markdown.renderer.rules.fence.bind(markdown.renderer.rules);
  markdown.renderer.rules.fence = (tokens, tokenIndex, options, environment, renderer) => {
    const token = tokens[tokenIndex];
    if (token.info.trim().split(/\s+/u, 1)[0].toLowerCase() !== "mermaid") return defaultFence(tokens, tokenIndex, options, environment, renderer);
    return `<div class="mermaid-placeholder" data-index="${token.attrGet("data-agent-diagram")}" data-agent-block="${token.attrGet("data-agent-block")}" data-agent-diagram="${token.attrGet("data-agent-diagram")}"></div>`;
  };
  const tokens = markdown.parse(source, {});
  const index = indexMarkdownTokens(tokens, source);
  const container = document.createElement("main");
  container.innerHTML = markdown.renderer.render(tokens, markdown.options, {});
  document.body.replaceChildren(container);
  return { container, index };
}

describe("Markdown semantic index", () => {
  test("defines nested heading sections and leaves preamble without an action", () => {
    const source = "preamble\n\n# Page\nintro\n\n## Details\none\n\n### Nested\ntwo\n\n## Details\nthree";
    const { index } = render(source);
    const headings = [...index.headings.values()];
    expect(headings.map(({ title, level, ordinal }) => [title, level, ordinal])).toEqual([
      ["Page", 1, 1], ["Details", 2, 1], ["Nested", 3, 1], ["Details", 2, 2],
    ]);
    expect(headings[1].markdown).toContain("### Nested\ntwo");
    expect(headings[1].markdown).not.toContain("## Details\nthree");
    const reference = sectionReference(headings[0], identity, "S".repeat(32));
    expect(reference.section_lines).toEqual({ start: 3, end: 14 });
    expect(reference.source.anchor.markdown.heading_path[0].title).toBe("Page");
  });

  test("maps a native selection to revision-pinned block anchors and Unicode scalar offsets", () => {
    const { container, index } = render("# Page\n\nAlpha 😀 beta\n\nGamma");
    const paragraphs = container.querySelectorAll("p");
    const range = document.createRange();
    range.setStart(paragraphs[0].firstChild, 6);
    range.setEnd(paragraphs[1].firstChild, 5);
    const selection = document.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    const reference = referenceFromSelection({ doc: document, container, index, identity, id: "T".repeat(32) });
    expect(reference.quote).toContain("😀 beta");
    expect(reference.source.anchor.markdown.start.offset).toBe(6);
    expect(reference.source.anchor.markdown.end.block).toBeGreaterThan(reference.source.anchor.markdown.start.block);
    expect(Object.keys(reference.source)).toEqual(["resource_kind", "resource_id", "resource_updated_at", "context_digest", "anchor"]);
  });

  test("indexes Mermaid fences as exact revision-pinned section references", () => {
    const source = "# Architecture\n\n```mermaid\nflowchart LR\n  A --> B\n```\n\n## Failure mode\n\n```mermaid\nthis is invalid {\n```\n";
    const { index } = render(source);
    const diagrams = [...index.diagrams.values()];
    expect(diagrams.map(({ label, ordinal }) => [label, ordinal])).toEqual([
      ["Architecture — Mermaid diagram 1", 1],
      ["Failure mode — Mermaid diagram 2", 2],
    ]);
    expect(diagrams[0].markdown).toBe("```mermaid\nflowchart LR\n  A --> B\n```");
    expect(diagrams[0]).toMatchObject({ id: "1", startLine: 3, endLine: 7 });
    expect(diagramReference(diagrams[0], identity, "D".repeat(32))).toMatchObject({
      kind: "section",
      label: "Architecture — Mermaid diagram 1",
      markdown: "```mermaid\nflowchart LR\n  A --> B\n```",
      section_lines: { start: 3, end: 7 },
      source: { resource_kind: "markdown", anchor: { markdown: { start: { block: 1, line: 3 }, end: { block: 1, line: 7 } } } },
    });
  });
});

describe("document actions", () => {
  test("adds section references explicitly and installs no heading action in a headingless page", () => {
    const added = vi.fn();
    let rendered = render("# Page\nbody\n\n## Child\ncopy");
    const controller = createMarkdownContextController({ doc: document, ...rendered, identity, idFactory: () => "I".repeat(32), onAdd: added });
    const buttons = [...rendered.container.querySelectorAll(".agent-section-action")];
    expect(buttons.map(({ textContent }) => textContent)).toEqual(["Add page", "Add section"]);
    buttons[1].click();
    expect(added.mock.calls[0][0]).toMatchObject({ kind: "section", label: "Child" });
    controller.destroy();

    rendered = render("Only a paragraph");
    const headingless = createMarkdownContextController({ doc: document, ...rendered, identity, idFactory: () => "J".repeat(32), onAdd: added });
    expect(rendered.container.querySelector(".agent-section-action")).toBeNull();
    headingless.destroy();
  });

  test("adds Mermaid source and keeps the action outside diagram rerenders", () => {
    const rendered = render("# Architecture\n\n```mermaid\nflowchart LR\n  A --> B\n```\n");
    const added = vi.fn();
    const controller = createMarkdownContextController({ doc: document, ...rendered, identity, idFactory: () => "M".repeat(32), onAdd: added });
    const button = rendered.container.querySelector(".agent-diagram-source-action");
    const placeholder = rendered.container.querySelector(".mermaid-placeholder");
    expect(button.textContent).toBe("Add diagram");
    expect(button.getAttribute("aria-label")).toBe("Add diagram: Architecture — Mermaid diagram 1");
    button.click();
    expect(added).toHaveBeenCalledWith(expect.objectContaining({
      kind: "section",
      markdown: "```mermaid\nflowchart LR\n  A --> B\n```",
    }));

    placeholder.replaceChildren(document.createElement("svg"));
    expect(button.isConnected).toBe(true);
    const scrollIntoView = vi.fn();
    placeholder.scrollIntoView = scrollIntoView;
    expect(controller.navigate(added.mock.calls[0][0])).toBe(true);
    expect(scrollIntoView).toHaveBeenCalledWith({ block: "center", behavior: "smooth" });
    controller.destroy();
  });
});
