import createDOMPurify from "dompurify";
import hljs from "highlight.js/lib/common";
import MarkdownIt from "markdown-it";
import mermaid from "mermaid";
import { createMarkdownContextController, imageReference, indexMarkdownTokens } from "./markdown-context.js";
import { createMessageEditor, cloneMessageContent, messageContentBytes, normalizeMessageContent, renderMessageContent } from "./message-editor.js";
import {
  cloneExecutionSettings,
  createCodexDraftState,
  createModelSettingsControl,
  editCodexDraft,
  formatEffort,
  readCodexSettingsPreference,
  reconcileCodexDraft,
  recordCodexSubmission,
  settingsCompatibility,
  validExecutionSettings,
  validModelCatalog,
  validPresentedExecutionSettings,
  writeCodexSettingsPreference,
} from "./model-settings.js";

export const DEFAULT_TITLE = "Untitled whiteboard";
export const THEME_STORAGE_KEY = "agent-whiteboard-theme";

const THEME_CONTROL_CLEANUP = Symbol("theme-control-cleanup");

const ALLOWED_THEMES = new Set(["light", "dark", "system"]);
const MERMAID_SECURE_KEYS = [
  "secure",
  "securityLevel",
  "startOnLoad",
  "maxTextSize",
  "suppressErrorRendering",
  "maxEdges",
  "theme",
  "htmlLabels",
  "themeCSS",
  "themeVariables",
];

export function normalizeTheme(value) {
  return ALLOWED_THEMES.has(value) ? value : "system";
}

export function resolveTheme(theme, mediaQuery) {
  if (normalizeTheme(theme) !== "system") return theme;
  return mediaQuery?.matches ? "dark" : "light";
}

function installTaskListRule(markdown) {
  markdown.core.ruler.after("inline", "task-list-items", (state) => {
    for (let index = 2; index < state.tokens.length; index += 1) {
      const inline = state.tokens[index];
      const listItem = state.tokens[index - 2];
      if (inline.type !== "inline" || listItem.type !== "list_item_open" || !inline.children?.length) continue;

      const first = inline.children[0];
      if (first.type !== "text") continue;
      const match = /^\[([ xX])\]\s+/.exec(first.content);
      if (!match) continue;

      first.content = first.content.slice(match[0].length);
      const checkbox = new state.Token("task_checkbox", "input", 0);
      checkbox.meta = { checked: match[1].toLowerCase() === "x" };
      inline.children.unshift(checkbox);
      listItem.attrJoin("class", "task-list-item");
    }
  });

  markdown.renderer.rules.task_checkbox = (tokens, index) =>
    `<input class="task-list-item-checkbox" type="checkbox" disabled${tokens[index].meta.checked ? " checked" : ""}> `;
}

function createMarkdownRenderer(diagramSources, { mermaidEnabled = true } = {}) {
  const markdown = new MarkdownIt({
    html: false,
    linkify: true,
  });
  const defaultFence = markdown.renderer.rules.fence.bind(markdown.renderer.rules);

  installTaskListRule(markdown);
  markdown.renderer.rules.fence = (tokens, index, options, environment, renderer) => {
    const token = tokens[index];
    const language = token.info.trim().split(/\s+/u, 1)[0].toLowerCase();
    if (language !== "mermaid" || !mermaidEnabled) {
      return defaultFence(tokens, index, options, environment, renderer);
    }

    const sourceIndex = diagramSources.push(token.content) - 1;
    return `<div class="mermaid-placeholder" data-index="${sourceIndex}"></div>`;
  };

  return markdown;
}

function purifierFor(doc) {
  return createDOMPurify(doc.defaultView ?? window);
}

function renderMarkdown(source, doc, { contextEnabled = false } = {}) {
  const diagramSources = [];
  const markdown = createMarkdownRenderer(diagramSources);
  const environment = {};
  const tokens = markdown.parse(source, environment);
  const semanticIndex = contextEnabled ? indexMarkdownTokens(tokens, source) : null;
  const rendered = markdown.renderer.render(tokens, markdown.options, environment);
  const sanitized = purifierFor(doc).sanitize(rendered);
  return { diagramSources, html: sanitized, semanticIndex };
}

function highlightCode(container) {
  for (const code of container.querySelectorAll("pre code")) {
    hljs.highlightElement(code);
  }
}

function setDocumentTitle(container, doc) {
  const firstHeading = container.querySelector("h1");
  const title = firstHeading?.textContent?.replace(/[\u0000-\u0008\u000B\u000C\u000E-\u001F]/gu, "").trim();
  doc.title = title || DEFAULT_TITLE;
}

function replaceWithDiagramError(placeholder, doc) {
  const error = doc.createElement("div");
  error.className = "mermaid-error";
  error.setAttribute("role", "img");
  error.setAttribute("aria-label", "Diagram rendering failed");
  error.textContent = "Unable to render diagram";
  placeholder.replaceChildren(error);
}

async function renderDiagrams({ container, diagramSources, doc, resolvedTheme, generation, isCurrent }) {
  mermaid.initialize({
    startOnLoad: false,
    securityLevel: "strict",
    suppressErrorRendering: true,
    maxTextSize: 50_000,
    maxEdges: 500,
    theme: resolvedTheme,
    htmlLabels: false,
    secure: MERMAID_SECURE_KEYS,
  });

  const purifier = purifierFor(doc);
  const placeholders = [...container.querySelectorAll(".mermaid-placeholder")];
  for (const placeholder of placeholders) {
    const index = Number.parseInt(placeholder.dataset.index ?? "", 10);
    const source = diagramSources[index];
    if (!Number.isSafeInteger(index) || typeof source !== "string") {
      replaceWithDiagramError(placeholder, doc);
      continue;
    }

    try {
      const id = `agent-whiteboard-mermaid-${generation}-${index}`;
      const result = await mermaid.render(id, source);
      if (!isCurrent()) return;
      const sanitizedSVG = purifier.sanitize(result.svg, {
        USE_PROFILES: { svg: true, svgFilters: true },
      });
      placeholder.innerHTML = sanitizedSVG;
    } catch {
      if (isCurrent()) replaceWithDiagramError(placeholder, doc);
    }
  }
}

function browserStorage(doc) {
  try {
    return doc.defaultView?.localStorage;
  } catch {
    return undefined;
  }
}

function browserMediaQuery(doc) {
  return typeof doc.defaultView?.matchMedia === "function"
    ? doc.defaultView.matchMedia("(prefers-color-scheme: dark)")
    : { matches: false, addEventListener() {}, removeEventListener() {} };
}

function readTheme(storage) {
  try {
    return normalizeTheme(storage?.getItem(THEME_STORAGE_KEY));
  } catch {
    return "system";
  }
}

function persistTheme(storage, theme) {
  try {
    storage?.setItem(THEME_STORAGE_KEY, theme);
  } catch {
    // Rendering remains available when browser storage is disabled.
  }
}

function themeLabel(theme) {
  return `${theme.slice(0, 1).toUpperCase()}${theme.slice(1)}`;
}

function createSVGElement(doc, name, attributes = {}) {
  const element = doc.createElementNS("http://www.w3.org/2000/svg", name);
  for (const [attribute, value] of Object.entries(attributes)) element.setAttribute(attribute, value);
  return element;
}

function themeIcon(doc, theme) {
  const svg = createSVGElement(doc, "svg", {
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    "stroke-width": "1.8",
    "stroke-linecap": "round",
    "stroke-linejoin": "round",
    "aria-hidden": "true",
  });
  svg.classList.add("theme-control-icon");
  svg.dataset.themeIcon = theme;

  if (theme === "light") {
    svg.append(createSVGElement(doc, "circle", { cx: "12", cy: "12", r: "3.5" }));
    for (const [x1, y1, x2, y2] of [
      [12, 2, 12, 4],
      [12, 20, 12, 22],
      [2, 12, 4, 12],
      [20, 12, 22, 12],
      [4.9, 4.9, 6.3, 6.3],
      [17.7, 17.7, 19.1, 19.1],
      [17.7, 6.3, 19.1, 4.9],
      [4.9, 19.1, 6.3, 17.7],
    ]) {
      svg.append(createSVGElement(doc, "line", { x1, y1, x2, y2 }));
    }
    return svg;
  }

  if (theme === "dark") {
    svg.append(
      createSVGElement(doc, "path", {
        d: "M20.2 15.2A8.5 8.5 0 0 1 8.8 3.8 8.5 8.5 0 1 0 20.2 15.2Z",
      }),
    );
    return svg;
  }

  svg.append(
    createSVGElement(doc, "circle", { cx: "12", cy: "12", r: "8.5" }),
    createSVGElement(doc, "path", { d: "M12 3.5a8.5 8.5 0 0 1 0 17Z", fill: "currentColor", stroke: "none" }),
  );
  return svg;
}

function createThemeControl({ doc, container, controller }) {
  const root = doc.createElement("div");
  root.className = "theme-control";

  const menuID = "agent-whiteboard-theme-menu";
  const trigger = doc.createElement("button");
  trigger.type = "button";
  trigger.className = "theme-control-trigger";
  trigger.dataset.themeControl = "";
  trigger.setAttribute("aria-controls", menuID);
  trigger.setAttribute("aria-expanded", "false");
  trigger.setAttribute("aria-haspopup", "menu");

  const menu = doc.createElement("div");
  menu.id = menuID;
  menu.className = "theme-control-menu";
  menu.dataset.themeMenu = "";
  menu.hidden = true;
  menu.setAttribute("role", "menu");
  menu.setAttribute("aria-label", "Theme selection");

  const descriptions = {
    system: "Match your device",
    light: "Bright appearance",
    dark: "Low-light appearance",
  };
  const options = ["system", "light", "dark"].map((value) => {
    const option = doc.createElement("button");
    option.type = "button";
    option.className = "theme-control-option";
    option.dataset.theme = value;
    option.dataset.themeOption = value;
    option.setAttribute("role", "menuitemradio");
    option.setAttribute("aria-checked", "false");
    const copy = doc.createElement("span");
    copy.className = "theme-control-option-copy";
    const title = doc.createElement("span");
    title.className = "theme-control-option-title";
    title.textContent = themeLabel(value);
    const description = doc.createElement("span");
    description.className = "theme-control-option-description";
    description.textContent = descriptions[value];
    const indicator = doc.createElement("span");
    indicator.className = "theme-control-option-indicator";
    indicator.setAttribute("aria-hidden", "true");
    copy.append(title, description);
    option.append(themeIcon(doc, value), copy, indicator);
    menu.append(option);
    return option;
  });

  root.append(trigger, menu);
  container.prepend(root);

  function sync() {
    const selected = controller.theme;
    const label = `Appearance: ${themeLabel(selected)}`;
    trigger.replaceChildren(themeIcon(doc, selected));
    trigger.setAttribute("aria-label", label);
    trigger.title = label;
    trigger.setAttribute("aria-expanded", String(!menu.hidden));
    for (const option of options) {
      const isSelected = option.dataset.theme === selected;
      option.setAttribute("aria-checked", String(isSelected));
      option.querySelector(".theme-control-option-indicator").textContent = isSelected ? "✓" : "";
    }
  }

  function close({ restoreFocus = false } = {}) {
    menu.hidden = true;
    sync();
    if (restoreFocus) trigger.focus();
  }

  const onTriggerClick = () => {
    menu.hidden = !menu.hidden;
    sync();
  };
  const onOptionClick = async (event) => {
    const pendingThemeChange = controller.setTheme(event.currentTarget.dataset.theme);
    sync();
    close({ restoreFocus: true });
    await pendingThemeChange;
  };
  const onDocumentPointerDown = (event) => {
    if (!root.contains(event.target)) close();
  };
  const onDocumentKeyDown = (event) => {
    if (event.key === "Escape" && !menu.hidden) close({ restoreFocus: true });
  };

  trigger.addEventListener("click", onTriggerClick);
  for (const option of options) option.addEventListener("click", onOptionClick);
  doc.addEventListener("pointerdown", onDocumentPointerDown);
  doc.addEventListener("keydown", onDocumentKeyDown);
  sync();

  return {
    destroy() {
      trigger.removeEventListener("click", onTriggerClick);
      for (const option of options) option.removeEventListener("click", onOptionClick);
      doc.removeEventListener("pointerdown", onDocumentPointerDown);
      doc.removeEventListener("keydown", onDocumentKeyDown);
      root.remove();
    },
  };
}

export async function renderWhiteboard(
  source,
  {
    container,
    doc = document,
    storage = browserStorage(doc),
    mediaQuery = browserMediaQuery(doc),
    contextEnabled = false,
  } = {},
) {
  if (typeof source !== "string") throw new TypeError("whiteboard source must be a string");
  if (!container) throw new TypeError("viewer container is required");

  container[THEME_CONTROL_CLEANUP]?.();
  container[THEME_CONTROL_CLEANUP] = undefined;
  const { diagramSources, html, semanticIndex } = renderMarkdown(source, doc, { contextEnabled });
  container.innerHTML = html;
  highlightCode(container);
  setDocumentTitle(container, doc);

  let theme = readTheme(storage);
  let generation = 0;
  let pendingRender = Promise.resolve();
  let subscribed = false;

  const onSystemThemeChange = () => {
    if (theme === "system") queueDiagramRender();
  };

  function syncSystemSubscription() {
    if (theme === "system" && !subscribed) {
      mediaQuery.addEventListener?.("change", onSystemThemeChange);
      subscribed = true;
    } else if (theme !== "system" && subscribed) {
      mediaQuery.removeEventListener?.("change", onSystemThemeChange);
      subscribed = false;
    }
  }

  function queueDiagramRender() {
    const selectedTheme = theme;
    const resolvedTheme = resolveTheme(selectedTheme, mediaQuery);
    const renderGeneration = ++generation;
    doc.documentElement.dataset.theme = resolvedTheme;
    doc.documentElement.style.colorScheme = resolvedTheme;
    pendingRender = pendingRender.then(() =>
      renderDiagrams({
        container,
        diagramSources,
        doc,
        resolvedTheme,
        generation: renderGeneration,
        isCurrent: () => renderGeneration === generation,
      }),
    );
    return pendingRender;
  }

  const controller = {
    diagramSources: [...diagramSources],
    semanticIndex,
    get theme() {
      return theme;
    },
    async setTheme(value) {
      theme = normalizeTheme(value);
      persistTheme(storage, theme);
      syncSystemSubscription();
      await queueDiagramRender();
    },
    settled() {
      return pendingRender;
    },
    destroy() {
      themeControl.destroy();
      container[THEME_CONTROL_CLEANUP] = undefined;
      if (subscribed) mediaQuery.removeEventListener?.("change", onSystemThemeChange);
      subscribed = false;
    },
  };

  const themeControl = createThemeControl({ doc, container, controller });
  container[THEME_CONTROL_CLEANUP] = themeControl.destroy;

  persistTheme(storage, theme);
  syncSystemSubscription();
  await queueDiagramRender();
  return controller;
}

function viewerContainer(doc) {
  const existing = doc.querySelector("#agent-whiteboard-content");
  if (existing) return existing;
  const container = doc.createElement("main");
  container.id = "agent-whiteboard-content";
  doc.body.append(container);
  return container;
}

export const AGENT_DRAWER_STORAGE_KEY = "agent-whiteboard-agent-drawer-open";
export const AGENT_PORT_STORAGE_KEY = "agent-whiteboard-agent-port";
export const AGENT_DRAWER_WIDTH_STORAGE_KEY = "agent-whiteboard-agent-drawer-width";
export const AGENT_PROVIDER_STORAGE_KEY = "agent-whiteboard-agent-provider";
export const DEFAULT_AGENT_PORT = 8568;
export const DEFAULT_AGENT_DRAWER_WIDTH = 420;
export const MIN_AGENT_DRAWER_WIDTH = 360;
export const MAX_AGENT_DRAWER_WIDTH = 720;
export const AGENT_DRAWER_DOCK_BREAKPOINT = 64 * 16;
export const AGENT_API_VERSION = "4";
export const AGENT_WEBSOCKET_PROTOCOL = "agent-whiteboard.v4";
export const MAX_AGENT_MESSAGE_BYTES = 64 * 1024;
export const MAX_AGENT_EVENT_BYTES = 1024 * 1024;
export const MAX_AGENT_IMAGES_PER_TURN = 8;
export const MAX_AGENT_IMAGE_BYTES = 10 * 1024 * 1024;
export const MAX_AGENT_TURN_IMAGE_BYTES = 20 * 1024 * 1024;
export const MAX_AGENT_IMAGE_NAME_BYTES = 255;

const AGENT_CONNECT_TIMEOUT_MS = 30_000;
const ID_PATTERN = /^[A-Za-z0-9_-]{32}$/u;
const DIGEST_PATTERN = /^[0-9a-f]{64}$/u;
const DATE_PATTERN = /^(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d):(\d\d)(?:\.(\d{1,9}))?(?:Z|([+-])(\d\d):(\d\d))$/u;
const MAX_STATE_EVENTS = 2048;
const MAX_TIMELINE_ITEMS = 200;
const MAX_ARCHIVES = 100;
const MAX_QUEUE_ITEMS = 64;
const MAX_PENDING_INTERACTIONS = 32;
const MAX_RETAINED_INTERACTIONS = 64;
const MAX_TIMELINE_TEXT_BYTES = 96 * 1024;
const MAX_DELTA_BYTES = 32 * 1024;
const PROVIDERS = new Set(["pi", "codex"]);
const IMAGE_MEDIA_TYPES = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"]);
const INTERACTION_KEY_PATTERN = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/u;
const encoder = new TextEncoder();

const ERROR_DEFINITIONS = {
  broker_unavailable: ["The local agent broker is unavailable.", "retry_connection"],
  wrong_port: ["No compatible local agent broker was found on this port.", "edit_port"],
  local_network_permission_denied: ["Browser permission to reach the local agent broker was denied.", "grant_local_network"],
  untrusted_origin: ["This whiteboard origin is not trusted by the local agent broker.", "trust_origin"],
  incompatible_api: ["The local agent broker uses an incompatible API version.", "update_broker"],
  provider_missing: ["The selected provider executable is not available.", "install_provider"],
  authentication_required: ["The selected provider requires provider-native authentication.", "provider_login"],
  no_usable_model: ["The selected provider has no usable default model.", "configure_model"],
  provider_startup_failed: ["The selected provider could not be started.", "try_again"],
  content_only_unavailable: ["The selected provider cannot enforce the required access policy.", "try_again"],
  context_too_large: ["The complete page context does not fit safely in the selected model.", "reduce_context"],
  native_session_missing: ["The provider session for this conversation is unavailable.", "restore_session"],
  provider_crashed: ["The selected provider stopped unexpectedly and the active turn was interrupted.", "retry_turn"],
  provider_recovery_failed: ["The selected provider could not recover the conversation.", "restart_provider"],
  turn_interrupted: ["The active turn was interrupted and was not replayed.", "retry_turn"],
  board_revision_unavailable: ["The current whiteboard revision is unavailable.", "reload_board"],
  board_revision_malformed: ["The current whiteboard revision is malformed.", "reload_board"],
  invalid_command: ["The broker rejected an invalid command.", "none"],
  invalid_state: ["The command is not valid for the current conversation state.", "refresh_state"],
  queue_full: ["The follow-up queue is full.", "edit_queue"],
  active_turn_conflict: ["Another turn is already active for this conversation.", "wait_for_turn"],
  stale_reference: ["The referenced conversation item is no longer current.", "refresh_state"],
  replay_window_unavailable: ["The requested replay window is no longer available.", "reload_conversation"],
  state_repair_failed: ["The broker could not repair the saved conversation state.", "try_again"],
  archive_delete_retained: ["The archive was retained because provider deletion did not complete.", "retry_archive_delete"],
  broker_shutting_down: ["The local agent broker is shutting down.", "retry_connection"],
  provider_protocol_failure: ["The provider protocol operation failed.", "restart_provider"],
  provider_malformed_stream: ["The provider returned a malformed event stream.", "restart_provider"],
  acceptance_outcome_unknown: ["The provider turn acceptance outcome is unknown.", "refresh_state"],
  invalid_model_configuration: ["The selected model settings are no longer available.", "configure_model"],
  image_input_unsupported: ["The selected model does not support image input.", "configure_model"],
  image_unsupported: ["The selected file is not a supported image.", "none"],
  image_too_large: ["The selected image is too large.", "none"],
  image_turn_limit: ["The message has too many or too much image data.", "none"],
  image_workspace_limit: ["This conversation has reached its image storage limit.", "none"],
  image_missing: ["The selected image is no longer available.", "none"],
  image_storage_failure: ["The selected image could not be stored safely.", "try_again"],
};

function isRecord(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function exactObject(value, required, optional = []) {
  if (!isRecord(value)) return false;
  const allowed = new Set([...required, ...optional]);
  const keys = Object.keys(value);
  return required.every((key) => Object.hasOwn(value, key)) && keys.every((key) => allowed.has(key));
}

function validID(value) {
  if (typeof value !== "string" || !ID_PATTERN.test(value)) return false;
  try {
    const decoded = atob(value.replaceAll("-", "+").replaceAll("_", "/"));
    return decoded.length === 24;
  } catch {
    return false;
  }
}

function validDate(value) {
  if (typeof value !== "string") return false;
  const match = DATE_PATTERN.exec(value);
  if (!match) return false;
  const [, yearText, monthText, dayText, hourText, minuteText, secondText, , , offsetHourText, offsetMinuteText] = match;
  const year = Number(yearText);
  const month = Number(monthText);
  const day = Number(dayText);
  const daysInMonth = [31, year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0) ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  const validOffset = offsetHourText === undefined || (Number(offsetHourText) <= 23 && Number(offsetMinuteText) <= 59);
  return month >= 1 && month <= 12 && day >= 1 && day <= daysInMonth[month - 1] && Number(hourText) <= 23 && Number(minuteText) <= 59 && Number(secondText) <= 59 && validOffset && Number.isFinite(Date.parse(value));
}

function validText(value, maximum, allowEmpty = false) {
  if (typeof value !== "string" || (!allowEmpty && value.length === 0) || encoder.encode(value).length > maximum) return false;
  for (const character of value) {
    const code = character.codePointAt(0);
    if (code < 0x20 && character !== "\t" && character !== "\n" && character !== "\r") return false;
  }
  return true;
}

function validImageName(value) {
  return validText(value, MAX_AGENT_IMAGE_NAME_BYTES) && value !== "." && value !== "..";
}

function validImageReference(value) {
  return exactObject(value, ["image_id", "name"]) && validID(value.image_id) && validImageName(value.name);
}

function validImageDescriptor(value) {
  return exactObject(value, ["image_id", "name", "media_type"]) && validID(value.image_id) && validImageName(value.name) && IMAGE_MEDIA_TYPES.has(value.media_type);
}

function validImages(value, validator) {
  return Array.isArray(value) && value.length <= MAX_AGENT_IMAGES_PER_TURN && value.every(validator) && new Set(value.map(({ image_id }) => image_id)).size === value.length;
}

function validTurnTextAndImages(text, images, validator = validImageDescriptor) {
  return validText(text, MAX_AGENT_MESSAGE_BYTES, true) && validImages(images, validator) && (text.length > 0 || images.length > 0);
}

function validAnchor(value) {
  return exactObject(value, ["block", "line", "offset"]) && Number.isInteger(value.block) && value.block >= 0 && Number.isInteger(value.line) && value.line >= 1 && Number.isInteger(value.offset) && value.offset >= 0;
}

function validHeading(value) {
  return exactObject(value, ["level", "title", "ordinal"]) && Number.isInteger(value.level) && value.level >= 1 && value.level <= 6 && validText(value.title, 256) && Number.isInteger(value.ordinal) && value.ordinal >= 1;
}

function validReferenceSource(value) {
  return exactObject(value, ["resource_kind", "resource_id", "resource_updated_at", "context_digest", "heading_path", "start", "end"])
    && value.resource_kind === "markdown" && validID(value.resource_id) && validDate(value.resource_updated_at) && DIGEST_PATTERN.test(value.context_digest)
    && Array.isArray(value.heading_path) && value.heading_path.length <= 12 && value.heading_path.every(validHeading)
    && validAnchor(value.start) && validAnchor(value.end)
    && (value.start.block < value.end.block || value.start.block === value.end.block && value.start.offset <= value.end.offset);
}

function validContextReference(value, event = false) {
  if (!exactObject(value, ["id", "kind", "label", "source"], ["quote", "markdown", "section_lines", "visual"]) || !validID(value.id) || !["text", "section", "image"].includes(value.kind) || !validText(value.label, 256) || !validReferenceSource(value.source)) return false;
  if (value.kind === "text") return exactObject(value, ["id", "kind", "label", "source", "quote"]) && validText(value.quote, 16 * 1024);
  if (value.kind === "section") return exactObject(value, ["id", "kind", "label", "source", "markdown", "section_lines"]) && validText(value.markdown, 48 * 1024) && exactObject(value.section_lines, ["start", "end"]) && Number.isInteger(value.section_lines.start) && value.section_lines.start >= 1 && Number.isInteger(value.section_lines.end) && value.section_lines.end > value.section_lines.start;
  if (!exactObject(value, ["id", "kind", "label", "source", "visual"]) || !isRecord(value.visual)) return false;
  const required = event ? ["image_id", "name", "alt", "media_type"] : ["image_id", "name", "alt"];
  return exactObject(value.visual, required) && validID(value.visual.image_id) && validImageName(value.visual.name) && validText(value.visual.alt, 512, true) && (!event || IMAGE_MEDIA_TYPES.has(value.visual.media_type));
}

export function validMessageContent(value, event = false) {
  if (!exactObject(value, ["parts"]) || !Array.isArray(value.parts) || value.parts.length > 64) return false;
  const ids = [];
  let previousText = false;
  for (const part of value.parts) {
    if (part?.type === "text") {
      if (!exactObject(part, ["type", "text"]) || !validText(part.text, MAX_AGENT_MESSAGE_BYTES) || previousText) return false;
      previousText = true;
    } else if (part?.type === "reference") {
      if (!exactObject(part, ["type", "reference"]) || !validContextReference(part.reference, event)) return false;
      ids.push(part.reference.id);
      previousText = false;
    } else return false;
  }
  return ids.length <= 16 && new Set(ids).size === ids.length && messageContentBytes(value) <= MAX_AGENT_MESSAGE_BYTES;
}

function inlineImages(content) {
  return content.parts.filter((part) => part.type === "reference" && part.reference.kind === "image").map((part) => ({ image_id: part.reference.visual.image_id, name: part.reference.visual.name }));
}

function messageContentForCommand(content) {
  const result = cloneMessageContent(content);
  for (const part of result.parts) {
    if (part.type === "reference" && part.reference.kind === "image") delete part.reference.visual.media_type;
  }
  return result;
}

function validContentAndImages(content, images, event = true) {
  if (!validMessageContent(content, event) || !validImages(images, event ? validImageDescriptor : validImageReference) || content.parts.length === 0 && images.length === 0) return false;
  const ids = [...inlineImages(content).map(({ image_id }) => image_id), ...images.map(({ image_id }) => image_id)];
  return ids.length <= MAX_AGENT_IMAGES_PER_TURN && new Set(ids).size === ids.length;
}

function validResource(value) {
  if (!exactObject(value, ["kind", "id", "created_at", "updated_at", "expires_at"])) return false;
  if (value.kind !== "markdown" || !validID(value.id) || !validDate(value.created_at) || !validDate(value.updated_at)) return false;
  if (value.expires_at !== null && !validDate(value.expires_at)) return false;
  const created = Date.parse(value.created_at);
  return Date.parse(value.updated_at) >= created && (value.expires_at === null || Date.parse(value.expires_at) >= created);
}

export function validateViewerPayload(value) {
  if (!isRecord(value) || typeof value.markdown !== "string" || encoder.encode(value.markdown).length > 10 * 1024 * 1024) throw new TypeError("invalid whiteboard source payload");
  if (!Object.hasOwn(value, "local_agent")) {
    if (!exactObject(value, ["markdown"])) throw new TypeError("invalid whiteboard source payload");
    return { markdown: value.markdown, context: "", local_agent: { enabled: false } };
  }
  if (!exactObject(value, ["markdown", "context", "local_agent"]) || typeof value.context !== "string" || encoder.encode(value.context).length > 1024 * 1024) {
    throw new TypeError("invalid whiteboard source payload");
  }
  const agent = value.local_agent;
  if (!exactObject(agent, ["enabled", "context_digest", "resource"]) || agent.enabled !== true || !DIGEST_PATTERN.test(agent.context_digest) || !validResource(agent.resource)) {
    throw new TypeError("invalid whiteboard source payload");
  }
  return value;
}

export function generateAgentID(cryptoObject = globalThis.crypto) {
  if (typeof cryptoObject?.getRandomValues !== "function") throw new TypeError("secure randomness is unavailable");
  const bytes = new Uint8Array(24);
  cryptoObject.getRandomValues(bytes);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/u, "");
}

export function normalizeAgentPort(value) {
  const text = typeof value === "number" ? String(value) : value;
  if (typeof text !== "string" || !/^[1-9]\d{0,4}$/u.test(text)) return DEFAULT_AGENT_PORT;
  const port = Number(text);
  return Number.isSafeInteger(port) && port <= 65535 ? port : DEFAULT_AGENT_PORT;
}

export function normalizeAgentDrawerWidth(value) {
  if (typeof value === "number") {
    return Number.isSafeInteger(value) && value >= MIN_AGENT_DRAWER_WIDTH && value <= MAX_AGENT_DRAWER_WIDTH
      ? value
      : DEFAULT_AGENT_DRAWER_WIDTH;
  }
  if (typeof value !== "string" || !/^(?:3[6-9]\d|[4-6]\d\d|7[01]\d|720)$/u.test(value)) return DEFAULT_AGENT_DRAWER_WIDTH;
  return Number(value);
}

export function maxAgentDrawerWidth(viewportWidth = globalThis.innerWidth) {
  const viewport = Number.isFinite(viewportWidth) ? viewportWidth : 0;
  return Math.max(MIN_AGENT_DRAWER_WIDTH, Math.min(MAX_AGENT_DRAWER_WIDTH, Math.floor(viewport * 0.55)));
}

export function clampAgentDrawerWidth(value, viewportWidth = globalThis.innerWidth) {
  const baseWidth = normalizeAgentDrawerWidth(value);
  return Math.min(baseWidth, maxAgentDrawerWidth(viewportWidth));
}

export function agentDrawerLayoutMode(viewportWidth = globalThis.innerWidth) {
  return Number.isFinite(viewportWidth) && viewportWidth >= AGENT_DRAWER_DOCK_BREAKPOINT ? "docked" : "modal";
}

function readBoolean(storage, key) {
  try {
    const value = storage?.getItem(key);
    return value === "true" ? true : value === "false" ? false : false;
  } catch {
    return false;
  }
}

export function readAgentPreferences(storage) {
  let port;
  let width;
  let provider;
  try {
    port = storage?.getItem(AGENT_PORT_STORAGE_KEY);
    width = storage?.getItem(AGENT_DRAWER_WIDTH_STORAGE_KEY);
    provider = storage?.getItem(AGENT_PROVIDER_STORAGE_KEY);
  } catch {
    port = undefined;
    width = undefined;
    provider = undefined;
  }
  return { open: readBoolean(storage, AGENT_DRAWER_STORAGE_KEY), port: normalizeAgentPort(port), width: normalizeAgentDrawerWidth(width), provider: PROVIDERS.has(provider) ? provider : "pi" };
}

export function persistAgentPreference(storage, key, value) {
  if (key !== AGENT_DRAWER_STORAGE_KEY && key !== AGENT_PORT_STORAGE_KEY && key !== AGENT_DRAWER_WIDTH_STORAGE_KEY && key !== AGENT_PROVIDER_STORAGE_KEY) throw new TypeError("unsupported agent preference");
  const stored = key === AGENT_DRAWER_STORAGE_KEY
    ? String(value === true)
    : key === AGENT_PORT_STORAGE_KEY
      ? String(normalizeAgentPort(value))
      : key === AGENT_PROVIDER_STORAGE_KEY
        ? (PROVIDERS.has(value) ? value : "pi")
        : String(normalizeAgentDrawerWidth(value));
  try {
    storage?.setItem(key, stored);
  } catch {
    // The drawer remains usable when browser storage is disabled.
  }
}

function agentOrigin(port) {
  return `http://127.0.0.1:${normalizeAgentPort(port)}`;
}

function commandEnvelope({ type, payload, clientID, conversationID, idFactory }) {
  const commandID = idFactory();
  if (!validID(commandID) || !validID(clientID) || (conversationID !== null && !validID(conversationID))) throw new TypeError("invalid agent command identity");
  return {
    api_version: AGENT_API_VERSION,
    command_id: commandID,
    client_id: clientID,
    conversation_id: conversationID,
    type,
    payload,
  };
}

export function createConnectCommand({ payload, provider = "pi", settings = null, clientID, replayAfter, idFactory = generateAgentID }) {
  validateViewerPayload(payload);
  if (payload.local_agent.enabled !== true) throw new TypeError("local agent is disabled");
  if (!PROVIDERS.has(provider) || provider === "pi" && settings !== null || settings !== null && !validExecutionSettings(settings)) throw new TypeError("invalid agent provider settings");
  if (replayAfter && !validID(replayAfter)) throw new TypeError("invalid replay event ID");
  const connectPayload = {
    provider,
    resource: { ...payload.local_agent.resource },
    context_digest: payload.local_agent.context_digest,
    settings: cloneExecutionSettings(settings),
  };
  if (replayAfter) connectPayload.replay_after = replayAfter;
  return commandEnvelope({ type: "connect", payload: connectPayload, clientID, conversationID: null, idFactory });
}

export function createPageContext(payload, { title, url, revision }) {
  let parsedURL;
  try { parsedURL = new URL(url); } catch { throw new TypeError("invalid page context"); }
  const rawHTTPOrigin = /^http:\/\/[^/?#]+/.exec(url)?.[0];
  const allowedOrigin = parsedURL.protocol === "https:" || (parsedURL.protocol === "http:" && parsedURL.hostname === "127.0.0.1" && rawHTTPOrigin === parsedURL.origin);
  if (!["initial", "replacement"].includes(revision) || !validText(title, 512) || !validText(url, 8 * 1024) || !allowedOrigin || parsedURL.username || parsedURL.password || !parsedURL.hostname) throw new TypeError("invalid page context");
  return {
    revision,
    markdown: payload.markdown,
    creator_context: payload.context,
    title,
    url,
    resource: { ...payload.local_agent.resource },
    digest: payload.local_agent.context_digest,
  };
}

export function createSubmitCommand({ content, message, images = [], payload, provider = "pi", settings = null, clientID, conversationID, title, url, revision, idFactory = generateAgentID }) {
  let orderedContent;
  try { orderedContent = content ?? normalizeMessageContent({ parts: message ? [{ type: "text", text: message }] : [] }); }
  catch { throw new TypeError("invalid agent message"); }
  if (!validContentAndImages(orderedContent, images, false) || (revision !== undefined && !["initial", "replacement"].includes(revision))) throw new TypeError("invalid agent message");
  if (!PROVIDERS.has(provider) || provider === "pi" && settings !== null || provider === "codex" && !validExecutionSettings(settings)) throw new TypeError("invalid agent provider settings");
  if (!validID(conversationID)) throw new TypeError("invalid agent conversation");
  const turnID = idFactory();
  const messageID = idFactory();
  if (!validID(turnID) || !validID(messageID)) throw new TypeError("invalid agent command identity");
  const submitPayload = { turn_id: turnID, message_id: messageID, content: cloneMessageContent(orderedContent), settings: cloneExecutionSettings(settings) };
  if (images.length > 0) submitPayload.images = images.map((image) => ({ ...image }));
  if (revision === "initial" || revision === "replacement") {
    submitPayload.context = createPageContext(payload, { title, url, revision });
  }
  return commandEnvelope({ type: "submit", payload: submitPayload, clientID, conversationID, idFactory });
}

export function createAgentCommand({ type, payload, clientID, conversationID, idFactory = generateAgentID }) {
  if (!validID(conversationID)) throw new TypeError("invalid agent conversation");
  const validators = {
    queue_edit: () => exactObject(payload, ["message_id", "content"]) && validID(payload.message_id) && validMessageContent(payload.content),
    queue_remove: () => exactObject(payload, ["message_id"]) && validID(payload.message_id),
    interrupt: () => exactObject(payload, ["turn_id"]) && validID(payload.turn_id),
    new: () => exactObject(payload, ["settings"]) && (payload.settings === null || validExecutionSettings(payload.settings)),
    archive_list: () => exactObject(payload, [], ["before", "limit"]) && (!payload.before || validID(payload.before)) && (!Object.hasOwn(payload, "limit") || Number.isInteger(payload.limit) && payload.limit >= 0 && payload.limit <= 100),
    history_page: () => exactObject(payload, [], ["before", "limit"]) && (!payload.before || validID(payload.before)) && (!Object.hasOwn(payload, "limit") || Number.isInteger(payload.limit) && payload.limit >= 0 && payload.limit <= 100),
    archive_restore: () => exactObject(payload, ["archive_id"]) && validID(payload.archive_id),
    archive_delete: () => exactObject(payload, ["archive_id"]) && validID(payload.archive_id),
    resync: () => exactObject(payload, [], ["after_event_id"]) && (!payload.after_event_id || validID(payload.after_event_id)),
    interaction_respond: () => exactObject(payload, ["request_id", "kind", "option_id", "answers"]) && validID(payload.request_id) && validInteractionKind(payload.kind) && (payload.option_id === "" || validInteractionKey(payload.option_id)) && validInteractionAnswers(payload.answers) && (payload.option_id !== "" || (payload.answers !== null && Object.keys(payload.answers).length > 0)),
  };
  if (!validators[type]?.()) throw new TypeError("invalid agent command");
  return commandEnvelope({ type, payload, clientID, conversationID, idFactory });
}

function validBrowserError(value) {
  if (!exactObject(value, ["code", "message", "action"])) return false;
  const definition = ERROR_DEFINITIONS[value.code];
  return definition?.[0] === value.message && definition?.[1] === value.action;
}

function validQueueItem(item) {
  return exactObject(item, ["turn_id", "message_id", "content", "settings"], ["images"])
    && validID(item.turn_id) && validID(item.message_id) && validContentAndImages(item.content, item.images ?? [])
    && (item.settings === null || validPresentedExecutionSettings(item.settings));
}

function queueItemBytes(item) {
  const settings = item.settings;
  return messageContentBytes(item.content) + (settings ? encoder.encode(settings.model + settings.effort + settings.speed + settings.model_display_name).length : 0);
}

function validTimelineItem(item) {
  if (!isRecord(item) || !validID(item.item_id) || !["user", "assistant", "activity"].includes(item.kind) || !validDate(item.created_at)) return false;
  const images = item.images ?? [];
  if (item.kind === "activity") return exactObject(item, ["item_id", "kind", "text", "created_at"], ["turn_id"]) && validText(item.text, MAX_AGENT_MESSAGE_BYTES) && (!Object.hasOwn(item, "turn_id") || validID(item.turn_id));
  if (item.kind === "assistant") return exactObject(item, ["item_id", "kind", "turn_id", "message_id", "text", "created_at"]) && validText(item.text, MAX_AGENT_MESSAGE_BYTES) && validID(item.turn_id) && validID(item.message_id);
  return exactObject(item, ["item_id", "kind", "turn_id", "message_id", "content", "created_at"], ["images"]) && validID(item.turn_id) && validID(item.message_id) && validContentAndImages(item.content, images);
}

function validArchiveItem(item) {
  return exactObject(item, ["archive_id", "created_at", "updated_at", "provider"], ["model", "preview"]) && validID(item.archive_id) && validDate(item.created_at) && validDate(item.updated_at) && Date.parse(item.updated_at) >= Date.parse(item.created_at) && PROVIDERS.has(item.provider) && (!Object.hasOwn(item, "model") || validText(item.model, 512, true)) && (!Object.hasOwn(item, "preview") || validText(item.preview, 512, true));
}

const TOOL_KINDS = new Set(["command", "file_change", "mcp", "web", "image", "collaboration", "plan", "other"]);
const TOOL_STATUSES = new Set(["running", "completed", "failed", "interrupted"]);
const INTERACTION_KINDS = new Set(["command_approval", "file_change_approval", "permission_approval", "user_input", "mcp_elicitation"]);
const FIELD_TYPES = new Set(["text", "number", "boolean", "select", "multi_select"]);
function validInteractionKey(value) { return typeof value === "string" && INTERACTION_KEY_PATTERN.test(value); }
function validInteractionKind(value) { return INTERACTION_KINDS.has(value); }
function validInteractionOption(value) { return exactObject(value, ["id", "label", "description"]) && validInteractionKey(value.id) && validText(value.label, 512) && validText(value.description, 8192, true); }
function validInteractionOptions(values) {
  return (values === null || Array.isArray(values)) && (values?.length ?? 0) <= 16 && (values ?? []).every(validInteractionOption) && new Set((values ?? []).map(({ id }) => id)).size === (values?.length ?? 0);
}
function interactionOptionIDsEqual(options, expected) {
  const ids = (options ?? []).map(({ id }) => id);
  return ids.length === expected.length && expected.every((id) => ids.includes(id));
}
function validInteractionAnswers(value) {
  if (value === null) return true;
  if (!isRecord(value) || Object.keys(value).length > 32) return false;
  let bytes = 0;
  return Object.entries(value).every(([key, answers]) => validInteractionKey(key) && Array.isArray(answers) && answers.length > 0 && answers.length <= 32 && answers.every((answer) => { bytes += encoder.encode(answer).length; return validText(answer, 64 * 1024) && bytes <= 64 * 1024; }));
}
function validToolActivity(payload) {
  return exactObject(payload, ["activity_id", "kind", "status", "title", "summary", "detail"], ["turn_id"]) && validID(payload.activity_id) && (!Object.hasOwn(payload, "turn_id") || validID(payload.turn_id)) && TOOL_KINDS.has(payload.kind) && TOOL_STATUSES.has(payload.status) && validText(payload.title, 512) && validText(payload.summary, 8192, true) && validText(payload.detail, 64 * 1024, true);
}
function validInteractionRequest(payload) {
  if (!exactObject(payload, ["request_id", "kind", "title", "summary", "command", "working_directory", "options", "questions", "fields"], ["turn_id"]) || !validID(payload.request_id) || (Object.hasOwn(payload, "turn_id") && !validID(payload.turn_id)) || !validInteractionKind(payload.kind) || !validText(payload.title, 512) || !validText(payload.summary, 8192, true) || !validText(payload.command, 64 * 1024, true) || !validText(payload.working_directory, 8192, true) || !validInteractionOptions(payload.options) || (payload.questions !== null && !Array.isArray(payload.questions)) || (payload.questions?.length ?? 0) > 3 || (payload.fields !== null && !Array.isArray(payload.fields)) || (payload.fields?.length ?? 0) > 32) return false;
  const validQuestion = (question) => exactObject(question, ["id", "header", "prompt", "options", "allow_other", "secret", "multiple"]) && validInteractionKey(question.id) && validText(question.header, 512) && validText(question.prompt, 8192) && validInteractionOptions(question.options) && typeof question.allow_other === "boolean" && typeof question.secret === "boolean" && typeof question.multiple === "boolean" && ((question.options?.length ?? 0) > 0 || question.allow_other);
  const validField = (field) => exactObject(field, ["id", "label", "description", "type", "required", "secret", "options"]) && validInteractionKey(field.id) && validText(field.label, 512) && validText(field.description, 8192, true) && FIELD_TYPES.has(field.type) && typeof field.required === "boolean" && typeof field.secret === "boolean" && validInteractionOptions(field.options) && (["select", "multi_select"].includes(field.type) ? (field.options?.length ?? 0) > 0 : (field.options?.length ?? 0) === 0);
  const questions = payload.questions ?? [];
  const fields = payload.fields ?? [];
  const structuredIDs = [...questions, ...fields].map(({ id }) => id);
  if (!questions.every(validQuestion) || !fields.every(validField) || new Set(structuredIDs).size !== structuredIDs.length) return false;
  switch (payload.kind) {
    case "command_approval":
    case "file_change_approval":
      return (payload.options?.length ?? 0) > 0 && questions.length === 0 && fields.length === 0;
    case "permission_approval":
      return interactionOptionIDsEqual(payload.options, ["grantTurn", "grantSession", "decline"]) && questions.length === 0 && fields.length === 1 && fields[0].id === "permissions" && fields[0].type === "multi_select";
    case "user_input":
      return (payload.options?.length ?? 0) === 0 && questions.length > 0 && fields.length === 0;
    case "mcp_elicitation":
      return interactionOptionIDsEqual(payload.options, ["accept", "decline", "cancel"]) && questions.length === 0;
    default:
      return false;
  }
}

const lifecycleValues = new Set(["connecting", "ready", "responding", "interrupted", "unavailable"]);
const contextValues = new Set(["pending", "accepted", "unchanged", "unavailable"]);
const providerValues = new Set(["starting", "ready", "unavailable", "recovering"]);

function validActiveTurn(lifecycle, turnID) {
  if (turnID !== null && !validID(turnID)) return false;
  return lifecycle === "responding" ? turnID !== null : turnID === null;
}

function validSettingsSnapshot(settingsState, effectiveSettings, catalog) {
  if (!validModelCatalog(catalog)) return false;
  if (settingsState === null) return effectiveSettings === null && catalog.length === 0;
  if (settingsState === "unverified") return effectiveSettings === null;
  if (settingsState !== "verified" || !validPresentedExecutionSettings(effectiveSettings)) return false;
  if (!effectiveSettings.selectable) return true;
  const model = catalog.find(({ model }) => model === effectiveSettings.model);
  return model !== undefined
    && model.model_display_name === effectiveSettings.model_display_name
    && settingsCompatibility(catalog, cloneExecutionSettings(effectiveSettings)).compatible;
}

function validateEventPayload(type, payload) {
  switch (type) {
    case "snapshot": {
      if (!exactObject(payload, ["lifecycle", "queue", "context_state", "active_turn_id", "supports_images", "settings_state", "effective_settings", "catalog"])
        || !lifecycleValues.has(payload.lifecycle) || !Array.isArray(payload.queue) || payload.queue.length > MAX_QUEUE_ITEMS || !payload.queue.every(validQueueItem)
        || !contextValues.has(payload.context_state) || !validActiveTurn(payload.lifecycle, payload.active_turn_id) || typeof payload.supports_images !== "boolean"
        || !validSettingsSnapshot(payload.settings_state, payload.effective_settings, payload.catalog)) return false;
      const selected = payload.effective_settings?.selectable && payload.catalog.find(({ model }) => model === payload.effective_settings.model);
      return !selected || selected.supports_images === payload.supports_images;
    }
    case "command_result":
      return exactObject(payload, ["command_id", "status"], ["error"]) && validID(payload.command_id) && ["succeeded", "rejected"].includes(payload.status) && (payload.status === "succeeded" ? !Object.hasOwn(payload, "error") : validBrowserError(payload.error));
    case "timeline":
      return exactObject(payload, ["command_id", "items", "next_cursor"]) && validID(payload.command_id) && Array.isArray(payload.items) && payload.items.length <= 100 && payload.items.every(validTimelineItem) && (payload.next_cursor === null || validID(payload.next_cursor));
    case "history":
      return exactObject(payload, ["command_id", "items", "next_cursor"]) && validID(payload.command_id) && Array.isArray(payload.items) && payload.items.length <= 100 && payload.items.every(validArchiveItem) && (payload.next_cursor === null || validID(payload.next_cursor));
    case "user_message":
      return exactObject(payload, ["turn_id", "message_id", "content", "created_at"], ["images"]) && validID(payload.turn_id) && validID(payload.message_id) && validContentAndImages(payload.content, payload.images ?? []) && validDate(payload.created_at);
    case "assistant_message":
      return exactObject(payload, ["turn_id", "message_id", "text", "created_at"]) && validID(payload.turn_id) && validID(payload.message_id) && validText(payload.text, MAX_AGENT_MESSAGE_BYTES) && validDate(payload.created_at);
    case "assistant_delta":
      return exactObject(payload, ["turn_id", "message_id", "text"]) && validID(payload.turn_id) && validID(payload.message_id) && validText(payload.text, MAX_DELTA_BYTES);
    case "queue":
      return exactObject(payload, ["items"]) && Array.isArray(payload.items) && payload.items.length <= MAX_QUEUE_ITEMS && payload.items.every(validQueueItem);
    case "lifecycle":
      return exactObject(payload, ["state", "turn_id"]) && lifecycleValues.has(payload.state) && validActiveTurn(payload.state, payload.turn_id);
    case "provider":
      return exactObject(payload, ["provider", "state", "supports_images"], ["model"]) && PROVIDERS.has(payload.provider) && providerValues.has(payload.state) && typeof payload.supports_images === "boolean" && (!Object.hasOwn(payload, "model") || validText(payload.model, 512, true)) && (payload.state !== "ready" || validText(payload.model, 512));
    case "settings":
      return exactObject(payload, ["settings_state", "effective_settings", "catalog", "accepted_turn_id"])
        && validSettingsSnapshot(payload.settings_state, payload.effective_settings, payload.catalog)
        && payload.settings_state !== null
        && (payload.accepted_turn_id === null || validID(payload.accepted_turn_id))
        && (payload.settings_state === "verified" || payload.accepted_turn_id === null);
    case "context":
      return exactObject(payload, ["digest", "state"]) && DIGEST_PATTERN.test(payload.digest) && contextValues.has(payload.state);
    case "activity":
      return exactObject(payload, ["kind", "summary"]) && ["status", "visible_summary", "retry", "compaction"].includes(payload.kind) && validText(payload.summary, 8192);
    case "blocked":
      return exactObject(payload, ["kind", "message"]) && ((payload.kind === "tool" && payload.message === "A provider tool request was blocked by content-only policy.") || (payload.kind === "permission" && payload.message === "A provider permission request was blocked by content-only policy."));
    case "error":
      return exactObject(payload, ["error"]) && validBrowserError(payload.error);
    case "completion":
      return exactObject(payload, ["turn_id"]) && validID(payload.turn_id);
    case "interruption":
      return exactObject(payload, ["turn_id", "reason"]) && validID(payload.turn_id) && ["requested", "provider_exit", "shutdown"].includes(payload.reason);
    case "archive":
      return exactObject(payload, ["action", "archive_id"]) && ["created", "restored", "deleted"].includes(payload.action) && validID(payload.archive_id);
    case "tool_activity":
      return validToolActivity(payload);
    case "interaction_request":
      return validInteractionRequest(payload);
    case "interaction_resolved":
      return exactObject(payload, ["request_id", "kind", "option_id"]) && validID(payload.request_id) && validInteractionKind(payload.kind) && (payload.option_id === "" || validInteractionKey(payload.option_id));
    default:
      return false;
  }
}

export function validateAgentEvent(value) {
  if (!exactObject(value, ["api_version", "event_id", "conversation_id", "type", "timestamp", "payload"]) || value.api_version !== AGENT_API_VERSION || !validID(value.event_id) || !validID(value.conversation_id) || !validDate(value.timestamp) || !validateEventPayload(value.type, value.payload)) {
    throw new TypeError("invalid agent event");
  }
  const payload = value.payload;
  if (value.type === "timeline" && (new Set(payload.items.map((item) => item.item_id)).size !== payload.items.length || payload.items.reduce((total, item) => total + (item.content ? messageContentBytes(item.content) : encoder.encode(item.text).length), 0) > MAX_TIMELINE_TEXT_BYTES || (payload.next_cursor !== null && (payload.items.length === 0 || payload.next_cursor !== payload.items.at(-1).item_id)))) throw new TypeError("invalid agent event");
  if (value.type === "history" && (new Set(payload.items.map((item) => item.archive_id)).size !== payload.items.length || (payload.next_cursor !== null && (payload.items.length === 0 || payload.next_cursor !== payload.items.at(-1).archive_id)))) throw new TypeError("invalid agent event");
  if (["snapshot", "queue"].includes(value.type)) {
    const items = value.type === "snapshot" ? payload.queue : payload.items;
    if (new Set(items.map((item) => item.message_id)).size !== items.length || new Set(items.map((item) => item.turn_id)).size !== items.length || items.reduce((total, item) => total + queueItemBytes(item), 0) > 96 * 1024) throw new TypeError("invalid agent event");
  }
  return value;
}

function parseJSONWithoutDuplicateKeys(source) {
  let index = 0;
  let depth = 0;
  const skipWhitespace = () => { while (/\s/u.test(source[index] ?? "")) index += 1; };
  function parseString() {
    const start = index++;
    while (index < source.length) {
      if (source[index] === "\\") { index += 2; continue; }
      if (source[index++] === '"') return JSON.parse(source.slice(start, index));
    }
    throw new SyntaxError("unterminated JSON string");
  }
  function parseValue() {
    skipWhitespace();
    if (++depth > 64) throw new SyntaxError("JSON nesting is too deep");
    const character = source[index];
    if (character === "{") {
      index += 1;
      const keys = new Set();
      skipWhitespace();
      if (source[index] === "}") index += 1;
      else for (;;) {
        skipWhitespace();
        if (source[index] !== '"') throw new SyntaxError("invalid JSON object key");
        const key = parseString();
        if (keys.has(key)) throw new SyntaxError("duplicate JSON object key");
        keys.add(key);
        skipWhitespace();
        if (source[index++] !== ":") throw new SyntaxError("invalid JSON object");
        parseValue();
        skipWhitespace();
        const delimiter = source[index++];
        if (delimiter === "}") break;
        if (delimiter !== ",") throw new SyntaxError("invalid JSON object");
      }
    } else if (character === "[") {
      index += 1;
      skipWhitespace();
      if (source[index] === "]") index += 1;
      else for (;;) {
        parseValue();
        skipWhitespace();
        const delimiter = source[index++];
        if (delimiter === "]") break;
        if (delimiter !== ",") throw new SyntaxError("invalid JSON array");
      }
    } else if (character === '"') {
      parseString();
    } else {
      const start = index;
      while (index < source.length && !/[\s,}\]]/u.test(source[index])) index += 1;
      JSON.parse(source.slice(start, index));
    }
    depth -= 1;
  }
  parseValue();
  skipWhitespace();
  if (index !== source.length) throw new SyntaxError("trailing JSON value");
  return JSON.parse(source);
}

export function decodeAgentEvent(source) {
  if (typeof source !== "string" || encoder.encode(source).length > MAX_AGENT_EVENT_BYTES) throw new TypeError("invalid agent event frame");
  let value;
  try {
    value = parseJSONWithoutDuplicateKeys(source);
  } catch {
    throw new TypeError("invalid agent event frame");
  }
  return validateAgentEvent(value);
}

async function readBoundedResponseText(response, maximum) {
  if (response.body?.getReader) {
    const reader = response.body.getReader();
    const decoder = new TextDecoder("utf-8", { fatal: true });
    let total = 0;
    let text = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > maximum) {
        void reader.cancel();
        throw new TypeError("agent response is too large");
      }
      text += decoder.decode(value, { stream: true });
    }
    return text + decoder.decode();
  }
  const text = await response.text();
  if (encoder.encode(text).length > maximum) throw new TypeError("agent response is too large");
  return text;
}

function parseStrictJSONOrNull(source) {
  try { return parseJSONWithoutDuplicateKeys(source); }
  catch { return null; }
}

export function createAgentState(provider = "pi") {
  if (!PROVIDERS.has(provider)) throw new TypeError("invalid agent provider");
  return {
    conversationID: null,
    lifecycle: "connecting",
    activeTurnID: null,
    contextState: "pending",
    contextDigest: null,
    provider: { provider, state: "starting", model: "", supportsImages: false },
    supportsImages: false,
    settingsState: null,
    effectiveSettings: null,
    catalog: [],
    timeline: [],
    queue: [],
    archives: [],
    timelineCursor: null,
    archiveCursor: null,
    errors: [],
    lastEventID: null,
    seenEventIDs: new Set(),
    pendingCommandIDs: new Set(),
    knownCommandIDs: new Set(),
    freshArchiveCommandIDs: new Set(),
    connected: false,
    interactions: [],
  };
}

function appendTimeline(state, item) {
  const key = item.item_id ?? item.message_id ?? `${item.kind}-${state.lastEventID}`;
  if (state.timeline.some((current) => (current.item_id ?? current.message_id) === key)) return;
  state.timeline.push({ ...item, content: item.content ? cloneMessageContent(item.content) : undefined, images: item.images?.map((image) => ({ ...image })) });
  if (state.timeline.length > MAX_TIMELINE_ITEMS) state.timeline.splice(0, state.timeline.length - MAX_TIMELINE_ITEMS);
}

function trimResolvedInteractions(state) {
  while (state.interactions.length > MAX_RETAINED_INTERACTIONS) {
    const resolvedIndex = state.interactions.findIndex((item) => item.resolved);
    if (resolvedIndex < 0) throw new TypeError("too many pending agent interactions");
    state.interactions.splice(resolvedIndex, 1);
  }
}

export function registerAgentCommand(state, command) {
  if (!validID(command?.command_id) || state.pendingCommandIDs.size >= 128) throw new TypeError("invalid pending agent command");
  while (state.knownCommandIDs.size >= 1024) {
    const oldest = state.knownCommandIDs.values().next().value;
    if (state.pendingCommandIDs.has(oldest)) throw new TypeError("agent command correlation window is full");
    state.knownCommandIDs.delete(oldest);
  }
  state.pendingCommandIDs.add(command.command_id);
  state.knownCommandIDs.add(command.command_id);
}

export function applyAgentEvent(state, untrustedEvent) {
  const draft = {
    ...state,
    provider: { ...state.provider },
    effectiveSettings: state.effectiveSettings ? { ...state.effectiveSettings } : null,
    catalog: state.catalog.map((model) => ({ ...model, supported_reasoning_efforts: model.supported_reasoning_efforts.map((effort) => ({ ...effort })) })),
    timeline: state.timeline.map((item) => ({ ...item, content: item.content ? cloneMessageContent(item.content) : undefined, images: item.images?.map((image) => ({ ...image })) })),
    queue: state.queue.map((item) => ({ ...item, content: cloneMessageContent(item.content), images: item.images?.map((image) => ({ ...image })), settings: item.settings ? { ...item.settings } : null })),
    archives: state.archives.map((item) => ({ ...item })),
    errors: state.errors.map((item) => ({ ...item })),
    seenEventIDs: new Set(state.seenEventIDs),
    pendingCommandIDs: new Set(state.pendingCommandIDs),
    knownCommandIDs: new Set(state.knownCommandIDs),
    freshArchiveCommandIDs: new Set(state.freshArchiveCommandIDs),
    interactions: state.interactions.map((item) => ({ ...item })),
  };
  const changed = applyAgentEventMutable(draft, untrustedEvent);
  if (changed) Object.assign(state, draft);
  return changed;
}

function applyAgentEventMutable(state, untrustedEvent) {
  const event = validateAgentEvent(untrustedEvent);
  if (state.conversationID !== null && state.conversationID !== event.conversation_id) throw new TypeError("agent conversation changed unexpectedly");
  if (state.seenEventIDs.has(event.event_id)) return false;
  if (["command_result", "timeline", "history"].includes(event.type) && !state.knownCommandIDs.has(event.payload.command_id)) throw new TypeError("uncorrelated agent command event");
  state.conversationID = event.conversation_id;
  state.lastEventID = event.event_id;
  state.connected = true;
  state.seenEventIDs.add(event.event_id);
  while (state.seenEventIDs.size > MAX_STATE_EVENTS) state.seenEventIDs.delete(state.seenEventIDs.values().next().value);
  const payload = event.payload;
  switch (event.type) {
    case "snapshot":
      state.lifecycle = payload.lifecycle;
      state.activeTurnID = payload.active_turn_id;
      state.contextState = payload.context_state;
      state.queue = payload.queue.map((item) => ({ ...item, content: cloneMessageContent(item.content), images: item.images?.map((image) => ({ ...image })), settings: item.settings ? { ...item.settings } : null }));
      state.supportsImages = payload.supports_images;
      state.provider.supportsImages = payload.supports_images;
      state.settingsState = payload.settings_state;
      state.effectiveSettings = payload.effective_settings ? { ...payload.effective_settings } : null;
      state.catalog = payload.catalog.map((model) => ({ ...model, supported_reasoning_efforts: model.supported_reasoning_efforts.map((effort) => ({ ...effort })) }));
      state.connected = true;
      break;
    case "timeline": {
      const known = new Set(state.timeline.map((item) => item.item_id ?? item.message_id));
      const older = payload.items.filter((item) => !known.has(item.item_id)).map((item) => ({ ...item, content: item.content ? cloneMessageContent(item.content) : undefined, images: item.images?.map((image) => ({ ...image })) })).reverse();
      state.timeline = [...older, ...state.timeline].slice(-MAX_TIMELINE_ITEMS);
      state.timelineCursor = payload.next_cursor;
      break;
    }
    case "history": {
      if (state.freshArchiveCommandIDs.delete(payload.command_id)) {
        state.archives = payload.items.map((item) => ({ ...item })).slice(0, MAX_ARCHIVES);
      } else {
        const known = new Set(state.archives.map((item) => item.archive_id));
        state.archives = [...state.archives, ...payload.items.filter((item) => !known.has(item.archive_id)).map((item) => ({ ...item }))].slice(0, MAX_ARCHIVES);
      }
      state.archiveCursor = payload.next_cursor;
      break;
    }
    case "user_message":
      appendTimeline(state, { kind: "user", ...payload });
      break;
    case "assistant_delta": {
      const current = state.timeline.find((item) => item.kind === "assistant" && item.message_id === payload.message_id);
      if (current) {
        const next = `${current.text}${payload.text}`;
        if (encoder.encode(next).length > MAX_AGENT_MESSAGE_BYTES) throw new TypeError("agent stream message is too large");
        current.text = next;
      } else appendTimeline(state, { kind: "assistant", ...payload, text: payload.text, streaming: true });
      break;
    }
    case "assistant_message": {
      const current = state.timeline.find((item) => item.kind === "assistant" && item.message_id === payload.message_id);
      if (current) Object.assign(current, payload, { streaming: false });
      else appendTimeline(state, { kind: "assistant", ...payload });
      break;
    }
    case "queue": state.queue = payload.items.map((item) => ({ ...item, content: cloneMessageContent(item.content), images: item.images?.map((image) => ({ ...image })), settings: item.settings ? { ...item.settings } : null })); break;
    case "settings":
      state.settingsState = payload.settings_state;
      state.effectiveSettings = payload.effective_settings ? { ...payload.effective_settings } : null;
      state.catalog = payload.catalog.map((model) => ({ ...model, supported_reasoning_efforts: model.supported_reasoning_efforts.map((effort) => ({ ...effort })) }));
      break;
    case "lifecycle": state.lifecycle = payload.state; state.activeTurnID = payload.turn_id; break;
    case "provider":
      if (payload.provider !== state.provider.provider) throw new TypeError("agent provider changed unexpectedly");
      state.provider = { provider: payload.provider, state: payload.state, model: payload.model ?? "", supportsImages: payload.supports_images };
      state.supportsImages = payload.supports_images;
      break;
    case "context": state.contextDigest = payload.digest; state.contextState = payload.state; break;
    case "activity": appendTimeline(state, { kind: "activity", activity: payload.kind, text: payload.summary, created_at: event.timestamp, item_id: event.event_id }); break;
    case "blocked": appendTimeline(state, { kind: "activity", activity: "blocked", blockedKind: payload.kind, text: payload.message, created_at: event.timestamp, item_id: event.event_id, expanded: true }); break;
    case "error": state.errors.push({ ...payload.error }); state.errors = state.errors.slice(-20); appendTimeline(state, { kind: "activity", activity: "error", text: payload.error.message, action: payload.error.action, created_at: event.timestamp, item_id: event.event_id, expanded: true }); break;
    case "completion": state.lifecycle = "ready"; state.activeTurnID = null; break;
    case "interruption": state.lifecycle = "interrupted"; state.activeTurnID = null; appendTimeline(state, { kind: "activity", activity: "interruption", text: "The active response was interrupted and was not replayed automatically.", created_at: event.timestamp, item_id: event.event_id, expanded: true }); break;
    case "archive":
      if (payload.action === "deleted" || payload.action === "restored") state.archives = state.archives.filter((item) => item.archive_id !== payload.archive_id);
      break;
    case "tool_activity": {
      const current = state.timeline.find((item) => item.kind === "tool" && item.activity_id === payload.activity_id);
      if (current) Object.assign(current, payload, { kind: "tool", tool_kind: payload.kind });
      else appendTimeline(state, { ...payload, kind: "tool", tool_kind: payload.kind, item_id: payload.activity_id, created_at: event.timestamp });
      break;
    }
    case "interaction_request":
      if (!state.interactions.some((item) => item.request_id === payload.request_id)) {
        if (state.interactions.filter((item) => !item.resolved).length >= MAX_PENDING_INTERACTIONS) throw new TypeError("too many pending agent interactions");
        state.interactions.push({ ...payload, options: payload.options ?? [], questions: payload.questions ?? [], fields: payload.fields ?? [], resolved: false, submitting: false, responseCommandID: null });
        trimResolvedInteractions(state);
      }
      break;
    case "interaction_resolved": {
      const request = state.interactions.find((item) => item.request_id === payload.request_id);
      if (request?.kind !== undefined && request.kind !== payload.kind) throw new TypeError("interaction kind changed unexpectedly");
      if (request) Object.assign(request, { resolved: true, submitting: false, responseCommandID: null, option_id: payload.option_id });
      trimResolvedInteractions(state);
      break;
    }
    case "command_result":
      state.pendingCommandIDs.delete(payload.command_id);
      if (payload.status === "rejected") {
        const request = state.interactions.find((item) => item.responseCommandID === payload.command_id);
        if (request) Object.assign(request, { submitting: false, responseCommandID: null });
      }
      if (payload.status === "rejected") {
        state.freshArchiveCommandIDs.delete(payload.command_id);
        state.errors.push({ ...payload.error });
        appendTimeline(state, { kind: "activity", activity: "error", text: payload.error.message, action: payload.error.action, created_at: event.timestamp, item_id: event.event_id, expanded: true });
      }
      state.errors = state.errors.slice(-20);
      break;
  }
  return true;
}

export function renderAgentMarkdown(source, doc = document) {
  const markdown = createMarkdownRenderer([], { mermaidEnabled: false });
  const rendered = markdown.render(source);
  const sanitized = purifierFor(doc).sanitize(rendered, { FORBID_TAGS: ["img", "picture", "source", "audio", "video"] });
  const template = doc.createElement("template");
  template.innerHTML = sanitized;
  highlightCode(template.content);
  return template.innerHTML;
}

function safeHTTPErrorCode(body, fallback) {
  try {
    return validBrowserError(body?.error) ? body.error.code : fallback;
  } catch {
    return fallback;
  }
}

export function createAgentTransport({
  payload,
  provider = "pi",
  port = DEFAULT_AGENT_PORT,
  clientID = generateAgentID(),
  fetchImpl = globalThis.fetch?.bind(globalThis),
  WebSocketImpl = globalThis.WebSocket,
  onEvent = () => {},
  onDisconnect = () => {},
  initialSettings = null,
  idFactory = generateAgentID,
} = {}) {
  let socket;
  let fallbackAbort;
  let currentPort = normalizeAgentPort(port);
  let conversationID = null;
  let lastEventID = null;
  let consented = false;
  let transportKind = null;
  let closed = false;
  const seenEventIDs = new Set();

  function acceptFrame(frame, { requireSnapshot = false } = {}) {
    const event = decodeAgentEvent(frame);
    if (requireSnapshot && event.type !== "snapshot") throw new TypeError("fresh agent connection must begin with a snapshot");
    if (conversationID !== null && event.conversation_id !== conversationID) throw new TypeError("agent conversation changed unexpectedly");
    if (seenEventIDs.has(event.event_id)) return event;
    onEvent(event);
    conversationID = event.conversation_id;
    seenEventIDs.add(event.event_id);
    while (seenEventIDs.size > MAX_STATE_EVENTS) seenEventIDs.delete(seenEventIDs.values().next().value);
    lastEventID = event.event_id;
    return event;
  }

  async function probe() {
    const response = await fetchImpl(`${agentOrigin(currentPort)}/api/v1/agent/status`, {
      method: "GET",
      credentials: "omit",
      referrerPolicy: "no-referrer",
      cache: "no-store",
    });
    const responseText = await readBoundedResponseText(response, 4096);
    const body = parseStrictJSONOrNull(responseText);
    if (!response.ok) return { ok: false, code: safeHTTPErrorCode(body, "wrong_port") };
    if (!exactObject(body, ["available", "api_version", "origin_trusted"]) || body.available !== true) return { ok: false, code: "wrong_port" };
    if (body.api_version !== AGENT_API_VERSION) return { ok: false, code: "incompatible_api" };
    if (body.origin_trusted !== true) return { ok: false, code: "untrusted_origin" };
    return { ok: true, code: null };
  }

  function imageHeaders(targetConversationID, mediaType = null, purpose = "attachment") {
    if (!validID(targetConversationID)) throw new TypeError("invalid agent conversation");
    const headers = {
      "X-Agent-Whiteboard-API-Version": AGENT_API_VERSION,
      "X-Agent-Whiteboard-Client-ID": clientID,
      "X-Agent-Whiteboard-Conversation-ID": targetConversationID,
    };
    if (mediaType !== null) {
      if (!IMAGE_MEDIA_TYPES.has(mediaType)) throw new TypeError("unsupported image type");
      headers["Content-Type"] = mediaType;
      headers["X-Agent-Whiteboard-Provider"] = provider;
      if (purpose === "inline_reference") headers["X-Agent-Whiteboard-Image-Purpose"] = purpose;
    }
    return headers;
  }

  async function imageFailure(response, fallback) {
    let body = null;
    try { body = parseStrictJSONOrNull(await readBoundedResponseText(response, 4096)); } catch { body = null; }
    const code = safeHTTPErrorCode(body, fallback);
    const error = new Error(code);
    error.code = code;
    throw error;
  }

  async function uploadImage(file, targetConversationID = conversationID, signal, purpose = "attachment") {
    if (!file || typeof file.size !== "number" || file.size <= 0 || file.size > MAX_AGENT_IMAGE_BYTES || !IMAGE_MEDIA_TYPES.has(file.type)) throw new TypeError("invalid image");
    const response = await fetchImpl(`${agentOrigin(currentPort)}/api/v1/agent/images`, {
      method: "POST",
      headers: imageHeaders(targetConversationID, file.type, purpose),
      body: file,
      credentials: "omit",
      referrerPolicy: "no-referrer",
      cache: "no-store",
      signal,
    });
    if (!response.ok) return imageFailure(response, "image_storage_failure");
    const body = parseStrictJSONOrNull(await readBoundedResponseText(response, 4096));
    if (!exactObject(body, ["image_id", "media_type", "bytes"]) || !validID(body.image_id) || !IMAGE_MEDIA_TYPES.has(body.media_type) || !Number.isSafeInteger(body.bytes) || body.bytes <= 0 || body.bytes > MAX_AGENT_IMAGE_BYTES) throw new TypeError("invalid image response");
    return body;
  }

  async function readImage(imageID, targetConversationID = conversationID, signal) {
    if (!validID(imageID)) throw new TypeError("invalid image ID");
    const response = await fetchImpl(`${agentOrigin(currentPort)}/api/v1/agent/images/${imageID}`, {
      method: "GET",
      headers: imageHeaders(targetConversationID),
      credentials: "omit",
      referrerPolicy: "no-referrer",
      cache: "no-store",
      signal,
    });
    if (!response.ok) return imageFailure(response, "image_missing");
    const mediaType = response.headers?.get?.("Content-Type")?.split(";", 1)[0]?.trim();
    if (!IMAGE_MEDIA_TYPES.has(mediaType)) throw new TypeError("invalid image response");
    const blob = await response.blob();
    if (blob.size <= 0 || blob.size > MAX_AGENT_IMAGE_BYTES) throw new TypeError("invalid image response");
    return blob.type === mediaType ? blob : new Blob([blob], { type: mediaType });
  }

  async function deleteImage(imageID, targetConversationID = conversationID, signal) {
    if (!validID(imageID)) throw new TypeError("invalid image ID");
    const response = await fetchImpl(`${agentOrigin(currentPort)}/api/v1/agent/images/${imageID}`, {
      method: "DELETE",
      headers: imageHeaders(targetConversationID),
      credentials: "omit",
      referrerPolicy: "no-referrer",
      cache: "no-store",
      signal,
    });
    if (!response.ok) return imageFailure(response, "image_missing");
  }

  function connectCommand() {
    const settings = typeof initialSettings === "function" ? initialSettings() : initialSettings;
    return createConnectCommand({ payload, provider, settings: settings ?? null, clientID, replayAfter: lastEventID, idFactory });
  }

  async function fallbackConnect(command) {
    transportKind = "fallback";
    fallbackAbort = new AbortController();
    const response = await fetchImpl(`${agentOrigin(currentPort)}/api/v1/agent/connect`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Agent-Whiteboard-API-Version": AGENT_API_VERSION },
      body: JSON.stringify(command),
      credentials: "omit",
      referrerPolicy: "no-referrer",
      cache: "no-store",
      signal: fallbackAbort.signal,
    });
    if (!response.ok || !response.body?.getReader) {
      let body = null;
      try { body = parseStrictJSONOrNull(await readBoundedResponseText(response, 4096)); } catch { body = null; }
      const error = new Error(safeHTTPErrorCode(body, "broker_unavailable"));
      error.code = safeHTTPErrorCode(body, "broker_unavailable");
      throw error;
    }
    const reader = response.body.getReader();
    const textDecoder = new TextDecoder("utf-8", { fatal: true });
    let buffered = "";
    let firstEvent;
    while (!closed) {
      const { value, done } = await reader.read();
      if (done) break;
      buffered += textDecoder.decode(value, { stream: true });
      if (encoder.encode(buffered).length > MAX_AGENT_EVENT_BYTES * 2) throw new TypeError("agent stream line is too large");
      for (;;) {
        const newline = buffered.indexOf("\n");
        if (newline < 0) break;
        const line = buffered.slice(0, newline);
        buffered = buffered.slice(newline + 1);
        if (!line || line.endsWith("\r")) throw new TypeError("invalid agent stream line");
        const accepted = acceptFrame(line, { requireSnapshot: firstEvent === undefined && !Object.hasOwn(command.payload, "replay_after") });
        firstEvent ??= accepted;
        if (firstEvent) return { firstEvent, reader, buffered, textDecoder };
      }
    }
    throw new Error("broker_unavailable");
  }

  async function continueFallback(reader, initialBuffer = "", textDecoder = new TextDecoder("utf-8", { fatal: true })) {
    let buffered = initialBuffer;
    try {
      while (!closed) {
        for (;;) {
          const newline = buffered.indexOf("\n");
          if (newline < 0) break;
          const line = buffered.slice(0, newline);
          buffered = buffered.slice(newline + 1);
          if (!line || line.endsWith("\r")) throw new TypeError("invalid agent stream line");
          acceptFrame(line);
        }
        const { value, done } = await reader.read();
        if (done) break;
        buffered += textDecoder.decode(value, { stream: true });
        if (encoder.encode(buffered).length > MAX_AGENT_EVENT_BYTES * 2) throw new TypeError("agent stream line is too large");
      }
      buffered += textDecoder.decode();
      if (buffered) throw new TypeError("truncated agent stream line");
      if (!closed) onDisconnect(new Error("agent connection closed"));
    } catch (error) {
      if (error instanceof TypeError) error.protocolViolation = true;
      if (!closed) onDisconnect(error);
    }
  }

  function websocketConnect(command) {
    return new Promise((resolve, reject) => {
      if (typeof WebSocketImpl !== "function") { reject(new Error("websocket unavailable")); return; }
      let settled = false;
      const ws = new WebSocketImpl(`${agentOrigin(currentPort).replace("http:", "ws:")}/api/v1/agent/connect`, AGENT_WEBSOCKET_PROTOCOL);
      const timer = setTimeout(() => {
        if (!settled) {
          ws.close();
          reject(new Error("websocket connection timed out"));
        }
      }, AGENT_CONNECT_TIMEOUT_MS);
      socket = ws;
      ws.addEventListener("open", () => {
        if (ws.protocol !== AGENT_WEBSOCKET_PROTOCOL) { clearTimeout(timer); ws.close(); reject(new Error("incompatible websocket")); return; }
        ws.send(JSON.stringify(command));
      });
      ws.addEventListener("message", (message) => {
        try {
          if (typeof message.data !== "string") throw new TypeError("text agent frames required");
          const event = acceptFrame(message.data, { requireSnapshot: !settled && !Object.hasOwn(command.payload, "replay_after") });
          if (!settled) { settled = true; clearTimeout(timer); transportKind = "websocket"; resolve(event); }
        } catch (error) {
          clearTimeout(timer);
          const failure = error instanceof Error ? error : new Error("invalid agent event");
          failure.protocolViolation = true;
          ws.close();
          if (!settled) reject(failure); else onDisconnect(failure);
        }
      });
      ws.addEventListener("error", () => { if (!settled) { clearTimeout(timer); reject(new Error("websocket handshake failed")); } });
      ws.addEventListener("close", () => {
        clearTimeout(timer);
        if (!settled) reject(new Error("websocket handshake failed"));
        else if (!closed) onDisconnect(new Error("agent connection closed"));
      });
    });
  }

  async function connect() {
    if (!consented) throw new Error("agent consent required");
    closed = false;
    const command = connectCommand();
    try {
      return await websocketConnect(command);
    } catch (error) {
      socket?.close?.();
      if (error?.protocolViolation) throw error;
      const connected = await fallbackConnect(command);
      void continueFallback(connected.reader, connected.buffered, connected.textDecoder);
      return connected.firstEvent;
    }
  }

  async function send(command) {
    if (!consented || !conversationID) throw new Error("agent is not connected");
    if (transportKind === "websocket" && socket?.readyState === 1) {
      socket.send(JSON.stringify(command));
      return;
    }
    const response = await fetchImpl(`${agentOrigin(currentPort)}/api/v1/agent/commands`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Agent-Whiteboard-API-Version": AGENT_API_VERSION },
      body: JSON.stringify(command),
      credentials: "omit",
      referrerPolicy: "no-referrer",
      cache: "no-store",
    });
    let responseText = "";
    try { responseText = await readBoundedResponseText(response, MAX_AGENT_EVENT_BYTES); } catch { responseText = ""; }
    if (!response.ok) {
      const body = parseStrictJSONOrNull(responseText);
      const error = new Error(safeHTTPErrorCode(body, "broker_unavailable"));
      error.code = safeHTTPErrorCode(body, "broker_unavailable");
      throw error;
    }
    if (!responseText) throw new TypeError("missing agent command result");
    if (conversationID !== command.conversation_id) throw new TypeError("stale agent command result");
    const result = acceptFrame(responseText);
    if (result.type !== "command_result" || result.payload.command_id !== command.command_id) throw new TypeError("uncorrelated agent command result");
  }

  return {
    clientID,
    get conversationID() { return conversationID; },
    get lastEventID() { return lastEventID; },
    get transportKind() { return transportKind; },
    get consented() { return consented; },
    probe,
    setPort(value) { currentPort = normalizeAgentPort(value); },
    grantConsent() { consented = true; },
    connect,
    reconnect: connect,
    resetConversation() { conversationID = null; lastEventID = null; seenEventIDs.clear(); },
    resetReplay() { lastEventID = null; seenEventIDs.clear(); },
    send,
    uploadImage,
    readImage,
    deleteImage,
    close() { closed = true; socket?.close?.(); fallbackAbort?.abort(); transportKind = null; },
  };
}

function actionGuidance(action, doc) {
  const origin = doc.location?.origin ?? "this HTTPS origin";
  const guidance = {
    none: "",
    retry_connection: "Check the broker and try connecting again.",
    edit_port: "Check the configured broker port.",
    grant_local_network: "Allow Local Network Access for this site in Chrome, then check again.",
    trust_origin: `Trust this exact origin with: agent-whiteboard agent trust add ${origin}`,
    update_broker: "Update agent-whiteboard so the browser and broker API versions match.",
    install_provider: "Install the selected provider executable, then restart the broker.",
    provider_login: "Complete provider-native login in a terminal, then try again.",
    configure_model: "Configure a usable default model for the selected provider, then try again.",
    try_again: "Try the operation again; if it still fails, restart the broker.",
    restart_provider: "Restart the local agent broker before trying again.",
    reduce_context: "Reduce the complete page Markdown or creator context before trying again.",
    restore_session: "Restore an available archive or start a new conversation.",
    retry_turn: "Send a new message when ready; interrupted turns are never replayed automatically.",
    reload_board: "Reload the whiteboard to obtain its current complete revision.",
    refresh_state: "Reconnect to refresh the conversation state; no turn will be resubmitted.",
    edit_queue: "Edit or remove a queued follow-up before trying again.",
    wait_for_turn: "Wait for the active turn to finish or stop it first.",
    reload_conversation: "Reconnect for a fresh snapshot; no turn will be resubmitted.",
    retry_archive_delete: "Try deleting the retained archive again.",
  };
  return guidance[action] ?? "";
}

function browserErrorText(code, doc, fallback) {
  const definition = ERROR_DEFINITIONS[code];
  if (!definition) return fallback;
  const guidance = actionGuidance(definition[1], doc);
  return guidance ? `${definition[0]} ${guidance}` : definition[0];
}

function appendAgentMessage(doc, container, item, providerName = "Pi", appendImages = () => {}, onReference = () => {}) {
  const article = doc.createElement("article");
  article.className = `agent-message agent-message-${item.kind}`;
  const label = doc.createElement("strong");
  label.className = "agent-message-author";
  label.textContent = item.kind === "assistant" ? providerName : "You";
  const body = doc.createElement("div");
  body.className = "agent-message-body";
  if (item.kind === "user" && item.content) {
    const textOnly = item.content.parts.length === 1 && item.content.parts[0].type === "text";
    if (textOnly) body.innerHTML = renderAgentMarkdown(item.content.parts[0].text, doc);
    else body.append(renderMessageContent(doc, item.content, { onReference }));
  } else body.innerHTML = renderAgentMarkdown(item.text, doc);
  article.append(label, body);
  appendImages(article, item.images ?? []);
  container.append(article);
}

function appendToolActivity(doc, container, item) {
  const details = doc.createElement("details");
  details.className = `agent-tool-activity agent-tool-${item.status}`;
  details.dataset.status = item.status;
  details.open = item.status === "failed" || item.status === "interrupted";
  const summary = doc.createElement("summary");
  const title = doc.createElement("strong");
  title.textContent = item.title;
  const status = doc.createElement("span");
  status.textContent = item.status;
  summary.append(title, status);
  const copy = doc.createElement("p");
  copy.textContent = item.summary;
  details.append(summary, copy);
  if (item.detail) {
    const detail = doc.createElement("pre");
    detail.textContent = item.detail;
    details.append(detail);
  }
  container.append(details);
}

function appendInteractionCard(doc, container, request, respond) {
  const disabled = request.resolved || request.submitting;
  const card = doc.createElement("article");
  card.className = "agent-interaction";
  card.dataset.kind = request.kind;
  card.dataset.state = request.resolved ? "resolved" : request.submitting ? "submitting" : "pending";
  card.setAttribute("aria-label", `${request.title} · ${request.resolved ? "Resolved" : request.submitting ? "Submitting" : "Response requested"}`);
  const heading = doc.createElement("h3");
  heading.textContent = request.title;
  const status = doc.createElement("p");
  status.className = "agent-interaction-status";
  status.setAttribute("role", "status");
  const selected = request.options.find((option) => option.id === request.option_id)?.label;
  status.textContent = request.resolved ? `Resolved${selected ? ` · ${selected}` : ""}` : request.submitting ? "Submitting response…" : "Response requested";
  const summary = doc.createElement("p");
  summary.className = "agent-interaction-summary";
  summary.textContent = request.summary;
  card.append(heading, status, summary);
  if (request.command) {
    const command = doc.createElement("pre");
    command.setAttribute("aria-label", "Requested command");
    command.textContent = request.command;
    card.append(command);
  }
  if (request.working_directory) {
    const directory = doc.createElement("p");
    directory.className = "agent-interaction-directory";
    directory.textContent = `Working directory: ${request.working_directory}`;
    card.append(directory);
  }
  const form = doc.createElement("form");
  form.className = "agent-interaction-form";
  form.setAttribute("aria-disabled", String(disabled));
  const controls = new Map();
  for (const question of request.questions) {
    const fieldset = doc.createElement("fieldset");
    fieldset.disabled = disabled;
    const legend = doc.createElement("legend");
    legend.textContent = question.header;
    const prompt = doc.createElement("p");
    prompt.textContent = question.prompt;
    fieldset.append(legend, prompt);
    const values = [];
    for (const option of question.options ?? []) {
      const label = doc.createElement("label");
      const input = doc.createElement("input");
      input.type = question.multiple ? "checkbox" : "radio";
      input.name = `interaction-${request.request_id}-${question.id}`;
      input.value = option.id;
      const copy = doc.createElement("span");
      copy.textContent = option.label;
      label.append(input, copy);
      if (option.description) {
        const description = doc.createElement("small");
        description.textContent = option.description;
        label.append(description);
      }
      fieldset.append(label);
      values.push(input);
    }
    let other = null;
    if (question.allow_other || (question.options?.length ?? 0) === 0) {
      const otherLabel = doc.createElement("label");
      const otherCopy = doc.createElement("span");
      otherCopy.textContent = question.allow_other ? "Other answer" : "Answer";
      other = doc.createElement("input");
      other.type = question.secret ? "password" : "text";
      otherLabel.append(otherCopy, other);
      fieldset.append(otherLabel);
    }
    controls.set(question.id, () => [...values.filter((input) => input.checked).map((input) => input.value), ...(other?.value ? [other.value] : [])]);
    form.append(fieldset);
  }
  for (const field of request.fields) {
    const wrapper = doc.createElement("div");
    wrapper.className = "agent-interaction-field";
    const label = doc.createElement("label");
    const fieldID = `interaction-${request.request_id}-${field.id}`;
    label.htmlFor = fieldID;
    label.textContent = field.label;
    let input;
    if (field.type === "select" || field.type === "multi_select") {
      input = doc.createElement("select");
      input.multiple = field.type === "multi_select";
      if (!field.required && !input.multiple) input.append(doc.createElement("option"));
      for (const option of field.options ?? []) {
        const item = doc.createElement("option");
        item.value = option.id;
        item.textContent = option.label;
        input.append(item);
      }
    } else {
      input = doc.createElement("input");
      input.type = field.secret ? "password" : field.type === "number" ? "number" : field.type === "boolean" ? "checkbox" : "text";
    }
    input.id = fieldID;
    input.required = field.required && field.type !== "boolean";
    if (field.required) input.setAttribute("aria-required", "true");
    input.disabled = disabled;
    wrapper.append(label, input);
    if (field.description) {
      const description = doc.createElement("p");
      description.id = `${fieldID}-description`;
      description.textContent = field.description;
      input.setAttribute("aria-describedby", description.id);
      wrapper.append(description);
    }
    controls.set(field.id, () => field.type === "multi_select"
      ? [...input.selectedOptions].map((option) => option.value)
      : field.type === "boolean"
        ? [String(input.checked)]
        : input.value ? [input.value] : []);
    form.append(wrapper);
  }
  const collectAnswers = () => {
    const answers = {};
    for (const [key, collect] of controls) {
      const values = collect();
      if (values.length > 0) answers[key] = values;
    }
    return answers;
  };
  const actions = doc.createElement("div");
  actions.className = "agent-interaction-actions";
  if (request.options.length > 0) {
    for (const option of request.options) {
      const button = doc.createElement("button");
      button.type = "button";
      button.textContent = option.label;
      button.title = option.description;
      button.disabled = disabled;
      button.addEventListener("click", () => {
        if (request.resolved || request.submitting) return;
        const answers = collectAnswers();
        if (request.kind === "mcp_elicitation" && option.id === "accept" && !form.reportValidity()) return;
        if (request.kind === "permission_approval" && (option.id === "grantTurn" || option.id === "grantSession") && (answers.permissions?.length ?? 0) === 0) {
          const permissionControl = form.querySelector('select[multiple]');
          permissionControl?.setCustomValidity("Select at least one permission to grant.");
          permissionControl?.reportValidity();
          return;
        }
        void respond(option.id, answers);
      });
      actions.append(button);
    }
  } else {
    const button = doc.createElement("button");
    button.type = "submit";
    button.textContent = "Submit answer";
    button.disabled = disabled;
    actions.append(button);
  }
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    if (request.resolved || request.submitting || !form.reportValidity()) return;
    const answers = collectAnswers();
    const invalidQuestion = request.questions.find((question) => {
      const values = answers[question.id] ?? [];
      return values.length === 0 || (!question.multiple && values.length !== 1);
    });
    if (invalidQuestion || Object.keys(answers).length === 0) {
      const firstControl = form.querySelector("input, select");
      firstControl?.setCustomValidity(invalidQuestion ? "Answer every question with the allowed number of choices." : "Provide at least one answer.");
      firstControl?.reportValidity();
      return;
    }
    void respond("", answers);
  });
  form.addEventListener("input", () => {
    for (const control of form.querySelectorAll("input, select")) control.setCustomValidity("");
  });
  form.append(actions);
  card.append(form);
  container.append(card);
}

export function createAgentDrawer({ payload, doc = document, storage = browserStorage(doc), transportFactory = createAgentTransport, pageTitle = doc.title, pageURL = doc.location.href, onReference = () => false } = {}) {
  const preferences = readAgentPreferences(storage);
  let selectedProvider = preferences.provider;
  let state;
  let transport;
  let controller;
  const controllers = new Map();
  let open = preferences.open;
  let port = preferences.port;
  let baseWidth = preferences.width;
  let effectiveWidth = clampAgentDrawerWidth(baseWidth, doc.defaultView?.innerWidth);
  let resizing = false;
  let resizePointerID = null;
  let resizeStartWidth = baseWidth;
  let resizePreviousUserSelect = "";
  let restoreFocus;
  let contextRevision;
  let contextAccepted = false;
  let contextCommandID = null;
  let contextDeliveryUnknown = false;
  let destroyed = false;
  let toastTimer = null;
  let handoffCommandID = null;
  let attachmentSerial = 0;
  let draftAttachments = [];
  const draftInlineVisuals = new Map();
  let queueEditors = [];
  const imageObjectURLs = new Map();
  const imageLoads = new Map();
  let pendingSubmitCommandID = null;
  let activeView = "conversation";
  let timelineScrollTop = 0;
  let followTimeline = true;
  let layoutWasModal = false;
  let brokerState = "checking";
  let brokerCode = "broker_unavailable";
  let brokerGuidance = "Checking for a compatible local broker. No page content has been shared.";
  let showView = () => {};

  const toggle = doc.createElement("button");
  toggle.type = "button";
  toggle.className = "agent-toggle";
  toggle.setAttribute("aria-controls", "agent-whiteboard-agent-drawer");
  toggle.setAttribute("aria-label", "Open Page agent");
  toggle.setAttribute("aria-expanded", "false");

  const statusDot = doc.createElement("span");
  statusDot.className = "agent-status-dot";
  statusDot.setAttribute("aria-hidden", "true");
  const toggleText = doc.createElement("span");
  toggleText.textContent = "Page agent";
  toggle.append(statusDot, toggleText);

  const overlay = doc.createElement("button");
  overlay.type = "button";
  overlay.className = "agent-overlay";
  overlay.setAttribute("aria-label", "Close local agent");

  const toast = doc.createElement("div");
  toast.className = "agent-toast";
  toast.setAttribute("role", "status");
  toast.setAttribute("aria-live", "polite");
  toast.setAttribute("aria-atomic", "true");
  toast.hidden = true;

  const drawer = doc.createElement("aside");
  drawer.id = "agent-whiteboard-agent-drawer";
  drawer.className = "agent-drawer";
  drawer.setAttribute("role", "complementary");
  drawer.setAttribute("aria-label", "Local agent conversation");
  drawer.setAttribute("aria-modal", "false");

  const header = doc.createElement("header");
  header.className = "agent-drawer-header";
  const headerIdentity = doc.createElement("div");
  headerIdentity.className = "agent-header-identity";
  const agentGlyph = doc.createElement("span");
  agentGlyph.className = "agent-header-glyph";
  agentGlyph.setAttribute("aria-hidden", "true");
  agentGlyph.textContent = "P";
  const headerCopy = doc.createElement("div");
  headerCopy.className = "agent-header-copy";
  const heading = doc.createElement("h2");
  heading.textContent = "Page agent";
  const headerSubtitle = doc.createElement("div");
  headerSubtitle.className = "agent-header-subtitle";
  const providerSelect = doc.createElement("select");
  providerSelect.className = "agent-provider-select";
  providerSelect.setAttribute("aria-label", "Conversation provider");
  for (const [value, label] of [["pi", "Pi"], ["codex", "Codex"]]) {
    const option = doc.createElement("option");
    option.value = value;
    option.textContent = label;
    providerSelect.append(option);
  }
  providerSelect.value = selectedProvider;
  const backButton = doc.createElement("button");
  backButton.type = "button";
  backButton.className = "agent-back-button";
  backButton.setAttribute("aria-label", "Back to conversation");
  backButton.textContent = "‹";
  backButton.hidden = true;
  headerCopy.append(heading, headerSubtitle);
  headerIdentity.append(agentGlyph, backButton, headerCopy);
  const headerActions = doc.createElement("div");
  headerActions.className = "agent-header-actions";
  const close = doc.createElement("button");
  close.type = "button";
  close.className = "agent-icon-button agent-close-button";
  close.setAttribute("aria-label", "Close local agent");
  close.textContent = "×";

  const statusCopy = doc.createElement("div");
  statusCopy.className = "agent-status-copy";
  const headerStatusDot = doc.createElement("span");
  headerStatusDot.className = "agent-status-dot";
  headerStatusDot.setAttribute("aria-hidden", "true");
  const liveStatus = doc.createElement("p");
  liveStatus.className = "agent-live-status";
  liveStatus.setAttribute("role", "status");
  liveStatus.setAttribute("aria-live", "polite");
  liveStatus.textContent = "Checking local broker…";
  const providerLabel = doc.createElement("span");
  providerLabel.className = "agent-provider-label";
  statusCopy.append(headerStatusDot, liveStatus);
  headerSubtitle.append(statusCopy, providerLabel);

  const setup = doc.createElement("section");
  setup.className = "agent-setup";
  const setupBody = doc.createElement("div");
  setupBody.className = "agent-setup-body";
  const setupIcon = doc.createElement("span");
  setupIcon.className = "agent-setup-icon";
  setupIcon.setAttribute("aria-hidden", "true");
  setupIcon.textContent = "⌁";
  const setupHeading = doc.createElement("h3");
  setupHeading.textContent = "Checking local broker…";

  const settings = doc.createElement("section");
  settings.className = "agent-settings";
  settings.hidden = true;
  const settingsHeading = doc.createElement("h3");
  settingsHeading.textContent = "Local connection";
  const portLabel = doc.createElement("label");
  portLabel.textContent = "Broker port";
  const portInput = doc.createElement("input");
  portInput.type = "number";
  portInput.min = "1";
  portInput.max = "65535";
  portInput.inputMode = "numeric";
  portInput.value = String(port);
  portInput.setAttribute("aria-label", "Local agent broker port");
  portLabel.append(portInput);
  const consentDisclosure = doc.createElement("p");
  consentDisclosure.className = "agent-consent";
  consentDisclosure.textContent = brokerGuidance;
  const consentList = doc.createElement("ul");
  consentList.className = "agent-consent-list";
  const contextListItem = doc.createElement("li");
  contextListItem.textContent = "Complete Markdown and creator notes on the first message";
  const accessListItem = doc.createElement("li");
  accessListItem.textContent = "Pi has no tools, files, network, or project access";
  consentList.append(contextListItem, accessListItem);
  consentList.hidden = true;
  const guidance = doc.createElement("p");
  guidance.className = "agent-guidance";
  const checkButton = doc.createElement("button");
  checkButton.type = "button";
  checkButton.textContent = "Check again";
  const setupCheckButton = doc.createElement("button");
  setupCheckButton.type = "button";
  setupCheckButton.textContent = "Check again";
  const directSettingsButton = doc.createElement("button");
  directSettingsButton.type = "button";
  directSettingsButton.textContent = "Connection settings";
  const connectButton = doc.createElement("button");
  connectButton.type = "button";
  connectButton.className = "agent-primary";
  connectButton.textContent = "Connect to Pi";
  connectButton.hidden = true;
  const setupButtons = doc.createElement("div");
  setupButtons.className = "agent-setup-buttons";
  setupButtons.append(setupCheckButton, directSettingsButton, connectButton);
  setupBody.append(setupIcon, setupHeading, consentDisclosure, consentList, setupButtons);
  const contextDisclosure = doc.createElement("button");
  contextDisclosure.type = "button";
  contextDisclosure.className = "agent-context-disclosure";
  const contextDisclosureIcon = doc.createElement("span");
  contextDisclosureIcon.className = "agent-context-disclosure-icon";
  contextDisclosureIcon.setAttribute("aria-hidden", "true");
  contextDisclosureIcon.textContent = "≡";
  const contextDisclosureCopy = doc.createElement("div");
  const contextDisclosureHeading = doc.createElement("strong");
  contextDisclosureHeading.textContent = "Page context";
  const contextDisclosureDescription = doc.createElement("span");
  contextDisclosureDescription.textContent = "Markdown + creator notes";
  contextDisclosureCopy.append(contextDisclosureHeading, contextDisclosureDescription);
  const contextDisclosureChevron = doc.createElement("span");
  contextDisclosureChevron.className = "agent-context-disclosure-chevron";
  contextDisclosureChevron.setAttribute("aria-hidden", "true");
  contextDisclosureChevron.textContent = "›";
  contextDisclosure.append(contextDisclosureIcon, contextDisclosureCopy, contextDisclosureChevron);
  setup.append(setupBody, contextDisclosure);
  settings.append(settingsHeading, portLabel, guidance, checkButton);

  const actions = doc.createElement("div");
  actions.className = "agent-actions";
  const reconnectButton = doc.createElement("button"); reconnectButton.type = "button"; reconnectButton.textContent = "Reconnect";
  const newButton = doc.createElement("button"); newButton.type = "button"; newButton.textContent = "New";
  const historyButton = doc.createElement("button"); historyButton.type = "button"; historyButton.textContent = "Archives";
  actions.append(reconnectButton, newButton, historyButton);
  actions.hidden = true;

  const overflow = doc.createElement("div");
  overflow.className = "agent-overflow";
  const overflowButton = doc.createElement("button");
  overflowButton.type = "button";
  overflowButton.className = "agent-icon-button agent-overflow-button";
  overflowButton.setAttribute("aria-label", "Open Page agent menu");
  overflowButton.setAttribute("aria-expanded", "false");
  overflowButton.setAttribute("aria-haspopup", "menu");
  overflowButton.textContent = "⋯";
  const overflowMenu = doc.createElement("div");
  overflowMenu.className = "agent-overflow-menu";
  overflowMenu.setAttribute("role", "menu");
  overflowMenu.hidden = true;
  const menuButton = (label, action) => {
    const button = doc.createElement("button");
    button.type = "button";
    button.textContent = label;
    button.setAttribute("role", "menuitem");
    button.addEventListener("click", () => {
      overflowMenu.hidden = true;
      overflowButton.setAttribute("aria-expanded", "false");
      action();
      if (doc.activeElement === button) overflowButton.focus();
    });
    overflowMenu.append(button);
    return button;
  };
  const newMenuButton = menuButton("New conversation", () => newButton.click());
  const archivesMenuButton = menuButton("Archives", () => historyButton.click());
  const reconnectMenuButton = menuButton("Reconnect", () => reconnectButton.click());
  const settingsMenuButton = menuButton("Connection settings", () => showView("settings"));
  const contextMenuButton = menuButton("Inspect page context", () => showView("context"));
  overflow.append(overflowButton, overflowMenu);
  headerActions.append(providerSelect, close, overflow);
  header.append(headerIdentity, headerActions);

  const contextDetails = doc.createElement("section");
  contextDetails.className = "agent-context";
  const contextIntro = doc.createElement("div");
  contextIntro.className = "agent-context-intro";
  const contextIntroHeading = doc.createElement("h3");
  contextIntroHeading.textContent = "What the agent receives";
  const contextIntroCopy = doc.createElement("p");
  contextIntroCopy.textContent = "Review the page material. Expand only the part you need.";
  contextIntro.append(contextIntroHeading, contextIntroCopy);
  const contextCard = ({ title, description, content, label, open = false }) => {
    const details = doc.createElement("details");
    details.className = "agent-context-card";
    details.open = open;
    const summary = doc.createElement("summary");
    const summaryCopy = doc.createElement("span");
    const summaryTitle = doc.createElement("strong");
    summaryTitle.textContent = title;
    const summaryDescription = doc.createElement("span");
    summaryDescription.textContent = description;
    summaryCopy.append(summaryTitle, summaryDescription);
    summary.append(summaryCopy);
    const preview = doc.createElement("pre");
    preview.className = "agent-context-content";
    preview.textContent = content;
    preview.tabIndex = 0;
    preview.setAttribute("aria-label", label);
    details.append(summary, preview);
    return details;
  };
  const markdownCard = contextCard({ title: "Page Markdown", description: "Original page content", content: payload.markdown, label: "Page Markdown", open: true });
  const creatorCard = contextCard({ title: "Creator notes", description: "Notes supplied by the creator", content: payload.context, label: "Creator notes" });
  const contextPrivacy = doc.createElement("p");
  contextPrivacy.className = "agent-context-privacy";
  contextPrivacy.textContent = "Context is included with the next message that needs it—not when you simply open this panel.";
  contextDetails.append(contextIntro, markdownCard, creatorCard, contextPrivacy);

  const timeline = doc.createElement("section");
  timeline.className = "agent-timeline";
  timeline.setAttribute("aria-label", "Conversation");

  const queue = doc.createElement("section");
  queue.className = "agent-queue";
  queue.setAttribute("aria-label", "Queued follow-ups");

  const archives = doc.createElement("section");
  archives.className = "agent-archives";
  archives.setAttribute("aria-label", "Conversation archives");
  archives.hidden = true;

  const composerWrap = doc.createElement("div");
  composerWrap.className = "agent-composer-wrap";
  const composer = doc.createElement("form");
  composer.className = "agent-composer";
  const attachmentList = doc.createElement("div");
  attachmentList.className = "agent-attachment-list";
  attachmentList.setAttribute("aria-label", "Image attachments");
  const attachmentStatus = doc.createElement("p");
  attachmentStatus.className = "agent-attachment-status";
  attachmentStatus.setAttribute("role", "status");
  attachmentStatus.setAttribute("aria-live", "polite");
  let messageEditor;
  messageEditor = createMessageEditor({
    doc,
    onChange: (content) => { handleDraftContentChange(content); resizeComposer(); updateComposerAvailability(); },
    onSubmit: () => composer.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true })),
  });
  const message = messageEditor.element;
  const composerBar = doc.createElement("div");
  composerBar.className = "agent-composer-bar";
  const imagePicker = doc.createElement("input");
  imagePicker.type = "file";
  imagePicker.multiple = true;
  imagePicker.accept = "image/png,image/jpeg,image/gif,image/webp";
  imagePicker.className = "agent-image-picker";
  imagePicker.tabIndex = -1;
  const imageButton = doc.createElement("button");
  imageButton.type = "button";
  imageButton.className = "agent-image-button";
  imageButton.setAttribute("aria-label", "Add images");
  const imageIcon = createSVGElement(doc, "svg", {
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    "stroke-width": "1.8",
    "stroke-linecap": "round",
    "stroke-linejoin": "round",
    "aria-hidden": "true",
  });
  imageIcon.append(
    createSVGElement(doc, "rect", { x: "3.5", y: "4.5", width: "17", height: "15", rx: "2.5" }),
    createSVGElement(doc, "circle", { cx: "9", cy: "10", r: "1.5" }),
    createSVGElement(doc, "path", { d: "m5.5 17 4.2-4.2 2.8 2.8 2.3-2.3 3.7 3.7" }),
  );
  imageButton.append(imageIcon);
  const contextChip = doc.createElement("button");
  contextChip.type = "button";
  contextChip.className = "agent-composer-chip";
  contextChip.textContent = "Context · available";
  const queueChip = doc.createElement("button");
  queueChip.type = "button";
  queueChip.className = "agent-composer-chip";
  queueChip.textContent = "Queue · 0";
  queueChip.hidden = true;
  const modelControl = createModelSettingsControl({ doc, onSelect: (next) => selectDraftSettings(next) });
  const stopButton = doc.createElement("button");
  stopButton.type = "button";
  stopButton.className = "agent-stop-button";
  stopButton.textContent = "Stop";
  const sendButton = doc.createElement("button");
  sendButton.type = "submit";
  sendButton.className = "agent-send-button";
  sendButton.setAttribute("aria-label", "Send");
  sendButton.textContent = "↑";
  composerBar.append(imageButton, contextChip, queueChip, modelControl.element, stopButton, sendButton);
  composer.append(imagePicker, attachmentList, attachmentStatus, message, composerBar);
  const composerFineprint = doc.createElement("p");
  composerFineprint.className = "agent-composer-fineprint";
  composerFineprint.textContent = "Pi can make mistakes. Review important details.";
  composerWrap.append(composer, composerFineprint);

  const separator = doc.createElement("div");
  separator.className = "agent-drawer-separator";
  separator.setAttribute("role", "separator");
  separator.setAttribute("aria-orientation", "vertical");
  separator.tabIndex = 0;
  separator.setAttribute("aria-valuemin", String(MIN_AGENT_DRAWER_WIDTH));
  separator.setAttribute("aria-valuemax", String(maxAgentDrawerWidth(doc.defaultView?.innerWidth)));
  separator.setAttribute("aria-valuenow", String(effectiveWidth));
  separator.setAttribute("aria-label", "Resize Page agent pane");

  drawer.append(header, separator, setup, settings, actions, contextDetails, timeline, queue, archives, composerWrap);
  doc.body.append(overlay, drawer, toggle, toast);

  function buildController(provider) {
    const initialSettings = provider === "codex" ? readCodexSettingsPreference(storage) : null;
    const owned = { provider, state: createAgentState(provider), settingsDraft: createCodexDraftState(initialSettings), transport: null, reconnectTimer: null, connecting: false, contextRevision: undefined, contextAccepted: false, contextCommandID: null, contextDeliveryUnknown: false, handoffCommandID: null, pendingSubmitCommandID: null, pendingSubmission: null };
    owned.transport = transportFactory({
      payload,
      provider,
      port,
      initialSettings: () => provider === "codex" ? cloneExecutionSettings(owned.settingsDraft.draft) : null,
      onEvent(event) {
        if (!applyAgentEvent(owned.state, event)) return;
        if (provider === "codex" && (event.type === "snapshot" || event.type === "settings")) {
          reconcileCodexDraft(owned.settingsDraft, {
            identity: event.conversation_id,
            settingsState: event.payload.settings_state,
            effectiveSettings: event.payload.effective_settings,
            catalog: event.payload.catalog,
            acceptedTurnID: event.type === "settings" ? event.payload.accepted_turn_id : null,
          });
          if (event.type === "settings" && event.payload.settings_state === "verified" && event.payload.accepted_turn_id !== null) {
            writeCodexSettingsPreference(storage, cloneExecutionSettings(event.payload.effective_settings));
          }
        }
        if (event.type === "command_result" && event.payload.command_id === owned.handoffCommandID && event.payload.status === "rejected") owned.handoffCommandID = null;
        if (event.type === "command_result" && event.payload.command_id === owned.pendingSubmitCommandID) {
          if (event.payload.status === "succeeded") completePendingSubmission(owned);
          owned.pendingSubmitCommandID = null;
          owned.pendingSubmission = null;
        }
        if (event.type === "command_result" && event.payload.command_id === owned.contextCommandID && event.payload.status === "rejected") owned.contextCommandID = null;
        if (event.type === "snapshot" && owned.contextDeliveryUnknown) {
          if (owned.contextCommandID !== null) owned.state.pendingCommandIDs.delete(owned.contextCommandID);
          owned.contextCommandID = null;
          owned.contextDeliveryUnknown = false;
        }
        if ((event.type === "context" && ["accepted", "unchanged"].includes(event.payload.state)) || (event.type === "snapshot" && ["accepted", "unchanged"].includes(event.payload.context_state))) {
          owned.contextAccepted = true;
          owned.contextRevision = undefined;
          owned.contextCommandID = null;
          owned.contextDeliveryUnknown = false;
        }
        if (event.type === "context" && event.payload.state === "pending" && owned.contextAccepted) owned.contextRevision = "replacement";
        if (event.type === "timeline" && owned.state.contextState === "pending" && owned.contextRevision === undefined && owned.contextCommandID === null) owned.contextRevision = owned.state.timeline.length > 0 ? "replacement" : "initial";
        if (owned !== controller) return;
        loadController(owned);
        render();
      },
      onDisconnect(error) {
        owned.connecting = false;
        owned.state.connected = false;
        owned.state.lifecycle = "unavailable";
        owned.pendingSubmitCommandID = null;
        owned.pendingSubmission = null;
        if (owned.contextCommandID !== null) {
          owned.contextDeliveryUnknown = true;
          resetControllerForFreshSnapshot(owned, { preserveContextDelivery: true });
        }
        if (owned.handoffCommandID !== null) {
          owned.transport.resetConversation();
          owned.state.conversationID = null;
          owned.state.seenEventIDs.clear();
          owned.state.timeline = [];
          owned.state.queue = [];
          owned.state.interactions = [];
          owned.state.timelineCursor = null;
          owned.state.pendingCommandIDs.clear();
          owned.state.knownCommandIDs.clear();
          owned.state.freshArchiveCommandIDs.clear();
          owned.state.contextState = "pending";
          owned.state.contextDigest = null;
          owned.state.settingsState = null;
          owned.state.effectiveSettings = null;
          owned.state.catalog = [];
          owned.contextAccepted = false;
          owned.contextRevision = undefined;
          owned.contextCommandID = null;
          owned.contextDeliveryUnknown = false;
          owned.handoffCommandID = null;
        }
        if (owned === controller) {
          brokerState = "offline";
          brokerCode = error?.protocolViolation ? "protocol_violation" : "broker_unavailable";
          brokerGuidance = error?.protocolViolation
            ? "The local broker sent an incompatible event stream. Update or restart it before reconnecting. No page content has been shared again."
            : "The local broker connection was interrupted. Check that it is running on this device, then try again.";
          loadController(owned);
          render();
        }
        if (!error?.protocolViolation && owned.transport.consented) scheduleReconnect(owned);
      },
    });
    controllers.set(provider, owned);
    return owned;
  }

  function saveController() {
    if (!controller) return;
    Object.assign(controller, { contextRevision, contextAccepted, contextCommandID, contextDeliveryUnknown, handoffCommandID, pendingSubmitCommandID });
  }

  function loadController(next) {
    controller = next;
    state = next.state;
    transport = next.transport;
    ({ contextRevision, contextAccepted, contextCommandID, contextDeliveryUnknown, handoffCommandID, pendingSubmitCommandID } = next);
  }

  loadController(buildController(selectedProvider));

  function resetControllerForFreshSnapshot(target, { preserveContextDelivery = false } = {}) {
    target.transport.resetReplay();
    target.state.seenEventIDs.clear();
    target.state.lastEventID = null;
    target.state.timeline = [];
    target.state.interactions = [];
    target.state.timelineCursor = null;
    target.state.pendingCommandIDs.clear();
    target.state.knownCommandIDs.clear();
    target.state.freshArchiveCommandIDs.clear();
    if (!preserveContextDelivery) {
      target.contextRevision = undefined;
      target.contextCommandID = null;
      target.contextDeliveryUnknown = false;
    }
    if (target === controller) loadController(target);
  }

  function resetForFreshSnapshot(options) {
    saveController();
    resetControllerForFreshSnapshot(controller, options);
  }

  function scheduleReconnect(target = controller) {
    clearTimeout(target.reconnectTimer);
    target.reconnectTimer = setTimeout(async () => {
      if (destroyed) return;
      try {
        await target.transport.reconnect();
        brokerState = "ready";
        if (target === controller) render();
        if (target.state.timeline.length === 0) void sendControllerCommand(target, "history_page", { limit: 50 });
      } catch (error) {
        if (error?.code === "replay_window_unavailable") resetControllerForFreshSnapshot(target);
        if (!destroyed) scheduleReconnect(target);
      }
    }, 1000);
  }

  function viewportWidth() {
    return Number.isFinite(doc.defaultView?.innerWidth) ? doc.defaultView.innerWidth : 0;
  }

  function isDockedViewport() {
    const breakpoint = doc.defaultView?.matchMedia?.("(max-width: 63.999rem)");
    return agentDrawerLayoutMode(viewportWidth()) === "docked" && breakpoint?.matches !== true;
  }

  function syncDrawerLayout() {
    const docked = isDockedViewport();
    const modal = open && !docked;
    effectiveWidth = clampAgentDrawerWidth(baseWidth, viewportWidth());
    doc.documentElement.style.setProperty("--agent-drawer-width", `${effectiveWidth}px`);
    drawer.classList.toggle("is-docked", open && docked);
    drawer.classList.toggle("is-modal", open && !docked);
    drawer.setAttribute("role", open && !docked ? "dialog" : "complementary");
    drawer.setAttribute("aria-modal", String(open && !docked));
    overlay.classList.toggle("is-open", open && !docked);
    doc.body.classList.toggle("agent-drawer-docked-open", open && docked);
    doc.body.classList.toggle("agent-drawer-modal-open", open && !docked);
    separator.hidden = !open || !docked;
    separator.setAttribute("aria-valuemax", String(maxAgentDrawerWidth(viewportWidth())));
    separator.setAttribute("aria-valuenow", String(effectiveWidth));
    if (modal && !layoutWasModal && !drawer.contains(doc.activeElement)) close.focus();
    layoutWasModal = modal;
    if (!docked && resizing) finishResize({ persist: false });
  }

  function setDrawerWidth(value, { persist = false, userSelected = false } = {}) {
    const absoluteWidth = typeof value === "number" && Number.isFinite(value)
      ? Math.min(MAX_AGENT_DRAWER_WIDTH, Math.max(MIN_AGENT_DRAWER_WIDTH, Math.round(value)))
      : normalizeAgentDrawerWidth(value);
    const width = userSelected ? Math.min(absoluteWidth, maxAgentDrawerWidth(viewportWidth())) : absoluteWidth;
    baseWidth = width;
    effectiveWidth = clampAgentDrawerWidth(baseWidth, viewportWidth());
    doc.documentElement.style.setProperty("--agent-drawer-width", `${effectiveWidth}px`);
    separator.setAttribute("aria-valuemax", String(maxAgentDrawerWidth(viewportWidth())));
    separator.setAttribute("aria-valuenow", String(effectiveWidth));
    if (persist) persistAgentPreference(storage, AGENT_DRAWER_WIDTH_STORAGE_KEY, baseWidth);
  }

  function finishResize({ persist = true } = {}) {
    if (!resizing) return;
    resizing = false;
    const shouldRestore = !persist;
    const pointerID = resizePointerID;
    resizePointerID = null;
    if (pointerID !== null) {
      try { separator.releasePointerCapture?.(pointerID); } catch { /* capture may already be gone */ }
    }
    doc.body.style.userSelect = resizePreviousUserSelect;
    doc.body.classList.remove("agent-drawer-resizing");
    if (shouldRestore) setDrawerWidth(resizeStartWidth);
    else persistAgentPreference(storage, AGENT_DRAWER_WIDTH_STORAGE_KEY, baseWidth);
  }

  function onPointerMove(event) {
    if (!resizing || (event.pointerId !== undefined && event.pointerId !== resizePointerID)) return;
    const next = Math.round(viewportWidth() - event.clientX);
    setDrawerWidth(next, { userSelected: true });
  }

  function onPointerFinish(event) {
    if (!resizing || (event.pointerId !== undefined && event.pointerId !== resizePointerID)) return;
    finishResize({ persist: event.type === "pointerup" });
  }

  function onPointerDown(event) {
    if (!open || !isDockedViewport() || event.button !== 0) return;
    resizing = true;
    resizeStartWidth = baseWidth;
    resizePointerID = event.pointerId;
    separator.setPointerCapture?.(event.pointerId);
    resizePreviousUserSelect = doc.body.style.userSelect;
    doc.body.style.userSelect = "none";
    doc.body.classList.add("agent-drawer-resizing");
    event.preventDefault();
  }

  function onSeparatorKeydown(event) {
    if (!open || !isDockedViewport()) return;
    const step = event.shiftKey ? 32 : 8;
    if (event.key === "ArrowLeft") {
      event.preventDefault();
      setDrawerWidth(effectiveWidth + step, { persist: true, userSelected: true });
    } else if (event.key === "ArrowRight") {
      event.preventDefault();
      setDrawerWidth(effectiveWidth - step, { persist: true, userSelected: true });
    } else if (event.key === "Home") {
      event.preventDefault();
      setDrawerWidth(DEFAULT_AGENT_DRAWER_WIDTH, { persist: true, userSelected: true });
    }
  }

  function onViewportResize() {
    syncDrawerLayout();
  }

  separator.addEventListener("pointerdown", onPointerDown);
  separator.addEventListener("pointermove", onPointerMove);
  separator.addEventListener("pointerup", onPointerFinish);
  separator.addEventListener("pointercancel", onPointerFinish);
  separator.addEventListener("keydown", onSeparatorKeydown);
  function onSeparatorDoubleClick() {
    if (isDockedViewport()) setDrawerWidth(DEFAULT_AGENT_DRAWER_WIDTH, { persist: true, userSelected: true });
  }
  separator.addEventListener("dblclick", onSeparatorDoubleClick);
  doc.defaultView?.addEventListener("resize", onViewportResize);

  function setOpen(next, { focus = true } = {}) {
    const wasOpen = open;
    if (next && focus) restoreFocus = doc.activeElement;
    if (!next && resizing) finishResize({ persist: false });
    open = next;
    if (wasOpen && !open) {
      modelControl.close();
      clearDraftAttachments();
    }
    const modal = open && !isDockedViewport();
    drawer.classList.toggle("is-open", open);
    overlay.classList.toggle("is-open", open && modal);
    drawer.setAttribute("role", modal ? "dialog" : "complementary");
    drawer.setAttribute("aria-modal", String(modal));
    toggle.setAttribute("aria-expanded", String(open));
    toggle.setAttribute("aria-label", open ? "Close Page agent" : "Open Page agent");
    persistAgentPreference(storage, AGENT_DRAWER_STORAGE_KEY, open);
    syncDrawerLayout();
    if (open && focus) {
      close.focus();
    } else if (!open && focus) {
      restoreFocus?.focus?.();
    }
  }

  function currentCodexSettings() {
    if (selectedProvider !== "codex" || state.settingsState !== "verified") return null;
    const candidate = controller.settingsDraft.draft;
    return candidate && settingsCompatibility(state.catalog, candidate).compatible ? cloneExecutionSettings(candidate) : null;
  }

  function selectedModelSupportsImages() {
    if (selectedProvider !== "codex") return state.supportsImages;
    const candidate = controller.settingsDraft.draft;
    const model = candidate && state.catalog.find(({ model: value }) => value === candidate.model);
    if (model) return model.supports_images;
    return candidate && state.effectiveSettings && candidate.model === state.effectiveSettings.model ? state.supportsImages : false;
  }

  function selectDraftSettings(next) {
    if (selectedProvider !== "codex") return;
    try {
      editCodexDraft(controller.settingsDraft, next);
      render();
    } catch {
      showTransientStatus("Model settings unavailable", "Choose another option", "The live Codex catalog no longer supports that combination.");
    }
  }

  function submitBlocked() {
    return state.contextState === "pending" && contextRevision === undefined || contextCommandID !== null || contextDeliveryUnknown || pendingSubmitCommandID !== null || selectedProvider === "codex" && currentCodexSettings() === null;
  }

  function revokeObjectURL(url) {
    if (url) doc.defaultView?.URL?.revokeObjectURL?.(url);
  }

  function createObjectURL(value) {
    return doc.defaultView?.URL?.createObjectURL?.(value) ?? "";
  }

  function attachmentSummary() {
    const preparing = draftAttachments.filter((item) => item.status === "preparing").length;
    const failed = draftAttachments.filter((item) => item.status === "failed").length;
    if (preparing > 0) return `Preparing ${preparing} image${preparing === 1 ? "" : "s"}…`;
    if (failed > 0) return `${failed} image${failed === 1 ? "" : "s"} need attention.`;
    if (draftAttachments.length > 0) return `${draftAttachments.length} image${draftAttachments.length === 1 ? "" : "s"} ready.`;
    return "";
  }

  function updateComposerAvailability() {
    const preparing = draftAttachments.some((item) => item.status === "preparing");
    const hasReadyImage = draftAttachments.some((item) => item.status === "ready");
    const hasMessage = messageEditor.getContent().parts.length > 0;
    const incompatibleImages = (hasReadyImage || draftInlineVisuals.size > 0) && !selectedModelSupportsImages();
    sendButton.disabled = submitBlocked() || preparing || incompatibleImages || (!hasMessage && !hasReadyImage);
  }

  function handleDraftContentChange(content) {
    const retained = new Set(inlineImages(content).map(({ image_id }) => image_id));
    for (const [imageID, visual] of draftInlineVisuals) {
      if (retained.has(imageID)) continue;
      draftInlineVisuals.delete(imageID);
      if (typeof visual.owner.transport.deleteImage === "function") void visual.owner.transport.deleteImage(imageID, visual.conversationID).catch(() => {});
    }
  }

  function clearDraftInlineVisuals({ deleteStaged = true } = {}) {
    if (!deleteStaged) draftInlineVisuals.clear();
    const content = messageEditor.getContent();
    const retained = content.parts.filter((part) => part.type !== "reference" || part.reference.kind !== "image");
    if (!deleteStaged) messageEditor.setContent({ parts: retained });
    else messageEditor.setContent({ parts: retained });
  }

  async function readRenderedImage(metadata) {
    const rawSource = metadata.element?.currentSrc || metadata.element?.src;
    if (!rawSource) throw new Error("This image has no readable source.");
    let parsed;
    try { parsed = new URL(rawSource, pageURL); } catch { throw new Error("This image source is invalid."); }
    const pageOrigin = new URL(pageURL).origin;
    if (parsed.protocol !== "data:" && parsed.origin !== pageOrigin) throw new Error("Only same-origin page images can be added.");
    let blob;
    if (parsed.protocol === "data:") {
      const match = /^data:(image\/(?:png|jpeg|gif|webp));base64,([A-Za-z0-9+/=\s]+)$/iu.exec(rawSource);
      if (!match || match[2].length > Math.ceil(MAX_AGENT_IMAGE_BYTES * 4 / 3) + 16) throw new Error("Use a base64 PNG, JPEG, GIF, or WebP image up to 10 MiB.");
      let decoded;
      try { decoded = doc.defaultView.atob(match[2].replace(/\s/gu, "")); } catch { throw new Error("This embedded image is invalid."); }
      const bytes = Uint8Array.from(decoded, (character) => character.charCodeAt(0));
      blob = new doc.defaultView.Blob([bytes], { type: match[1].toLowerCase() });
    } else {
      const fetchImpl = doc.defaultView?.fetch ?? globalThis.fetch;
      if (typeof fetchImpl !== "function") throw new Error("Image reading is unavailable.");
      const response = await fetchImpl(parsed.href, { method: "GET", credentials: "omit", referrerPolicy: "no-referrer", cache: "no-store", redirect: "error" });
      if (!response.ok) throw new Error("The selected image could not be read.");
      if (response.url && new URL(response.url).origin !== pageOrigin) throw new Error("The selected image redirected away from this page.");
      blob = await response.blob();
    }
    const mediaType = blob.type.split(";", 1)[0].trim();
    if (!IMAGE_MEDIA_TYPES.has(mediaType) || blob.size <= 0 || blob.size > MAX_AGENT_IMAGE_BYTES) throw new Error("Use a PNG, JPEG, GIF, or WebP image up to 10 MiB.");
    const fallbackExtension = { "image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp" }[mediaType];
    const candidate = parsed.protocol === "data:" ? "" : decodeURIComponent(parsed.pathname.split("/").at(-1) || "");
    const name = validImageName(candidate) ? candidate : `image-${metadata.ordinal}.${fallbackExtension}`;
    const FileType = doc.defaultView?.File;
    return { file: FileType ? new FileType([blob], name, { type: mediaType }) : Object.assign(blob, { name }), name };
  }

  async function addInlineImage(metadata) {
    setOpen(true, { focus: false });
    showView("conversation", { focus: false });
    if (!state.connected || !selectedModelSupportsImages() || !transport.conversationID) {
      showTransientStatus("Image reference unavailable", "Connect an image-capable model", "Connect Page Agent with image input enabled, then add this image again.");
      throw new Error("Connect an image-capable Page Agent before adding this image.");
    }
    if (draftAttachments.length + draftInlineVisuals.size >= MAX_AGENT_IMAGES_PER_TURN) throw new Error(`A message can contain at most ${MAX_AGENT_IMAGES_PER_TURN} images.`);
    const owner = controller;
    const conversationID = transport.conversationID;
    const { file, name } = await readRenderedImage(metadata);
    const aggregate = draftAttachments.reduce((total, item) => total + item.file.size, 0) + [...draftInlineVisuals.values()].reduce((total, item) => total + item.bytes, 0) + file.size;
    if (aggregate > MAX_AGENT_TURN_IMAGE_BYTES) throw new Error("A message can contain at most 20 MiB of image data.");
    if (owner !== controller || conversationID !== transport.conversationID) throw new Error("The Page Agent conversation changed while preparing this image.");
    const staged = await owner.transport.uploadImage(file, conversationID, undefined, "inline_reference");
    if (owner !== controller || conversationID !== transport.conversationID) {
      await owner.transport.deleteImage?.(staged.image_id, conversationID).catch(() => {});
      throw new Error("The Page Agent conversation changed while preparing this image.");
    }
    draftInlineVisuals.set(staged.image_id, { owner, conversationID, bytes: file.size });
    const reference = imageReference(metadata, { resource: payload.local_agent.resource, digest: payload.local_agent.context_digest }, metadata.referenceID, staged.image_id, name);
    messageEditor.insertReference(reference);
    announce(`Added ${reference.label} to the message.`);
    return reference;
  }

  function renderDraftAttachments() {
    attachmentList.replaceChildren();
    attachmentStatus.textContent = attachmentSummary();
    for (const item of draftAttachments) {
      const preview = doc.createElement("article");
      preview.className = "agent-attachment-preview";
      preview.dataset.state = item.status;
      const image = doc.createElement("img");
      image.alt = "";
      if (item.objectURL) image.src = item.objectURL;
      const copy = doc.createElement("div");
      const name = doc.createElement("strong");
      name.textContent = item.name;
      const status = doc.createElement("span");
      status.textContent = item.status === "preparing" ? "Preparing…" : item.status === "ready" ? "Ready" : item.error;
      copy.append(name, status);
      const actions = doc.createElement("div");
      actions.className = "agent-attachment-actions";
      if (item.status === "failed" && item.retryable) {
        const retry = doc.createElement("button");
        retry.type = "button";
        retry.textContent = "Retry";
        retry.setAttribute("aria-label", `Retry ${item.name}`);
        retry.addEventListener("click", () => void stageAttachment(item));
        actions.append(retry);
      }
      const remove = doc.createElement("button");
      remove.type = "button";
      remove.textContent = "×";
      remove.setAttribute("aria-label", `Remove ${item.name}`);
      remove.addEventListener("click", () => removeAttachment(item));
      actions.append(remove);
      preview.append(image, copy, actions);
      attachmentList.append(preview);
    }
    updateComposerAvailability();
  }

  async function stageAttachment(item) {
    if (!draftAttachments.includes(item) || destroyed) return;
    const owner = item.owner;
    item.abort?.abort();
    item.abort = new AbortController();
    item.status = "preparing";
    item.error = "";
    item.retryable = false;
    renderDraftAttachments();
    try {
      if (typeof owner.transport.uploadImage !== "function") throw new Error("image_storage_failure");
      const staged = await owner.transport.uploadImage(item.file, item.conversationID, item.abort.signal);
      if (!draftAttachments.includes(item) || item.abort.signal.aborted) return;
      item.imageID = staged.image_id;
      item.mediaType = staged.media_type;
      item.status = "ready";
    } catch (error) {
      if (!draftAttachments.includes(item) || item.abort.signal.aborted) return;
      item.status = "failed";
      item.error = browserErrorText(error?.code, doc, "Could not prepare this image.");
      item.retryable = error?.code === "image_storage_failure" || !error?.code;
    }
    renderDraftAttachments();
  }

  function addImageFiles(files) {
    if (!state.connected || !selectedModelSupportsImages()) {
      showTransientStatus("Images unavailable", "Choose another model", "The selected model does not support image input.");
      return;
    }
    const owner = controller;
    const conversationID = owner.transport.conversationID;
    let aggregate = draftAttachments.reduce((total, item) => total + item.file.size, 0);
    for (const file of files) {
      if (!file || draftAttachments.length >= MAX_AGENT_IMAGES_PER_TURN) {
        showTransientStatus("Image limit reached", `Maximum ${MAX_AGENT_IMAGES_PER_TURN}`, "Remove an image before adding another one.");
        break;
      }
      if (!IMAGE_MEDIA_TYPES.has(file.type) || file.size <= 0 || file.size > MAX_AGENT_IMAGE_BYTES || !validImageName(file.name)) {
        showTransientStatus("Image not added", "Unsupported file", "Use a PNG, JPEG, GIF, or WebP image up to 10 MiB.");
        continue;
      }
      if (aggregate + file.size > MAX_AGENT_TURN_IMAGE_BYTES) {
        showTransientStatus("Image limit reached", "Maximum 20 MiB", "Remove an image before adding another one.");
        break;
      }
      aggregate += file.size;
      const item = {
        key: ++attachmentSerial,
        owner,
        conversationID,
        file,
        name: file.name,
        objectURL: createObjectURL(file),
        status: "preparing",
        error: "",
        retryable: false,
        imageID: null,
        mediaType: null,
        abort: null,
      };
      draftAttachments.push(item);
      void stageAttachment(item);
    }
    renderDraftAttachments();
  }

  function removeAttachment(item, { deleteStaged = true } = {}) {
    if (!draftAttachments.includes(item)) return;
    item.abort?.abort();
    draftAttachments = draftAttachments.filter((candidate) => candidate !== item);
    revokeObjectURL(item.objectURL);
    if (deleteStaged && item.imageID && typeof item.owner.transport.deleteImage === "function") void item.owner.transport.deleteImage(item.imageID, item.conversationID).catch(() => {});
    renderDraftAttachments();
  }

  function clearDraftAttachments({ deleteStaged = true } = {}) {
    for (const item of [...draftAttachments]) removeAttachment(item, { deleteStaged });
  }

  function completePendingSubmission(owner) {
    const submission = owner.pendingSubmission;
    if (!submission || owner !== controller) return;
    if (JSON.stringify(messageEditor.getContent()) === JSON.stringify(submission.content)) {
      for (const { image_id: imageID } of inlineImages(submission.content)) draftInlineVisuals.delete(imageID);
      messageEditor.clear();
      resizeComposer();
    }
    for (const item of submission.attachments) removeAttachment(item, { deleteStaged: false });
  }

  function appendDescriptorImages(parent, images) {
    if (images.length === 0) return;
    const list = doc.createElement("div");
    list.className = "agent-message-images";
    for (const descriptor of images) {
      const figure = doc.createElement("figure");
      const image = doc.createElement("img");
      image.alt = descriptor.name;
      const caption = doc.createElement("figcaption");
      caption.textContent = descriptor.name;
      const conversationID = transport.conversationID;
      const cacheKey = `${conversationID}:${descriptor.image_id}`;
      const cached = imageObjectURLs.get(cacheKey);
      if (cached) image.src = cached;
      else if (conversationID && typeof transport.readImage === "function" && !imageLoads.has(cacheKey)) {
        const load = transport.readImage(descriptor.image_id, conversationID)
          .then((blob) => {
            if (destroyed) return;
            const url = createObjectURL(blob);
            if (url) imageObjectURLs.set(cacheKey, url);
            render();
          })
          .catch(() => {})
          .finally(() => imageLoads.delete(cacheKey));
        imageLoads.set(cacheKey, load);
      }
      figure.append(image, caption);
      list.append(figure);
    }
    parent.append(list);
  }

  function render() {
    saveController();
    const providerName = selectedProvider === "codex" ? "Codex" : "Pi";
    const connectionState = controller.connecting ? "connecting" : brokerState;
    agentGlyph.textContent = providerName[0];
    connectButton.textContent = `Connect to ${providerName}`;
    message.setAttribute("aria-label", `Message ${providerName} about this whiteboard`);
    composerFineprint.textContent = `${providerName} can make mistakes. Review important details.`;
    accessListItem.textContent = selectedProvider === "codex"
      ? "Uses your current Codex tools, approvals, sandbox, and configuration"
      : "Pi has no tools, files, network, or project access";
    headerSubtitle.removeAttribute("title");
    if (!timeline.hidden) {
      timelineScrollTop = timeline.scrollTop;
      followTimeline = timeline.scrollHeight - timeline.scrollTop - timeline.clientHeight <= 48;
    }
    const statusState = state.connected
      ? state.lifecycle
      : connectionState === "ready"
        ? "ready"
        : connectionState === "connecting" ? "connecting" : "unavailable";
    statusDot.dataset.state = statusState;
    headerStatusDot.dataset.state = statusState;
    headerSubtitle.dataset.state = statusState;
    if (state.connected) {
      providerLabel.textContent = state.provider.model || providerName;
      providerLabel.hidden = false;
      liveStatus.textContent = state.lifecycle === "responding"
        ? "Responding"
        : pendingSubmitCommandID !== null
          ? "Sending"
          : "Connected";
    } else {
      providerLabel.hidden = true;
      if (connectionState === "checking") {
        liveStatus.textContent = "Checking local broker…";
      } else if (connectionState === "connecting") {
        liveStatus.textContent = "Connecting…";
      } else if (connectionState === "ready") {
        liveStatus.textContent = `${providerName} ready`;
      } else {
        const shortStatus = {
          wrong_port: "Broker not found",
          local_network_permission_denied: "Local access blocked",
          untrusted_origin: "Origin not trusted",
          incompatible_api: "Broker incompatible",
          protocol_violation: "Broker incompatible",
          broker_unavailable: "Broker unavailable",
        };
        liveStatus.textContent = shortStatus[brokerCode] ?? shortStatus.broker_unavailable;
      }
    }
    setup.hidden = state.connected || activeView !== "conversation";
    setup.dataset.state = connectionState;
    if (!state.connected) {
      const ready = connectionState === "ready" || connectionState === "connecting";
      setupIcon.textContent = ready ? "✓" : "⌁";
      setupHeading.textContent = ready
        ? connectionState === "connecting" ? `Connecting to ${providerName}…` : "Ready to connect"
        : connectionState === "checking" ? "Checking local broker…" : `${providerName} isn’t available on this device`;
      consentDisclosure.textContent = ready
        ? `Connecting starts or resumes a local ${providerName} conversation and sends no page content. Complete context is included only with the next message that needs it.`
        : brokerGuidance;
      consentList.hidden = !ready;
      setupCheckButton.hidden = connectionState !== "offline";
      directSettingsButton.hidden = ready;
      connectButton.hidden = !ready;
      connectButton.disabled = connectionState === "connecting";
    }
    settings.hidden = activeView !== "settings";
    newMenuButton.disabled = !state.connected || selectedProvider === "codex" && currentCodexSettings() === null;
    archivesMenuButton.disabled = !state.connected;
    reconnectMenuButton.disabled = transport.consented !== true;
    composerWrap.hidden = !state.connected || activeView !== "conversation";
    timeline.hidden = !state.connected || activeView !== "conversation";
    queue.hidden = !state.connected || activeView !== "conversation" || state.queue.length === 0;
    actions.hidden = true;
    const alternateView = activeView !== "conversation";
    const viewTitles = { settings: "Connection settings", context: "Page context", archives: "Archives" };
    heading.textContent = viewTitles[activeView] ?? "Page agent";
    agentGlyph.hidden = alternateView;
    backButton.hidden = !alternateView;
    headerSubtitle.hidden = alternateView;
    providerSelect.hidden = alternateView;
    overflow.hidden = alternateView;
    overflowButton.hidden = alternateView;
    contextDetails.hidden = activeView !== "context";
    contextDetails.classList.toggle("agent-view-hidden", activeView !== "context");
    contextDetails.setAttribute("aria-hidden", String(activeView !== "context"));
    archives.hidden = activeView !== "archives";
    const contextAttached = state.contextState === "accepted" || state.contextState === "unchanged";
    const draftSupportsImages = selectedModelSupportsImages();
    imageButton.disabled = !state.connected || !draftSupportsImages;
    imageButton.title = draftSupportsImages ? "Add PNG, JPEG, GIF, or WebP images" : "The selected model does not support image input.";
    const codexDraft = selectedProvider === "codex" ? controller.settingsDraft : null;
    modelControl.render({
      visible: selectedProvider === "codex",
      enabled: state.connected && state.settingsState === "verified" && state.catalog.length > 0,
      settings: codexDraft?.draft ?? null,
      presentation: codexDraft?.effectivePresentation ?? state.effectiveSettings,
      catalog: state.catalog,
    });
    renderDraftAttachments();
    stopButton.disabled = state.activeTurnID === null;
    stopButton.hidden = state.activeTurnID === null;
    contextChip.textContent = `Context · ${contextAttached ? "current" : "available"}`;
    queueChip.textContent = `Queue · ${state.queue.length}`;
    queueChip.hidden = state.queue.length === 0;

    timeline.replaceChildren();
    const contextSummary = doc.createElement("article");
    contextSummary.className = "agent-context-summary";
    const contextSummaryCopy = doc.createElement("div");
    contextSummaryCopy.className = "agent-context-summary-copy";
    const contextEyebrow = doc.createElement("span");
    contextEyebrow.className = "agent-context-eyebrow";
    contextEyebrow.textContent = "Page context";
    const contextTitle = doc.createElement("h3");
    contextTitle.textContent = pageTitle;
    const contextSummaryMeta = doc.createElement("p");
    const contextStateLabel = contextDeliveryUnknown ? "Delivery uncertain" : contextAttached ? "Context attached" : "Context available";
    contextSummaryMeta.textContent = `${contextStateLabel} · Updated ${payload.local_agent.resource.updated_at}`;
    const inspectContext = doc.createElement("button");
    inspectContext.type = "button";
    inspectContext.textContent = "Inspect context";
    inspectContext.addEventListener("click", () => showView("context"));
    contextSummaryCopy.append(contextEyebrow, contextTitle, contextSummaryMeta);
    contextSummary.append(contextSummaryCopy, inspectContext);
    timeline.append(contextSummary);

    if (state.timelineCursor) {
      const older = doc.createElement("button");
      older.type = "button";
      older.className = "agent-page-button";
      older.textContent = "Load older messages";
      older.addEventListener("click", () => void sendCommand("history_page", { before: state.timelineCursor, limit: 50 }));
      timeline.append(older);
    }
    for (const item of state.timeline) {
      if (item.kind === "user" || item.kind === "assistant") appendAgentMessage(doc, timeline, item, providerName, appendDescriptorImages, onReference);
      else if (item.kind === "tool") appendToolActivity(doc, timeline, item);
      else {
        const details = doc.createElement("details");
        details.className = `agent-activity agent-activity-${item.activity}`;
        details.open = item.expanded === true;
        const labels = {
          visible_summary: "Work summary",
          status: "Status",
          retry: "Retrying",
          compaction: "Compaction",
          blocked_tool: "Tool request blocked",
          blocked_permission: "Permission request blocked",
          blocked: "Request blocked",
          error: "Error",
          interruption: "Interrupted",
        };
        const activityLabel = item.activity === "blocked" ? labels[`blocked_${item.blockedKind}`] ?? labels.blocked : labels[item.activity];
        const summary = doc.createElement("summary"); summary.textContent = activityLabel ?? "Activity";
        const content = doc.createElement("p");
        const guidanceText = actionGuidance(item.action, doc);
        content.textContent = guidanceText ? `${item.text} ${guidanceText}` : item.text;
        details.append(summary, content); timeline.append(details);
      }
    }
    for (const request of state.interactions) {
      appendInteractionCard(doc, timeline, request, (optionID, answers) => respondToInteraction(request, optionID, answers));
    }
    const hasActiveAssistant = state.activeTurnID !== null && state.timeline.some((item) => item.kind === "assistant" && item.turn_id === state.activeTurnID);
    if (state.lifecycle === "responding" && !hasActiveAssistant) {
      const loading = doc.createElement("div");
      loading.className = "agent-response-loading";
      loading.setAttribute("role", "status");
      loading.setAttribute("aria-label", `${providerName} is responding`);
      const loadingGlyph = doc.createElement("span");
      loadingGlyph.className = "agent-loading-glyph";
      loadingGlyph.setAttribute("aria-hidden", "true");
      loadingGlyph.textContent = providerName[0];
      const loadingCopy = doc.createElement("span");
      loadingCopy.className = "agent-loading-copy";
      const loadingLabel = doc.createElement("strong");
      loadingLabel.textContent = providerName;
      const loadingText = doc.createElement("span");
      loadingText.className = "agent-response-text";
      loadingText.textContent = `${providerName} is responding`;
      const dots = doc.createElement("span");
      dots.className = "agent-response-dots";
      dots.setAttribute("aria-hidden", "true");
      for (let index = 0; index < 3; index += 1) {
        const dot = doc.createElement("span");
        dot.className = "agent-response-dot";
        dots.append(dot);
      }
      loadingCopy.append(loadingLabel, loadingText, dots);
      loading.append(loadingGlyph, loadingCopy);
      timeline.append(loading);
    }
    if (!timeline.hidden) {
      timeline.scrollTop = followTimeline ? timeline.scrollHeight : timelineScrollTop;
      timelineScrollTop = timeline.scrollTop;
    }
    for (const editor of queueEditors) editor.destroy();
    queueEditors = [];
    queue.replaceChildren();
    if (state.queue.length) {
      const title = doc.createElement("h3"); title.textContent = "Queued follow-ups"; queue.append(title);
    }
    for (const item of state.queue) {
      const row = doc.createElement("div"); row.className = "agent-queue-item";
      const editor = createMessageEditor({ doc, content: item.content, placeholder: "Edit queued message", ariaLabel: "Edit queued message" });
      queueEditors.push(editor);
      const input = editor.element;
      const save = doc.createElement("button"); save.type = "button"; save.textContent = "Save";
      const remove = doc.createElement("button"); remove.type = "button"; remove.textContent = "Remove";
      save.addEventListener("click", () => {
        const content = messageContentForCommand(editor.getContent());
        if (!validMessageContent(content, false) || content.parts.length === 0 && !(item.images?.length > 0)) {
          showTransientStatus("Queued message required", "Keep content", "Enter text, retain a page reference, or keep at least one image attached.");
          return;
        }
        void sendCommand("queue_edit", { message_id: item.message_id, content });
      });
      remove.addEventListener("click", () => void sendCommand("queue_remove", { message_id: item.message_id }));
      const queuedContent = doc.createElement("div");
      queuedContent.className = "agent-queue-content";
      queuedContent.append(input);
      if (item.settings) {
        const summary = doc.createElement("p");
        summary.className = "agent-queue-settings";
        summary.textContent = `${item.settings.model_display_name} · ${formatEffort(item.settings.effort)} · ${item.settings.speed === "fast" ? "Fast" : "Standard"}`;
        queuedContent.append(summary);
      }
      if (item.images?.length) appendDescriptorImages(queuedContent, item.images);
      row.append(queuedContent, save, remove); queue.append(row);
    }
    archives.replaceChildren();
    const archivesHeading = doc.createElement("h3");
    archivesHeading.textContent = "Archives";
    archives.append(archivesHeading);
    for (const item of state.archives) {
      const row = doc.createElement("article");
      const preview = doc.createElement("p"); preview.textContent = `Updated ${item.updated_at}${item.model ? ` · ${item.model}` : ""}`;
      const restore = doc.createElement("button"); restore.type = "button"; restore.textContent = "Restore";
      const remove = doc.createElement("button"); remove.type = "button"; remove.textContent = "Delete";
      restore.addEventListener("click", () => {
        if (doc.defaultView?.confirm("Archive the current conversation and restore this one?")) void forcedConversationCommand("archive_restore", { archive_id: item.archive_id });
      });
      remove.addEventListener("click", () => {
        if (doc.defaultView?.confirm("Delete this archived conversation permanently?")) void sendCommand("archive_delete", { archive_id: item.archive_id });
      });
      row.append(preview, restore, remove); archives.append(row);
    }
    if (state.archiveCursor) {
      const more = doc.createElement("button");
      more.type = "button";
      more.className = "agent-page-button";
      more.textContent = "Load more archives";
      more.addEventListener("click", () => void sendCommand("archive_list", { before: state.archiveCursor, limit: 50 }));
      archives.append(more);
    }
  }

  function announce(messageText) {
    if (toastTimer !== null) doc.defaultView?.clearTimeout?.(toastTimer);
    const value = String(messageText ?? "").trim();
    toast.textContent = value;
    toast.hidden = value === "";
    if (value !== "") {
      toastTimer = doc.defaultView?.setTimeout?.(() => {
        toast.hidden = true;
        toast.textContent = "";
        toastTimer = null;
      }, 2400) ?? null;
    }
  }

  function showTransientStatus(summary, detail, explanation) {
    liveStatus.textContent = summary;
    providerLabel.textContent = detail;
    providerLabel.hidden = detail.length === 0;
    if (explanation) headerSubtitle.title = explanation;
  }

  async function sendCommand(type, commandPayload, { handoff = false, freshArchivePage = false } = {}) {
    saveController();
    const target = controller;
    const command = createAgentCommand({ type, payload: commandPayload, clientID: target.transport.clientID, conversationID: target.transport.conversationID });
    registerAgentCommand(target.state, command);
    if (handoff) target.handoffCommandID = command.command_id;
    if (freshArchivePage) target.state.freshArchiveCommandIDs.add(command.command_id);
    try { await target.transport.send(command); }
    catch (error) {
      if (error?.code) {
        target.state.pendingCommandIDs.delete(command.command_id);
        target.state.freshArchiveCommandIDs.delete(command.command_id);
      }
      if (target.handoffCommandID === command.command_id) target.handoffCommandID = null;
      if (target === controller) {
        loadController(target);
        showTransientStatus("Action failed", "Retry", browserErrorText(error.code, doc, "The local broker is unavailable."));
      }
    }
    return command;
  }

  async function sendControllerCommand(target, type, commandPayload) {
    const command = createAgentCommand({ type, payload: commandPayload, clientID: target.transport.clientID, conversationID: target.transport.conversationID });
    registerAgentCommand(target.state, command);
    try {
      await target.transport.send(command);
    } catch (error) {
      if (error?.code) target.state.pendingCommandIDs.delete(command.command_id);
      if (target === controller) showTransientStatus("Action failed", "Retry", browserErrorText(error.code, doc, "The local broker is unavailable."));
    }
    return command;
  }

  async function respondToInteraction(request, optionID, answers) {
    saveController();
    const target = controller;
    const current = target.state.interactions.find((item) => item.request_id === request.request_id);
    if (!current || current.resolved || current.submitting) return;
    const command = createAgentCommand({
      type: "interaction_respond",
      payload: { request_id: current.request_id, kind: current.kind, option_id: optionID, answers },
      clientID: target.transport.clientID,
      conversationID: target.transport.conversationID,
    });
    registerAgentCommand(target.state, command);
    current.submitting = true;
    current.responseCommandID = command.command_id;
    render();
    try {
      await target.transport.send(command);
    } catch (error) {
      if (error?.code) {
        target.state.pendingCommandIDs.delete(command.command_id);
        const pending = target.state.interactions.find((item) => item.request_id === request.request_id);
        if (pending) Object.assign(pending, { submitting: false, responseCommandID: null });
      }
      if (target === controller) {
        loadController(target);
        showTransientStatus("Response failed", "Retry", browserErrorText(error.code, doc, "The response outcome is unknown; reconnect before responding again."));
      }
    }
    if (target === controller) render();
  }

  async function forcedConversationCommand(type, commandPayload) {
    await sendCommand(type, commandPayload, { handoff: true });
  }

  async function probe() {
    brokerState = "checking";
    brokerCode = "broker_unavailable";
    brokerGuidance = "Checking for a compatible local broker. No page content has been shared.";
    render();
    try {
      const result = await transport.probe();
      const messages = {
        wrong_port: "No compatible broker was found on this port. Check the connection settings, then try again. No page content has been shared.",
        local_network_permission_denied: "Allow Local Network Access in Chrome, then check again. No page content has been shared.",
        untrusted_origin: `This whiteboard origin is not trusted. Run agent-whiteboard agent trust add ${doc.location.origin}, then check again. No page content has been shared.`,
        incompatible_api: "The local broker version is incompatible. Update or restart it, then check again. No page content has been shared.",
        broker_unavailable: "Start the local broker on this device, then check again. No page content has been shared.",
      };
      brokerState = result.ok ? "ready" : "offline";
      brokerCode = result.code ?? "broker_unavailable";
      brokerGuidance = result.ok
        ? "The local broker is available. Connect when you are ready."
        : messages[result.code] ?? messages.broker_unavailable;
      guidance.textContent = brokerGuidance;
      render();
      return result;
    } catch {
      let permissionDenied = false;
      try {
        const permission = await doc.defaultView?.navigator?.permissions?.query({ name: "local-network-access" });
        permissionDenied = permission?.state === "denied";
      } catch {
        permissionDenied = false;
      }
      brokerCode = permissionDenied ? "local_network_permission_denied" : "broker_unavailable";
      brokerState = "offline";
      brokerGuidance = permissionDenied
        ? "Allow Local Network Access for this site in Chrome, then check again. No page content has been shared."
        : "Start the local broker on this device, then check again. No page content has been shared.";
      guidance.textContent = brokerGuidance;
      render();
      return { ok: false, code: brokerCode };
    }
  }

  showView = (view, { focus = true } = {}) => {
    const previousView = activeView;
    activeView = ["conversation", "settings", "context", "archives"].includes(view) ? view : "conversation";
    if (activeView === "context") markdownCard.open = true;
    if (activeView === "archives" && previousView !== "archives" && state.connected) void sendCommand("archive_list", { limit: 50 }, { freshArchivePage: true });
    render();
    if (!focus) return;
    if (activeView !== "conversation") backButton.focus();
    else if (state.connected) messageEditor.focus();
    else overflowButton.focus();
  };
  backButton.addEventListener("click", () => showView("conversation"));
  contextChip.addEventListener("click", () => showView("context"));
  contextDisclosure.addEventListener("click", () => showView("context"));
  directSettingsButton.addEventListener("click", () => showView("settings"));
  queueChip.addEventListener("click", () => queue.querySelector(".agent-message-editor")?.focus());
  providerSelect.addEventListener("change", () => {
    const nextProvider = providerSelect.value;
    if (!PROVIDERS.has(nextProvider) || nextProvider === selectedProvider) return;
    clearDraftAttachments();
    clearDraftInlineVisuals();
    saveController();
    selectedProvider = nextProvider;
    persistAgentPreference(storage, AGENT_PROVIDER_STORAGE_KEY, selectedProvider);
    loadController(controllers.get(selectedProvider) ?? buildController(selectedProvider));
    activeView = "conversation";
    modelControl.close();
    overflowMenu.hidden = true;
    overflowButton.setAttribute("aria-expanded", "false");
    render();
  });
  function enabledOverflowItems() {
    return [...overflowMenu.querySelectorAll('[role="menuitem"]:not([disabled])')];
  }
  overflowButton.addEventListener("click", () => {
    const opening = overflowMenu.hidden;
    overflowMenu.hidden = !opening;
    overflowButton.setAttribute("aria-expanded", String(opening));
    if (opening) enabledOverflowItems()[0]?.focus();
    else overflowButton.focus();
  });
  toggle.addEventListener("click", () => setOpen(!open));
  close.addEventListener("click", () => setOpen(false));
  overlay.addEventListener("click", () => setOpen(false));
  portInput.addEventListener("change", () => {
    const normalized = normalizeAgentPort(portInput.value);
    if (String(normalized) !== portInput.value) {
      portInput.setCustomValidity("Enter a decimal port from 1 to 65535.");
      portInput.reportValidity();
      return;
    }
    portInput.setCustomValidity("");
    port = normalized;
    persistAgentPreference(storage, AGENT_PORT_STORAGE_KEY, port);
    for (const owned of controllers.values()) owned.transport.setPort(port);
    void probe();
  });
  checkButton.addEventListener("click", () => void probe());
  setupCheckButton.addEventListener("click", () => void probe());
  connectButton.addEventListener("click", async () => {
    saveController();
    const target = controller;
    target.transport.grantConsent();
    target.connecting = true;
    render();
    try {
      await target.transport.connect();
      target.connecting = false;
      brokerState = "ready";
      if (target === controller) render();
      void sendControllerCommand(target, "history_page", { limit: 50 });
    } catch (error) {
      target.connecting = false;
      if (target === controller) {
        brokerState = "offline";
        brokerCode = error?.code ?? "broker_unavailable";
        brokerGuidance = `Unable to connect: ${browserErrorText(error.code, doc, "check the broker, port, Local Network Access, and trust.")} No page content has been shared.`;
        render();
      }
    }
  });
  function resizeComposer() {
    message.style.height = "auto";
    const maximum = 160;
    const height = Math.min(Math.max(message.scrollHeight, 48), maximum);
    message.style.height = `${height}px`;
    message.style.overflowY = message.scrollHeight > maximum ? "auto" : "hidden";
  }
  imageButton.addEventListener("click", () => imagePicker.click());
  imagePicker.addEventListener("change", () => {
    addImageFiles([...imagePicker.files]);
    imagePicker.value = "";
  });
  message.addEventListener("paste", (event) => {
    const files = [...(event.clipboardData?.items ?? [])]
      .filter((item) => item.kind === "file" && item.type.startsWith("image/"))
      .map((item) => item.getAsFile())
      .filter(Boolean);
    if (files.length > 0) addImageFiles(files);
  });
  resizeComposer();
  composer.addEventListener("submit", async (event) => {
    event.preventDefault();
    const content = messageEditor.getContent();
    const sentAttachments = draftAttachments.filter((item) => item.status === "ready");
    const imageReferences = sentAttachments.map((item) => ({ image_id: item.imageID, name: item.name }));
    if (draftAttachments.some((item) => item.status === "preparing")) {
      showTransientStatus("Preparing images", "Please wait", "Images must finish preparing before this message can be sent.");
      return;
    }
    if (!validContentAndImages(content, imageReferences, false)) { showTransientStatus("Message required", "Add text or an image", "Enter a message, add a page reference, or add at least one ready image."); return; }
    saveController();
    const target = controller;
    if (target.state.contextState === "pending" && target.contextRevision === undefined) {
      showTransientStatus("Context pending", "Please wait", "Wait for the broker to determine whether this page context is initial or replacement.");
      return;
    }
    if (target.contextCommandID !== null || target.contextDeliveryUnknown) {
      showTransientStatus("Context pending", "Reconnect", "Wait for the complete context handoff to be confirmed before sending another message.");
      return;
    }
    const revision = target.state.contextState === "pending" ? target.contextRevision : undefined;
    const executionSettings = selectedProvider === "codex" ? currentCodexSettings() : null;
    if (selectedProvider === "codex" && executionSettings === null) {
      showTransientStatus("Model options unavailable", "Choose valid settings", "Reconnect or choose a model, effort, and speed supported by the live Codex catalog.");
      return;
    }
    if (selectedProvider === "codex" && (imageReferences.length > 0 || inlineImages(content).length > 0) && !selectedModelSupportsImages()) {
      showTransientStatus("Images unavailable", "Choose another model", "The selected model does not support image input.");
      return;
    }
    const command = createSubmitCommand({ content, images: imageReferences, payload, provider: selectedProvider, settings: executionSettings, clientID: target.transport.clientID, conversationID: target.transport.conversationID, title: pageTitle, url: pageURL, revision });
    registerAgentCommand(target.state, command);
    target.pendingSubmitCommandID = command.command_id;
    target.pendingSubmission = { content: cloneMessageContent(content), attachments: [...sentAttachments] };
    if (selectedProvider === "codex") recordCodexSubmission(target.settingsDraft, command.payload.turn_id);
    if (revision !== undefined) target.contextCommandID = command.command_id;
    loadController(target);
    render();
    try {
      await target.transport.send(command);
    }
    catch (error) {
      target.pendingSubmitCommandID = null;
      target.pendingSubmission = null;
      if (error?.code) target.state.pendingCommandIDs.delete(command.command_id);
      if (target.contextCommandID === command.command_id) {
        if (error?.code) target.contextCommandID = null;
        else target.contextDeliveryUnknown = true;
      }
      if (target === controller) {
        loadController(target);
        render();
        showTransientStatus("Send failed", "Retry", browserErrorText(error.code, doc, "the delivery outcome is unknown; reconnect before trying again."));
      }
    }
  });
  stopButton.addEventListener("click", () => { if (state.activeTurnID) void sendCommand("interrupt", { turn_id: state.activeTurnID }); });
  reconnectButton.addEventListener("click", () => {
    transport.close();
    state.connected = false;
    state.lifecycle = "unavailable";
    render();
    scheduleReconnect();
  });
  newButton.addEventListener("click", () => {
    if (!doc.defaultView?.confirm("Archive this conversation and start a new one?")) return;
    const executionSettings = selectedProvider === "codex" ? currentCodexSettings() : null;
    if (selectedProvider === "codex" && executionSettings === null) {
      showTransientStatus("Model options unavailable", "New conversation not started", "Choose settings supported by the live Codex catalog.");
      return;
    }
    clearDraftAttachments();
    clearDraftInlineVisuals();
    showView("conversation");
    void forcedConversationCommand("new", { settings: executionSettings });
  });
  historyButton.addEventListener("click", () => { showView("archives"); });
  function onOverflowOutsidePointerDown(event) {
    if (!overflow.contains(event.target)) {
      overflowMenu.hidden = true;
      overflowButton.setAttribute("aria-expanded", "false");
    }
  }
  doc.addEventListener("pointerdown", onOverflowOutsidePointerDown);
  doc.addEventListener("keydown", onKeydown);
  function onKeydown(event) {
    if (event.defaultPrevented) return;
    if (!overflowMenu.hidden && event.target?.getAttribute?.("role") === "menuitem" && ["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) {
      const items = enabledOverflowItems();
      const current = items.indexOf(event.target);
      if (items.length > 0) {
        event.preventDefault();
        const next = event.key === "Home"
          ? 0
          : event.key === "End"
            ? items.length - 1
            : event.key === "ArrowDown"
              ? (current + 1) % items.length
              : (current - 1 + items.length) % items.length;
        items[next].focus();
      }
      return;
    }
    if (event.key === "Escape" && !overflowMenu.hidden) {
      overflowMenu.hidden = true;
      overflowButton.setAttribute("aria-expanded", "false");
      overflowButton.focus();
      return;
    }
    if (event.key === "Escape" && open) { setOpen(false); return; }
    if (event.key !== "Tab" || !open || isDockedViewport()) return;
    const available = [...drawer.querySelectorAll("button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [contenteditable=\"true\"], summary")].filter((element) => element.closest("[hidden]") === null);
    const focusable = [close, ...available.filter((element) => element !== close)];
    if (focusable.length === 0) return;
    const first = focusable[0];
    const last = focusable.at(-1);
    if (!drawer.contains(doc.activeElement)) {
      event.preventDefault();
      (event.shiftKey ? last : first).focus();
    } else if (event.shiftKey && doc.activeElement === first) { event.preventDefault(); last.focus(); }
    else if (!event.shiftKey && doc.activeElement === last) { event.preventDefault(); first.focus(); }
  }

  setOpen(open, { focus: open });
  render();
  void probe();

  return {
    get state() { return state; },
    get transport() { return transport; },
    elements: { toggle, drawer, close, overlay, toast, separator, overflowButton, overflowMenu, backButton, headerActions, providerSelect, setup, settings, contextDetails, portInput, connectButton, composerWrap, composer, message, imagePicker, imageButton, modelControl, attachmentList, attachmentStatus, sendButton, stopButton, timeline, queue, archives },
    get open() { return open; },
    setOpen,
    probe,
    sendCommand,
    addReference(reference) {
      setOpen(true, { focus: false });
      showView("conversation");
      const content = messageEditor.insertReference(reference);
      updateComposerAvailability();
      return content;
    },
    addImageReference: addInlineImage,
    announce,
    destroy() {
      destroyed = true;
      if (toastTimer !== null) doc.defaultView?.clearTimeout?.(toastTimer);
      clearDraftInlineVisuals();
      for (const editor of queueEditors) editor.destroy();
      queueEditors = [];
      clearDraftAttachments();
      modelControl.destroy();
      messageEditor.destroy();
      for (const url of imageObjectURLs.values()) revokeObjectURL(url);
      imageObjectURLs.clear();
      for (const owned of controllers.values()) clearTimeout(owned.reconnectTimer);
      finishResize({ persist: false });
      for (const owned of controllers.values()) owned.transport.close();
      doc.removeEventListener("keydown", onKeydown);
      doc.removeEventListener("pointerdown", onOverflowOutsidePointerDown);
      doc.defaultView?.removeEventListener("resize", onViewportResize);
      separator.removeEventListener("pointerdown", onPointerDown);
      separator.removeEventListener("pointermove", onPointerMove);
      separator.removeEventListener("pointerup", onPointerFinish);
      separator.removeEventListener("pointercancel", onPointerFinish);
      separator.removeEventListener("keydown", onSeparatorKeydown);
      separator.removeEventListener("dblclick", onSeparatorDoubleClick);
      doc.body.classList.remove("agent-drawer-modal-open", "agent-drawer-docked-open");
      doc.documentElement.style.removeProperty("--agent-drawer-width");
      toggle.remove(); overlay.remove(); drawer.remove(); toast.remove();
    },
  };
}

export async function bootViewer(doc = document) {
  const sourceElement = doc.querySelector("#agent-whiteboard-source");
  if (!sourceElement) return undefined;
  let parsed;
  try { parsed = JSON.parse(sourceElement.textContent || "null"); }
  catch { throw new TypeError("invalid whiteboard source payload"); }
  const payload = validateViewerPayload(parsed);
  const container = viewerContainer(doc);
  const viewer = await renderWhiteboard(payload.markdown, { container, doc, contextEnabled: payload.local_agent.enabled });
  if (payload.local_agent.enabled) {
    let contextController;
    viewer.agent = createAgentDrawer({ payload, doc, onReference: (reference) => contextController?.navigate(reference) ?? false });
    contextController = createMarkdownContextController({
      doc,
      container,
      index: viewer.semanticIndex,
      identity: { resource: payload.local_agent.resource, digest: payload.local_agent.context_digest },
      idFactory: generateAgentID,
      onAdd: (reference) => viewer.agent.addReference(reference),
      onImageAdd: (metadata) => viewer.agent.addImageReference(metadata),
      announce: (messageText) => viewer.agent.announce(messageText),
    });
    viewer.context = contextController;
    const destroyViewer = viewer.destroy.bind(viewer);
    viewer.destroy = () => { contextController.destroy(); viewer.agent.destroy(); destroyViewer(); };
  }
  return viewer;
}

function startBrowserEntry() {
  void bootViewer().catch(() => {
    const container = viewerContainer(document);
    container.replaceChildren();
    const error = document.createElement("p");
    error.className = "viewer-error";
    error.textContent = "Unable to render whiteboard";
    container.append(error);
    document.title = DEFAULT_TITLE;
  });
}

if (typeof document !== "undefined") {
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", startBrowserEntry, { once: true });
  } else if (document.querySelector("#agent-whiteboard-source")) {
    startBrowserEntry();
  }
}
