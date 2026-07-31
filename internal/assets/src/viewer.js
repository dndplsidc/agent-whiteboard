import createDOMPurify from "dompurify";
import hljs from "highlight.js/lib/common";
import MarkdownIt from "markdown-it";
import mermaid from "mermaid";

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

function renderMarkdown(source, doc) {
  const diagramSources = [];
  const markdown = createMarkdownRenderer(diagramSources);
  const rendered = markdown.render(source);
  const sanitized = purifierFor(doc).sanitize(rendered);
  return { diagramSources, html: sanitized };
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
  } = {},
) {
  if (typeof source !== "string") throw new TypeError("whiteboard source must be a string");
  if (!container) throw new TypeError("viewer container is required");

  container[THEME_CONTROL_CLEANUP]?.();
  container[THEME_CONTROL_CLEANUP] = undefined;
  const { diagramSources, html } = renderMarkdown(source, doc);
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
export const DEFAULT_AGENT_PORT = 8568;
export const DEFAULT_AGENT_DRAWER_WIDTH = 420;
export const MIN_AGENT_DRAWER_WIDTH = 360;
export const MAX_AGENT_DRAWER_WIDTH = 720;
export const AGENT_DRAWER_DOCK_BREAKPOINT = 64 * 16;
export const AGENT_API_VERSION = "1";
export const AGENT_WEBSOCKET_PROTOCOL = "agent-whiteboard.v1";
export const MAX_AGENT_MESSAGE_BYTES = 64 * 1024;
export const MAX_AGENT_EVENT_BYTES = 256 * 1024;

const AGENT_CONNECT_TIMEOUT_MS = 30_000;
const ID_PATTERN = /^[A-Za-z0-9_-]{32}$/u;
const DIGEST_PATTERN = /^[0-9a-f]{64}$/u;
const DATE_PATTERN = /^(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d):(\d\d)(?:\.(\d{1,9}))?(?:Z|([+-])(\d\d):(\d\d))$/u;
const MAX_STATE_EVENTS = 2048;
const MAX_TIMELINE_ITEMS = 200;
const MAX_ARCHIVES = 100;
const MAX_QUEUE_ITEMS = 64;
const MAX_TIMELINE_TEXT_BYTES = 96 * 1024;
const MAX_DELTA_BYTES = 32 * 1024;
const encoder = new TextEncoder();

const ERROR_DEFINITIONS = {
  broker_unavailable: ["The local agent broker is unavailable.", "retry_connection"],
  wrong_port: ["No compatible local agent broker was found on this port.", "edit_port"],
  local_network_permission_denied: ["Browser permission to reach the local agent broker was denied.", "grant_local_network"],
  untrusted_origin: ["This whiteboard origin is not trusted by the local agent broker.", "trust_origin"],
  incompatible_api: ["The local agent broker uses an incompatible API version.", "update_broker"],
  provider_missing: ["The Pi provider executable is not available.", "install_provider"],
  authentication_required: ["Pi requires provider-native authentication.", "provider_login"],
  no_usable_model: ["Pi has no usable default model.", "configure_model"],
  provider_startup_failed: ["Pi could not be started.", "try_again"],
  content_only_unavailable: ["Pi cannot enforce content-only access.", "try_again"],
  context_too_large: ["The complete page context does not fit safely in the selected model.", "reduce_context"],
  native_session_missing: ["The provider session for this conversation is unavailable.", "restore_session"],
  provider_crashed: ["Pi stopped unexpectedly and the active turn was interrupted.", "retry_turn"],
  provider_recovery_failed: ["Pi could not recover the conversation.", "restart_provider"],
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
  try {
    port = storage?.getItem(AGENT_PORT_STORAGE_KEY);
    width = storage?.getItem(AGENT_DRAWER_WIDTH_STORAGE_KEY);
  } catch {
    port = undefined;
    width = undefined;
  }
  return { open: readBoolean(storage, AGENT_DRAWER_STORAGE_KEY), port: normalizeAgentPort(port), width: normalizeAgentDrawerWidth(width) };
}

export function persistAgentPreference(storage, key, value) {
  if (key !== AGENT_DRAWER_STORAGE_KEY && key !== AGENT_PORT_STORAGE_KEY && key !== AGENT_DRAWER_WIDTH_STORAGE_KEY) throw new TypeError("unsupported agent preference");
  const stored = key === AGENT_DRAWER_STORAGE_KEY
    ? String(value === true)
    : key === AGENT_PORT_STORAGE_KEY
      ? String(normalizeAgentPort(value))
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

export function createConnectCommand({ payload, clientID, replayAfter, idFactory = generateAgentID }) {
  validateViewerPayload(payload);
  if (payload.local_agent.enabled !== true) throw new TypeError("local agent is disabled");
  if (replayAfter && !validID(replayAfter)) throw new TypeError("invalid replay event ID");
  const connectPayload = {
    provider: "pi",
    resource: { ...payload.local_agent.resource },
    context_digest: payload.local_agent.context_digest,
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

export function createSubmitCommand({ message, payload, clientID, conversationID, title, url, revision, idFactory = generateAgentID }) {
  if (!validText(message, MAX_AGENT_MESSAGE_BYTES) || (revision !== undefined && !["initial", "replacement"].includes(revision))) throw new TypeError("invalid agent message");
  if (!validID(conversationID)) throw new TypeError("invalid agent conversation");
  const turnID = idFactory();
  const messageID = idFactory();
  if (!validID(turnID) || !validID(messageID)) throw new TypeError("invalid agent command identity");
  const submitPayload = { turn_id: turnID, message_id: messageID, message };
  if (revision === "initial" || revision === "replacement") {
    submitPayload.context = createPageContext(payload, { title, url, revision });
  }
  return commandEnvelope({ type: "submit", payload: submitPayload, clientID, conversationID, idFactory });
}

export function createAgentCommand({ type, payload, clientID, conversationID, idFactory = generateAgentID }) {
  if (!validID(conversationID)) throw new TypeError("invalid agent conversation");
  const validators = {
    queue_edit: () => exactObject(payload, ["message_id", "message"]) && validID(payload.message_id) && validText(payload.message, MAX_AGENT_MESSAGE_BYTES),
    queue_remove: () => exactObject(payload, ["message_id"]) && validID(payload.message_id),
    interrupt: () => exactObject(payload, ["turn_id"]) && validID(payload.turn_id),
    new: () => exactObject(payload, []),
    archive_list: () => exactObject(payload, [], ["before", "limit"]) && (!payload.before || validID(payload.before)) && (!Object.hasOwn(payload, "limit") || Number.isInteger(payload.limit) && payload.limit >= 0 && payload.limit <= 100),
    history_page: () => exactObject(payload, [], ["before", "limit"]) && (!payload.before || validID(payload.before)) && (!Object.hasOwn(payload, "limit") || Number.isInteger(payload.limit) && payload.limit >= 0 && payload.limit <= 100),
    archive_restore: () => exactObject(payload, ["archive_id"]) && validID(payload.archive_id),
    archive_delete: () => exactObject(payload, ["archive_id"]) && validID(payload.archive_id),
    resync: () => exactObject(payload, [], ["after_event_id"]) && (!payload.after_event_id || validID(payload.after_event_id)),
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
  return exactObject(item, ["turn_id", "message_id", "message"]) && validID(item.turn_id) && validID(item.message_id) && validText(item.message, MAX_AGENT_MESSAGE_BYTES);
}

function validTimelineItem(item) {
  if (!exactObject(item, ["item_id", "kind", "text", "created_at"], ["turn_id", "message_id"]) || !validID(item.item_id) || !["user", "assistant", "activity"].includes(item.kind) || !validText(item.text, MAX_AGENT_MESSAGE_BYTES) || !validDate(item.created_at)) return false;
  if (item.kind === "activity") return !Object.hasOwn(item, "message_id") && (!Object.hasOwn(item, "turn_id") || validID(item.turn_id));
  return validID(item.turn_id) && validID(item.message_id);
}

function validArchiveItem(item) {
  return exactObject(item, ["archive_id", "created_at", "updated_at", "provider"], ["model", "preview"]) && validID(item.archive_id) && validDate(item.created_at) && validDate(item.updated_at) && Date.parse(item.updated_at) >= Date.parse(item.created_at) && item.provider === "pi" && (!Object.hasOwn(item, "model") || validText(item.model, 512, true)) && (!Object.hasOwn(item, "preview") || validText(item.preview, 512, true));
}

const lifecycleValues = new Set(["connecting", "ready", "responding", "interrupted", "unavailable"]);
const contextValues = new Set(["pending", "accepted", "unchanged", "unavailable"]);
const providerValues = new Set(["starting", "ready", "unavailable", "recovering"]);

function validActiveTurn(lifecycle, turnID) {
  if (turnID !== null && !validID(turnID)) return false;
  return lifecycle === "responding" ? turnID !== null : turnID === null;
}

function validateEventPayload(type, payload) {
  switch (type) {
    case "snapshot":
      return exactObject(payload, ["lifecycle", "queue", "context_state", "active_turn_id"]) && lifecycleValues.has(payload.lifecycle) && Array.isArray(payload.queue) && payload.queue.length <= MAX_QUEUE_ITEMS && payload.queue.every(validQueueItem) && contextValues.has(payload.context_state) && validActiveTurn(payload.lifecycle, payload.active_turn_id);
    case "command_result":
      return exactObject(payload, ["command_id", "status"], ["error"]) && validID(payload.command_id) && ["succeeded", "rejected"].includes(payload.status) && (payload.status === "succeeded" ? !Object.hasOwn(payload, "error") : validBrowserError(payload.error));
    case "timeline":
      return exactObject(payload, ["command_id", "items", "next_cursor"]) && validID(payload.command_id) && Array.isArray(payload.items) && payload.items.length <= 100 && payload.items.every(validTimelineItem) && (payload.next_cursor === null || validID(payload.next_cursor));
    case "history":
      return exactObject(payload, ["command_id", "items", "next_cursor"]) && validID(payload.command_id) && Array.isArray(payload.items) && payload.items.length <= 100 && payload.items.every(validArchiveItem) && (payload.next_cursor === null || validID(payload.next_cursor));
    case "user_message":
    case "assistant_message":
      return exactObject(payload, ["turn_id", "message_id", "text", "created_at"]) && validID(payload.turn_id) && validID(payload.message_id) && validText(payload.text, MAX_AGENT_MESSAGE_BYTES) && validDate(payload.created_at);
    case "assistant_delta":
      return exactObject(payload, ["turn_id", "message_id", "text"]) && validID(payload.turn_id) && validID(payload.message_id) && validText(payload.text, MAX_DELTA_BYTES);
    case "queue":
      return exactObject(payload, ["items"]) && Array.isArray(payload.items) && payload.items.length <= MAX_QUEUE_ITEMS && payload.items.every(validQueueItem);
    case "lifecycle":
      return exactObject(payload, ["state", "turn_id"]) && lifecycleValues.has(payload.state) && validActiveTurn(payload.state, payload.turn_id);
    case "provider":
      return exactObject(payload, ["provider", "state"], ["model"]) && payload.provider === "pi" && providerValues.has(payload.state) && (!Object.hasOwn(payload, "model") || validText(payload.model, 512, true)) && (payload.state !== "ready" || validText(payload.model, 512));
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
    default:
      return false;
  }
}

export function validateAgentEvent(value) {
  if (!exactObject(value, ["api_version", "event_id", "conversation_id", "type", "timestamp", "payload"]) || value.api_version !== AGENT_API_VERSION || !validID(value.event_id) || !validID(value.conversation_id) || !validDate(value.timestamp) || !validateEventPayload(value.type, value.payload)) {
    throw new TypeError("invalid agent event");
  }
  const payload = value.payload;
  if (value.type === "timeline" && (new Set(payload.items.map((item) => item.item_id)).size !== payload.items.length || encoder.encode(payload.items.map((item) => item.text).join("")).length > MAX_TIMELINE_TEXT_BYTES || (payload.next_cursor !== null && (payload.items.length === 0 || payload.next_cursor !== payload.items.at(-1).item_id)))) throw new TypeError("invalid agent event");
  if (value.type === "history" && (new Set(payload.items.map((item) => item.archive_id)).size !== payload.items.length || (payload.next_cursor !== null && (payload.items.length === 0 || payload.next_cursor !== payload.items.at(-1).archive_id)))) throw new TypeError("invalid agent event");
  if (["snapshot", "queue"].includes(value.type)) {
    const items = value.type === "snapshot" ? payload.queue : payload.items;
    if (new Set(items.map((item) => item.message_id)).size !== items.length || new Set(items.map((item) => item.turn_id)).size !== items.length || encoder.encode(items.map((item) => item.message).join("")).length > 96 * 1024) throw new TypeError("invalid agent event");
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

export function createAgentState() {
  return {
    conversationID: null,
    lifecycle: "connecting",
    activeTurnID: null,
    contextState: "pending",
    contextDigest: null,
    provider: { provider: "pi", state: "starting", model: "" },
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
  };
}

function appendTimeline(state, item) {
  const key = item.item_id ?? item.message_id ?? `${item.kind}-${state.lastEventID}`;
  if (state.timeline.some((current) => (current.item_id ?? current.message_id) === key)) return;
  state.timeline.push({ ...item });
  if (state.timeline.length > MAX_TIMELINE_ITEMS) state.timeline.splice(0, state.timeline.length - MAX_TIMELINE_ITEMS);
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
    timeline: state.timeline.map((item) => ({ ...item })),
    queue: state.queue.map((item) => ({ ...item })),
    archives: state.archives.map((item) => ({ ...item })),
    errors: state.errors.map((item) => ({ ...item })),
    seenEventIDs: new Set(state.seenEventIDs),
    pendingCommandIDs: new Set(state.pendingCommandIDs),
    knownCommandIDs: new Set(state.knownCommandIDs),
    freshArchiveCommandIDs: new Set(state.freshArchiveCommandIDs),
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
      state.queue = payload.queue.map((item) => ({ ...item }));
      state.connected = true;
      break;
    case "timeline": {
      const known = new Set(state.timeline.map((item) => item.item_id ?? item.message_id));
      const older = payload.items.filter((item) => !known.has(item.item_id)).map((item) => ({ ...item })).reverse();
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
    case "queue": state.queue = payload.items.map((item) => ({ ...item })); break;
    case "lifecycle": state.lifecycle = payload.state; state.activeTurnID = payload.turn_id; break;
    case "provider": state.provider = { provider: payload.provider, state: payload.state, model: payload.model ?? "" }; break;
    case "context": state.contextDigest = payload.digest; state.contextState = payload.state; break;
    case "activity": appendTimeline(state, { kind: "activity", activity: payload.kind, text: payload.summary, created_at: event.timestamp, item_id: event.event_id }); break;
    case "blocked": appendTimeline(state, { kind: "activity", activity: "blocked", blockedKind: payload.kind, text: payload.message, created_at: event.timestamp, item_id: event.event_id, expanded: true }); break;
    case "error": state.errors.push({ ...payload.error }); state.errors = state.errors.slice(-20); appendTimeline(state, { kind: "activity", activity: "error", text: payload.error.message, action: payload.error.action, created_at: event.timestamp, item_id: event.event_id, expanded: true }); break;
    case "completion": state.lifecycle = "ready"; state.activeTurnID = null; break;
    case "interruption": state.lifecycle = "interrupted"; state.activeTurnID = null; appendTimeline(state, { kind: "activity", activity: "interruption", text: "The active response was interrupted and was not replayed automatically.", created_at: event.timestamp, item_id: event.event_id, expanded: true }); break;
    case "archive":
      if (payload.action === "deleted" || payload.action === "restored") state.archives = state.archives.filter((item) => item.archive_id !== payload.archive_id);
      break;
    case "command_result":
      state.pendingCommandIDs.delete(payload.command_id);
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
  return purifierFor(doc).sanitize(rendered, { FORBID_TAGS: ["img", "picture", "source", "audio", "video"] });
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
  port = DEFAULT_AGENT_PORT,
  clientID = generateAgentID(),
  fetchImpl = globalThis.fetch?.bind(globalThis),
  WebSocketImpl = globalThis.WebSocket,
  onEvent = () => {},
  onDisconnect = () => {},
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

  function connectCommand() {
    return createConnectCommand({ payload, clientID, replayAfter: lastEventID, idFactory });
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
    install_provider: "Install the pinned Pi provider executable, then restart the broker.",
    provider_login: "Run Pi in a terminal and complete provider-native login, then try again.",
    configure_model: "Configure a usable default model in Pi, then try again.",
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

function appendAgentMessage(doc, container, item) {
  const article = doc.createElement("article");
  article.className = `agent-message agent-message-${item.kind}`;
  const label = doc.createElement("strong");
  label.textContent = item.kind === "assistant" ? "Pi" : "You";
  const body = doc.createElement("div");
  body.className = "agent-message-body";
  body.innerHTML = renderAgentMarkdown(item.text, doc);
  article.append(label, body);
  container.append(article);
}

export function createAgentDrawer({ payload, doc = document, storage = browserStorage(doc), transportFactory = createAgentTransport, pageTitle = doc.title, pageURL = doc.location.href } = {}) {
  const preferences = readAgentPreferences(storage);
  const state = createAgentState();
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
  let reconnectTimer;
  let destroyed = false;
  let handoffCommandID = null;
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
  const headerSubtitle = doc.createElement("p");
  headerSubtitle.className = "agent-header-subtitle";
  headerSubtitle.textContent = "Content-only · Local Pi";
  const backButton = doc.createElement("button");
  backButton.type = "button";
  backButton.className = "agent-back-button";
  backButton.textContent = "Back to conversation";
  backButton.hidden = true;
  headerCopy.append(heading, headerSubtitle, backButton);
  headerIdentity.append(agentGlyph, headerCopy);
  const headerActions = doc.createElement("div");
  headerActions.className = "agent-header-actions";
  const close = doc.createElement("button");
  close.type = "button";
  close.className = "agent-icon-button agent-close-button";
  close.setAttribute("aria-label", "Close local agent");
  close.textContent = "×";

  const statusBar = doc.createElement("div");
  statusBar.className = "agent-status-bar";
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
  providerLabel.textContent = `Port ${port}`;
  statusCopy.append(headerStatusDot, liveStatus);
  statusBar.append(statusCopy, providerLabel);

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
  settingsHeading.textContent = "Connection settings";
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
  accessListItem.textContent = "No tools, files, network, or project access";
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
  const contextDisclosure = doc.createElement("div");
  contextDisclosure.className = "agent-context-disclosure";
  const contextDisclosureCopy = doc.createElement("div");
  const contextDisclosureHeading = doc.createElement("strong");
  contextDisclosureHeading.textContent = "Page context";
  const contextDisclosureDescription = doc.createElement("span");
  contextDisclosureDescription.textContent = "Full Markdown + creator notes";
  contextDisclosureCopy.append(contextDisclosureHeading, contextDisclosureDescription);
  const contextDisclosureActions = doc.createElement("div");
  const contextShareStatus = doc.createElement("span");
  contextShareStatus.className = "agent-context-share-status";
  contextShareStatus.textContent = "Not shared";
  const reviewContextButton = doc.createElement("button");
  reviewContextButton.type = "button";
  reviewContextButton.textContent = "Review";
  contextDisclosureActions.append(contextShareStatus, reviewContextButton);
  contextDisclosure.append(contextDisclosureCopy, contextDisclosureActions);
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
  headerActions.append(close, overflow);
  header.append(headerIdentity, headerActions);

  const contextDetails = doc.createElement("details");
  contextDetails.className = "agent-context";
  const contextSummary = doc.createElement("summary"); contextSummary.textContent = "Page context";
  const contextStatus = doc.createElement("p");
  const contextMetadata = doc.createElement("p");
  contextMetadata.textContent = `Markdown resource updated ${payload.local_agent.resource.updated_at}. Complete context is sent only with the next initial or replacement message.`;
  const markdownLabel = doc.createElement("h3"); markdownLabel.textContent = "Page Markdown";
  const markdownContext = doc.createElement("pre"); markdownContext.textContent = payload.markdown; markdownContext.setAttribute("aria-label", "Page Markdown");
  const creatorLabel = doc.createElement("h3"); creatorLabel.textContent = "Creator context";
  const creatorContext = doc.createElement("pre"); creatorContext.textContent = payload.context; creatorContext.setAttribute("aria-label", "Creator context");
  contextDetails.append(contextSummary, contextStatus, contextMetadata, markdownLabel, markdownContext, creatorLabel, creatorContext);

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
  const message = doc.createElement("textarea");
  message.maxLength = MAX_AGENT_MESSAGE_BYTES;
  message.rows = 2;
  message.placeholder = "Ask about this page…";
  message.setAttribute("aria-label", "Message Pi about this whiteboard");
  const composerBar = doc.createElement("div");
  composerBar.className = "agent-composer-bar";
  const contextChip = doc.createElement("button");
  contextChip.type = "button";
  contextChip.className = "agent-composer-chip";
  contextChip.textContent = "Context · available";
  const queueChip = doc.createElement("button");
  queueChip.type = "button";
  queueChip.className = "agent-composer-chip";
  queueChip.textContent = "Queue · 0";
  queueChip.hidden = true;
  const stopButton = doc.createElement("button");
  stopButton.type = "button";
  stopButton.className = "agent-stop-button";
  stopButton.textContent = "Stop";
  const sendButton = doc.createElement("button");
  sendButton.type = "submit";
  sendButton.className = "agent-send-button";
  sendButton.setAttribute("aria-label", "Send");
  sendButton.textContent = "↑";
  composerBar.append(contextChip, queueChip, stopButton, sendButton);
  composer.append(message, composerBar);
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

  drawer.append(header, statusBar, separator, setup, settings, actions, contextDetails, timeline, queue, archives, composerWrap);
  doc.body.append(overlay, drawer, toggle);

  const transport = transportFactory({
    payload,
    port,
    onEvent(event) {
      if (!applyAgentEvent(state, event)) return;
      if (event.type === "command_result" && event.payload.command_id === handoffCommandID && event.payload.status === "rejected") handoffCommandID = null;
      if (event.type === "command_result" && event.payload.command_id === pendingSubmitCommandID) pendingSubmitCommandID = null;
      if (event.type === "command_result" && event.payload.command_id === contextCommandID && event.payload.status === "rejected") contextCommandID = null;
      if (event.type === "snapshot" && contextDeliveryUnknown) {
        if (contextCommandID !== null) state.pendingCommandIDs.delete(contextCommandID);
        contextCommandID = null;
        contextDeliveryUnknown = false;
      }
      if ((event.type === "context" && ["accepted", "unchanged"].includes(event.payload.state)) || (event.type === "snapshot" && ["accepted", "unchanged"].includes(event.payload.context_state))) {
        contextAccepted = true;
        contextRevision = undefined;
        contextCommandID = null;
        contextDeliveryUnknown = false;
      }
      if (event.type === "context" && event.payload.state === "pending" && contextAccepted) contextRevision = "replacement";
      if (event.type === "timeline" && state.contextState === "pending" && contextRevision === undefined && contextCommandID === null) contextRevision = state.timeline.length > 0 ? "replacement" : "initial";
      render();
    },
    onDisconnect(error) {
      state.connected = false;
      state.lifecycle = "unavailable";
      brokerState = "offline";
      brokerCode = error?.protocolViolation ? "protocol_violation" : "broker_unavailable";
      brokerGuidance = error?.protocolViolation
        ? "The local broker sent an incompatible event stream. Update or restart it before reconnecting. No page content has been shared again."
        : "The local broker connection was interrupted. Check that it is running on this device, then try again.";
      pendingSubmitCommandID = null;
      if (contextCommandID !== null) {
        contextDeliveryUnknown = true;
        resetForFreshSnapshot({ preserveContextDelivery: true });
      }
      if (handoffCommandID !== null) {
        transport.resetConversation();
        state.conversationID = null;
        state.seenEventIDs.clear();
        state.timeline = [];
        state.queue = [];
        state.timelineCursor = null;
        state.pendingCommandIDs.clear();
        state.knownCommandIDs.clear();
        state.freshArchiveCommandIDs.clear();
        state.contextState = "pending";
        state.contextDigest = null;
        contextAccepted = false;
        contextRevision = undefined;
        contextCommandID = null;
        contextDeliveryUnknown = false;
        handoffCommandID = null;
      }
      render();
      if (error?.protocolViolation) return;
      if (transport.consented) scheduleReconnect();
    },
  });

  function resetForFreshSnapshot({ preserveContextDelivery = false } = {}) {
    transport.resetReplay();
    state.seenEventIDs.clear();
    state.lastEventID = null;
    state.timeline = [];
    state.timelineCursor = null;
    state.pendingCommandIDs.clear();
    state.knownCommandIDs.clear();
    state.freshArchiveCommandIDs.clear();
    if (!preserveContextDelivery) {
      contextRevision = undefined;
      contextCommandID = null;
      contextDeliveryUnknown = false;
    }
  }

  function scheduleReconnect() {
    clearTimeout(reconnectTimer);
    reconnectTimer = setTimeout(async () => {
      if (destroyed) return;
      try {
        await transport.reconnect();
        if (state.timeline.length === 0) void sendCommand("history_page", { limit: 50 });
      } catch (error) {
        if (error?.code === "replay_window_unavailable") resetForFreshSnapshot();
        if (!destroyed) scheduleReconnect();
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
    if (next && focus) restoreFocus = doc.activeElement;
    if (!next && resizing) finishResize({ persist: false });
    open = next;
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

  function submitBlocked() {
    return state.contextState === "pending" && contextRevision === undefined || contextCommandID !== null || contextDeliveryUnknown;
  }

  function render() {
    statusBar.removeAttribute("title");
    if (!timeline.hidden) {
      timelineScrollTop = timeline.scrollTop;
      followTimeline = timeline.scrollHeight - timeline.scrollTop - timeline.clientHeight <= 48;
    }
    const statusState = state.connected
      ? state.lifecycle
      : brokerState === "ready"
        ? "ready"
        : brokerState === "connecting" ? "connecting" : "unavailable";
    statusDot.dataset.state = statusState;
    headerStatusDot.dataset.state = statusState;
    statusBar.dataset.state = statusState;
    if (state.connected) {
      providerLabel.textContent = state.provider.model || "Pi";
      providerLabel.hidden = false;
      liveStatus.textContent = state.lifecycle === "responding"
        ? "Responding"
        : pendingSubmitCommandID !== null
          ? "Sending"
          : "Connected";
    } else {
      providerLabel.hidden = false;
      if (brokerState === "checking") {
        liveStatus.textContent = "Checking local broker…";
        providerLabel.textContent = `Port ${port}`;
      } else if (brokerState === "connecting") {
        liveStatus.textContent = "Connecting…";
        providerLabel.textContent = "Local Pi";
      } else if (brokerState === "ready") {
        liveStatus.textContent = "Pi ready";
        providerLabel.textContent = "Not connected";
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
        providerLabel.textContent = `Port ${port}`;
      }
    }
    setup.hidden = state.connected || activeView !== "conversation";
    setup.dataset.state = brokerState;
    if (!state.connected) {
      const ready = brokerState === "ready" || brokerState === "connecting";
      setupIcon.textContent = ready ? "✓" : "⌁";
      setupHeading.textContent = ready
        ? brokerState === "connecting" ? "Connecting to Pi…" : "Ready to connect"
        : brokerState === "checking" ? "Checking local broker…" : "Pi isn’t available on this device";
      consentDisclosure.textContent = ready
        ? "Connecting starts or resumes a local Pi conversation and sends no page content. Complete context is included only with the next message that needs it."
        : brokerGuidance;
      consentList.hidden = !ready;
      setupCheckButton.hidden = brokerState !== "offline";
      directSettingsButton.hidden = ready;
      connectButton.hidden = !ready;
      connectButton.disabled = brokerState === "connecting";
      contextShareStatus.textContent = contextDeliveryUnknown
        ? "Uncertain"
        : contextAccepted || ["accepted", "unchanged"].includes(state.contextState) ? "Shared previously" : "Not shared";
    }
    settings.hidden = activeView !== "settings";
    newMenuButton.disabled = !state.connected;
    archivesMenuButton.disabled = !state.connected;
    reconnectMenuButton.disabled = transport.consented !== true;
    composerWrap.hidden = !state.connected || activeView !== "conversation";
    timeline.hidden = !state.connected || activeView !== "conversation";
    queue.hidden = !state.connected || activeView !== "conversation" || state.queue.length === 0;
    actions.hidden = true;
    backButton.hidden = activeView === "conversation";
    headerSubtitle.hidden = activeView !== "conversation";
    contextDetails.hidden = activeView !== "context";
    contextDetails.classList.toggle("agent-view-hidden", activeView !== "context");
    contextDetails.setAttribute("aria-hidden", String(activeView !== "context"));
    archives.hidden = activeView !== "archives";
    const awaitingContextDecision = state.contextState === "pending" && contextRevision === undefined;
    const contextAttached = state.contextState === "accepted" || state.contextState === "unchanged";
    sendButton.disabled = submitBlocked();
    stopButton.disabled = state.activeTurnID === null;
    stopButton.hidden = state.activeTurnID === null;
    contextChip.textContent = `Context · ${contextAttached ? "current" : "available"}`;
    queueChip.textContent = `Queue · ${state.queue.length}`;
    queueChip.hidden = state.queue.length === 0;
    message.setAttribute("aria-describedby", "agent-whiteboard-context-status");
    contextStatus.id = "agent-whiteboard-context-status";
    contextStatus.textContent = contextDeliveryUnknown
      ? `Digest ${state.contextDigest ?? payload.local_agent.context_digest}; delivery outcome unknown. Reconnect before sending another message.`
      : awaitingContextDecision
        ? `Digest ${state.contextDigest ?? payload.local_agent.context_digest}; checking whether complete context is initial or replacement.`
        : `Digest ${state.contextDigest ?? payload.local_agent.context_digest}; context ${state.contextState}.`;

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
      if (item.kind === "user" || item.kind === "assistant") appendAgentMessage(doc, timeline, item);
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
    const hasActiveAssistant = state.activeTurnID !== null && state.timeline.some((item) => item.kind === "assistant" && item.turn_id === state.activeTurnID);
    if (state.lifecycle === "responding" && !hasActiveAssistant) {
      const loading = doc.createElement("div");
      loading.className = "agent-response-loading";
      loading.setAttribute("role", "status");
      loading.setAttribute("aria-label", "Pi is responding");
      const loadingGlyph = doc.createElement("span");
      loadingGlyph.className = "agent-loading-glyph";
      loadingGlyph.setAttribute("aria-hidden", "true");
      loadingGlyph.textContent = "P";
      const loadingCopy = doc.createElement("span");
      loadingCopy.className = "agent-loading-copy";
      const loadingLabel = doc.createElement("strong");
      loadingLabel.textContent = "Pi";
      const loadingText = doc.createElement("span");
      loadingText.className = "agent-response-text";
      loadingText.textContent = "Pi is responding";
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
    queue.replaceChildren();
    if (state.queue.length) {
      const title = doc.createElement("h3"); title.textContent = "Queued follow-ups"; queue.append(title);
    }
    for (const item of state.queue) {
      const row = doc.createElement("div"); row.className = "agent-queue-item";
      const input = doc.createElement("textarea"); input.value = item.message; input.maxLength = MAX_AGENT_MESSAGE_BYTES; input.setAttribute("aria-label", "Edit queued message");
      const save = doc.createElement("button"); save.type = "button"; save.textContent = "Save";
      const remove = doc.createElement("button"); remove.type = "button"; remove.textContent = "Remove";
      save.addEventListener("click", () => {
        if (!validText(input.value, MAX_AGENT_MESSAGE_BYTES)) {
          input.setCustomValidity("Enter a queued message no larger than 64 KiB.");
          input.reportValidity();
          return;
        }
        input.setCustomValidity("");
        void sendCommand("queue_edit", { message_id: item.message_id, message: input.value });
      });
      remove.addEventListener("click", () => void sendCommand("queue_remove", { message_id: item.message_id }));
      row.append(input, save, remove); queue.append(row);
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

  function showTransientStatus(summary, detail, explanation) {
    liveStatus.textContent = summary;
    providerLabel.textContent = detail;
    if (explanation) statusBar.title = explanation;
  }

  async function sendCommand(type, commandPayload, { handoff = false, freshArchivePage = false } = {}) {
    const command = createAgentCommand({ type, payload: commandPayload, clientID: transport.clientID, conversationID: transport.conversationID });
    registerAgentCommand(state, command);
    if (handoff) handoffCommandID = command.command_id;
    if (freshArchivePage) state.freshArchiveCommandIDs.add(command.command_id);
    try { await transport.send(command); }
    catch (error) {
      if (error?.code) {
        state.pendingCommandIDs.delete(command.command_id);
        state.freshArchiveCommandIDs.delete(command.command_id);
      }
      if (handoffCommandID === command.command_id) handoffCommandID = null;
      showTransientStatus("Action failed", "Retry", browserErrorText(error.code, doc, "The local broker is unavailable."));
    }
    return command;
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
    if (activeView === "context") contextDetails.open = true;
    if (activeView === "archives" && previousView !== "archives" && state.connected) void sendCommand("archive_list", { limit: 50 }, { freshArchivePage: true });
    render();
    if (!focus) return;
    if (activeView !== "conversation") backButton.focus();
    else if (state.connected) message.focus();
    else overflowButton.focus();
  };
  backButton.addEventListener("click", () => showView("conversation"));
  contextChip.addEventListener("click", () => showView("context"));
  reviewContextButton.addEventListener("click", () => showView("context"));
  directSettingsButton.addEventListener("click", () => showView("settings"));
  queueChip.addEventListener("click", () => queue.querySelector("textarea")?.focus());
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
    transport.setPort(port);
    void probe();
  });
  checkButton.addEventListener("click", () => void probe());
  setupCheckButton.addEventListener("click", () => void probe());
  connectButton.addEventListener("click", async () => {
    transport.grantConsent();
    brokerState = "connecting";
    render();
    try {
      await transport.connect();
      render();
      void sendCommand("history_page", { limit: 50 });
    } catch (error) {
      brokerState = "offline";
      brokerCode = error?.code ?? "broker_unavailable";
      brokerGuidance = `Unable to connect: ${browserErrorText(error.code, doc, "check the broker, port, Local Network Access, and trust.")} No page content has been shared.`;
      render();
    }
  });
  function resizeComposer() {
    message.style.height = "auto";
    const maximum = 160;
    const height = Math.min(Math.max(message.scrollHeight, 48), maximum);
    message.style.height = `${height}px`;
    message.style.overflowY = message.scrollHeight > maximum ? "auto" : "hidden";
  }
  let composing = false;
  message.addEventListener("compositionstart", () => { composing = true; });
  message.addEventListener("compositionend", () => { composing = false; });
  message.addEventListener("input", resizeComposer);
  message.addEventListener("keydown", (event) => {
    if (event.key !== "Enter" || event.shiftKey || composing || event.isComposing || event.keyCode === 229) return;
    event.preventDefault();
    composer.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  });
  resizeComposer();
  composer.addEventListener("submit", async (event) => {
    event.preventDefault();
    const text = message.value;
    if (!validText(text, MAX_AGENT_MESSAGE_BYTES)) { message.setCustomValidity("Enter a message no larger than 64 KiB."); message.reportValidity(); return; }
    if (state.contextState === "pending" && contextRevision === undefined) {
      showTransientStatus("Context pending", "Please wait", "Wait for the broker to determine whether this page context is initial or replacement.");
      return;
    }
    if (contextCommandID !== null || contextDeliveryUnknown) {
      showTransientStatus("Context pending", "Reconnect", "Wait for the complete context handoff to be confirmed before sending another message.");
      return;
    }
    message.setCustomValidity("");
    const revision = state.contextState === "pending" ? contextRevision : undefined;
    const command = createSubmitCommand({ message: text, payload, clientID: transport.clientID, conversationID: transport.conversationID, title: pageTitle, url: pageURL, revision });
    registerAgentCommand(state, command);
    pendingSubmitCommandID = command.command_id;
    if (revision !== undefined) contextCommandID = command.command_id;
    render();
    try { await transport.send(command); message.value = ""; resizeComposer(); }
    catch (error) {
      pendingSubmitCommandID = null;
      if (error?.code) state.pendingCommandIDs.delete(command.command_id);
      if (contextCommandID === command.command_id) {
        if (error?.code) contextCommandID = null;
        else contextDeliveryUnknown = true;
      }
      render();
      showTransientStatus("Send failed", "Retry", browserErrorText(error.code, doc, "the delivery outcome is unknown; reconnect before trying again."));
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
    if (doc.defaultView?.confirm("Archive this conversation and start a new one?")) { showView("conversation"); void forcedConversationCommand("new", {}); }
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
    const focusable = [...drawer.querySelectorAll("button:not([disabled]), input:not([disabled]), textarea:not([disabled]), summary")].filter((element) => element.closest("[hidden]") === null);
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
    state,
    transport,
    elements: { toggle, drawer, close, overlay, separator, overflowButton, overflowMenu, backButton, headerActions, setup, settings, contextDetails, portInput, connectButton, composerWrap, composer, message, sendButton, stopButton, timeline, queue, archives },
    get open() { return open; },
    setOpen,
    probe,
    sendCommand,
    destroy() {
      destroyed = true;
      clearTimeout(reconnectTimer);
      finishResize({ persist: false });
      transport.close();
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
      toggle.remove(); overlay.remove(); drawer.remove();
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
  const viewer = await renderWhiteboard(payload.markdown, { container: viewerContainer(doc), doc });
  if (payload.local_agent.enabled) {
    viewer.agent = createAgentDrawer({ payload, doc });
    const destroyViewer = viewer.destroy.bind(viewer);
    viewer.destroy = () => { viewer.agent.destroy(); destroyViewer(); };
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
