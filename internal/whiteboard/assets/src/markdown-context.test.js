import MarkdownIt from "markdown-it";
import { describe, expect, test, vi } from "vitest";
import { createMarkdownContextController, indexMarkdownTokens, referenceFromSelection, sectionReference } from "./markdown-context.js";

const identity = { resource: { id: "R".repeat(32), updated_at: "2026-08-10T00:00:00Z" }, digest: "a".repeat(64) };

function render(source) {
  const markdown = new MarkdownIt();
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
});
