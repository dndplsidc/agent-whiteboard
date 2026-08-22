import { decodeChildBridgeFrame, decodeParentBridgeFrame, HTML_BRIDGE_VERSION, MAX_HTML_BRIDGE_COMPONENTS } from "./html-bridge-protocol.js";

const encoder = new TextEncoder();
const TYPES = new Set(["section", "image", "chart", "table", "code", "quote", "component"]);
const EXCLUDED_SELECTOR = "nav,header,footer,form,input,button,select,textarea,option,fieldset,script,style,template,noscript,video,audio,[hidden],[inert],[aria-hidden=\"true\"]";
const LABEL_BYTES = 256;
const EXCERPT_BYTES = 48 * 1024;
const RASTER_PATTERN = /^data:(image\/(?:png|jpeg|gif|webp));base64,([A-Za-z0-9+/=\s]+)$/iu;

function normalizeText(value) { return (value ?? "").replace(/\s+/gu, " ").trim(); }
function bounded(value, bytes) { return value.length > 0 && encoder.encode(value).length <= bytes; }

function parserFor(options) {
  const Parser = options?.DOMParser ?? globalThis.DOMParser;
  if (typeof Parser !== "function") throw new TypeError("DOMParser is unavailable");
  return new Parser();
}

function labelledBy(element, doc) {
  const ids = normalizeText(element.getAttribute("aria-labelledby")).split(" ").filter(Boolean);
  if (!ids.length) return "";
  const labels = ids.map((id) => doc.getElementById(id)).filter(Boolean).map((item) => normalizeText(item.textContent)).filter(Boolean);
  return labels.length === ids.length ? normalizeText(labels.join(" ")) : "";
}

function codeLanguage(element) {
  const code = element.matches("code") ? element : element.querySelector(":scope > code");
  const explicit = normalizeText(element.getAttribute("data-language") || code?.getAttribute("data-language"));
  if (explicit) return explicit;
  const match = /(?:^|\s)language-([^\s]+)/u.exec(code?.className || "");
  return match ? `${match[1]} code` : "";
}

function fallbackLabel(element, type) {
  if (type === "section") return normalizeText(element.querySelector("h1,h2,h3,h4,h5,h6")?.textContent);
  if (type === "image" || type === "chart") {
    if (element.matches("figure")) return normalizeText(element.querySelector(":scope > figcaption")?.textContent || element.querySelector("img")?.getAttribute("alt"));
    return normalizeText(element.getAttribute("alt") || element.querySelector("title")?.textContent);
  }
  if (type === "table") return normalizeText(element.querySelector(":scope > caption")?.textContent);
  if (type === "code") return codeLanguage(element) || normalizeText(element.textContent).slice(0, 160);
  if (type === "quote") return normalizeText(element.textContent).slice(0, 160);
  return "";
}

function labelFor(element, type, doc) {
  const value = labelledBy(element, doc) || normalizeText(element.getAttribute("aria-label")) || fallbackLabel(element, type);
  return bounded(value, LABEL_BYTES) ? value : "";
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
  if (element.matches("pre") && element.querySelector(":scope > code")) return "code";
  if (element.matches("blockquote,[role=\"note\"],[role=\"alert\"]")) return "quote";
  if (element.matches("section,article,[role=\"region\"]")) return "section";
  return "";
}

function declarationState(doc) {
  const declarations = [...doc.querySelectorAll("[data-agent-select],[data-agent-section],[data-agent-section-ignore]")];
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
    if (!id || explicitIDs.has(id)) return { valid: false };
    explicitIDs.add(id);
    if (!labelFor(element, type, doc)) return { valid: false };
  }
  return { valid: true };
}

function excluded(element, explicitType) {
  if (element.closest("[data-agent-section-ignore],[data-agent-select=\"none\"]")) return true;
  for (let current = element; current; current = current.parentElement) {
    const style = current.getAttribute("style") || "";
    if (/(?:^|;)\s*(?:display\s*:\s*none|visibility\s*:\s*hidden)\s*(?:;|$)/iu.test(style)) return true;
  }
  const boundary = element.closest(EXCLUDED_SELECTOR);
  if (!boundary) return false;
  return !(explicitType === "component" && boundary === element && !element.matches("form,input,button,select,textarea,option,fieldset,script,style,template,noscript,video,audio"));
}

function rasterFor(element, type) {
  if (type !== "image") return undefined;
  const images = element.matches("img") ? [element] : [...element.querySelectorAll("img")];
  const eligible = images.map((image) => RASTER_PATTERN.exec(image.getAttribute("src") || "")).filter(Boolean);
  if (eligible.length !== 1 || images.length !== 1) return undefined;
  const [, mediaType, base64] = eligible[0];
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
  const idCounts = new Map();
  for (const element of doc.querySelectorAll("[id]")) idCounts.set(element.id, (idCounts.get(element.id) ?? 0) + 1);
  const components = [];
  for (const element of doc.querySelectorAll("[id]")) {
    if (components.length >= MAX_HTML_BRIDGE_COMPONENTS) break;
    const explicitType = element.hasAttribute("data-agent-section") ? "section" : TYPES.has(element.getAttribute("data-agent-select")) ? element.getAttribute("data-agent-select") : "";
    if (!bounded(element.id, 256) || element.id.trim() !== element.id || /[\u0000-\u001f\u007f]/u.test(element.id) || excluded(element, explicitType) || idCounts.get(element.id) !== 1) continue;
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

export function createHTMLContextController({ doc = document, surface, frame, index, identity, idFactory, epochFactory, onAdd, announce = () => {} }) {
  if (!surface?.contains(frame) || !index?.components || !identity?.resource || typeof idFactory !== "function" || typeof onAdd !== "function") throw new TypeError("invalid HTML context controller");
  const view = doc.defaultView;
  const makeEpoch = epochFactory ?? (() => defaultEpoch(view));
  let epoch = "";
  let candidate = null;
  let pendingFrame = null;
  let destroyed = false;
  let timer = null;
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
  surface.append(chooser);

  function dismiss() {
    candidate = null;
    outline.hidden = true;
    addButton.hidden = true;
    addButton.disabled = false;
    addButton.dataset.state = "";
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
    const clipped = clippedRect(rect);
    if (!clipped) { dismiss(); return; }
    candidate = component;
    Object.assign(outline.style, { left: `${clipped.left}px`, top: `${clipped.top}px`, width: `${clipped.width}px`, height: `${clipped.height}px` });
    outline.hidden = false;
    addButton.hidden = false;
    addButton.textContent = "+ Add";
    addButton.setAttribute("aria-label", `Add ${component.type}: ${component.label} to message`);
    const buttonWidth = addButton.offsetWidth || 56;
    const buttonHeight = addButton.offsetHeight || 32;
    addButton.style.left = `${Math.max(8, Math.min(clipped.left + clipped.width - buttonWidth, view.innerWidth - buttonWidth - 8))}px`;
    addButton.style.top = `${Math.max(8, Math.min(clipped.top + 6, view.innerHeight - buttonHeight - 8))}px`;
  }

  function schedule(component, rect) {
    pendingFrame = { component, rect };
    if (timer !== null) return;
    timer = view.requestAnimationFrame(() => {
      timer = null;
      const next = pendingFrame;
      pendingFrame = null;
      if (next && !destroyed) renderCandidate(next.component, next.rect);
    });
  }

  function onMessage(event) {
    if (destroyed || event.source !== frame.contentWindow) return;
    let message;
    try { message = decodeChildBridgeFrame(event.data); } catch { dismiss(); return; }
    if (message.epoch !== epoch) return;
    if (message.type === "clear") { dismiss(); return; }
    if (message.type === "ready") { post({ version: HTML_BRIDGE_VERSION, type: "manifest", epoch, components: index.projection }); return; }
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
    }
  }

  addButton.addEventListener("pointerdown", (event) => event.preventDefault());
  addButton.addEventListener("click", () => { if (candidate) void activate(candidate, addButton); });
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
      if (timer !== null) view.cancelAnimationFrame(timer);
      for (const feedbackTimer of feedbackTimers) view.clearTimeout(feedbackTimer);
      feedbackTimers.clear();
      frame.removeEventListener("load", reset);
      view.removeEventListener("message", onMessage);
      chooser.remove(); outline.remove(); addButton.remove();
    },
  };
}
