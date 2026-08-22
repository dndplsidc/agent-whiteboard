const encoder = new TextEncoder();

export const MESSAGE_LIMITS = Object.freeze({
  parts: 64,
  references: 16,
  skills: 16,
  bytes: 64 * 1024,
});

export function cloneMessageContent(content) {
  return { parts: (content?.parts ?? []).map((part) => {
    if (part.type === "text") return { type: "text", text: part.text };
    if (part.type === "skill") return { type: "skill", skill: { ...part.skill } };
    return { type: "reference", reference: structuredClone(part.reference) };
  }) };
}

export function normalizeMessageContent(content) {
  if (!content || !Array.isArray(content.parts)) throw new TypeError("invalid message content");
  const parts = [];
  for (const candidate of content.parts) {
    if (candidate?.type === "text") {
      if (typeof candidate.text !== "string") throw new TypeError("invalid text part");
      if (candidate.text === "") continue;
      if (parts.at(-1)?.type === "text") parts.at(-1).text += candidate.text;
      else parts.push({ type: "text", text: candidate.text });
      continue;
    }
    if (candidate?.type === "skill" && candidate.skill && typeof candidate.skill.id === "string" && typeof candidate.skill.name === "string") {
      parts.push({ type: "skill", skill: { id: candidate.skill.id, name: candidate.skill.name } });
      continue;
    }
    if (candidate?.type !== "reference" || !candidate.reference) throw new TypeError("invalid reference part");
    parts.push({ type: "reference", reference: structuredClone(candidate.reference) });
  }
  const normalized = { parts };
  const skills = parts.filter(({ type }) => type === "skill");
  if (parts.length > MESSAGE_LIMITS.parts || parts.filter(({ type }) => type === "reference").length > MESSAGE_LIMITS.references || skills.length > MESSAGE_LIMITS.skills || new Set(skills.map(({ skill }) => skill.id)).size !== skills.length || new Set(skills.map(({ skill }) => skill.name)).size !== skills.length || messageContentBytes(normalized) > MESSAGE_LIMITS.bytes) {
    throw new RangeError("message content is too large");
  }
  return normalized;
}

export function messageContentBytes(content) {
  let bytes = 0;
  for (const part of content?.parts ?? []) {
    if (part.type === "text") bytes += encoder.encode(part.text).length;
    else if (part.type === "skill") bytes += encoder.encode(part.skill.id + part.skill.name).length;
    else if (part.reference) bytes += encoder.encode(JSON.stringify(part.reference)).length;
  }
  return bytes;
}

export function messageContentText(content) {
  return (content?.parts ?? []).map((part) => part.type === "text" ? part.text : part.type === "skill" ? `$${part.skill.name}` : `[${part.reference.label}]`).join("");
}

function normalizedCaret(content, caret) {
  if (!caret || !Number.isInteger(caret.part) || !Number.isInteger(caret.offset)) return { part: content.parts.length, offset: 0 };
  if (caret.part < 0) return { part: 0, offset: 0 };
  if (caret.part >= content.parts.length) return { part: content.parts.length, offset: 0 };
  const part = content.parts[caret.part];
  if (part.type !== "text") return { part: caret.part + (caret.offset > 0 ? 1 : 0), offset: 0 };
  return { part: caret.part, offset: Math.min(Math.max(caret.offset, 0), [...part.text].length) };
}

function splitScalars(value, offset) {
  const scalars = [...value];
  return [scalars.slice(0, offset).join(""), scalars.slice(offset).join("")];
}

function insertAtomicPart(content, atomicPart, caret) {
  const current = normalizeMessageContent(content);
  const position = normalizedCaret(current, caret);
  const before = current.parts.slice(0, position.part);
  const after = current.parts.slice(position.part);
  if (position.part < current.parts.length && current.parts[position.part].type === "text") {
    const [left, right] = splitScalars(current.parts[position.part].text, position.offset);
    before.push(...(left ? [{ type: "text", text: left }] : []));
    after.shift();
    if (right) after.unshift({ type: "text", text: right });
  }
  const leftText = before.at(-1)?.type === "text" ? before.at(-1) : null;
  const rightText = after[0]?.type === "text" ? after[0] : null;
  if (leftText && !/\s$/u.test(leftText.text)) leftText.text += " ";
  let trailingSpace = false;
  if (rightText && !/^\s/u.test(rightText.text)) {
    rightText.text = ` ${rightText.text}`;
    trailingSpace = true;
  } else if (!rightText) {
    after.unshift({ type: "text", text: " " });
    trailingSpace = true;
  }
  const insertedIndex = before.length;
  const next = normalizeMessageContent({ parts: [...before, atomicPart, ...after] });
  return { content: next, caret: { part: insertedIndex + 1, offset: trailingSpace ? 1 : 0 } };
}

export function insertMessageReference(content, reference, caret) {
  if (!reference || typeof reference !== "object") throw new TypeError("invalid reference");
  return insertAtomicPart(content, { type: "reference", reference }, caret);
}

function referenceLabel(reference) {
  const kind = reference.kind === "section" ? "Section" : reference.kind === "image" ? "Image" : reference.kind === "component" ? "Component" : "Selection";
  return `${kind}: ${reference.label}`;
}

function skillDisplayLabel(skill) {
  const displayName = typeof skill?.display_name === "string" ? skill.display_name.trim() : "";
  if (displayName) return displayName;
  const nativeName = typeof skill?.name === "string" ? skill.name.split(":").at(-1) : "";
  const readable = nativeName.replace(/[-_]+/gu, " ").replace(/\s+/gu, " ").trim();
  return readable ? `${readable[0].toLocaleUpperCase()}${readable.slice(1)}` : "Skill";
}

function skillTokenText(skill, displayName) {
  return `Skill: ${displayName || skillDisplayLabel(skill)}`;
}

function referenceLabelElement(doc, label) {
  const element = doc.createElement("span");
  element.className = "agent-message-reference-label";
  element.textContent = label;
  return element;
}

export function renderMessageContent(doc, content, { interactive = true, onReference } = {}) {
  const fragment = doc.createDocumentFragment();
  for (const part of normalizeMessageContent(content).parts) {
    if (part.type === "text") {
      const text = doc.createElement("span");
      text.className = "agent-message-text-part";
      text.textContent = part.text;
      fragment.append(text);
      continue;
    }
    if (part.type === "skill") {
      const token = doc.createElement("span");
      token.className = "agent-message-skill";
      token.dataset.skillId = part.skill.id;
      token.textContent = skillTokenText(part.skill);
      fragment.append(token);
      continue;
    }
    const token = doc.createElement(interactive && onReference ? "button" : "span");
    if (token instanceof doc.defaultView.HTMLButtonElement) token.type = "button";
    token.className = "agent-message-reference";
    token.dataset.referenceId = part.reference.id;
    token.dataset.referenceKind = part.reference.kind;
    token.setAttribute("aria-label", referenceLabel(part.reference));
    token.append(referenceLabelElement(doc, part.reference.label));
    if (interactive && onReference) token.addEventListener("click", () => onReference(part.reference));
    fragment.append(token);
  }
  return fragment;
}

export function createMessageEditor({ doc = document, content = { parts: [] }, placeholder = "Ask about this page…", ariaLabel = "Message Page Agent about this whiteboard", maxSelectedSkills = MESSAGE_LIMITS.skills, onChange = () => {}, onSubmit = () => {} } = {}) {
  const root = doc.createElement("div");
  root.className = "agent-message-editor";
  root.contentEditable = "true";
  root.tabIndex = 0;
  root.setAttribute("role", "textbox");
  root.setAttribute("aria-multiline", "true");
  root.setAttribute("aria-label", ariaLabel);
  root.dataset.placeholder = placeholder;
  let model = normalizeMessageContent(content);
  let savedCaret = { part: model.parts.length, offset: 0 };
  let composing = false;
  let skillLimit = Number.isInteger(maxSelectedSkills) && maxSelectedSkills >= 0 && maxSelectedSkills <= MESSAGE_LIMITS.skills ? maxSelectedSkills : MESSAGE_LIMITS.skills;
  const references = new Map();
  const skills = new Map();
  const skillDisplayNames = new Map();

  function render() {
    references.clear();
    skills.clear();
    root.replaceChildren();
    for (const part of model.parts) {
      if (part.type === "text") {
        root.append(doc.createTextNode(part.text));
      } else if (part.type === "skill") {
        skills.set(part.skill.id, { ...part.skill });
        const token = doc.createElement("span");
        token.className = "agent-message-skill";
        token.contentEditable = "false";
        token.dataset.skillId = part.skill.id;
        const displayName = skillDisplayNames.get(part.skill.id) || skillDisplayLabel(part.skill);
        token.setAttribute("aria-label", `${skillTokenText(part.skill, displayName)}. Press Backspace or Delete to remove.`);
        token.textContent = skillTokenText(part.skill, displayName);
        root.append(token);
      } else {
        references.set(part.reference.id, structuredClone(part.reference));
        const token = doc.createElement("span");
        token.className = "agent-message-reference";
        token.contentEditable = "false";
        token.dataset.referenceId = part.reference.id;
        token.dataset.referenceKind = part.reference.kind;
        token.setAttribute("role", "button");
        token.setAttribute("aria-label", `${referenceLabel(part.reference)}. Press Backspace or Delete to remove.`);
        token.append(referenceLabelElement(doc, part.reference.label));
        root.append(token);
      }
    }
  }

  function modelCaretFromSelection() {
    const selection = doc.getSelection?.();
    if (!selection || selection.rangeCount === 0 || !root.contains(selection.anchorNode)) return savedCaret;
    const range = selection.getRangeAt(0).cloneRange();
    range.setStart(root, 0);
    const prefix = range.cloneContents();
    let part = 0;
    let offset = 0;
    for (const node of prefix.childNodes) {
      if (node.nodeType === 3) {
        offset = [...node.textContent].length;
      } else if (node.nodeType === 1 && (node.dataset?.referenceId || node.dataset?.skillId)) {
        part += 1;
        offset = 0;
      } else {
        offset += [...(node.textContent ?? "")].length;
      }
    }
    return normalizedCaret(model, { part, offset });
  }

  function restoreCaret(caret = savedCaret) {
    root.focus();
    const position = normalizedCaret(model, caret);
    const range = doc.createRange();
    const selection = doc.getSelection?.();
    if (position.part < root.childNodes.length && root.childNodes[position.part]?.nodeType === 3) {
      const node = root.childNodes[position.part];
      range.setStart(node, Math.min(position.offset, node.textContent.length));
    } else {
      range.setStart(root, Math.min(position.part, root.childNodes.length));
    }
    range.collapse(true);
    selection?.removeAllRanges();
    selection?.addRange(range);
    savedCaret = position;
  }

  function readDOM() {
    const parts = [];
    for (const node of root.childNodes) {
      if (node.nodeType === 1 && node.dataset?.referenceId && references.has(node.dataset.referenceId)) {
        parts.push({ type: "reference", reference: references.get(node.dataset.referenceId) });
      } else if (node.nodeType === 1 && node.dataset?.skillId && skills.has(node.dataset.skillId)) {
        parts.push({ type: "skill", skill: skills.get(node.dataset.skillId) });
      } else {
        const text = node.textContent ?? "";
        if (text) parts.push({ type: "text", text });
      }
    }
    model = normalizeMessageContent({ parts });
    savedCaret = modelCaretFromSelection();
    onChange(cloneMessageContent(model));
  }

  function adjacentToken(direction) {
    const selection = doc.getSelection?.();
    if (!selection?.isCollapsed || selection.rangeCount === 0 || !root.contains(selection.anchorNode)) return null;
    const range = selection.getRangeAt(0);
    if (range.startContainer === root) return root.childNodes[range.startOffset + (direction < 0 ? -1 : 0)] ?? null;
    if (range.startContainer.nodeType === 3) {
      if (direction < 0 && range.startOffset === 0) return range.startContainer.previousSibling;
      if (direction > 0 && range.startOffset === range.startContainer.textContent.length) return range.startContainer.nextSibling;
    }
    return null;
  }

  Object.defineProperties(root, {
    value: {
      configurable: true,
      get: () => messageContentText(model),
      set: (value) => {
        model = normalizeMessageContent({ parts: value ? [{ type: "text", text: String(value) }] : [] });
        savedCaret = { part: model.parts.length, offset: 0 };
        render();
        onChange(cloneMessageContent(model));
      },
    },
    placeholder: {
      configurable: true,
      get: () => root.dataset.placeholder,
      set: (value) => { root.dataset.placeholder = String(value); },
    },
  });

  root.addEventListener("compositionstart", () => { composing = true; });
  root.addEventListener("compositionend", () => { composing = false; readDOM(); });
  root.addEventListener("input", () => { if (!composing) readDOM(); });
  root.addEventListener("focusout", () => { savedCaret = modelCaretFromSelection(); });
  const onSelectionChange = () => { if (root.contains(doc.getSelection?.()?.anchorNode)) savedCaret = modelCaretFromSelection(); };
  doc.addEventListener("selectionchange", onSelectionChange);
  root.addEventListener("paste", (event) => {
    const text = typeof event.clipboardData?.getData === "function" ? event.clipboardData.getData("text/plain") : undefined;
    if (typeof text !== "string") return;
    event.preventDefault();
    doc.execCommand?.("insertText", false, text);
  });
  root.addEventListener("keydown", (event) => {
    if (event.key === "Enter" && !event.shiftKey && !composing && !event.isComposing && event.keyCode !== 229) {
      event.preventDefault();
      readDOM();
      onSubmit();
      return;
    }
    const direction = event.key === "Backspace" ? -1 : event.key === "Delete" ? 1 : 0;
    const token = direction && adjacentToken(direction);
    if (token?.nodeType === 1 && (token.dataset?.referenceId || token.dataset?.skillId)) {
      event.preventDefault();
      token.remove();
      readDOM();
      render();
      restoreCaret({ part: Math.max(0, savedCaret.part - (direction < 0 ? 1 : 0)), offset: 0 });
    }
  });
  render();

  return {
    element: root,
    getContent: () => cloneMessageContent(model),
    setContent(next) { model = normalizeMessageContent(next); savedCaret = { part: model.parts.length, offset: 0 }; render(); onChange(cloneMessageContent(model)); },
    clear() { this.setContent({ parts: [] }); },
    saveCaret() { savedCaret = modelCaretFromSelection(); return { ...savedCaret }; },
    insertReference(reference) {
      const result = insertMessageReference(model, reference, savedCaret);
      model = result.content;
      savedCaret = result.caret;
      render();
      restoreCaret();
      onChange(cloneMessageContent(model));
      return cloneMessageContent(model);
    },
    insertSkill(skill) {
      const selectedSkills = model.parts.filter((part) => part.type === "skill");
      if (!skill || typeof skill.id !== "string" || typeof skill.name !== "string" || selectedSkills.length >= skillLimit || selectedSkills.some((part) => part.skill.id === skill.id || part.skill.name === skill.name)) return cloneMessageContent(model);
      skillDisplayNames.set(skill.id, skillDisplayLabel(skill));
      const result = insertAtomicPart(model, { type: "skill", skill: { id: skill.id, name: skill.name } }, savedCaret);
      model = result.content;
      savedCaret = result.caret;
      render();
      restoreCaret();
      onChange(cloneMessageContent(model));
      return cloneMessageContent(model);
    },
    setMaxSelectedSkills(limit) {
      if (!Number.isInteger(limit) || limit < 0 || limit > MESSAGE_LIMITS.skills) throw new TypeError("invalid skill selection limit");
      skillLimit = limit;
    },
    markUnavailableSkills(availableIDs) {
      const available = new Set(availableIDs);
      for (const token of root.querySelectorAll(".agent-message-skill")) {
        const unavailable = !available.has(token.dataset.skillId);
        token.classList.toggle("is-unavailable", unavailable);
        token.setAttribute("aria-invalid", String(unavailable));
      }
    },
    focus() { restoreCaret(); },
    destroy() { doc.removeEventListener("selectionchange", onSelectionChange); root.remove(); },
  };
}
