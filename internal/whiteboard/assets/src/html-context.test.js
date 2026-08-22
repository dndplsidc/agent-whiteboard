import { readFileSync } from "node:fs";
import { describe, expect, test, vi } from "vitest";
import {
  buildHTMLComponentIndex,
  componentReference,
  createHTMLContextController,
  decodeEmbeddedRaster,
} from "./html-context.js";

const fixture = JSON.parse(readFileSync("internal/whiteboard/testdata/html-component-declarations-v1.json", "utf8"));
const identity = { resource: { id: "R".repeat(32), updated_at: "2026-08-10T00:00:00Z" }, digest: "a".repeat(64) };

function ids(index) { return index.components.map(({ id, type, label }) => ({ id, type, label })); }

function shell() {
  document.body.innerHTML = '<main id="surface"><iframe id="frame"></iframe></main>';
  const surface = document.querySelector("#surface");
  const frame = document.querySelector("#frame");
  Object.defineProperty(frame, "getBoundingClientRect", { configurable: true, value: () => ({ left: 10, top: 20, right: 310, bottom: 220, width: 300, height: 200 }) });
  Object.defineProperty(window, "innerWidth", { configurable: true, value: 400 });
  Object.defineProperty(window, "innerHeight", { configurable: true, value: 300 });
  return { surface, frame };
}

describe("inert HTML component index", () => {
  test("matches every frozen explicit declaration case", () => {
    for (const item of fixture.cases) {
      expect(ids(buildHTMLComponentIndex(item.html)), item.name).toEqual(item.browser.canonical_components);
    }
  });

  test("detects ordered semantic kinds, exclusions, nesting, precedence, and the cap", () => {
    const many = Array.from({ length: 130 }, (_, index) => `<section id="s${index}"><h2>Section ${index}</h2></section>`).join("");
    const source = `<nav><section id="nav"><h2>Navigation</h2></section></nav>
      <section id="parent"><h2>Parent</h2><table id="table"><caption>Totals</caption></table></section>
      <figure id="photo"><img alt="Sunset" src="data:image/png;base64,iVBORw0KGgo="><figcaption>Evening</figcaption></figure>
      <svg id="chart" role="img" aria-label="Revenue"></svg>
      <pre id="code"><code class="language-js">let x = 1</code></pre>
      <blockquote id="quote"> A useful quotation with   spacing. </blockquote>
      <div id="ordinary" class="card">Not semantic</div><video id="movie"></video>${many}`;
    const index = buildHTMLComponentIndex(source);
    expect(index.components.slice(0, 7).map(({ id, type }) => [id, type])).toEqual([
      ["parent", "section"], ["table", "table"], ["photo", "image"], ["chart", "chart"], ["code", "code"], ["quote", "quote"], ["s0", "section"],
    ]);
    expect(index.components).toHaveLength(128);
    expect(index.byID.has("nav")).toBe(false);
    expect(index.byID.has("ordinary")).toBe(false);
    expect(index.byID.has("movie")).toBe(false);
  });

  test("uses stable unique IDs and bounded accessible labels and excerpts", () => {
    const source = `<section id="dup" aria-label="One"></section><table id="dup" aria-label="Two"></table>
      <section id="long" aria-label="${"x".repeat(257)}"></section><section id="${"i".repeat(257)}" aria-label="Long ID"></section>
      <section id="hidden" style="display: none"><h2>Hidden</h2></section>
      <section id="safe"><h2> Safe   label </h2><img alt="pixel" src="data:image/png;base64,${"A".repeat(2000)}"><script>secret()</script></section>`;
    const index = buildHTMLComponentIndex(source);
    expect(index.components.map(({ id }) => id)).toEqual(["safe"]);
    const safe = index.byID.get("safe");
    expect(safe.label).toBe("Safe label");
    expect(safe.sourceExcerpt).not.toContain("A".repeat(100));
    expect(safe.sourceExcerpt).toContain("secret()");
    expect(safe.sourceExcerpt).toContain("embedded image/png");
  });

  test("derives only eligible unambiguous embedded raster descriptors", () => {
    const index = buildHTMLComponentIndex(`<img id="png" alt="Pixel" src="data:image/png;base64,iVBORw0KGgo=">
      <img id="svg" alt="Vector" src="data:image/svg+xml;base64,PHN2Zz4=">
      <figure id="amb"><img alt="One" src="data:image/png;base64,iVBORw0KGgo="><img alt="Two" src="data:image/png;base64,iVBORw0KGgo="><figcaption>Two</figcaption></figure>
      <img id="remote" alt="Remote" src="https://example.test/a.png">`);
    expect(index.byID.get("png").raster).toMatchObject({ mediaType: "image/png" });
    expect(index.byID.get("svg").raster).toBeUndefined();
    expect(index.byID.get("amb").raster).toBeUndefined();
    expect(index.byID.get("remote").raster).toBeUndefined();
    const decoded = decodeEmbeddedRaster(index.byID.get("png").raster, window);
    expect(decoded.mediaType).toBe("image/png");
    expect(decoded.bytes).toBeInstanceOf(Uint8Array);
  });

  test("builds the frozen nested component reference from parent metadata", () => {
    const component = buildHTMLComponentIndex('<table id="revenue"><caption>Revenue</caption></table>').components[0];
    expect(componentReference(component, identity, "I".repeat(32))).toEqual({
      id: "I".repeat(32), kind: "component", label: "Revenue",
      source: { resource_kind: "html", resource_id: identity.resource.id, resource_updated_at: identity.resource.updated_at, context_digest: identity.digest, anchor: { html: { element_id: "revenue", tag: "table", ordinal: 1 } } },
      component: { type: "table", source_excerpt: '<table id="revenue"><caption>Revenue</caption></table>' },
    });
  });
});

describe("trusted parent HTML context controller", () => {
  test("projects only IDs/types, validates the current WindowProxy and clips candidate geometry", async () => {
    const { surface, frame } = shell();
    const postMessage = vi.spyOn(frame.contentWindow, "postMessage");
    const index = buildHTMLComponentIndex('<section id="main"><h2>Main</h2></section>');
    const added = vi.fn();
    const controller = createHTMLContextController({ doc: document, surface, frame, index, identity, idFactory: () => "I".repeat(32), epochFactory: () => "epoch_1", onAdd: added });
    expect(postMessage.mock.calls[0][0]).toEqual({ version: 1, type: "manifest", epoch: "epoch_1", components: [{ id: "main", type: "section" }] });
    expect(JSON.stringify(postMessage.mock.calls[0][0])).not.toContain("Main");

    const otherFrame = document.createElement("iframe");
    document.body.append(otherFrame);
    window.dispatchEvent(new MessageEvent("message", { data: { version: 1, type: "candidate", epoch: "epoch_1", id: "main", rect: { x: 0, y: 0, width: 10, height: 10 } }, source: otherFrame.contentWindow }));
    expect(document.querySelector(".agent-html-add").hidden).toBe(true);
    window.dispatchEvent(new MessageEvent("message", { data: { version: 1, type: "candidate", epoch: "stale", id: "main", rect: { x: 0, y: 0, width: 10, height: 10 } }, source: frame.contentWindow }));
    expect(document.querySelector(".agent-html-add").hidden).toBe(true);

    window.dispatchEvent(new MessageEvent("message", { data: { version: 1, type: "candidate", epoch: "epoch_1", id: "main", rect: { x: -20, y: -30, width: 500, height: 400 } }, source: frame.contentWindow }));
    await new Promise((resolve) => requestAnimationFrame(resolve));
    const button = document.querySelector(".agent-html-add");
    expect(button.hidden).toBe(false);
    expect(button.textContent).toBe("+ Add");
    expect(button.getAttribute("aria-label")).toBe("Add section: Main to message");
    expect(document.querySelector(".agent-html-outline").style.cssText).toContain("width: 300px");

    window.dispatchEvent(new MessageEvent("message", { data: { version: 1, type: "candidate", epoch: "epoch_1", id: "forged", rect: { x: 0, y: 0, width: 20, height: 20 } }, source: frame.contentWindow }));
    expect(button.hidden).toBe(true);
    window.dispatchEvent(new MessageEvent("message", { data: { version: 1, type: "candidate", epoch: "epoch_1", id: "main", rect: { x: 0, y: 0, width: 20, height: 20 }, label: "forged" }, source: frame.contentWindow }));
    expect(button.hidden).toBe(true);
    button.click();
    expect(added).not.toHaveBeenCalled();
    controller.destroy();
  });

  test("keeps an ordered chooser functional without bridge hints and inserts only on parent activation", async () => {
    const { surface, frame } = shell();
    const index = buildHTMLComponentIndex('<section id="parent"><h2>Parent</h2><table id="child"><caption>Child</caption></table></section>');
    const added = vi.fn(async (reference) => reference);
    const controller = createHTMLContextController({ doc: document, surface, frame, index, identity, idFactory: () => "I".repeat(32), epochFactory: () => "epoch_2", onAdd: added });
    const options = [...document.querySelectorAll(".agent-html-chooser button")];
    expect(options.map(({ textContent }) => textContent)).toEqual(["Parent — section", "Child — table"]);
    options[1].click();
    await vi.waitFor(() => expect(added).toHaveBeenCalledOnce());
    expect(added.mock.calls[0][0]).toMatchObject({ kind: "component", label: "Child" });
    controller.destroy();
  });

  test("resets epochs on load, ignores stale revisions, locates current tokens, and cleans up", () => {
    const { surface, frame } = shell();
    const postMessage = vi.spyOn(frame.contentWindow, "postMessage");
    const epochs = ["one", "two"];
    const index = buildHTMLComponentIndex('<section id="main"><h2>Main</h2></section>');
    const controller = createHTMLContextController({ doc: document, surface, frame, index, identity, idFactory: () => "I".repeat(32), epochFactory: () => epochs.shift(), onAdd: vi.fn() });
    frame.dispatchEvent(new Event("load"));
    const reference = componentReference(index.components[0], identity, "I".repeat(32));
    expect(controller.navigate(reference)).toBe(true);
    expect(postMessage.mock.calls.at(-1)[0]).toEqual({ version: 1, type: "locate", epoch: "two", id: "main" });
    expect(controller.navigate({ ...reference, source: { ...reference.source, context_digest: "b".repeat(64) } })).toBe(false);
    controller.destroy();
    expect(document.querySelector(".agent-html-chooser")).toBeNull();
    expect(document.querySelector(".agent-html-outline")).toBeNull();
  });
});
