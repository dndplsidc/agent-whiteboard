import { beforeEach, describe, expect, test, vi } from "vitest";

const mermaidMocks = vi.hoisted(() => ({
  initialize: vi.fn(),
  render: vi.fn(),
}));

vi.mock("mermaid", () => ({
  default: mermaidMocks,
}));

import {
  AGENT_DRAWER_STORAGE_KEY,
  AGENT_DRAWER_WIDTH_STORAGE_KEY,
  AGENT_PORT_STORAGE_KEY,
  AGENT_WEBSOCKET_PROTOCOL,
  DEFAULT_AGENT_PORT,
  DEFAULT_AGENT_DRAWER_WIDTH,
  DEFAULT_TITLE,
  MAX_AGENT_MESSAGE_BYTES,
  maxAgentDrawerWidth,
  THEME_STORAGE_KEY,
  agentDrawerLayoutMode,
  applyAgentEvent,
  bootViewer,
  clampAgentDrawerWidth,
  createAgentCommand,
  createAgentDrawer,
  createAgentState,
  createAgentTransport,
  createConnectCommand,
  createSubmitCommand,
  decodeAgentEvent,
  generateAgentID,
  normalizeAgentDrawerWidth,
  normalizeAgentPort,
  normalizeTheme,
  readAgentPreferences,
  registerAgentCommand,
  renderAgentMarkdown,
  renderWhiteboard,
  validateViewerPayload,
} from "./viewer.js";

function makeMediaQuery(dark = false) {
  let matches = dark;
  const listeners = new Set();

  return {
    get matches() {
      return matches;
    },
    addEventListener: vi.fn((event, listener) => {
      if (event === "change") listeners.add(listener);
    }),
    removeEventListener: vi.fn((event, listener) => {
      if (event === "change") listeners.delete(listener);
    }),
    change(next) {
      matches = next;
      for (const listener of listeners) listener({ matches });
    },
  };
}

function setupDOM() {
  document.head.innerHTML = "";
  document.body.innerHTML = '<main id="viewer"></main>';
  document.documentElement.removeAttribute("data-theme");
  Object.defineProperty(window, "innerWidth", { configurable: true, value: 1024 });
  localStorage.clear();
  return document.querySelector("#viewer");
}

beforeEach(() => {
  vi.clearAllMocks();
  setupDOM();
  mermaidMocks.render.mockImplementation(async (id, source) => ({
    svg: `<svg xmlns="http://www.w3.org/2000/svg" data-id="${id}"><text>${source}</text></svg>`,
  }));
});

describe("Markdown rendering", () => {
  test("boots from the viewer shell JSON object contract", async () => {
    const source = "# Shell title\n\nRendered from the shell.";
    const sourceElement = document.createElement("script");
    sourceElement.type = "application/json";
    sourceElement.id = "agent-whiteboard-source";
    sourceElement.textContent = JSON.stringify({ markdown: source });
    document.body.replaceChildren(sourceElement);

    const viewer = await bootViewer(document);

    const container = document.querySelector("#agent-whiteboard-content");
    expect(container).not.toBeNull();
    expect(container.querySelector("h1")?.textContent).toBe("Shell title");
    expect(container.textContent).toContain("Rendered from the shell.");
    expect(document.title).toBe("Shell title");
    viewer.destroy();
  });

  test("renders supported Markdown and task lists with real markdown-it", async () => {
    const container = document.querySelector("#viewer");
    const source = [
      "# Board",
      "",
      "> quoted",
      "",
      "| Name | Value |",
      "| --- | --- |",
      "| A | B |",
      "",
      "- [ ] pending",
      "- [x] complete",
      "",
      "[safe](https://example.com)",
      "",
      "```js",
      "const answer = 42;",
      "```",
    ].join("\n");

    await renderWhiteboard(source, { container });

    expect(container.querySelector("h1")?.textContent).toBe("Board");
    expect(container.querySelector("blockquote")?.textContent).toContain("quoted");
    expect(container.querySelector("table tbody td")?.textContent).toBe("A");
    expect(container.querySelector('a[href="https://example.com"]')?.textContent).toBe("safe");
    expect(container.querySelector("pre code")?.textContent).toContain("const answer = 42;");
    expect(container.querySelector("pre code")?.classList.contains("hljs")).toBe(true);
    expect([...container.querySelectorAll('input[type="checkbox"]')]).toHaveLength(2);
    expect([...container.querySelectorAll('input[type="checkbox"]')].every((box) => box.disabled)).toBe(true);
    expect(container.querySelectorAll('input[type="checkbox"]')[1].checked).toBe(true);
  });

  test("does not allow raw Markdown HTML or javascript links to survive", async () => {
    const container = document.querySelector("#viewer");

    await renderWhiteboard(
      '<img src="x" onerror="globalThis.pwned=true">\n\n[unsafe](javascript:alert(1))',
      { container },
    );

    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector('a[href^="javascript:"]')).toBeNull();
  });

  test("uses the first rendered H1 as the title", async () => {
    const container = document.querySelector("#viewer");

    await renderWhiteboard("## Before\n\n# First title\n\n# Second title", { container });

    expect(document.title).toBe("First title");
  });

  test("uses the fallback title when there is no H1", async () => {
    const container = document.querySelector("#viewer");

    await renderWhiteboard("## Board", { container });

    expect(document.title).toBe(DEFAULT_TITLE);
  });

  test("provides a theme menu that applies and persists Light", async () => {
    const container = document.querySelector("#viewer");

    const viewer = await renderWhiteboard("# Board", { container });

    const trigger = container.querySelector("[data-theme-control]");
    const menu = container.querySelector("[data-theme-menu]");
    expect(trigger?.getAttribute("aria-label")).toBe("Appearance: System");
    expect(trigger?.querySelector('[data-theme-icon="system"]')).not.toBeNull();
    expect(menu?.hidden).toBe(true);
    expect(
      [...container.querySelectorAll("[data-theme-option]")].map(
        (option) => option.querySelector(".theme-control-option-title")?.textContent,
      ),
    ).toEqual(["System", "Light", "Dark"]);

    trigger.click();
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    container.querySelector('[data-theme-option="light"]').click();
    await viewer.settled();

    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("light");
    expect(document.documentElement.dataset.theme).toBe("light");
    expect(trigger.getAttribute("aria-label")).toBe("Appearance: Light");
    expect(trigger.querySelector('[data-theme-icon="light"]')).not.toBeNull();
    expect(menu.hidden).toBe(true);
  });
});

describe("themes", () => {
  test.each([
    ["light", "light"],
    ["dark", "dark"],
    ["system", "system"],
    ["unknown", "system"],
    [null, "system"],
    [undefined, "system"],
  ])("normalizes %j to %s", (input, expected) => {
    expect(normalizeTheme(input)).toBe(expected);
  });

  test("persists only the allowed theme key and follows live system changes", async () => {
    const container = document.querySelector("#viewer");
    const mediaQuery = makeMediaQuery(false);
    localStorage.setItem(THEME_STORAGE_KEY, "not-a-theme");

    const viewer = await renderWhiteboard("```mermaid\ngraph TD; A-->B\n```", {
      container,
      mediaQuery,
      storage: localStorage,
    });

    expect(viewer.theme).toBe("system");
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("system");
    expect(document.documentElement.dataset.theme).toBe("light");
    expect(mermaidMocks.render).toHaveBeenCalledTimes(1);

    mediaQuery.change(true);
    await viewer.settled();

    expect(document.documentElement.dataset.theme).toBe("dark");
    expect(mermaidMocks.render).toHaveBeenCalledTimes(2);

    await viewer.setTheme("light");
    mediaQuery.change(false);
    await viewer.settled();

    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("light");
    expect(document.documentElement.dataset.theme).toBe("light");
    expect(mermaidMocks.render).toHaveBeenCalledTimes(3);
    expect(Object.keys(localStorage)).toEqual([THEME_STORAGE_KEY]);
  });

  test("restores stored theme and closes the menu with Escape or outside click", async () => {
    const container = document.querySelector("#viewer");
    localStorage.setItem(THEME_STORAGE_KEY, "dark");

    const viewer = await renderWhiteboard("# Board", { container });
    const trigger = container.querySelector("[data-theme-control]");
    const menu = container.querySelector("[data-theme-menu]");

    expect(trigger.getAttribute("aria-label")).toBe("Appearance: Dark");
    expect(trigger.querySelector('[data-theme-icon="dark"]')).not.toBeNull();
    trigger.click();
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(menu.hidden).toBe(true);
    expect(document.activeElement).toBe(trigger);

    trigger.click();
    document.dispatchEvent(new Event("pointerdown", { bubbles: true }));
    expect(menu.hidden).toBe(true);

    viewer.destroy();
    expect(container.querySelector("[data-theme-control]")).toBeNull();
  });
});

const agentIDs = {
  event: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
  conversation: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
  resource: "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",
  turn: "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD",
  message: "EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE",
  command: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
  archive: "GGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGG",
};

function agentPayload(overrides = {}) {
  return {
    markdown: "# Agent board\n\nTrusted only as page content.",
    context: "Creator summary",
    local_agent: {
      enabled: true,
      context_digest: "0".repeat(64),
      resource: {
        kind: "markdown",
        id: agentIDs.resource,
        created_at: "2026-07-27T01:02:03Z",
        updated_at: "2026-07-27T02:03:04Z",
        expires_at: null,
      },
    },
    ...overrides,
  };
}

function agentEvent(type, payload, overrides = {}) {
  return {
    api_version: "1",
    event_id: agentIDs.event,
    conversation_id: agentIDs.conversation,
    type,
    timestamp: "2026-07-27T03:04:05Z",
    payload,
    ...overrides,
  };
}

function snapshotEvent(overrides = {}) {
  return agentEvent("snapshot", {
    lifecycle: "ready",
    queue: [],
    context_state: "pending",
    active_turn_id: null,
  }, overrides);
}

function fixedIDFactory() {
  return agentIDs.command;
}

describe("local agent source and commands", () => {
  test("validates the closed enabled payload and keeps legacy Markdown payloads", () => {
    expect(validateViewerPayload(agentPayload()).local_agent.enabled).toBe(true);
    expect(validateViewerPayload({ markdown: "legacy" }).local_agent.enabled).toBe(false);
    expect(() => validateViewerPayload({ ...agentPayload(), extra: true })).toThrow(TypeError);
    expect(() => validateViewerPayload({ ...agentPayload(), local_agent: { ...agentPayload().local_agent, context_digest: "A".repeat(64) } })).toThrow(TypeError);
    expect(() => validateViewerPayload({ ...agentPayload(), local_agent: { ...agentPayload().local_agent, resource: { ...agentPayload().local_agent.resource, kind: "html" } } })).toThrow(TypeError);
  });

  test("accepts strict RFC3339 offsets and rejects normalized calendar dates", () => {
    const offsetResource = { ...agentPayload().local_agent.resource, created_at: "2026-07-27T01:02:03+01:00", updated_at: "2026-07-27T02:03:04.123456789+01:00" };
    expect(validateViewerPayload({ ...agentPayload(), local_agent: { ...agentPayload().local_agent, resource: offsetResource } }).local_agent.resource).toEqual(offsetResource);
    const invalidResource = { ...agentPayload().local_agent.resource, updated_at: "2026-02-31T02:03:04Z" };
    expect(() => validateViewerPayload({ ...agentPayload(), local_agent: { ...agentPayload().local_agent, resource: invalidResource } })).toThrow(TypeError);
  });

  test("generates 32-character base64url IDs from exactly 24 random bytes", () => {
    const cryptoObject = { getRandomValues: vi.fn((bytes) => { bytes.forEach((_, index) => { bytes[index] = index; }); return bytes; }) };
    const id = generateAgentID(cryptoObject);
    expect(id).toMatch(/^[A-Za-z0-9_-]{32}$/u);
    expect(cryptoObject.getRandomValues.mock.calls[0][0]).toHaveLength(24);
  });

  test.each([
    [undefined, DEFAULT_AGENT_PORT], ["", DEFAULT_AGENT_PORT], ["0", DEFAULT_AGENT_PORT], ["65536", DEFAULT_AGENT_PORT],
    ["localhost", DEFAULT_AGENT_PORT], ["8568", 8568], [65535, 65535],
  ])("normalizes agent port %j", (input, expected) => {
    expect(normalizeAgentPort(input)).toBe(expected);
  });

  test("reads only boolean drawer state, decimal port, and canonical width preferences", () => {
    localStorage.setItem(AGENT_DRAWER_STORAGE_KEY, "yes");
    localStorage.setItem(AGENT_PORT_STORAGE_KEY, "08080");
    localStorage.setItem(AGENT_DRAWER_WIDTH_STORAGE_KEY, "0420");
    localStorage.setItem("conversation-id", agentIDs.conversation);
    expect(readAgentPreferences(localStorage)).toEqual({ open: false, port: DEFAULT_AGENT_PORT, width: DEFAULT_AGENT_DRAWER_WIDTH });

    localStorage.setItem(AGENT_DRAWER_WIDTH_STORAGE_KEY, "720");
    expect(readAgentPreferences(localStorage).width).toBe(720);
  });

  test("clamps effective width without changing the saved base preference", () => {
    expect(agentDrawerLayoutMode(1024)).toBe("docked");
    expect(agentDrawerLayoutMode(1023)).toBe("modal");
    expect(maxAgentDrawerWidth(1024)).toBe(563);
    expect(clampAgentDrawerWidth(720, 1024)).toBe(563);
    expect(clampAgentDrawerWidth(360, 800)).toBe(360);
  });

  test.each([
    ["360", 360], ["420", 420], ["720", 720],
    ["", DEFAULT_AGENT_DRAWER_WIDTH], ["0420", DEFAULT_AGENT_DRAWER_WIDTH],
    ["360.0", DEFAULT_AGENT_DRAWER_WIDTH], ["721", DEFAULT_AGENT_DRAWER_WIDTH],
    ["359", DEFAULT_AGENT_DRAWER_WIDTH], ["-1", DEFAULT_AGENT_DRAWER_WIDTH], [420, DEFAULT_AGENT_DRAWER_WIDTH],
  ])("normalizes saved drawer width %j", (input, expected) => {
    expect(normalizeAgentDrawerWidth(input)).toBe(expected);
  });

  test("builds an exact context-free connect command", () => {
    const command = createConnectCommand({ payload: agentPayload(), clientID: agentIDs.message, replayAfter: agentIDs.event, idFactory: fixedIDFactory });
    expect(command).toEqual({
      api_version: "1",
      command_id: agentIDs.command,
      client_id: agentIDs.message,
      conversation_id: null,
      type: "connect",
      payload: {
        provider: "pi",
        resource: agentPayload().local_agent.resource,
        context_digest: "0".repeat(64),
        replay_after: agentIDs.event,
      },
    });
    expect(JSON.stringify(command)).not.toContain(agentPayload().markdown);
    expect(JSON.stringify(command)).not.toContain(agentPayload().context);
  });

  test("builds initial, replacement, and unchanged continuation submits", () => {
    const common = { message: "Question", payload: agentPayload(), clientID: agentIDs.message, conversationID: agentIDs.conversation, title: "Agent board", url: "https://board.example/m/abc", idFactory: fixedIDFactory };
    const initial = createSubmitCommand({ ...common, revision: "initial" });
    const replacement = createSubmitCommand({ ...common, revision: "replacement" });
    const continuation = createSubmitCommand(common);
    expect(initial.payload.context).toEqual(expect.objectContaining({ revision: "initial", markdown: agentPayload().markdown, creator_context: agentPayload().context, title: "Agent board", url: "https://board.example/m/abc", digest: "0".repeat(64) }));
    expect(replacement.payload.context.revision).toBe("replacement");
    expect(continuation.payload).not.toHaveProperty("context");
    expect(initial.payload.turn_id).toHaveLength(32);
    expect(initial.payload.message_id).toHaveLength(32);
  });

  test("accepts only HTTPS or literal-loopback HTTP page context URLs", () => {
    const common = { message: "Question", payload: agentPayload(), clientID: agentIDs.message, conversationID: agentIDs.conversation, title: "Agent board", revision: "initial", idFactory: fixedIDFactory };
    expect(() => createSubmitCommand({ ...common, url: "http://127.0.0.1:8080/m/abc" })).not.toThrow();
    for (const url of [
      "http://localhost:8080/m/abc",
      "http://127.0.0.1:80/m/abc",
      "http://127.0.0.1:080/m/abc",
      "http://127.0.0.2:8080/m/abc",
      "http://[::1]:8080/m/abc",
      "http://user@127.0.0.1:8080/m/abc",
      "http://example.test/m/abc",
    ]) expect(() => createSubmitCommand({ ...common, url })).toThrow(TypeError);
  });

  test("measures the message limit as UTF-8 bytes", () => {
    const common = { payload: agentPayload(), clientID: agentIDs.message, conversationID: agentIDs.conversation, title: "Board", url: "https://board.example/m/abc", idFactory: fixedIDFactory };
    expect(() => createSubmitCommand({ ...common, message: "é".repeat(MAX_AGENT_MESSAGE_BYTES / 2) })).not.toThrow();
    expect(() => createSubmitCommand({ ...common, message: `é${"a".repeat(MAX_AGENT_MESSAGE_BYTES - 1)}` })).toThrow(TypeError);
  });
});

describe("local agent transport and event state", () => {
  test("probes only the literal loopback status endpoint before consent", async () => {
    const fetchImpl = vi.fn(async () => ({ ok: true, text: async () => JSON.stringify({ available: true, api_version: "1", origin_trusted: true }) }));
    const transport = createAgentTransport({ payload: agentPayload(), port: 9123, clientID: agentIDs.message, fetchImpl, WebSocketImpl: undefined, idFactory: fixedIDFactory });
    await expect(transport.connect()).rejects.toThrow("consent");
    await expect(transport.probe()).resolves.toEqual({ ok: true, code: null });
    expect(fetchImpl).toHaveBeenCalledOnce();
    expect(fetchImpl.mock.calls[0][0]).toBe("http://127.0.0.1:9123/api/v1/agent/status");
    expect(fetchImpl.mock.calls[0][1]).toMatchObject({ method: "GET", credentials: "omit", referrerPolicy: "no-referrer" });
    expect(fetchImpl.mock.calls[0][1]).not.toHaveProperty("body");
  });

  test("uses the required WebSocket subprotocol and sends no context in connect", async () => {
    const sockets = [];
    class FakeWebSocket {
      constructor(url, protocol) {
        this.url = url; this.protocol = protocol; this.readyState = 1; this.listeners = {}; sockets.push(this);
        queueMicrotask(() => this.listeners.open?.forEach((listener) => listener({})));
      }
      addEventListener(type, listener) { (this.listeners[type] ??= []).push(listener); }
      send(frame) {
        this.frame = frame;
        queueMicrotask(() => this.listeners.message.forEach((listener) => listener({ data: JSON.stringify(snapshotEvent()) })));
      }
      close() {}
    }
    const transport = createAgentTransport({ payload: agentPayload(), clientID: agentIDs.message, fetchImpl: vi.fn(), WebSocketImpl: FakeWebSocket, idFactory: fixedIDFactory });
    transport.grantConsent();
    await transport.connect();
    expect(sockets[0].url).toBe("ws://127.0.0.1:8568/api/v1/agent/connect");
    expect(sockets[0].protocol).toBe(AGENT_WEBSOCKET_PROTOCOL);
    expect(JSON.parse(sockets[0].frame).payload).not.toHaveProperty("context");
    expect(sockets[0].frame).not.toContain("Creator summary");
    transport.close();
  });

  test("falls back to POST NDJSON and frames commands with the API header", async () => {
    class RejectedWebSocket {
      constructor() { this.protocol = ""; this.listeners = {}; queueMicrotask(() => this.listeners.open?.forEach((listener) => listener({}))); }
      addEventListener(type, listener) { (this.listeners[type] ??= []).push(listener); }
      close() {}
    }
    const line = `${JSON.stringify(snapshotEvent())}\n`;
    const fetchImpl = vi.fn(async () => ({
      ok: true,
      body: new ReadableStream({ start(controller) { controller.enqueue(new TextEncoder().encode(line)); controller.close(); } }),
      json: async () => null,
    }));
    const transport = createAgentTransport({ payload: agentPayload(), clientID: agentIDs.message, fetchImpl, WebSocketImpl: RejectedWebSocket, idFactory: fixedIDFactory });
    transport.grantConsent();
    await transport.connect();
    expect(transport.transportKind).toBe("fallback");
    expect(fetchImpl.mock.calls[0][0]).toBe("http://127.0.0.1:8568/api/v1/agent/connect");
    expect(fetchImpl.mock.calls[0][1].headers).toEqual({ "Content-Type": "application/json", "X-Agent-Whiteboard-API-Version": "1" });
    expect(fetchImpl.mock.calls[0][1].body).not.toContain("Creator summary");
    transport.close();
  });

  test("processes every complete fallback event already buffered after the snapshot", async () => {
    const events = [];
    const frames = [
      snapshotEvent(),
      agentEvent("provider", { provider: "pi", state: "ready", model: "local-model" }, { event_id: "HHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH" }),
    ];
    const body = new ReadableStream({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(`${frames.map((frame) => JSON.stringify(frame)).join("\n")}\n`));
        controller.close();
      },
    });
    const fetchImpl = vi.fn(async () => ({ ok: true, body }));
    const transport = createAgentTransport({ payload: agentPayload(), clientID: agentIDs.message, fetchImpl, WebSocketImpl: undefined, onEvent: (event) => events.push(event), idFactory: fixedIDFactory });
    transport.grantConsent();
    await transport.connect();
    await vi.waitFor(() => expect(events).toHaveLength(2));
    expect(events.map((event) => event.type)).toEqual(["snapshot", "provider"]);
    transport.close();
  });

  test("strictly rejects unknown event fields, malformed IDs, and raw provider fields", () => {
    expect(() => decodeAgentEvent(JSON.stringify({ ...snapshotEvent(), extra: true }))).toThrow(TypeError);
    expect(() => decodeAgentEvent(JSON.stringify(snapshotEvent()).replace('"api_version":"1"', '"api_version":"1","api_version":"1"'))).toThrow(TypeError);
    expect(() => decodeAgentEvent(JSON.stringify(snapshotEvent({ event_id: "short" })))).toThrow(TypeError);
    expect(() => decodeAgentEvent(JSON.stringify(agentEvent("assistant_delta", { turn_id: agentIDs.turn, message_id: agentIDs.message, text: "hello", reasoning: "secret" })))).toThrow(TypeError);
  });

  test("normalizes snapshot, streaming, completion, queue, and deduplicates event IDs", () => {
    const state = createAgentState();
    expect(applyAgentEvent(state, snapshotEvent())).toBe(true);
    expect(applyAgentEvent(state, snapshotEvent())).toBe(false);
    applyAgentEvent(state, agentEvent("assistant_delta", { turn_id: agentIDs.turn, message_id: agentIDs.message, text: "hel" }, { event_id: "HHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH" }));
    applyAgentEvent(state, agentEvent("assistant_delta", { turn_id: agentIDs.turn, message_id: agentIDs.message, text: "lo" }, { event_id: "IIIIIIIIIIIIIIIIIIIIIIIIIIIIIIII" }));
    applyAgentEvent(state, agentEvent("queue", { items: [{ turn_id: "JJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJ", message_id: "KKKKKKKKKKKKKKKKKKKKKKKKKKKKKKKK", message: "next" }] }, { event_id: "LLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLL" }));
    applyAgentEvent(state, agentEvent("completion", { turn_id: agentIDs.turn }, { event_id: "MMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMM" }));
    expect(state.timeline[0]).toMatchObject({ kind: "assistant", text: "hello", streaming: true });
    expect(state.queue[0].message).toBe("next");
    expect(state.lifecycle).toBe("ready");
    expect(state.lastEventID).toBe("MMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMM");
  });

  test("keeps command correlation when an HTTP result arrives before its streamed page", () => {
    const state = createAgentState();
    applyAgentEvent(state, snapshotEvent());
    const command = createAgentCommand({ type: "history_page", payload: { limit: 50 }, clientID: agentIDs.message, conversationID: agentIDs.conversation, idFactory: fixedIDFactory });
    registerAgentCommand(state, command);
    applyAgentEvent(state, agentEvent("command_result", { command_id: command.command_id, status: "succeeded" }, { event_id: "HHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH" }));
    expect(() => applyAgentEvent(state, agentEvent("timeline", { command_id: command.command_id, items: [], next_cursor: null }, { event_id: "IIIIIIIIIIIIIIIIIIIIIIIIIIIIIIII" }))).not.toThrow();
  });

  test("normalizes newest-first history pages into chronological display order", () => {
    const state = createAgentState();
    applyAgentEvent(state, snapshotEvent());
    const command = createAgentCommand({ type: "history_page", payload: { limit: 50 }, clientID: agentIDs.message, conversationID: agentIDs.conversation, idFactory: fixedIDFactory });
    registerAgentCommand(state, command);
    applyAgentEvent(state, agentEvent("timeline", {
      command_id: command.command_id,
      items: [
        { item_id: "HHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH", kind: "assistant", turn_id: agentIDs.turn, message_id: "IIIIIIIIIIIIIIIIIIIIIIIIIIIIIIII", text: "newer", created_at: "2026-07-27T03:03:00Z" },
        { item_id: "JJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJ", kind: "user", turn_id: agentIDs.turn, message_id: "KKKKKKKKKKKKKKKKKKKKKKKKKKKKKKKK", text: "older", created_at: "2026-07-27T03:02:00Z" },
      ],
      next_cursor: "JJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJ",
    }, { event_id: "LLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLL" }));
    expect(state.timeline.map((item) => item.text)).toEqual(["older", "newer"]);
  });

  test("accepts empty archive previews and normalizes archive deletion", () => {
    const state = createAgentState();
    applyAgentEvent(state, snapshotEvent());
    const listCommand = createAgentCommand({ type: "archive_list", payload: {}, clientID: agentIDs.message, conversationID: agentIDs.conversation, idFactory: fixedIDFactory });
    registerAgentCommand(state, listCommand);
    applyAgentEvent(state, agentEvent("history", {
      command_id: listCommand.command_id,
      items: [{ archive_id: agentIDs.archive, created_at: "2026-07-27T01:02:03Z", updated_at: "2026-07-27T02:03:04Z", provider: "pi", preview: "" }],
      next_cursor: null,
    }, { event_id: "RRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRR" }));
    expect(state.archives).toEqual([expect.objectContaining({ archive_id: agentIDs.archive, preview: "" })]);
    expect(createAgentCommand({ type: "archive_restore", payload: { archive_id: agentIDs.archive }, clientID: agentIDs.message, conversationID: agentIDs.conversation, idFactory: fixedIDFactory }).payload.archive_id).toBe(agentIDs.archive);
    applyAgentEvent(state, agentEvent("archive", { action: "deleted", archive_id: agentIDs.archive }, { event_id: "SSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSS" }));
    expect(state.archives).toEqual([]);
  });

  test("requires command-result correlation and surfaces only validated stable errors", () => {
    const state = createAgentState();
    applyAgentEvent(state, snapshotEvent());
    const command = createAgentCommand({ type: "new", payload: {}, clientID: agentIDs.message, conversationID: agentIDs.conversation, idFactory: fixedIDFactory });
    registerAgentCommand(state, command);
    const result = agentEvent("command_result", { command_id: command.command_id, status: "rejected", error: { code: "invalid_state", message: "The command is not valid for the current conversation state.", action: "refresh_state" } }, { event_id: "NNNNNNNNNNNNNNNNNNNNNNNNNNNNNNNN" });
    expect(applyAgentEvent(state, result)).toBe(true);
    expect(applyAgentEvent(state, result)).toBe(false);
    expect(state.errors).toEqual([expect.objectContaining({ code: "invalid_state" })]);
    expect(() => applyAgentEvent(createAgentState(), { ...result, event_id: "OOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOO" })).toThrow("uncorrelated");
  });

  test("does not advance the replay cursor when state application rejects an event", async () => {
    class FakeWebSocket {
      constructor(_url, protocol) { this.protocol = protocol; this.readyState = 1; this.listeners = {}; queueMicrotask(() => this.listeners.open.forEach((listener) => listener({}))); }
      addEventListener(type, listener) { (this.listeners[type] ??= []).push(listener); }
      send() { queueMicrotask(() => this.listeners.message.forEach((listener) => listener({ data: JSON.stringify(snapshotEvent()) }))); }
      close() {}
    }
    const transport = createAgentTransport({
      payload: agentPayload(), clientID: agentIDs.message, fetchImpl: vi.fn(), WebSocketImpl: FakeWebSocket, idFactory: fixedIDFactory,
      onEvent: () => { throw new TypeError("state rejected"); },
    });
    transport.grantConsent();
    await expect(transport.connect()).rejects.toThrow("state rejected");
    expect(transport.lastEventID).toBeNull();
    expect(transport.conversationID).toBeNull();
    transport.close();
  });

  test("can force a fresh reconnect snapshot without replaying or resubmitting a turn", async () => {
    const frames = [];
    class FakeWebSocket {
      constructor(_url, protocol) { this.protocol = protocol; this.readyState = 1; this.listeners = {}; queueMicrotask(() => this.listeners.open.forEach((listener) => listener({}))); }
      addEventListener(type, listener) { (this.listeners[type] ??= []).push(listener); }
      send(frame) {
        frames.push(JSON.parse(frame));
        queueMicrotask(() => this.listeners.message.forEach((listener) => listener({ data: JSON.stringify(snapshotEvent({ event_id: frames.length === 1 ? agentIDs.event : "PPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPP" })) })));
      }
      close() {}
    }
    const transport = createAgentTransport({ payload: agentPayload(), clientID: agentIDs.message, fetchImpl: vi.fn(), WebSocketImpl: FakeWebSocket, idFactory: fixedIDFactory });
    transport.grantConsent();
    await transport.connect();
    transport.resetReplay();
    await transport.reconnect();
    expect(frames.map((frame) => frame.type)).toEqual(["connect", "connect"]);
    expect(frames[1].payload).not.toHaveProperty("replay_after");
    expect(frames.every((frame) => frame.type !== "submit")).toBe(true);
    transport.close();
  });

  test("a reconnect command includes only last-event replay and never replays a submit", async () => {
    const frames = [];
    class FakeWebSocket {
      constructor(_url, protocol) { this.protocol = protocol; this.readyState = 1; this.listeners = {}; queueMicrotask(() => this.listeners.open.forEach((listener) => listener({}))); }
      addEventListener(type, listener) { (this.listeners[type] ??= []).push(listener); }
      send(frame) { frames.push(JSON.parse(frame)); queueMicrotask(() => this.listeners.message.forEach((listener) => listener({ data: JSON.stringify(snapshotEvent({ event_id: frames.length === 1 ? agentIDs.event : "PPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPP" })) }))); }
      close() {}
    }
    const transport = createAgentTransport({ payload: agentPayload(), clientID: agentIDs.message, fetchImpl: vi.fn(), WebSocketImpl: FakeWebSocket, idFactory: fixedIDFactory });
    transport.grantConsent();
    await transport.connect();
    await transport.reconnect();
    expect(frames.map((frame) => frame.type)).toEqual(["connect", "connect"]);
    expect(frames[1].payload.replay_after).toBe(agentIDs.event);
    expect(frames[1].payload).not.toHaveProperty("context");
    transport.close();
  });
});

describe("local agent rendering and controls", () => {
  test("separates identity, concise status, and offline, ready, and connected bodies", async () => {
    let options;
    const transport = {
      clientID: agentIDs.message,
      conversationID: agentIDs.conversation,
      consented: false,
      probe: vi.fn()
        .mockResolvedValueOnce({ ok: false, code: "broker_unavailable" })
        .mockResolvedValueOnce({ ok: true, code: null }),
      grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn(), send: vi.fn(async () => {}),
    };
    const drawer = createAgentDrawer({
      payload: agentPayload(), doc: document, storage: localStorage,
      pageTitle: "Agent board", pageURL: "https://board.example/m/abc",
      transportFactory: (input) => { options = input; return transport; },
    });

    const header = drawer.elements.drawer.querySelector(".agent-drawer-header");
    const statusBar = drawer.elements.drawer.querySelector(".agent-status-bar");
    const setup = drawer.elements.setup;
    await vi.waitFor(() => expect(statusBar?.textContent).toContain("Broker unavailable"));
    expect(header?.textContent).toContain("Page agent");
    expect(header?.textContent).toContain("Content-only · Local Pi");
    expect(header?.textContent).not.toContain("Broker unavailable");
    expect(statusBar?.textContent).toContain("Port 8568");
    expect(setup.querySelector("h3")?.textContent).toBe("Pi isn’t available on this device");
    expect(setup.textContent).toContain("No page content has been shared");
    expect(setup.querySelector(".agent-context-disclosure")?.textContent).toContain("Full Markdown + creator notes");
    expect(setup.querySelector(".agent-context-disclosure")?.textContent).toContain("Not shared");
    expect(drawer.elements.connectButton.hidden).toBe(true);
    setup.querySelector(".agent-context-disclosure button").click();
    expect(drawer.elements.contextDetails.hidden).toBe(false);
    expect(document.activeElement).toBe(drawer.elements.backButton);
    drawer.elements.backButton.click();

    await drawer.probe();
    expect(statusBar?.textContent).toContain("Pi ready");
    expect(statusBar?.textContent).toContain("Not connected");
    expect(setup.querySelector("h3")?.textContent).toBe("Ready to connect");
    expect(setup.textContent).toContain("Complete Markdown and creator notes on the first message");
    expect(drawer.elements.connectButton.textContent).toBe("Connect to Pi");
    expect(drawer.elements.connectButton.hidden).toBe(false);

    options.onEvent(snapshotEvent());
    options.onEvent(agentEvent("provider", { provider: "pi", state: "ready", model: "fixture-model" }, { event_id: "HHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH" }));
    expect(statusBar?.textContent).toContain("Connected");
    expect(statusBar?.textContent).toContain("fixture-model");
    expect(setup.hidden).toBe(true);
    expect(drawer.elements.timeline.hidden).toBe(false);
    drawer.destroy();
  });

  test("builds the ChatGPT-like shell with context first and a visible processing indicator", () => {
    let options;
    const transport = {
      clientID: agentIDs.message, conversationID: agentIDs.conversation, consented: true,
      probe: vi.fn(async () => ({ ok: true, code: null })), grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn(), send: vi.fn(async () => {}),
    };
    const drawer = createAgentDrawer({
      payload: agentPayload(), doc: document, storage: localStorage,
      pageTitle: "Agent board", pageURL: "https://board.example/m/abc",
      transportFactory: (input) => { options = input; return transport; },
    });
    options.onEvent(agentEvent("snapshot", { lifecycle: "responding", queue: [], context_state: "accepted", active_turn_id: agentIDs.turn }));

    const headerActions = drawer.elements.drawer.querySelector(".agent-header-actions");
    expect(headerActions?.contains(drawer.elements.overflowButton)).toBe(true);
    expect(headerActions?.contains(drawer.elements.close)).toBe(true);
    expect(drawer.elements.timeline.firstElementChild?.classList.contains("agent-context-summary")).toBe(true);
    expect(drawer.elements.timeline.firstElementChild?.textContent).toContain("Agent board");
    expect(drawer.elements.timeline.firstElementChild?.textContent).toContain("Context attached");
    expect(drawer.elements.timeline.firstElementChild?.querySelector("button")?.textContent).toBe("Inspect context");
    expect(drawer.elements.timeline.querySelectorAll(".agent-response-dot")).toHaveLength(3);
    expect(drawer.elements.timeline.querySelector(".agent-response-loading")?.getAttribute("aria-label")).toBe("Pi is responding");
    expect(drawer.elements.composer.parentElement?.classList.contains("agent-composer-wrap")).toBe(true);
    expect(drawer.elements.message.placeholder).toBe("Ask about this page…");
    drawer.destroy();
  });

  test("discloses authority and gates complete context until revision and delivery are known", async () => {
    let options;
    const sent = [];
    const transport = {
      clientID: agentIDs.message,
      conversationID: agentIDs.conversation,
      consented: true,
      probe: vi.fn(async () => ({ ok: true, code: null })),
      grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn(),
      send: vi.fn(async (command) => {
        sent.push(command);
        if (sent.length === 1) {
          const error = new Error("invalid_state");
          error.code = "invalid_state";
          throw error;
        }
      }),
    };
    const drawer = createAgentDrawer({
      payload: agentPayload(), doc: document, storage: localStorage,
      pageTitle: "Agent board", pageURL: "https://board.example/m/abc",
      transportFactory: (input) => { options = input; return transport; },
    });
    await vi.waitFor(() => expect(document.querySelector(".agent-consent")?.textContent).toContain("sends no page content"));
    options.onEvent(snapshotEvent());
    expect(drawer.elements.composer.querySelector('button[type="submit"]').disabled).toBe(true);

    const historyCommand = createAgentCommand({ type: "history_page", payload: { limit: 50 }, clientID: agentIDs.message, conversationID: agentIDs.conversation, idFactory: fixedIDFactory });
    registerAgentCommand(drawer.state, historyCommand);
    options.onEvent(agentEvent("timeline", { command_id: historyCommand.command_id, items: [], next_cursor: null }, { event_id: "HHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH" }));
    expect(drawer.elements.composer.querySelector('button[type="submit"]').disabled).toBe(false);

    drawer.elements.message.value = "first";
    drawer.elements.composer.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0].payload.context.revision).toBe("initial");
    expect(drawer.elements.composer.querySelector('button[type="submit"]').disabled).toBe(false);

    drawer.elements.message.value = "second";
    drawer.elements.composer.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(sent).toHaveLength(2));
    expect(sent[1].payload.context.revision).toBe("initial");
    expect(drawer.elements.composer.querySelector('button[type="submit"]').disabled).toBe(true);

    options.onDisconnect(new Error("socket closed"));
    expect(transport.resetReplay).toHaveBeenCalledOnce();
    expect(document.querySelector(".agent-context")?.textContent).toContain("delivery outcome unknown");
    options.onEvent(snapshotEvent({ event_id: "IIIIIIIIIIIIIIIIIIIIIIIIIIIIIIII" }));
    expect(drawer.elements.composer.querySelector('button[type="submit"]').disabled).toBe(false);
    drawer.destroy();
  });

  test("waits for history before sending a complete replacement context", async () => {
    let options;
    const sent = [];
    const transport = {
      clientID: agentIDs.message, conversationID: agentIDs.conversation, consented: true,
      probe: vi.fn(async () => ({ ok: true, code: null })), grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn(),
      send: vi.fn(async (command) => sent.push(command)),
    };
    const drawer = createAgentDrawer({
      payload: agentPayload(), doc: document, storage: localStorage,
      pageTitle: "Agent board", pageURL: "https://board.example/m/abc",
      transportFactory: (input) => { options = input; return transport; },
    });
    options.onEvent(snapshotEvent());
    const historyCommand = createAgentCommand({ type: "history_page", payload: { limit: 50 }, clientID: agentIDs.message, conversationID: agentIDs.conversation, idFactory: fixedIDFactory });
    registerAgentCommand(drawer.state, historyCommand);
    options.onEvent(agentEvent("timeline", {
      command_id: historyCommand.command_id,
      items: [{ item_id: "HHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH", kind: "user", turn_id: agentIDs.turn, message_id: "IIIIIIIIIIIIIIIIIIIIIIIIIIIIIIII", text: "existing", created_at: "2026-07-27T03:03:00Z" }],
      next_cursor: null,
    }, { event_id: "JJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJ" }));
    drawer.elements.message.value = "replacement";
    drawer.elements.composer.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0].payload.context.revision).toBe("replacement");
    drawer.destroy();
  });

  test("sanitizes chat Markdown, keeps Mermaid inert, and renders no raw reasoning fields", () => {
    const html = renderAgentMarkdown('safe <img src=x onerror=alert(1)>\n\n![remote](https://tracker.invalid/pixel)\n\n```mermaid\ngraph TD; A-->B\n```', document);
    const host = document.createElement("div"); host.innerHTML = html;
    expect(host.querySelector("img")).toBeNull();
    expect(host.querySelector(".mermaid-placeholder")).toBeNull();
    expect(host.querySelector("code.language-mermaid")?.textContent).toContain("graph TD");
  });

  test("renders shared queue edit/remove and archive restore/delete controls", async () => {
    const calls = [];
    let options;
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    const transport = {
      clientID: agentIDs.message,
      conversationID: agentIDs.conversation,
      consented: true,
      probe: vi.fn(async () => ({ ok: true, code: null })),
      grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn(),
      send: vi.fn(async (command) => { calls.push(command); }),
    };
    const drawer = createAgentDrawer({ payload: agentPayload(), doc: document, storage: localStorage, transportFactory: (input) => { options = input; return transport; } });
    options.onEvent(snapshotEvent());
    options.onEvent(agentEvent("queue", { items: [{ turn_id: agentIDs.turn, message_id: agentIDs.message, message: "queued" }] }, { event_id: "QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ" }));
    drawer.elements.queue.querySelector("textarea").value = "edited";
    drawer.elements.queue.querySelector("button").click();
    drawer.elements.queue.querySelectorAll("button")[1].click();

    const listCommand = createAgentCommand({ type: "archive_list", payload: {}, clientID: agentIDs.message, conversationID: agentIDs.conversation, idFactory: fixedIDFactory });
    registerAgentCommand(drawer.state, listCommand);
    options.onEvent(agentEvent("history", {
      command_id: listCommand.command_id,
      items: [{ archive_id: agentIDs.archive, created_at: "2026-07-27T01:02:03Z", updated_at: "2026-07-27T02:03:04Z", provider: "pi", preview: "" }],
      next_cursor: null,
    }, { event_id: "RRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRR" }));
    drawer.elements.archives.querySelector("button").click();
    drawer.elements.archives.querySelectorAll("button")[1].click();
    await vi.waitFor(() => expect(calls).toHaveLength(4));
    expect(calls.map((command) => command.type)).toEqual(["queue_edit", "queue_remove", "archive_restore", "archive_delete"]);
    expect(calls[0].payload).toEqual({ message_id: agentIDs.message, message: "edited" });
    expect(confirm).toHaveBeenCalledTimes(2);
    drawer.destroy();
    confirm.mockRestore();
  });

  test("traps focus and exposes modal semantics at the mobile breakpoint", () => {
    const previousMatchMedia = window.matchMedia;
    window.matchMedia = vi.fn(() => ({ matches: true }));
    try {
      const transport = { clientID: agentIDs.message, conversationID: null, consented: false, probe: vi.fn(async () => ({ ok: false, code: "broker_unavailable" })), grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), send: vi.fn(), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn() };
      const drawer = createAgentDrawer({ payload: agentPayload(), doc: document, storage: localStorage, transportFactory: () => transport });
      drawer.elements.toggle.focus();
      drawer.elements.toggle.click();
      expect(document.querySelector(".agent-drawer")?.getAttribute("role")).toBe("dialog");
      expect(document.querySelector(".agent-drawer")?.getAttribute("aria-modal")).toBe("true");
      expect(document.body.classList.contains("agent-drawer-modal-open")).toBe(true);
      const outside = document.createElement("button");
      document.querySelector("#viewer").append(outside);
      outside.focus();
      outside.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true }));
      expect(document.activeElement).toBe(drawer.elements.close);
      drawer.elements.close.click();
      expect(document.activeElement).toBe(drawer.elements.toggle);
      drawer.destroy();
      expect(document.body.classList.contains("agent-drawer-modal-open")).toBe(false);
    } finally {
      window.matchMedia = previousMatchMedia;
    }
  });

  test("desktop separator resizes with pointer capture and persists once after cleanup", () => {
    const transport = { clientID: agentIDs.message, conversationID: null, consented: false, probe: vi.fn(async () => ({ ok: false, code: "broker_unavailable" })), grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), send: vi.fn(), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn() };
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 1400 });
    const drawer = createAgentDrawer({ payload: agentPayload(), doc: document, storage: localStorage, transportFactory: () => transport });
    drawer.setOpen(true);
    const separator = drawer.elements.separator;
    expect(separator.getAttribute("role")).toBe("separator");
    expect(separator.getAttribute("aria-valuemin")).toBe("360");
    expect(separator.getAttribute("aria-valuemax")).toBe("720");
    expect(separator.getAttribute("aria-valuenow")).toBe("420");

    separator.setPointerCapture = vi.fn();
    separator.releasePointerCapture = vi.fn();
    separator.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, pointerId: 7, clientX: 900 }));
    expect(separator.setPointerCapture).toHaveBeenCalledWith(7);
    separator.dispatchEvent(new PointerEvent("pointermove", { bubbles: true, pointerId: 7, clientX: 760 }));
    expect(localStorage.getItem(AGENT_DRAWER_WIDTH_STORAGE_KEY)).toBeNull();
    separator.dispatchEvent(new PointerEvent("pointerup", { bubbles: true, pointerId: 7, clientX: 760 }));
    expect(localStorage.getItem(AGENT_DRAWER_WIDTH_STORAGE_KEY)).toBe("640");
    expect(separator.releasePointerCapture).toHaveBeenCalledWith(7);
    expect(document.body.style.userSelect).toBe("");
    drawer.destroy();
  });

  test("keyboard separator controls use growth steps, reset, and never persist clamped viewport width", () => {
    localStorage.setItem(AGENT_DRAWER_WIDTH_STORAGE_KEY, "720");
    const transport = { clientID: agentIDs.message, conversationID: null, consented: false, probe: vi.fn(async () => ({ ok: false, code: "broker_unavailable" })), grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), send: vi.fn(), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn() };
    const drawer = createAgentDrawer({ payload: agentPayload(), doc: document, storage: localStorage, transportFactory: () => transport });
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 1024 });
    drawer.setOpen(true);
    window.dispatchEvent(new Event("resize"));
    expect(drawer.elements.separator.getAttribute("aria-valuenow")).toBe("563");
    expect(localStorage.getItem(AGENT_DRAWER_WIDTH_STORAGE_KEY)).toBe("720");
    drawer.elements.separator.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowLeft", bubbles: true }));
    expect(localStorage.getItem(AGENT_DRAWER_WIDTH_STORAGE_KEY)).toBe("563");
    drawer.elements.separator.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowRight", shiftKey: true, bubbles: true }));
    expect(localStorage.getItem(AGENT_DRAWER_WIDTH_STORAGE_KEY)).toBe("531");
    drawer.elements.separator.dispatchEvent(new MouseEvent("dblclick", { bubbles: true }));
    expect(localStorage.getItem(AGENT_DRAWER_WIDTH_STORAGE_KEY)).toBe("420");
    drawer.elements.separator.dispatchEvent(new KeyboardEvent("keydown", { key: "Home", bubbles: true }));
    expect(localStorage.getItem(AGENT_DRAWER_WIDTH_STORAGE_KEY)).toBe("420");
    drawer.destroy();
  });

  test("cancels pointer resize on cancellation, close, breakpoint change, and teardown", () => {
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 1400 });
    const transport = { clientID: agentIDs.message, conversationID: null, consented: false, probe: vi.fn(async () => ({ ok: false, code: "broker_unavailable" })), grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), send: vi.fn(), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn() };
    const drawer = createAgentDrawer({ payload: agentPayload(), doc: document, storage: localStorage, transportFactory: () => transport });
    drawer.setOpen(true);
    const separator = drawer.elements.separator;
    separator.setPointerCapture = vi.fn();
    separator.releasePointerCapture = vi.fn();
    const start = (pointerId) => {
      separator.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, pointerId, button: 0, clientX: 980 }));
      separator.dispatchEvent(new PointerEvent("pointermove", { bubbles: true, pointerId, clientX: 820 }));
      expect(document.body.style.userSelect).toBe("none");
    };

    start(1);
    separator.dispatchEvent(new PointerEvent("pointercancel", { bubbles: true, pointerId: 1 }));
    expect(document.body.style.userSelect).toBe("");
    expect(separator.getAttribute("aria-valuenow")).toBe("420");
    expect(localStorage.getItem(AGENT_DRAWER_WIDTH_STORAGE_KEY)).toBeNull();

    start(2);
    drawer.setOpen(false);
    expect(document.body.style.userSelect).toBe("");
    expect(separator.releasePointerCapture).toHaveBeenCalledWith(2);

    drawer.setOpen(true);
    start(3);
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 800 });
    window.dispatchEvent(new Event("resize"));
    expect(document.body.style.userSelect).toBe("");
    expect(drawer.elements.drawer.getAttribute("role")).toBe("dialog");

    Object.defineProperty(window, "innerWidth", { configurable: true, value: 1400 });
    window.dispatchEvent(new Event("resize"));
    start(4);
    drawer.destroy();
    expect(document.body.style.userSelect).toBe("");
    expect(document.body.classList.contains("agent-drawer-resizing")).toBe(false);
  });

  test("keeps onboarding, settings, context, and refreshed archives as alternate views", async () => {
    let options;
    const sent = [];
    const transport = { clientID: agentIDs.message, conversationID: agentIDs.conversation, consented: false, probe: vi.fn(async () => ({ ok: true, code: null })), grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), send: vi.fn(async (command) => sent.push(command)), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn() };
    const drawer = createAgentDrawer({ payload: agentPayload(), doc: document, storage: localStorage, transportFactory: (input) => { options = input; return transport; } });
    drawer.setOpen(true);
    expect(drawer.elements.drawer.querySelector("header")?.contains(drawer.elements.overflowButton)).toBe(true);
    expect(drawer.elements.setup.hidden).toBe(false);
    expect(drawer.elements.settings.hidden).toBe(true);
    expect(drawer.elements.contextDetails.hidden).toBe(true);

    const chooseMenu = (label) => {
      drawer.elements.overflowButton.click();
      const button = [...drawer.elements.overflowMenu.querySelectorAll('[role="menuitem"]')].find((item) => item.textContent === label);
      button.focus();
      button.click();
    };
    chooseMenu("Connection settings");
    expect(drawer.elements.setup.hidden).toBe(true);
    expect(drawer.elements.settings.hidden).toBe(false);
    expect(document.activeElement).toBe(drawer.elements.backButton);
    drawer.elements.backButton.click();
    expect(document.activeElement).toBe(drawer.elements.overflowButton);
    chooseMenu("Inspect page context");
    expect(drawer.elements.contextDetails.hidden).toBe(false);
    expect(drawer.elements.timeline.hidden).toBe(true);
    expect(document.activeElement).toBe(drawer.elements.backButton);
    drawer.elements.backButton.click();
    expect(document.activeElement).toBe(drawer.elements.overflowButton);

    options.onEvent(agentEvent("snapshot", { lifecycle: "ready", queue: [], context_state: "accepted", active_turn_id: null }));
    chooseMenu("Archives");
    await vi.waitFor(() => expect(sent.filter((command) => command.type === "archive_list")).toHaveLength(1));
    const firstList = sent.filter((command) => command.type === "archive_list").at(-1);
    options.onEvent(agentEvent("history", { command_id: firstList.command_id, items: [{ archive_id: agentIDs.archive, created_at: "2026-07-27T01:02:03Z", updated_at: "2026-07-27T02:03:04Z", provider: "pi", model: "old-model", preview: "" }], next_cursor: null }, { event_id: "HHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH" }));
    expect(drawer.state.archives).toEqual([expect.objectContaining({ model: "old-model" })]);

    drawer.elements.backButton.click();
    expect(document.activeElement).toBe(drawer.elements.message);
    chooseMenu("Archives");
    await vi.waitFor(() => expect(sent.filter((command) => command.type === "archive_list")).toHaveLength(2));
    const refreshedList = sent.filter((command) => command.type === "archive_list").at(-1);
    options.onEvent(agentEvent("history", { command_id: refreshedList.command_id, items: [{ archive_id: agentIDs.archive, created_at: "2026-07-27T01:02:03Z", updated_at: "2026-07-27T03:03:04Z", provider: "pi", model: "new-model", preview: "" }], next_cursor: null }, { event_id: "IIIIIIIIIIIIIIIIIIIIIIIIIIIIIIII" }));
    expect(drawer.state.archives).toEqual([expect.objectContaining({ model: "new-model" })]);

    drawer.elements.backButton.click();
    chooseMenu("Archives");
    await vi.waitFor(() => expect(sent.filter((command) => command.type === "archive_list")).toHaveLength(3));
    const emptyList = sent.filter((command) => command.type === "archive_list").at(-1);
    options.onEvent(agentEvent("history", { command_id: emptyList.command_id, items: [], next_cursor: null }, { event_id: "JJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJ" }));
    expect(drawer.state.archives).toEqual([]);
    drawer.destroy();
  });

  test("moves outside focus into a pane that becomes modal and tolerates storage failures", () => {
    const previousMatchMedia = window.matchMedia;
    window.matchMedia = vi.fn((query) => ({ matches: query.includes("63.999") && window.innerWidth < 1024 }));
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 1200 });
    const storage = { getItem: vi.fn(() => { throw new Error("disabled"); }), setItem: vi.fn(() => { throw new Error("disabled"); }) };
    try {
      const transport = { clientID: agentIDs.message, conversationID: null, consented: false, probe: vi.fn(async () => ({ ok: false, code: "broker_unavailable" })), grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), send: vi.fn(), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn() };
      const outside = document.createElement("button"); document.querySelector("#viewer").append(outside);
      const drawer = createAgentDrawer({ payload: agentPayload(), doc: document, storage, transportFactory: () => transport });
      drawer.setOpen(true);
      outside.focus();
      Object.defineProperty(window, "innerWidth", { configurable: true, value: 800 });
      window.dispatchEvent(new Event("resize"));
      expect(document.activeElement).toBe(drawer.elements.close);
      expect(drawer.elements.drawer.getAttribute("aria-modal")).toBe("true");
      outside.focus();
      outside.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true }));
      expect(document.activeElement).toBe(drawer.elements.close);
      expect(() => drawer.setOpen(false)).not.toThrow();
      drawer.destroy();
    } finally {
      window.matchMedia = previousMatchMedia;
    }
  });

  test("enter submits without IME submission while Shift+Enter inserts a newline", async () => {
    let options;
    const transport = { clientID: agentIDs.message, conversationID: agentIDs.conversation, consented: true, probe: vi.fn(async () => ({ ok: true, code: null })), grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), send: vi.fn(async () => {}), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn() };
    const drawer = createAgentDrawer({ payload: agentPayload(), doc: document, storage: localStorage, transportFactory: (input) => { options = input; return transport; } });
    options.onEvent(agentEvent("snapshot", { lifecycle: "ready", queue: [], context_state: "accepted", active_turn_id: null }));
    Object.defineProperty(drawer.elements.message, "scrollHeight", { configurable: true, value: 240 });
    drawer.elements.message.dispatchEvent(new Event("input", { bubbles: true }));
    expect(drawer.elements.message.style.height).toBe("160px");
    expect(drawer.elements.message.style.overflowY).toBe("auto");
    drawer.elements.message.value = "hello";
    drawer.elements.message.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(transport.send).toHaveBeenCalledOnce());
    drawer.elements.message.value = "line";
    const shift = new KeyboardEvent("keydown", { key: "Enter", shiftKey: true, bubbles: true, cancelable: true });
    drawer.elements.message.dispatchEvent(shift);
    expect(shift.defaultPrevented).toBe(false);
    const composing = new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true });
    Object.defineProperty(composing, "isComposing", { value: true });
    drawer.elements.message.dispatchEvent(composing);
    expect(transport.send).toHaveBeenCalledOnce();

    options.onEvent(agentEvent("snapshot", { lifecycle: "responding", queue: [], context_state: "accepted", active_turn_id: agentIDs.turn }, { event_id: "HHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH" }));
    drawer.elements.message.value = "queue this";
    drawer.elements.composer.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(transport.send).toHaveBeenCalledTimes(2));
    expect(drawer.elements.stopButton.disabled).toBe(false);
    drawer.elements.stopButton.click();
    await vi.waitFor(() => expect(transport.send).toHaveBeenCalledTimes(3));
    expect(transport.send.mock.calls.map(([command]) => command.type)).toEqual(["submit", "submit", "interrupt"]);
    drawer.destroy();
  });

  test("shows Sending until correlated acceptance and clears it on rejection or disconnect", async () => {
    let options;
    const transport = { clientID: agentIDs.message, conversationID: agentIDs.conversation, consented: true, probe: vi.fn(async () => ({ ok: true, code: null })), grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), send: vi.fn(() => new Promise(() => {})), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn() };
    const drawer = createAgentDrawer({ payload: agentPayload(), doc: document, storage: localStorage, transportFactory: (input) => { options = input; return transport; } });
    options.onEvent(agentEvent("snapshot", { lifecycle: "ready", queue: [], context_state: "accepted", active_turn_id: null }));
    drawer.elements.message.value = "first";
    drawer.elements.composer.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(drawer.elements.drawer.querySelector(".agent-live-status")?.textContent).toBe("Sending"));
    const first = transport.send.mock.calls[0][0];
    options.onEvent(agentEvent("command_result", { command_id: first.command_id, status: "rejected", error: { code: "invalid_state", message: "The command is not valid for the current conversation state.", action: "refresh_state" } }, { event_id: "HHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH" }));
    expect(drawer.elements.drawer.querySelector(".agent-live-status")?.textContent).toBe("Connected");

    drawer.elements.message.value = "second";
    drawer.elements.composer.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(transport.send).toHaveBeenCalledTimes(2));
    options.onDisconnect(new Error("closed"));
    expect(drawer.elements.drawer.querySelector(".agent-live-status")?.textContent).toBe("Broker unavailable");
    expect(drawer.elements.timeline.querySelector(".agent-response-loading")).toBeNull();
    drawer.destroy();
  });

  test("renders authoritative loading and clears it for every terminal transition", () => {
    let options;
    const transport = { clientID: agentIDs.message, conversationID: agentIDs.conversation, consented: true, probe: vi.fn(async () => ({ ok: true, code: null })), grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), send: vi.fn(async () => {}), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn() };
    const drawer = createAgentDrawer({ payload: agentPayload(), doc: document, storage: localStorage, transportFactory: (input) => { options = input; return transport; } });
    const loading = () => drawer.elements.timeline.querySelector(".agent-response-loading");

    options.onEvent(agentEvent("snapshot", { lifecycle: "responding", queue: [], context_state: "accepted", active_turn_id: agentIDs.turn }));
    expect(loading()?.textContent).toContain("Pi is responding");
    options.onEvent(agentEvent("error", { error: { code: "provider_startup_failed", message: "Pi could not be started.", action: "try_again" } }, { event_id: "HHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH" }));
    expect(loading()).not.toBeNull();
    options.onEvent(agentEvent("completion", { turn_id: agentIDs.turn }, { event_id: "IIIIIIIIIIIIIIIIIIIIIIIIIIIIIIII" }));
    expect(loading()).toBeNull();

    options.onEvent(agentEvent("snapshot", { lifecycle: "responding", queue: [], context_state: "accepted", active_turn_id: agentIDs.turn }, { event_id: "JJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJ" }));
    expect(loading()).not.toBeNull();
    options.onEvent(agentEvent("interruption", { turn_id: agentIDs.turn, reason: "requested" }, { event_id: "KKKKKKKKKKKKKKKKKKKKKKKKKKKKKKKK" }));
    expect(loading()).toBeNull();

    options.onEvent(agentEvent("snapshot", { lifecycle: "responding", queue: [], context_state: "accepted", active_turn_id: agentIDs.turn }, { event_id: "LLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLL" }));
    expect(loading()).not.toBeNull();
    options.onEvent(agentEvent("snapshot", { lifecycle: "ready", queue: [], context_state: "accepted", active_turn_id: null }, { event_id: "MMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMM" }));
    expect(loading()).toBeNull();

    options.onEvent(agentEvent("snapshot", { lifecycle: "responding", queue: [], context_state: "accepted", active_turn_id: agentIDs.turn }, { event_id: "NNNNNNNNNNNNNNNNNNNNNNNNNNNNNNNN" }));
    expect(loading()).not.toBeNull();
    options.onDisconnect(new Error("closed"));
    expect(loading()).toBeNull();

    options.onEvent(agentEvent("snapshot", { lifecycle: "responding", queue: [], context_state: "accepted", active_turn_id: agentIDs.turn }, { event_id: "OOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOO" }));
    options.onEvent(agentEvent("assistant_delta", { turn_id: agentIDs.turn, message_id: agentIDs.message, text: "partial" }, { event_id: "PPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPP" }));
    expect(loading()).toBeNull();
    options.onEvent(agentEvent("assistant_message", { turn_id: agentIDs.turn, message_id: agentIDs.message, text: "final answer", created_at: "2026-07-27T03:04:05Z" }, { event_id: "QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ" }));
    expect(drawer.elements.timeline.textContent).toContain("final answer");
    expect(drawer.elements.timeline.textContent).not.toContain("partial");
    drawer.destroy();
  });

  test("labels normalized activity with the approved disclosure defaults", () => {
    let options;
    const transport = { clientID: agentIDs.message, conversationID: agentIDs.conversation, consented: true, probe: vi.fn(async () => ({ ok: true, code: null })), grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), send: vi.fn(async () => {}), close: vi.fn(), resetConversation: vi.fn(), resetReplay: vi.fn(), setPort: vi.fn() };
    const drawer = createAgentDrawer({ payload: agentPayload(), doc: document, storage: localStorage, transportFactory: (input) => { options = input; return transport; } });
    options.onEvent(snapshotEvent());
    const events = [
      agentEvent("activity", { kind: "visible_summary", summary: "Checked the page headings." }, { event_id: "HHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH" }),
      agentEvent("activity", { kind: "status", summary: "Working." }, { event_id: "IIIIIIIIIIIIIIIIIIIIIIIIIIIIIIII" }),
      agentEvent("activity", { kind: "retry", summary: "Retrying safely." }, { event_id: "JJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJJ" }),
      agentEvent("activity", { kind: "compaction", summary: "Compacted context." }, { event_id: "KKKKKKKKKKKKKKKKKKKKKKKKKKKKKKKK" }),
      agentEvent("blocked", { kind: "tool", message: "A provider tool request was blocked by content-only policy." }, { event_id: "LLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLL" }),
      agentEvent("blocked", { kind: "permission", message: "A provider permission request was blocked by content-only policy." }, { event_id: "MMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMM" }),
      agentEvent("error", { error: { code: "provider_startup_failed", message: "Pi could not be started.", action: "try_again" } }, { event_id: "NNNNNNNNNNNNNNNNNNNNNNNNNNNNNNNN" }),
    ];
    for (const event of events) options.onEvent(event);
    const activities = [...drawer.elements.timeline.querySelectorAll(".agent-activity")];
    expect(activities.map((activity) => activity.querySelector("summary")?.textContent)).toEqual([
      "Work summary", "Status", "Retrying", "Compaction", "Tool request blocked", "Permission request blocked", "Error",
    ]);
    expect(activities.map((activity) => activity.open)).toEqual([false, false, false, false, true, true, true]);
    expect(drawer.elements.timeline.textContent).not.toContain("thinking_delta");
    expect(drawer.elements.timeline.textContent).not.toContain("tool_name");
    drawer.destroy();
  });

  test("restores focus, handles Escape, and persists only open state and port", async () => {
    const before = document.createElement("button"); document.body.append(before); before.focus();
    const transport = { clientID: agentIDs.message, conversationID: null, consented: false, probe: vi.fn(async () => ({ ok: false, code: "broker_unavailable" })), grantConsent() {}, connect: vi.fn(), reconnect: vi.fn(), send: vi.fn(), close: vi.fn(), resetConversation: vi.fn(), setPort: vi.fn() };
    const drawer = createAgentDrawer({ payload: agentPayload(), doc: document, storage: localStorage, transportFactory: () => transport });
    drawer.setOpen(true);
    expect(document.activeElement).toBe(drawer.elements.close);
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(drawer.open).toBe(false);
    expect(document.activeElement).toBe(before);
    drawer.elements.portInput.value = "9000";
    drawer.elements.portInput.dispatchEvent(new Event("change", { bubbles: true }));
    await vi.waitFor(() => expect(localStorage.getItem(AGENT_PORT_STORAGE_KEY)).toBe("9000"));
    expect(Object.keys(localStorage).sort()).toEqual([AGENT_DRAWER_STORAGE_KEY, AGENT_PORT_STORAGE_KEY].sort());
    drawer.destroy();
  });
});

describe("Mermaid rendering", () => {
  test("emits indexed placeholders while retaining source outside HTML attributes", async () => {
    const container = document.querySelector("#viewer");
    const first = 'flowchart LR\nA["quoted"] --> B';
    const second = "sequenceDiagram\nA->>B: hello";

    const viewer = await renderWhiteboard(
      `\`\`\`mermaid\n${first}\n\`\`\`\n\n\`\`\`mermaid\n${second}\n\`\`\``,
      { container },
    );

    const placeholders = [...container.querySelectorAll(".mermaid-placeholder")];
    expect(placeholders.map((node) => node.dataset.index)).toEqual(["0", "1"]);
    expect(placeholders.every((node) => node.getAttribute("data-source") === null)).toBe(true);
    expect(viewer.diagramSources).toEqual([`${first}\n`, `${second}\n`]);
  });

  test("isolates an invalid diagram to its own error block", async () => {
    const container = document.querySelector("#viewer");
    mermaidMocks.render.mockImplementation(async (id, source) => {
      if (source.includes("invalid")) throw new Error("parse details must not leak");
      return { svg: `<svg xmlns="http://www.w3.org/2000/svg"><text>${id}</text></svg>` };
    });

    await renderWhiteboard(
      "```mermaid\ngraph TD; A-->B\n```\n\n```mermaid\ninvalid\n```\n\n```mermaid\ngraph LR; C-->D\n```",
      { container },
    );

    const placeholders = [...container.querySelectorAll(".mermaid-placeholder")];
    expect(placeholders[0].querySelector("svg")).not.toBeNull();
    expect(placeholders[1].querySelector(".mermaid-error")?.textContent).toBe("Unable to render diagram");
    expect(placeholders[1].textContent).not.toContain("parse details");
    expect(placeholders[2].querySelector("svg")).not.toBeNull();
  });

  test("re-renders every diagram from retained source after a theme change", async () => {
    const container = document.querySelector("#viewer");
    const mediaQuery = makeMediaQuery(false);
    const viewer = await renderWhiteboard(
      "```mermaid\ngraph TD; A-->B\n```\n\n```mermaid\ngraph LR; C-->D\n```",
      { container, mediaQuery },
    );

    expect(mermaidMocks.render.mock.calls.map((call) => call[1])).toEqual([
      "graph TD; A-->B\n",
      "graph LR; C-->D\n",
    ]);

    await viewer.setTheme("dark");

    expect(mermaidMocks.render.mock.calls.map((call) => call[1])).toEqual([
      "graph TD; A-->B\n",
      "graph LR; C-->D\n",
      "graph TD; A-->B\n",
      "graph LR; C-->D\n",
    ]);
    expect(mermaidMocks.initialize).toHaveBeenLastCalledWith(
      expect.objectContaining({
        startOnLoad: false,
        securityLevel: "strict",
        theme: "dark",
        htmlLabels: false,
        secure: expect.arrayContaining([
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
        ]),
      }),
    );
  });

  test("sanitizes rendered SVG before insertion", async () => {
    const container = document.querySelector("#viewer");
    mermaidMocks.render.mockResolvedValue({
      svg: '<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script><a href="javascript:alert(1)"><text>safe</text></a></svg>',
    });

    await renderWhiteboard("```mermaid\ngraph TD; A-->B\n```", { container });

    expect(container.querySelector("svg")).not.toBeNull();
    expect(container.querySelector("svg script")).toBeNull();
    expect(container.querySelector('svg a[href^="javascript:"]')).toBeNull();
  });
});
