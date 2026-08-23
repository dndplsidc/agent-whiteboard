import { decodeChildBridgeFrame, decodeParentBridgeFrame, HTML_BRIDGE_VERSION, MAX_HTML_BRIDGE_COMPONENTS } from "./html-bridge-protocol.js";

const encoder = new TextEncoder();
const TYPES = new Set(["section", "image", "chart", "table", "code", "quote", "component"]);
const EXCLUDED_SELECTOR = "nav,header,footer,form,input,button,select,textarea,option,fieldset,script,style,template,noscript,video,audio,[hidden],[inert]";
const LABEL_BYTES = 256;
const EXCERPT_BYTES = 48 * 1024;
const ADD_TRANSFER_GRACE_MS = 80;
const ADD_BUTTON_INSET = 12;
const RASTER_PATTERN = /^data:(image\/(?:png|jpeg|gif|webp));base64,([A-Za-z0-9+/=\s]+)$/iu;
const sourceIDCache = new WeakMap();

function normalizeText(value) { return (value ?? "").replace(/\s+/gu, " ").trim(); }
function bounded(value, bytes) {
  return value.length > 0 && encoder.encode(value).length <= bytes && !/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/u.test(value);
}

function parserFor(options) {
  const Parser = options?.DOMParser ?? globalThis.DOMParser;
  if (typeof Parser !== "function") throw new TypeError("DOMParser is unavailable");
  return new Parser();
}

function sourceIDs(doc) {
  let result = sourceIDCache.get(doc);
  if (result) return result;
  result = new Map();
  for (const element of doc.querySelectorAll("[id]")) {
    const matches = result.get(element.id) ?? [];
    matches.push(element);
    result.set(element.id, matches);
  }
  sourceIDCache.set(doc, result);
  return result;
}

function labelledBy(element, doc) {
  const ids = normalizeText(element.getAttribute("aria-labelledby")).split(" ").filter(Boolean);
  if (!ids.length) return "";
  const index = sourceIDs(doc);
  const labels = ids.map((id) => index.get(id) ?? []);
  if (labels.some((matches) => matches.length !== 1)) return "";
  const text = labels.map(([item]) => normalizeText(item.textContent));
  return text.every(Boolean) ? normalizeText(text.join(" ")) : "";
}

function codeLanguage(element) {
  const code = element.matches("code") ? element : element.querySelector("code");
  const explicit = normalizeText(element.getAttribute("data-language") || code?.getAttribute("data-language"));
  if (explicit) return explicit;
  const match = /(?:^|\s)language-([^\s]+)/u.exec(code?.className || "");
  return match ? `${match[1]} code` : "";
}

function fallbackPreview(value) { return [...normalizeText(value)].slice(0, 160).join(""); }

function fallbackLabel(element, type) {
  if (type === "section") return normalizeText(element.querySelector("h1,h2,h3,h4,h5,h6")?.textContent);
  if (type === "image" || type === "chart") {
    if (element.matches("figure")) return normalizeText(element.querySelector(":scope > figcaption")?.textContent || element.querySelector("img")?.getAttribute("alt"));
    return normalizeText(element.getAttribute("alt") || element.querySelector("img")?.getAttribute("alt") || element.querySelector("title")?.textContent);
  }
  if (type === "table") return normalizeText(element.querySelector(":scope > caption")?.textContent);
  if (type === "code") return codeLanguage(element) || fallbackPreview(element.textContent);
  if (type === "quote") return fallbackPreview(element.textContent);
  return "";
}

function labelFor(element, type, doc) {
  for (const value of [labelledBy(element, doc), normalizeText(element.getAttribute("aria-label")), fallbackLabel(element, type)]) {
    if (bounded(value, LABEL_BYTES)) return value;
  }
  return "";
}

function meaningfulImage(image) {
  if (!image || normalizeText(image.getAttribute("alt")) === "" && image.hasAttribute("alt")) return false;
  return Boolean(labelledBy(image, image.ownerDocument) || normalizeText(image.getAttribute("aria-label")) || normalizeText(image.getAttribute("alt")));
}

function figureType(element) {
  const visuals = [...element.querySelectorAll("img,svg,canvas")];
  const primary = visuals[0];
  if (!primary) return "";
  if (primary.matches("img") && meaningfulImage(primary)) return "image";
  if (primary.matches("svg,canvas")) return "chart";
  return "";
}

function automaticType(element) {
  if (element.matches("figure")) return figureType(element);
  if (element.matches("img")) return meaningfulImage(element) && !element.closest("figure") ? "image" : "";
  if (element.matches("svg,canvas")) return !element.closest("figure") ? "chart" : "";
  if (element.matches("table")) return "table";
  if (element.matches("pre") && element.querySelector("code")) return "code";
  if (element.matches("blockquote,[role=\"note\"],[role=\"alert\"]")) return "quote";
  if (element.matches("section,article,[role=\"region\"]")) return "section";
  return "";
}

function declarationState(doc) {
  const declarations = [...doc.querySelectorAll("[data-agent-select],[data-agent-section],[data-agent-section-ignore]")];
  const idCounts = new Map([...sourceIDs(doc)].map(([id, matches]) => [id, matches.length]));
  const explicitIDs = new Set();
  for (const element of declarations) {
    const hasSelect = element.hasAttribute("data-agent-select");
    const select = element.getAttribute("data-agent-select");
    const shorthand = element.hasAttribute("data-agent-section");
    const ignore = element.hasAttribute("data-agent-section-ignore");
    if (shorthand && hasSelect || ignore && hasSelect && select !== "none" || shorthand && ignore) return { valid: false };
    if (hasSelect && select !== "none" && !TYPES.has(select)) return { valid: false };
    const type = shorthand ? "section" : hasSelect && select !== "none" ? select : "";
    if (!type) continue;
    const id = element.id;
    if (!bounded(id, 256) || id.trim() !== id || explicitIDs.has(id) || idCounts.get(id) !== 1) return { valid: false };
    explicitIDs.add(id);
    if (excluded(element) || !labelFor(element, type, doc)) return { valid: false };
  }
  return { valid: true };
}

function excluded(element) {
  if (element.closest("[data-agent-section-ignore],[data-agent-select=\"none\"]")) return true;
  for (let current = element; current; current = current.parentElement) {
    const style = current.getAttribute("style") || "";
    if (/(?:^|;)\s*(?:display\s*:\s*none|visibility\s*:\s*hidden)\s*(?:;|$)/iu.test(style)) return true;
    if (normalizeText(current.getAttribute("aria-hidden")).toLowerCase() === "true") return true;
  }
  return Boolean(element.closest(EXCLUDED_SELECTOR));
}

function rasterFor(element, type) {
  if (type !== "image") return undefined;
  const images = element.matches("img") ? [element] : [...element.querySelectorAll("img")];
  if (images.length !== 1 || element.matches("svg,canvas") || element.querySelector("svg,canvas")) return undefined;
  const [image] = images;
  const picture = image.closest("picture");
  if (image.hasAttribute("srcset") || picture?.querySelector("source") || element.querySelector("source,[srcset]")) return undefined;
  const eligible = RASTER_PATTERN.exec(image.getAttribute("src") || "");
  if (!eligible) return undefined;
  const [, mediaType, base64] = eligible;
  return { mediaType: mediaType.toLowerCase(), dataURL: `data:${mediaType.toLowerCase()};base64,${base64.replace(/\s/gu, "")}` };
}

function sourceExcerpt(element) {
  const clone = element.cloneNode(true);
  for (const media of [clone, ...clone.querySelectorAll("[src],[srcset],[poster],[href],[xlink\\:href]")]) {
    for (const attribute of ["src", "srcset", "poster", "href", "xlink:href"]) {
      const value = media.getAttribute?.(attribute);
      if (!value?.startsWith("data:")) continue;
      const type = /^data:([^;,\s]+)/iu.exec(value)?.[1]?.toLowerCase() || "embedded media";
      media.setAttribute(attribute, `[embedded ${type}; ${encoder.encode(value).length} source bytes omitted]`);
    }
  }
  const excerpt = normalizeText(clone.outerHTML);
  return bounded(excerpt, EXCERPT_BYTES) ? excerpt : "";
}

export function buildHTMLComponentIndex(source, options = {}) {
  if (typeof source !== "string") throw new TypeError("invalid HTML source");
  const doc = parserFor(options).parseFromString(source, "text/html");
  const declaration = declarationState(doc);
  if (!declaration.valid) return { components: [], byID: new Map(), projection: [] };
  const idCounts = new Map([...sourceIDs(doc)].map(([id, matches]) => [id, matches.length]));
  const components = [];
  for (const element of doc.querySelectorAll("[id]")) {
    if (components.length >= MAX_HTML_BRIDGE_COMPONENTS) break;
    const explicitType = element.hasAttribute("data-agent-section") ? "section" : TYPES.has(element.getAttribute("data-agent-select")) ? element.getAttribute("data-agent-select") : "";
    if (!bounded(element.id, 256) || element.id.trim() !== element.id || /[\u0000-\u001f\u007f]/u.test(element.id) || excluded(element) || idCounts.get(element.id) !== 1) continue;
    const type = explicitType || automaticType(element);
    if (!type) continue;
    const label = labelFor(element, type, doc);
    const excerpt = sourceExcerpt(element);
    if (!label || !excerpt) continue;
    const component = { id: element.id, type, label, tag: element.localName, ordinal: components.length + 1, sourceExcerpt: excerpt };
    const raster = rasterFor(element, type);
    if (raster) component.raster = raster;
    components.push(Object.freeze(component));
  }
  const byID = new Map(components.map((component) => [component.id, component]));
  return { components: Object.freeze(components), byID, projection: Object.freeze(components.map(({ id, type }) => Object.freeze({ id, type }))) };
}

export function componentReference(component, identity, id) {
  if (!component || !identity?.resource || typeof id !== "string") throw new TypeError("invalid HTML component reference");
  return {
    id, kind: "component", label: component.label,
    source: {
      resource_kind: "html", resource_id: identity.resource.id, resource_updated_at: identity.resource.updated_at, context_digest: identity.digest,
      anchor: { html: { element_id: component.id, tag: component.tag, ordinal: component.ordinal } },
    },
    component: { type: component.type, source_excerpt: component.sourceExcerpt },
  };
}

export function decodeEmbeddedRaster(descriptor, view = globalThis) {
  const match = descriptor && RASTER_PATTERN.exec(descriptor.dataURL);
  if (!match || match[1].toLowerCase() !== descriptor.mediaType) throw new TypeError("invalid embedded raster");
  let binary;
  try { binary = view.atob(match[2].replace(/\s/gu, "")); } catch { throw new TypeError("invalid embedded raster"); }
  const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
  if (!bytes.length || bytes.length > 10 * 1024 * 1024) throw new RangeError("embedded raster is too large");
  return { mediaType: descriptor.mediaType, bytes };
}

function defaultEpoch(view) {
  const bytes = new Uint8Array(18);
  view.crypto.getRandomValues(bytes);
  return [...bytes].map((value) => value.toString(16).padStart(2, "0")).join("");
}

function sameIdentity(reference, identity) {
  const source = reference?.source;
  return source?.resource_kind === "html" && source.resource_id === identity.resource.id && source.resource_updated_at === identity.resource.updated_at && source.context_digest === identity.digest;
}

export function createHTMLContextController({ doc = document, chooserHost, surface, frame, index, identity, idFactory, epochFactory, onAdd, announce = () => {} }) {
  if (!chooserHost || !surface?.contains(frame) || surface.contains(chooserHost) || !index?.components || !identity?.resource || typeof idFactory !== "function" || typeof onAdd !== "function") throw new TypeError("invalid HTML context controller");
  const view = doc.defaultView;
  const makeEpoch = epochFactory ?? (() => defaultEpoch(view));
  let epoch = "";
  let candidate = null;
  let pendingFrame = null;
  let destroyed = false;
  let timer = null;
  let addTransferTimer = null;
  let addPointerInside = false;
  let generation = 0;
  const feedbackTimers = new Set();

  const outline = doc.createElement("div");
  outline.className = "agent-html-outline";
  outline.hidden = true;
  outline.setAttribute("aria-hidden", "true");
  const addButton = doc.createElement("button");
  addButton.type = "button";
  addButton.className = "agent-source-action agent-html-add";
  addButton.textContent = "+ Add";
  addButton.hidden = true;
  const chooser = doc.createElement("details");
  chooser.className = "agent-html-chooser";
  const summary = doc.createElement("summary");
  summary.textContent = "Components";
  const list = doc.createElement("ol");
  chooser.append(summary, list);
  doc.body.append(outline, addButton);
  chooserHost.append(chooser);

  function clearPending() {
    generation += 1;
    pendingFrame = null;
    if (timer !== null) view.cancelAnimationFrame(timer);
    timer = null;
  }

  function clearAddTransfer() {
    if (addTransferTimer !== null) view.clearTimeout(addTransferTimer);
    addTransferTimer = null;
  }

  function dismiss() {
    clearPending();
    clearAddTransfer();
    addPointerInside = false;
    candidate = null;
    outline.hidden = true;
    addButton.hidden = true;
    addButton.disabled = false;
    addButton.dataset.state = "";
  }

  function scheduleAddTransferDismiss() {
    clearAddTransfer();
    if (destroyed || addButton.hidden) return;
    addTransferTimer = view.setTimeout(() => {
      addTransferTimer = null;
      if (!addPointerInside && doc.activeElement !== addButton && !addButton.disabled) dismiss();
    }, ADD_TRANSFER_GRACE_MS);
  }

  function post(frameValue) {
    const safe = decodeParentBridgeFrame(frameValue);
    frame.contentWindow?.postMessage(safe, "*");
  }

  function reset() {
    dismiss();
    epoch = makeEpoch();
    post({ version: HTML_BRIDGE_VERSION, type: "manifest", epoch, components: index.projection });
  }

  function clippedRect(rect) {
    const host = frame.getBoundingClientRect();
    const left = Math.max(0, host.left, host.left + rect.x);
    const top = Math.max(0, host.top, host.top + rect.y);
    const right = Math.min(view.innerWidth, host.right, host.left + rect.x + rect.width);
    const bottom = Math.min(view.innerHeight, host.bottom, host.top + rect.y + rect.height);
    return right > left && bottom > top ? { left, top, width: right - left, height: bottom - top } : null;
  }

  function renderCandidate(component, rect) {
    clearAddTransfer();
    const clipped = clippedRect(rect);
    if (!clipped) { dismiss(); return; }
    candidate = { component, rect };
    Object.assign(outline.style, { left: `${clipped.left}px`, top: `${clipped.top}px`, width: `${clipped.width}px`, height: `${clipped.height}px` });
    outline.hidden = false;
    addButton.hidden = false;
    addButton.textContent = "+ Add";
    addButton.setAttribute("aria-label", `Add ${component.type}: ${component.label} to message`);
    const buttonWidth = addButton.offsetWidth || 56;
    const buttonHeight = addButton.offsetHeight || 32;
    const horizontalInset = Math.min(ADD_BUTTON_INSET, Math.max(0, (clipped.width - buttonWidth) / 2));
    const verticalInset = Math.min(ADD_BUTTON_INSET, Math.max(0, (clipped.height - buttonHeight) / 2));
    addButton.style.left = `${Math.max(8, Math.min(clipped.left + clipped.width - buttonWidth - horizontalInset, view.innerWidth - buttonWidth - 8))}px`;
    addButton.style.top = `${Math.max(8, Math.min(clipped.top + verticalInset, view.innerHeight - buttonHeight - 8))}px`;
  }

  function schedule(component, rect) {
    pendingFrame = { component, rect, generation };
    if (timer !== null) return;
    const scheduledGeneration = generation;
    timer = view.requestAnimationFrame(() => {
      if (scheduledGeneration !== generation) return;
      timer = null;
      const next = pendingFrame;
      pendingFrame = null;
      if (next && next.generation === generation && !destroyed) renderCandidate(next.component, next.rect);
    });
  }

  function onMessage(event) {
    if (destroyed || event.source !== frame.contentWindow) return;
    if (event.ports?.length) { dismiss(); return; }
    let message;
    try { message = decodeChildBridgeFrame(event.data); } catch { dismiss(); return; }
    if (message.epoch !== epoch) return;
    if (message.type === "clear") {
      clearPending();
      if (!addPointerInside && doc.activeElement !== addButton) dismiss();
      return;
    }
    if (message.type === "ready") return;
    const component = index.byID.get(message.id);
    if (!component) { dismiss(); return; }
    schedule(component, message.rect);
  }

  async function activate(component, button) {
    if (destroyed || index.byID.get(component.id) !== component) return;
    button.disabled = true;
    button.dataset.state = "disabled";
    try {
      const reference = componentReference(component, identity, idFactory());
      await onAdd(reference, component, { semanticOnly: button.dataset.visualFailed === "true" });
      if (button !== addButton) chooser.open = false;
      button.dataset.state = "added";
      button.textContent = button === addButton ? "+ Add" : button.textContent;
      announce(`Added ${component.label} to the message.`);
      outline.classList.add("agent-source-added");
      const feedbackTimer = view.setTimeout(() => {
        feedbackTimers.delete(feedbackTimer);
        outline.classList.remove("agent-source-added");
        if (button === addButton) dismiss();
        else { button.disabled = false; button.dataset.state = ""; }
      }, 650);
      feedbackTimers.add(feedbackTimer);
    } catch (error) {
      button.disabled = false;
      button.dataset.state = "error";
      if (component.raster) button.dataset.visualFailed = "true";
      announce(error?.message || "Unable to add this component.");
      if (button === addButton) scheduleAddTransferDismiss();
    }
  }

  function onOuterLayout() {
    if (candidate && !destroyed) renderCandidate(candidate.component, candidate.rect);
  }

  addButton.addEventListener("pointerenter", () => { addPointerInside = true; clearAddTransfer(); });
  addButton.addEventListener("pointerleave", () => { addPointerInside = false; scheduleAddTransferDismiss(); });
  addButton.addEventListener("focus", clearAddTransfer);
  addButton.addEventListener("blur", scheduleAddTransferDismiss);
  addButton.addEventListener("pointerdown", (event) => event.preventDefault());
  addButton.addEventListener("click", () => { if (candidate) void activate(candidate.component, addButton); });
  for (const component of index.components) {
    const item = doc.createElement("li");
    const button = doc.createElement("button");
    button.type = "button";
    button.textContent = `${component.label} — ${component.type}`;
    button.setAttribute("aria-label", `Add ${component.type}: ${component.label} to message`);
    button.addEventListener("click", () => void activate(component, button));
    item.append(button);
    list.append(item);
  }
  if (!index.components.length) {
    const empty = doc.createElement("p");
    empty.textContent = "No labeled components detected.";
    chooser.append(empty);
  }

  frame.addEventListener("load", reset);
  view.addEventListener("message", onMessage);
  view.addEventListener("resize", onOuterLayout);
  const resizeObserver = typeof view.ResizeObserver === "function" ? new view.ResizeObserver(onOuterLayout) : null;
  resizeObserver?.observe(surface);
  resizeObserver?.observe(frame);
  reset();

  return {
    index,
    navigate(reference) {
      if (!sameIdentity(reference, identity)) return false;
      const id = reference.source.anchor?.html?.element_id;
      if (!index.byID.has(id)) return false;
      post({ version: HTML_BRIDGE_VERSION, type: "locate", epoch, id });
      return true;
    },
    dismiss,
    destroy() {
      if (destroyed) return;
      destroyed = true;
      clearPending();
      clearAddTransfer();
      for (const feedbackTimer of feedbackTimers) view.clearTimeout(feedbackTimer);
      feedbackTimers.clear();
      frame.removeEventListener("load", reset);
      view.removeEventListener("message", onMessage);
      view.removeEventListener("resize", onOuterLayout);
      resizeObserver?.disconnect();
      chooser.remove(); outline.remove(); addButton.remove();
    },
  };
}
