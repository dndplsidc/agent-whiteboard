import { describe, expect, test, vi } from "vitest";
import { createMessageEditor, insertMessageReference, messageContentText, normalizeMessageContent, renderMessageContent } from "./message-editor.js";

const reference = (id, label = id) => ({ id, kind: "text", label, source: { resource_kind: "markdown" }, quote: label });

describe("ordered message model", () => {
  test("normalizes text and inserts multiple references at model positions", () => {
    const normalized = normalizeMessageContent({ parts: [{ type: "text", text: "hello" }, { type: "text", text: " world" }] });
    expect(normalized.parts).toEqual([{ type: "text", text: "hello world" }]);
    const first = insertMessageReference(normalized, reference("a", "Alpha"), { part: 0, offset: 5 });
    expect(messageContentText(first.content)).toBe("hello [Alpha] world");
    const second = insertMessageReference(first.content, reference("b", "Beta"), first.caret);
    expect(messageContentText(second.content)).toBe("hello [Alpha][Beta] world");
    expect(second.content.parts.filter(({ type }) => type === "reference").map(({ reference: item }) => item.id)).toEqual(["a", "b"]);
  });

  test("keeps skill tokens ordered, distinct, and counted as content", () => {
    const content = normalizeMessageContent({ parts: [
      { type: "skill", skill: { id: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", name: "review-helper" } },
      { type: "text", text: " check this" },
      { type: "skill", skill: { id: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", name: "test-helper" } },
    ] });
    expect(messageContentText(content)).toBe("$review-helper check this$test-helper");
    expect(() => normalizeMessageContent({ parts: [...content.parts, content.parts[0]] })).toThrow(RangeError);
  });

  test("renders hostile labels as text and reference order remains explicit", () => {
    const container = document.createElement("div");
    container.append(renderMessageContent(document, { parts: [
      { type: "text", text: "before " },
      { type: "reference", reference: reference("a", "<img onerror=alert(1)>") },
      { type: "text", text: " after" },
    ] }));
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector(".agent-message-reference-label")?.textContent).toBe("<img onerror=alert(1)>");
    expect(container.textContent).toBe("before <img onerror=alert(1)> after");

    const component = reference("component", "Revenue");
    component.kind = "component";
    const componentContainer = document.createElement("div");
    componentContainer.append(renderMessageContent(document, { parts: [{ type: "reference", reference: component }] }));
    expect(componentContainer.querySelector(".agent-message-reference")?.getAttribute("aria-label")).toBe("Component: Revenue");
  });
});

describe("message editor", () => {
  test("projects atomic tokens and submits on Enter but not Shift+Enter", () => {
    const submit = vi.fn();
    const editor = createMessageEditor({ doc: document, content: { parts: [{ type: "text", text: "hello" }] }, onSubmit: submit });
    document.body.append(editor.element);
    editor.insertReference(reference("a", "Selected sentence"));
    const token = editor.element.querySelector(".agent-message-reference");
    expect(token.contentEditable).toBe("false");
    expect(token.querySelector(".agent-message-reference-label")?.textContent).toBe("Selected sentence");
    expect(editor.getContent().parts.map(({ type }) => type)).toEqual(["text", "reference", "text"]);
    editor.element.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    editor.element.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", shiftKey: true, bubbles: true }));
    expect(submit).toHaveBeenCalledTimes(1);
  });

  test("inserts non-editable skills and marks catalog drift unavailable", () => {
    const editor = createMessageEditor({ doc: document, content: { parts: [{ type: "text", text: "check" }] } });
    document.body.append(editor.element);
    editor.insertSkill({ id: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", name: "review-helper", display_name: "Review helper" });
    const token = editor.element.querySelector(".agent-message-skill");
    expect(token.contentEditable).toBe("false");
    expect(token.textContent).toBe("Skill: Review helper");
    expect(editor.getContent().parts.some(({ type }) => type === "skill")).toBe(true);
    editor.markUnavailableSkills([]);
    expect(token.classList.contains("is-unavailable")).toBe(true);
    expect(token.getAttribute("aria-invalid")).toBe("true");
  });

  test("enforces the advertised skill limit without changing the existing draft", () => {
    const editor = createMessageEditor({ doc: document, content: { parts: [{ type: "text", text: "keep this" }] }, maxSelectedSkills: 1 });
    document.body.append(editor.element);
    editor.insertSkill({ id: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", name: "review" });
    const accepted = editor.getContent();
    const rejected = editor.insertSkill({ id: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", name: "test" });
    expect(rejected).toEqual(accepted);
    expect(editor.element.querySelectorAll(".agent-message-skill")).toHaveLength(1);
    editor.setMaxSelectedSkills(2);
    editor.insertSkill({ id: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", name: "test" });
    expect(editor.element.querySelectorAll(".agent-message-skill")).toHaveLength(2);
  });

  test("does not trust pasted token-shaped HTML", () => {
    const editor = createMessageEditor({ doc: document });
    editor.element.innerHTML = '<span class="agent-message-reference" data-reference-id="forged">forged</span>';
    editor.element.dispatchEvent(new InputEvent("input", { bubbles: true }));
    expect(editor.getContent()).toEqual({ parts: [{ type: "text", text: "forged" }] });
  });

  test("inserts a later reference at the live caret after browser input", () => {
    const editor = createMessageEditor({ doc: document });
    document.body.append(editor.element);
    editor.insertReference(reference("a", "First"));
    editor.element.lastChild.textContent = " and ";
    const text = editor.element.lastChild;
    const range = document.createRange();
    range.setStart(text, text.textContent.length);
    range.collapse(true);
    document.getSelection().removeAllRanges();
    document.getSelection().addRange(range);
    editor.element.dispatchEvent(new InputEvent("input", { bubbles: true }));

    editor.insertReference(reference("b", "Second"));

    expect(editor.getContent().parts.map(({ type }) => type)).toEqual(["reference", "text", "reference", "text"]);
    expect(messageContentText(editor.getContent())).toBe("[First] and [Second] ");
  });
});
