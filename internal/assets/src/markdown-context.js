const BLOCK_TYPES = new Set(["heading_open", "paragraph_open", "fence", "code_block", "blockquote_open", "list_item_open", "table_open"]);

function visibleInlineText(token) {
  return (token.children ?? []).map((child) => child.type === "text" || child.type === "code_inline" ? child.content : child.type === "softbreak" || child.type === "hardbreak" ? " " : "").join("").trim();
}

function cloneHeadingPath(path) { return path.map((item) => ({ ...item })); }

export function indexMarkdownTokens(tokens, source) {
  const lines = source.split("\n");
  const blocks = new Map();
  const headings = new Map();
  const images = new Map();
  const headingStack = [];
  const siblingCounts = new Map();
  let blockOrdinal = 0;
  let imageOrdinal = 0;
  let currentBlock = null;

  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    if (BLOCK_TYPES.has(token.type) && token.map) {
      const id = String(blockOrdinal++);
      token.attrSet("data-agent-block", id);
      const block = { id, startLine: token.map[0] + 1, endLine: token.map[1] + 1, headingPath: cloneHeadingPath(headingStack) };
      blocks.set(id, block);
      currentBlock = block;
    }
    if (token.type === "heading_open" && token.map) {
      const level = Number.parseInt(token.tag.slice(1), 10);
      const title = visibleInlineText(tokens[index + 1] ?? {});
      while (headingStack.at(-1)?.level >= level) headingStack.pop();
      const parentKey = headingStack.map((item) => `${item.level}:${item.ordinal}:${item.title}`).join("/");
      const siblingKey = `${parentKey}|${level}`;
      const ordinal = (siblingCounts.get(siblingKey) ?? 0) + 1;
      siblingCounts.set(siblingKey, ordinal);
      const heading = { level, title, ordinal };
      const path = [...cloneHeadingPath(headingStack), heading];
      const id = token.attrGet("data-agent-block");
      token.attrSet("data-agent-heading", id);
      headings.set(id, { ...blocks.get(id), level, title, ordinal, headingPath: path, sectionEndLine: lines.length + 1 });
      headingStack.push(heading);
      blocks.get(id).headingPath = path;
      currentBlock = blocks.get(id);
    }
    if (token.type === "inline") {
      const containing = currentBlock;
      for (const child of token.children ?? []) {
        if (child.type !== "image" || !containing) continue;
        const id = String(imageOrdinal++);
        child.attrSet("data-agent-image", id);
        const alt = visibleInlineText(child);
        images.set(id, { id, ordinal: imageOrdinal, alt, block: containing.id, startLine: token.map?.[0] + 1 || containing.startLine, endLine: token.map?.[1] + 1 || containing.endLine, headingPath: cloneHeadingPath(headingStack) });
      }
    }
  }

  const orderedHeadings = [...headings.values()];
  for (let index = 0; index < orderedHeadings.length; index += 1) {
    const heading = orderedHeadings[index];
    const next = orderedHeadings.slice(index + 1).find((candidate) => candidate.level <= heading.level);
    heading.sectionEndLine = next?.startLine ?? lines.length + 1;
    heading.markdown = lines.slice(heading.startLine - 1, heading.sectionEndLine - 1).join("\n");
  }
  return { blocks, headings, images, lineCount: lines.length };
}

function scalarLength(value) { return [...value].length; }

function closestBlock(node, container) {
  const element = node?.nodeType === 1 ? node : node?.parentElement;
  const block = element?.closest?.("[data-agent-block]");
  return block && container.contains(block) ? block : null;
}

function endpointOffset(doc, block, node, offset) {
  const range = doc.createRange();
  range.selectNodeContents(block);
  try { range.setEnd(node, offset); } catch { return 0; }
  return scalarLength(range.toString());
}

export function referenceFromSelection({ doc, container, index, identity, id }) {
  const selection = doc.getSelection?.();
  if (!selection || selection.rangeCount !== 1 || selection.isCollapsed) return null;
  const range = selection.getRangeAt(0);
  const startElement = closestBlock(range.startContainer, container);
  const endElement = closestBlock(range.endContainer, container);
  if (!startElement || !endElement || !container.contains(range.commonAncestorContainer)) return null;
  const quote = range.toString();
  if (!quote.trim()) return null;
  const start = index.blocks.get(startElement.dataset.agentBlock);
  const end = index.blocks.get(endElement.dataset.agentBlock);
  if (!start || !end) return null;
  return {
    id,
    kind: "text",
    label: quote.replace(/\s+/gu, " ").trim().slice(0, 80),
    quote,
    source: {
      resource_kind: "markdown",
      resource_id: identity.resource.id,
      resource_updated_at: identity.resource.updated_at,
      context_digest: identity.digest,
      heading_path: cloneHeadingPath(start.headingPath),
      start: { block: Number(start.id), line: start.startLine, offset: endpointOffset(doc, startElement, range.startContainer, range.startOffset) },
      end: { block: Number(end.id), line: end.startLine, offset: endpointOffset(doc, endElement, range.endContainer, range.endOffset) },
    },
  };
}

function sourceBase(identity, metadata) {
  return {
    resource_kind: "markdown",
    resource_id: identity.resource.id,
    resource_updated_at: identity.resource.updated_at,
    context_digest: identity.digest,
    heading_path: cloneHeadingPath(metadata.headingPath),
    start: { block: Number(metadata.id ?? metadata.block), line: metadata.startLine, offset: 0 },
    end: { block: Number(metadata.id ?? metadata.block), line: metadata.endLine, offset: 0 },
  };
}

export function sectionReference(metadata, identity, id) {
  return {
    id, kind: "section", label: metadata.title, markdown: metadata.markdown,
    section_lines: { start: metadata.startLine, end: metadata.sectionEndLine },
    source: sourceBase(identity, { ...metadata, endLine: metadata.sectionEndLine }),
  };
}

export function imageReference(metadata, identity, id, imageID, name) {
  return {
    id, kind: "image", label: metadata.alt || `Image ${metadata.ordinal}`,
    source: sourceBase(identity, metadata),
    visual: { image_id: imageID, name, alt: metadata.alt },
  };
}

function actionButton(doc, label, accessibleLabel) {
  const button = doc.createElement("button");
  button.type = "button";
  button.className = "agent-source-action";
  button.textContent = label;
  button.setAttribute("aria-label", accessibleLabel);
  return button;
}

export function createMarkdownContextController({ doc = document, container, index, identity, idFactory, onAdd, onImageAdd, announce = () => {} }) {
  if (!container || !index || !identity || typeof idFactory !== "function" || typeof onAdd !== "function") throw new TypeError("invalid Markdown context controller");
  const popup = actionButton(doc, "Add to message", "Add selected text to message");
  popup.classList.add("agent-selection-action");
  popup.hidden = true;
  doc.body.append(popup);
  let pending = null;
  let destroyed = false;
  const installed = [];

  function pulse(element) {
    element.classList.remove("agent-source-added");
    void element.offsetWidth;
    element.classList.add("agent-source-added");
    doc.defaultView?.setTimeout?.(() => element.classList.remove("agent-source-added"), 900);
  }

  function dismiss() { pending = null; popup.hidden = true; popup.dataset.state = ""; }

  function updateSelection() {
    if (destroyed) return;
    const reference = referenceFromSelection({ doc, container, index, identity, id: idFactory() });
    if (!reference) { dismiss(); return; }
    const range = doc.getSelection().getRangeAt(0);
    const rect = range.getClientRects?.()[0] ?? closestBlock(range.startContainer, container)?.getBoundingClientRect?.();
    if (!rect) { dismiss(); return; }
    pending = { reference, source: closestBlock(range.startContainer, container) };
    popup.hidden = false;
    const width = popup.offsetWidth || 118;
    const height = popup.offsetHeight || 34;
    const viewportWidth = doc.defaultView?.innerWidth ?? 1024;
    const top = rect.top - height - 8 >= 8 ? rect.top - height - 8 : rect.bottom + 8;
    popup.style.left = `${Math.max(8, Math.min(rect.left + rect.width / 2 - width / 2, viewportWidth - width - 8))}px`;
    popup.style.top = `${Math.max(8, top)}px`;
  }

  popup.addEventListener("pointerdown", (event) => event.preventDefault());
  popup.addEventListener("click", () => {
    if (!pending) return;
    try {
      onAdd(pending.reference);
      pulse(pending.source);
      popup.textContent = "Added";
      popup.dataset.state = "added";
      announce(`Added ${pending.reference.label} to the message.`);
      doc.defaultView?.setTimeout?.(() => { popup.textContent = "Add to message"; dismiss(); }, 550);
    } catch (error) {
      announce(error?.message || "Unable to add this selection.");
    }
  });

  for (const [id, metadata] of index.headings) {
    const heading = container.querySelector(`[data-agent-heading="${id}"]`);
    if (!heading) continue;
    const button = actionButton(doc, metadata.level === 1 ? "Add page" : "Add section", `${metadata.level === 1 ? "Add page" : "Add section"}: ${metadata.title}`);
    button.classList.add("agent-section-action");
    button.addEventListener("click", () => { onAdd(sectionReference(metadata, identity, idFactory())); pulse(heading); announce(`Added ${metadata.title} to the message.`); });
    heading.classList.add("agent-source-heading");
    heading.append(button);
    installed.push(button);
  }

  for (const [id, metadata] of index.images) {
    const image = container.querySelector(`img[data-agent-image="${id}"]`);
    if (!image) continue;
    const wrapper = doc.createElement("span");
    wrapper.className = "agent-source-image";
    image.replaceWith(wrapper);
    wrapper.append(image);
    const label = metadata.alt || `Image ${metadata.ordinal}`;
    const button = actionButton(doc, "Add image", `Add image: ${label}`);
    button.classList.add("agent-image-source-action");
    button.addEventListener("click", async () => {
      if (typeof onImageAdd !== "function") return;
      try { await onImageAdd({ ...metadata, element: image, identity, referenceID: idFactory() }); pulse(wrapper); }
      catch (error) { announce(error?.message || "Unable to add this image."); }
    });
    wrapper.append(button);
    installed.push(button);
  }

  const onSelectionChange = () => updateSelection();
  const onKeyDown = (event) => {
    if (event.key === "Escape") dismiss();
    else if (event.key === "Tab" && !popup.hidden && container.contains(doc.activeElement)) { event.preventDefault(); popup.focus(); }
  };
  const onScroll = () => dismiss();
  doc.addEventListener("selectionchange", onSelectionChange);
  doc.addEventListener("keydown", onKeyDown);
  doc.defaultView?.addEventListener("scroll", onScroll, true);

  return {
    navigate(reference) {
      if (reference?.source?.resource_id !== identity.resource.id || reference?.source?.context_digest !== identity.digest) return false;
      const target = container.querySelector(`[data-agent-block="${reference.source.start.block}"]`);
      if (!target) return false;
      target.scrollIntoView?.({ block: "center", behavior: "smooth" });
      pulse(target);
      return true;
    },
    dismiss,
    destroy() {
      destroyed = true;
      doc.removeEventListener("selectionchange", onSelectionChange);
      doc.removeEventListener("keydown", onKeyDown);
      doc.defaultView?.removeEventListener("scroll", onScroll, true);
      popup.remove();
      for (const button of installed) button.remove();
    },
  };
}
