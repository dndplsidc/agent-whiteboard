import { expect, test as base } from "@playwright/test";
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { promises as fs } from "node:fs";
import http from "node:http";
import https from "node:https";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const projectRoot = fileURLToPath(new URL("../../", import.meta.url));
const processTimeout = 10_000;
const pollInterval = 20;

function isolatedEnvironment(home) {
  const environment = {};
  for (const [key, value] of Object.entries(process.env)) {
    const normalized = key.toUpperCase();
    if (["HOME", "USERPROFILE", "XDG_CONFIG_HOME"].includes(normalized)) continue;
    if (normalized.startsWith("AGENT_WHITEBOARD_")) continue;
    environment[key] = value;
  }
  return {
    ...environment,
    HOME: home,
    USERPROFILE: home,
    XDG_CONFIG_HOME: path.join(home, ".config"),
  };
}

function runProcess(command, args, { cwd = projectRoot, env = process.env, timeout = 60_000 } = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { cwd, env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    let timedOut = false;
    let killWaitTimer;
    const timer = setTimeout(() => {
      timedOut = true;
      child.kill("SIGKILL");
      killWaitTimer = setTimeout(() => {
        reject(new Error(`timed-out process did not exit after SIGKILL: ${command} ${args.join(" ")}`));
      }, 5_000);
    }, timeout);
    child.stdout.setEncoding("utf8");
    child.stderr.setEncoding("utf8");
    child.stdout.on("data", (chunk) => {
      stdout += chunk;
    });
    child.stderr.on("data", (chunk) => {
      stderr += chunk;
    });
    child.once("error", (error) => {
      clearTimeout(timer);
      clearTimeout(killWaitTimer);
      reject(error);
    });
    child.once("exit", (code, signal) => {
      clearTimeout(timer);
      clearTimeout(killWaitTimer);
      if (timedOut) {
        reject(new Error(`process timed out: ${command} ${args.join(" ")}\nstdout:\n${stdout}\nstderr:\n${stderr}`));
        return;
      }
      if (code === 0) {
        resolve({ stdout, stderr });
        return;
      }
      reject(new Error(`process failed (${code ?? signal}): ${command} ${args.join(" ")}\nstdout:\n${stdout}\nstderr:\n${stderr}`));
    });
  });
}

function startServer(binary, storage, env, globalArgs = []) {
  const child = spawn(
    binary,
    [...globalArgs, "serve", "--host", "127.0.0.1", "--port", "0", "--storage", storage, "--log-mode", "json"],
    { cwd: projectRoot, env, stdio: ["ignore", "pipe", "pipe"] },
  );
  child.stdout.setEncoding("utf8");
  child.stderr.setEncoding("utf8");

  let stdout = "";
  let stderr = "";
  let pending = "";
  let settled = false;
  const listening = new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      reject(new Error(`server listening log timed out\nstdout:\n${stdout}\nstderr:\n${stderr}`));
    }, processTimeout);
    const fail = (error) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(error);
    };
    child.once("error", fail);
    child.once("exit", (code, signal) => {
      fail(new Error(`server exited before listening (${code ?? signal})\nstdout:\n${stdout}\nstderr:\n${stderr}`));
    });
    child.stderr.on("data", (chunk) => {
      stderr += chunk;
      pending += chunk;
      for (;;) {
        const newline = pending.indexOf("\n");
        if (newline < 0) break;
        const line = pending.slice(0, newline).trim();
        pending = pending.slice(newline + 1);
        try {
          const entry = JSON.parse(line);
          if (entry.msg !== "server listening") continue;
          const parsed = new URL(entry.url);
          if (parsed.protocol !== "http:" || !entry.address) throw new Error("invalid listening log");
          if (!settled) {
            settled = true;
            clearTimeout(timer);
            resolve({ address: entry.address, url: parsed.origin });
          }
        } catch (error) {
          if (line.includes('"msg":"server listening"')) fail(error);
        }
      }
    });
  });
  child.stdout.on("data", (chunk) => {
    stdout += chunk;
  });
  return { child, listening, output: () => ({ stdout, stderr }) };
}

async function waitForReady(url, child, output) {
  const deadline = Date.now() + processTimeout;
  let lastError;
  while (Date.now() < deadline) {
    if (child.exitCode !== null || child.signalCode !== null) {
      const captured = output();
      throw new Error(`server exited before readiness\nstdout:\n${captured.stdout}\nstderr:\n${captured.stderr}`);
    }
    try {
      const response = await fetch(`${url}/readyz`, { signal: AbortSignal.timeout(500) });
      await response.body?.cancel();
      if (response.status === 200) return;
    } catch (error) {
      lastError = error;
    }
    await new Promise((resolve) => setTimeout(resolve, pollInterval));
  }
  const captured = output();
  throw new Error(`server readiness timed out: ${lastError}\nstdout:\n${captured.stdout}\nstderr:\n${captured.stderr}`);
}

async function waitForExit(child, timeout) {
  if (child.exitCode !== null || child.signalCode !== null) return true;
  return Promise.race([
    new Promise((resolve) => child.once("exit", () => resolve(true))),
    new Promise((resolve) => setTimeout(() => resolve(false), timeout)),
  ]);
}

async function stopServer(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  child.kill("SIGTERM");
  if (await waitForExit(child, 5_000)) return;
  child.kill("SIGKILL");
  if (!(await waitForExit(child, 5_000))) throw new Error("server process did not exit after SIGKILL");
}

function listen(server, host) {
  return new Promise((resolve, reject) => {
    const fail = (error) => reject(error);
    server.once("error", fail);
    server.listen(0, host, () => {
      server.off("error", fail);
      const address = server.address();
      if (!address || typeof address === "string") {
        reject(new Error("test server did not expose a TCP address"));
        return;
      }
      resolve(address.port);
    });
  });
}

function trackConnections(server) {
  const sockets = new Set();
  server.on("connection", (socket) => {
    sockets.add(socket);
    socket.once("close", () => sockets.delete(socket));
  });
  return sockets;
}

async function closeNodeServer(server, sockets) {
  if (!server.listening) return;
  const closed = new Promise((resolve, reject) => server.close((error) => (error ? reject(error) : resolve())));
  server.closeIdleConnections?.();
  const graceful = await Promise.race([
    closed.then(() => true),
    new Promise((resolve) => setTimeout(() => resolve(false), 2_000)),
  ]);
  if (graceful) return;
  for (const socket of sockets) socket.destroy();
  server.closeAllConnections?.();
  await closed;
}

async function createTestCertificate(root) {
  const config = path.join(root, "https-certificate.cnf");
  const key = path.join(root, "https-key.pem");
  const certificate = path.join(root, "https-certificate.pem");
  await fs.writeFile(
    config,
    [
      "[req]",
      "distinguished_name = subject",
      "x509_extensions = extensions",
      "prompt = no",
      "[subject]",
      "CN = agent-whiteboard-browser-test",
      "[extensions]",
      "subjectAltName = @names",
      "basicConstraints = critical,CA:FALSE",
      "keyUsage = critical,digitalSignature,keyEncipherment",
      "extendedKeyUsage = serverAuth",
      "[names]",
      "IP.1 = ::1",
      "",
    ].join("\n"),
    { mode: 0o600 },
  );
  await runProcess(
    "openssl",
    ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-config", config, "-keyout", key, "-out", certificate],
    { timeout: processTimeout },
  );
  return { key: await fs.readFile(key), cert: await fs.readFile(certificate) };
}

function createHTTPSSource(credentials, upstreamURL) {
  const upstream = new URL(upstreamURL);
  const requests = [];
  const server = https.createServer(credentials, (request, response) => {
    requests.push({ method: request.method, url: request.url, headers: { ...request.headers } });
    if (request.url === "/__local-agent-transport") {
      response.writeHead(200, {
        "Content-Type": "text/html; charset=utf-8",
        "Cache-Control": "no-store",
      });
      response.end("<!doctype html><meta charset=utf-8><title>Local agent transport proof</title>");
      return;
    }
    const proxyRequest = http.request(
      {
        hostname: upstream.hostname,
        port: upstream.port,
        method: request.method,
        path: request.url,
        headers: { ...request.headers, host: upstream.host },
      },
      (proxyResponse) => {
        response.writeHead(proxyResponse.statusCode ?? 502, proxyResponse.headers);
        proxyResponse.pipe(response);
      },
    );
    proxyRequest.once("error", (error) => {
      if (!response.headersSent) response.writeHead(502, { "Content-Type": "text/plain; charset=utf-8" });
      response.end(`HTTPS test proxy failed: ${error.message}`);
    });
    request.pipe(proxyRequest);
  });
  return { server, requests };
}

function webSocketFrame(payload) {
  const body = Buffer.from(payload);
  if (body.length < 126) return Buffer.concat([Buffer.from([0x81, body.length]), body]);
  if (body.length <= 0xffff) {
    const header = Buffer.alloc(4);
    header[0] = 0x81;
    header[1] = 126;
    header.writeUInt16BE(body.length, 2);
    return Buffer.concat([header, body]);
  }
  const header = Buffer.alloc(10);
  header[0] = 0x81;
  header[1] = 127;
  header.writeBigUInt64BE(BigInt(body.length), 2);
  return Buffer.concat([header, body]);
}

function consumeClientWebSocketFrames(buffer, onPayload) {
  let offset = 0;
  while (buffer.length - offset >= 2) {
    const first = buffer[offset];
    const second = buffer[offset + 1];
    let length = second & 0x7f;
    let headerLength = 2;
    if (length === 126) {
      if (buffer.length - offset < 4) break;
      length = buffer.readUInt16BE(offset + 2);
      headerLength = 4;
    } else if (length === 127) {
      if (buffer.length - offset < 10) break;
      const wide = buffer.readBigUInt64BE(offset + 2);
      if (wide > BigInt(Number.MAX_SAFE_INTEGER)) throw new Error("oversized WebSocket test frame");
      length = Number(wide);
      headerLength = 10;
    }
    if ((second & 0x80) === 0 || buffer.length - offset < headerLength + 4 + length) break;
    const maskOffset = offset + headerLength;
    const bodyOffset = maskOffset + 4;
    const payload = Buffer.alloc(length);
    for (let index = 0; index < length; index += 1) payload[index] = buffer[bodyOffset + index] ^ buffer[maskOffset + (index % 4)];
    if (first !== 0x81) throw new Error("unexpected WebSocket test frame");
    onPayload(payload.toString("utf8"));
    offset = bodyOffset + length;
  }
  return buffer.subarray(offset);
}

function requestRecord(request) {
  return {
    method: request.method,
    url: request.url,
    headers: { ...request.headers },
    responseHeaders: {},
    status: undefined,
  };
}

function createLoopbackBroker(initialAllowedOrigin) {
  const requests = [];
  let allowedOrigin = initialAllowedOrigin;
  let forceWebSocketFailure = false;

  const send = (response, record, status, headers = {}, body = "") => {
    record.status = status;
    record.responseHeaders = { ...headers };
    response.writeHead(status, headers);
    response.end(body);
  };
  const corsHeaders = () => ({
    "Access-Control-Allow-Origin": allowedOrigin,
    Vary: "Origin",
  });
  const originAllowed = (request) => request.headers.origin === allowedOrigin;

  const server = http.createServer((request, response) => {
    const record = requestRecord(request);
    requests.push(record);
    if (!originAllowed(request)) {
      send(response, record, 403, { "Content-Type": "application/json" }, '{"error":"origin denied"}');
      return;
    }
    if (request.method === "OPTIONS") {
      const requestedMethod = request.headers["access-control-request-method"];
      const requestedHeaders = request.headers["access-control-request-headers"];
      if (
        !["GET", "POST"].includes(requestedMethod) ||
        (requestedHeaders && !requestedHeaders.toLowerCase().includes("x-agent-whiteboard-api-version"))
      ) {
        send(response, record, 400, corsHeaders());
        return;
      }
      const headers = {
        ...corsHeaders(),
        "Access-Control-Allow-Methods": requestedMethod,
        "Access-Control-Allow-Headers": "content-type, x-agent-whiteboard-api-version",
      };
      if (request.headers["access-control-request-private-network"] === "true") {
        headers["Access-Control-Allow-Private-Network"] = "true";
      }
      send(response, record, 204, headers);
      return;
    }
    if (request.method === "GET" && request.url === "/api/v1/agent/status") {
      send(
        response,
        record,
        200,
        { ...corsHeaders(), "Content-Type": "application/json", "Cache-Control": "no-store" },
        '{"available":true,"api_version":"4"}',
      );
      return;
    }
    if (request.method === "POST" && request.url === "/api/v1/agent/connect") {
      if (request.headers["x-agent-whiteboard-api-version"] !== "4") {
        send(response, record, 400, corsHeaders(), '{"error":"unsupported API version"}');
        return;
      }
      record.status = 200;
      record.responseHeaders = {
        ...corsHeaders(),
        "Content-Type": "application/x-ndjson",
        "Cache-Control": "no-store",
      };
      response.writeHead(200, record.responseHeaders);
      response.write('{"type":"ready","transport":"http"}\n');
      setTimeout(() => response.end('{"type":"delta","text":"fallback stream"}\n'), 25);
      return;
    }
    send(response, record, 404, corsHeaders());
  });

  server.on("upgrade", (request, socket) => {
    const record = requestRecord(request);
    requests.push(record);
    if (!originAllowed(request) || request.url !== "/api/v1/agent/connect" || request.headers["sec-websocket-protocol"] !== "agent-whiteboard.v4") {
      record.status = 403;
      socket.end("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
      return;
    }
    if (forceWebSocketFailure) {
      record.status = 503;
      socket.end("HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
      return;
    }
    const key = request.headers["sec-websocket-key"];
    if (typeof key !== "string") {
      record.status = 400;
      socket.destroy();
      return;
    }
    const accept = createHash("sha1").update(`${key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest("base64");
    record.status = 101;
    record.responseHeaders = { Upgrade: "websocket", Connection: "Upgrade", "Sec-WebSocket-Accept": accept, "Sec-WebSocket-Protocol": "agent-whiteboard.v4" };
    socket.write(
      [
        "HTTP/1.1 101 Switching Protocols",
        "Upgrade: websocket",
        "Connection: Upgrade",
        `Sec-WebSocket-Accept: ${accept}`,
        "Sec-WebSocket-Protocol: agent-whiteboard.v4",
        "",
        "",
      ].join("\r\n"),
    );
    socket.write(webSocketFrame('{"type":"ready","transport":"websocket"}'));
    setTimeout(() => socket.end(webSocketFrame('{"type":"delta","text":"websocket stream"}')), 25);
  });

  return {
    server,
    requests,
    reset: () => {
      requests.splice(0);
      forceWebSocketFailure = false;
    },
    setWebSocketFailure: (value) => {
      forceWebSocketFailure = value;
    },
    setAllowedOrigin: (origin) => {
      allowedOrigin = origin;
    },
  };
}

function protocolID(value) {
  return Buffer.alloc(24, value % 256).toString("base64url");
}

function createSidebarBroker(initialAllowedOrigin) {
  const requests = [];
  const streams = new Set();
  const webSockets = new Set();
  const webSocketCommands = [];
  const interactionResults = [];
  let allowedOrigin = initialAllowedOrigin;
  let webSocketEnabled = false;
  const fixtureImages = new Map();
  let imageSequence = 300;
  let nextImageFailure = null;
  const codexCatalog = [{
    model: "gpt-5.6-sol",
    model_display_name: "5.6 Sol",
    description: "Strong general-purpose coding model.",
    default_effort: "high",
    supported_reasoning_efforts: [
      { effort: "medium", description: "Balanced reasoning." },
      { effort: "high", description: "Deeper reasoning." },
      { effort: "xhigh", description: "Maximum reasoning." },
    ],
    supports_images: true,
    default: true,
    supports_fast: true,
  }, {
    model: "gpt-5.6-luna",
    model_display_name: "5.6 Luna",
    description: "Fast focused coding model.",
    default_effort: "medium",
    supported_reasoning_efforts: [{ effort: "medium", description: "Balanced reasoning." }],
    supports_images: false,
    default: false,
    supports_fast: false,
  }];
  const presentedSettings = (settings) => {
    const model = codexCatalog.find(({ model: value }) => value === settings.model);
    if (!model) throw new Error(`unknown fixture model: ${settings.model}`);
    return { ...settings, model_display_name: model.model_display_name, selectable: true };
  };
  const defaultCodexSettings = presentedSettings({ model: "gpt-5.6-sol", effort: "high", speed: "fast" });
  const providerDefinitions = {
    pi: { conversationID: protocolID(201), model: "fixture-model", sequence: 1, identitySequence: 40, archiveID: protocolID(220) },
    codex: { conversationID: protocolID(202), model: defaultCodexSettings.model_display_name, sequence: 101, identitySequence: 80, archiveID: protocolID(221) },
  };
  const createProviderState = (provider) => {
    const definition = providerDefinitions[provider];
    return {
      provider,
      ...definition,
      available: true,
      supportsImages: provider === "codex" ? codexCatalog[0].supports_images : true,
      settingsState: provider === "codex" ? "verified" : null,
      effectiveSettings: provider === "codex" ? { ...defaultCodexSettings } : null,
      catalog: provider === "codex" ? structuredClone(codexCatalog) : [],
      rejectNextSettings: false,
      rejectionCatalog: null,
      applyConnectSettings: false,
      acceptedSettings: [],
      createdSettings: [],
      contextState: "pending",
      contextDigest: "0".repeat(64),
      holdResponses: false,
      phaseResponses: false,
      responseText: null,
      activeTurn: null,
      activeCompact: null,
      holdInterruptCompletion: false,
      pendingInterruptCompletion: null,
      skillsState: provider === "codex" ? "ready" : null,
      skills: provider === "codex" ? [
        { id: protocolID(230), name: "review-helper", display_name: "Review helper", description: "Review the current work", scope: "repo" },
        { id: protocolID(231), name: "personal-helper-with-an-intentionally-long-name-for-narrow-layout", display_name: "Personal helper with an intentionally long name for narrow layout", description: "Use a personally installed skill with a long description that must remain contained in the compact row", scope: "user" },
      ] : [],
      supportsCompact: provider === "codex",
      pendingResponse: null,
      queue: [],
      history: [],
      interactions: new Map(),
      archiveMode: "populated",
      archiveDelay: null,
      pendingArchiveResponse: null,
      holdInteractionResolution: false,
      eventLog: [],
      eventPositions: new Map(),
    };
  };
  const providers = new Map(Object.keys(providerDefinitions).map((provider) => [provider, createProviderState(provider)]));

  const providerState = (provider) => {
    const state = providers.get(provider);
    if (!state) throw new Error(`unsupported sidebar provider: ${provider}`);
    return state;
  };
  const stateForCommand = (command, connectedProvider) => {
    if (command.type === "connect") return providerState(command.payload.provider);
    if (connectedProvider) return providerState(connectedProvider);
    const state = [...providers.values()].find((candidate) => candidate.conversationID === command.conversation_id);
    if (!state) throw new Error("unknown sidebar conversation");
    return state;
  };
  const claimFixtureImage = (state, command, reference) => {
    const image = fixtureImages.get(reference.image_id);
    if (!image || image.state !== "staged" || image.clientID !== command.client_id || image.conversationID !== state.conversationID || image.provider !== state.provider) throw new Error("missing staged fixture image");
    image.state = "claimed";
    image.messageID = command.payload.message_id;
    image.name = reference.name;
    return { image_id: image.id, name: image.name, media_type: image.mediaType };
  };
  const nextEvent = (state, type, payload) => ({
    api_version: "4",
    event_id: protocolID(state.sequence++),
    conversation_id: state.conversationID,
    type,
    timestamp: "2026-07-27T03:04:05Z",
    payload,
  });
  const snapshotPayload = (state) => ({
    lifecycle: state.activeCompact !== null ? "compacting" : state.activeTurn === null ? "ready" : "responding",
    queue: state.queue.map((item) => structuredClone(item)),
    context_state: state.contextState,
    active_work: state.activeCompact !== null ? { work_id: state.activeCompact, kind: "compact", state: "running" } : state.activeTurn === null ? null : { work_id: state.activeTurn, kind: "turn", state: "running" },
    supports_images: state.supportsImages,
    settings_state: state.settingsState,
    effective_settings: state.effectiveSettings === null ? null : { ...state.effectiveSettings },
    catalog: structuredClone(state.catalog),
    skills_state: state.skillsState,
    skills: structuredClone(state.skills),
    supports_compact: state.supportsCompact,
  });
  const archivePayload = (state, { id = state.archiveID, createdAt = "2026-07-26T01:02:03Z", updatedAt = "2026-07-26T02:03:04Z", preview = "" } = {}) => ({
    archive_id: id,
    created_at: createdAt,
    updated_at: updatedAt,
    provider: state.provider,
    model: state.model,
    preview,
  });
  const corsHeaders = () => ({ "Access-Control-Allow-Origin": allowedOrigin, Vary: "Origin" });
  const sendJSON = (response, record, status, value) => {
    const body = `${JSON.stringify(value)}\n`;
    record.status = status;
    record.responseHeaders = { ...corsHeaders(), "Content-Type": "application/json", "Cache-Control": "no-store" };
    response.writeHead(status, record.responseHeaders);
    response.end(body);
  };
  const broadcast = (state, event, clientID = null) => {
    const encoded = JSON.stringify(event);
    for (const stream of streams) {
      if (stream.provider === state.provider && (clientID === null || stream.clientID === clientID)) stream.response.write(`${encoded}\n`);
    }
    for (const connection of webSockets) {
      if (connection.provider === state.provider && (clientID === null || connection.clientID === clientID)) connection.socket.write(webSocketFrame(encoded));
    }
  };
  const emit = (state, type, payload) => {
    const event = nextEvent(state, type, payload);
    state.eventLog.push(event);
    state.eventPositions.set(event.event_id, state.eventLog.length);
    broadcast(state, event);
    return event;
  };
  const targetedEvent = (state, type, payload, clientID) => {
    const event = nextEvent(state, type, payload);
    state.eventPositions.set(event.event_id, state.eventLog.length);
    broadcast(state, event, clientID);
    return event;
  };
  const bootstrapEvents = (state, replayAfter) => {
    if (replayAfter) {
      const position = state.eventPositions.get(replayAfter);
      if (position === undefined) return null;
      return state.eventLog.slice(position);
    }
    const events = [
      nextEvent(state, "snapshot", snapshotPayload(state)),
      nextEvent(state, "provider", { provider: state.provider, state: "ready", model: state.model, supports_images: state.supportsImages }),
    ];
    for (const event of events) state.eventPositions.set(event.event_id, state.eventLog.length);
    return events;
  };
  const commandResult = (state, command, error) => targetedEvent(state, "command_result", error
    ? { command_id: command.command_id, status: "rejected", error }
    : { command_id: command.command_id, status: "succeeded" }, command.client_id);
  const emitResponsePhase = (phase, provider = "pi") => {
    const state = providerState(provider);
    if (!state.pendingResponse) throw new Error(`no pending ${provider} sidebar response for ${phase}`);
    const response = state.pendingResponse;
    const firstDelta = provider === "codex" ? "Codex fixture " : "Fixture ";
    const finalText = state.responseText ?? (provider === "codex" ? "Codex fixture reply" : "Fixture reply");
    if (phase === "first_delta" && response.phase === "responding") {
      emit(state, "assistant_delta", { turn_id: response.turnID, message_id: response.assistantID, text: firstDelta });
      response.phase = "first_delta";
      return;
    }
    if (phase === "later_delta" && response.phase === "first_delta") {
      emit(state, "assistant_delta", { turn_id: response.turnID, message_id: response.assistantID, text: "reply" });
      response.phase = "later_delta";
      return;
    }
    if (phase === "completion" && response.phase === "later_delta") {
      const assistant = { item_id: response.assistantID, kind: "assistant", turn_id: response.turnID, message_id: response.assistantID, text: finalText, created_at: response.createdAt };
      state.history.push(assistant);
      emit(state, "assistant_message", { turn_id: assistant.turn_id, message_id: assistant.message_id, text: assistant.text, created_at: assistant.created_at });
      emit(state, "completion", { turn_id: response.turnID });
      state.activeTurn = null;
      state.pendingResponse = null;
      if (state.queue.length > 0) {
        const next = state.queue.shift();
        emit(state, "queue", { items: state.queue.map((item) => structuredClone(item)) });
        if (state.provider === "codex") acceptCodexSettings(state, {
          model: next.settings.model,
          effort: next.settings.effort,
          speed: next.settings.speed,
        }, next.turn_id);
        state.activeTurn = next.turn_id;
        emit(state, "user_message", {
          turn_id: next.turn_id,
          message_id: next.message_id,
          content: next.content,
          ...(next.images?.length ? { images: next.images } : {}),
          created_at: response.createdAt,
        });
        emit(state, "lifecycle", { state: "responding", active_work: { work_id: next.turn_id, kind: "turn", state: "running" } });
        state.pendingResponse = { turnID: next.turn_id, assistantID: protocolID(state.identitySequence++), createdAt: response.createdAt, phase: "responding" };
      }
      return;
    }
    throw new Error(`sidebar response cannot release ${phase} from ${response.phase}`);
  };
  const acceptCodexSettings = (state, settings, turnID) => {
    state.effectiveSettings = presentedSettings(settings);
    state.model = state.effectiveSettings.model_display_name;
    state.supportsImages = state.catalog.find(({ model }) => model === settings.model).supports_images;
    state.acceptedSettings.push({ turn_id: turnID, settings: structuredClone(settings) });
    emit(state, "settings", {
      settings_state: "verified",
      effective_settings: { ...state.effectiveSettings },
      catalog: structuredClone(state.catalog),
      accepted_turn_id: turnID,
    });
    emit(state, "provider", { provider: state.provider, state: "ready", model: state.model, supports_images: state.supportsImages });
  };
  const handleCommand = (command, connectedProvider) => {
    const state = stateForCommand(command, connectedProvider);
    if (command.type === "history_page") {
      targetedEvent(state, "timeline", { command_id: command.command_id, items: [...state.history].reverse(), next_cursor: null }, command.client_id);
    } else if (command.type === "submit") {
      if (state.provider === "codex" && state.rejectNextSettings) {
        state.rejectNextSettings = false;
        state.catalog = state.rejectionCatalog ?? state.catalog;
        state.rejectionCatalog = null;
        emit(state, "settings", {
          settings_state: "verified",
          effective_settings: { ...state.effectiveSettings },
          catalog: structuredClone(state.catalog),
          accepted_turn_id: null,
        });
        return commandResult(state, command, { code: "invalid_model_configuration", message: "The selected model settings are no longer available.", action: "configure_model" });
      }
      const inlineReferences = command.payload.content.parts.filter((part) => part.type === "reference" && part.reference.kind === "image");
      if ((command.payload.images?.length ?? 0) + inlineReferences.length > 0 && !state.supportsImages) {
        return commandResult(state, command, { code: "image_input_unsupported", message: "The selected model does not support image input.", action: "configure_model" });
      }
      const content = structuredClone(command.payload.content);
      for (const part of content.parts) {
        if (part.type !== "reference" || part.reference.kind !== "image") continue;
        const descriptor = claimFixtureImage(state, command, part.reference.visual);
        part.reference.visual.media_type = descriptor.media_type;
      }
      const imageDescriptors = (command.payload.images ?? []).map((image) => claimFixtureImage(state, command, image));
      if (state.activeTurn !== null) {
        state.queue.push({
          turn_id: command.payload.turn_id,
          message_id: command.payload.message_id,
          content,
          settings: state.provider === "codex" ? presentedSettings(command.payload.settings) : null,
          ...(imageDescriptors.length ? { images: imageDescriptors } : {}),
        });
        emit(state, "queue", { items: state.queue.map((item) => structuredClone(item)) });
      } else {
        if (state.provider === "codex") acceptCodexSettings(state, command.payload.settings, command.payload.turn_id);
        if (command.payload.context) {
          state.contextState = "accepted";
          state.contextDigest = command.payload.context.digest;
          emit(state, "context", { digest: state.contextDigest, state: "accepted" });
        }
        const createdAt = "2026-07-27T03:04:05Z";
        const user = { item_id: command.payload.message_id, kind: "user", turn_id: command.payload.turn_id, message_id: command.payload.message_id, content, ...(imageDescriptors.length ? { images: imageDescriptors } : {}), created_at: createdAt };
        state.history.push(user);
        state.activeTurn = command.payload.turn_id;
        emit(state, "user_message", { turn_id: user.turn_id, message_id: user.message_id, content: user.content, ...(imageDescriptors.length ? { images: imageDescriptors } : {}), created_at: createdAt });
        emit(state, "lifecycle", { state: "responding", active_work: { work_id: command.payload.turn_id, kind: "turn", state: "running" } });
        const assistantID = protocolID(150 + state.history.length + (state.provider === "codex" ? 25 : 0));
        if (state.phaseResponses) {
          state.pendingResponse = { turnID: command.payload.turn_id, assistantID, createdAt, phase: "responding" };
        } else if (!state.holdResponses) {
          state.pendingResponse = { turnID: command.payload.turn_id, assistantID, createdAt, phase: "responding" };
          emitResponsePhase("first_delta", state.provider);
          emitResponsePhase("later_delta", state.provider);
          emitResponsePhase("completion", state.provider);
        }
      }
    } else if (command.type === "new") {
      if (state.provider === "codex" && state.rejectNextSettings) {
        state.rejectNextSettings = false;
        return commandResult(state, command, { code: "invalid_model_configuration", message: "The selected model settings are no longer available.", action: "configure_model" });
      }
      state.createdSettings.push(command.payload.settings === null ? null : structuredClone(command.payload.settings));
      const result = commandResult(state, command);
      state.conversationID = protocolID(state.identitySequence++);
      state.contextState = "pending";
      state.activeTurn = null;
      state.pendingResponse = null;
      state.queue = [];
      state.history = [];
      if (state.provider === "codex") {
        state.effectiveSettings = presentedSettings(command.payload.settings);
        state.model = state.effectiveSettings.model_display_name;
        state.supportsImages = state.catalog.find(({ model }) => model === command.payload.settings.model).supports_images;
      }
      setImmediate(() => {
        for (const stream of [...streams]) {
          if (stream.provider === state.provider) stream.response.end();
        }
        for (const connection of [...webSockets]) {
          if (connection.provider === state.provider) connection.socket.destroy();
        }
      });
      return result;
    } else if (command.type === "queue_edit") {
      const item = state.queue.find((candidate) => candidate.message_id === command.payload.message_id);
      if (item) item.content = structuredClone(command.payload.content);
      emit(state, "queue", { items: state.queue.map((candidate) => structuredClone(candidate)) });
    } else if (command.type === "queue_remove") {
      const index = state.queue.findIndex((candidate) => candidate.message_id === command.payload.message_id);
      if (index >= 0) {
        for (const image of state.queue[index].images ?? []) fixtureImages.delete(image.image_id);
        state.queue.splice(index, 1);
      }
      emit(state, "queue", { items: state.queue.map((candidate) => ({ ...candidate })) });
    } else if (command.type === "compact" && state.provider === "codex" && state.activeTurn === null && state.activeCompact === null) {
      state.activeCompact = command.payload.work_id;
      emit(state, "lifecycle", { state: "compacting", active_work: { work_id: state.activeCompact, kind: "compact", state: "running" } });
      emit(state, "compaction", { work_id: state.activeCompact, status: "running" });
    } else if (command.type === "interrupt" && state.activeTurn === command.payload.work_id) {
      emit(state, "lifecycle", { state: "responding", active_work: { work_id: state.activeTurn, kind: "turn", state: "stopping" } });
      const complete = () => {
        emit(state, "interruption", { turn_id: state.activeTurn, reason: "requested" });
        state.activeTurn = null;
        state.pendingResponse = null;
      };
      if (state.holdInterruptCompletion) state.pendingInterruptCompletion = complete;
      else complete();
    } else if (command.type === "interrupt" && state.activeCompact === command.payload.work_id) {
      emit(state, "lifecycle", { state: "compacting", active_work: { work_id: state.activeCompact, kind: "compact", state: "stopping" } });
      emit(state, "compaction", { work_id: state.activeCompact, status: "stopping" });
      const complete = () => {
        emit(state, "compaction", { work_id: state.activeCompact, status: "interrupted" });
        state.activeCompact = null;
        emit(state, "lifecycle", { state: "interrupted", active_work: null });
      };
      if (state.holdInterruptCompletion) state.pendingInterruptCompletion = complete;
      else complete();
    } else if (command.type === "archive_list") {
      const firstPage = !command.payload.before;
      const paginated = state.archiveMode === "paginated";
      const secondID = protocolID(state.provider === "codex" ? 223 : 222);
      const items = state.archiveMode === "empty"
        ? []
        : firstPage
          ? [archivePayload(state, { preview: "Earlier conversation" })]
          : [archivePayload(state, { id: secondID, createdAt: "2026-07-25T01:02:03Z", updatedAt: "2026-07-25T02:03:04Z", preview: "Older conversation" })];
      const respond = () => targetedEvent(state, "history", { command_id: command.command_id, items, next_cursor: paginated && firstPage ? state.archiveID : null }, command.client_id);
      if (state.archiveDelay) state.pendingArchiveResponse = respond;
      else respond();
    } else if (command.type === "archive_restore" || command.type === "archive_delete") {
      emit(state, "archive", { action: command.type === "archive_restore" ? "restored" : "deleted", archive_id: command.payload.archive_id });
    } else if (command.type === "interaction_respond") {
      const interaction = state.interactions.get(command.payload.request_id);
      if (!interaction || interaction.resolved || interaction.kind !== command.payload.kind) {
        interactionResults.push({ provider: state.provider, requestID: command.payload.request_id, optionID: command.payload.option_id, status: "rejected" });
        return commandResult(state, command, { code: "invalid_state", message: "The command is not valid for the current conversation state.", action: "refresh_state" });
      }
      interaction.resolved = true;
      interaction.response = structuredClone(command.payload);
      interactionResults.push({ provider: state.provider, requestID: interaction.requestID, optionID: command.payload.option_id, status: "accepted", answers: structuredClone(command.payload.answers) });
      if (!state.holdInteractionResolution) emit(state, "interaction_resolved", { request_id: interaction.requestID, kind: interaction.kind, option_id: command.payload.option_id });
    }
    return commandResult(state, command);
  };

  const server = http.createServer((request, response) => {
    const record = requestRecord(request);
    requests.push(record);
    if (request.headers.origin !== allowedOrigin) {
      sendJSON(response, record, 403, { error: { code: "untrusted_origin", message: "This whiteboard origin is not trusted by the local agent broker.", action: "trust_origin" } });
      return;
    }
    if (request.method === "OPTIONS") {
      record.status = 204;
      record.responseHeaders = {
        ...corsHeaders(),
        "Access-Control-Allow-Methods": request.headers["access-control-request-method"] ?? "POST",
        "Access-Control-Allow-Headers": "content-type, x-agent-whiteboard-api-version, x-agent-whiteboard-client-id, x-agent-whiteboard-conversation-id, x-agent-whiteboard-provider, x-agent-whiteboard-image-purpose",
      };
      response.writeHead(204, record.responseHeaders);
      response.end();
      return;
    }
    if (request.method === "GET" && request.url === "/api/v1/agent/status") {
      sendJSON(response, record, 200, { available: true, api_version: "4", origin_trusted: true });
      return;
    }
    const chunks = [];
    request.on("data", (chunk) => chunks.push(chunk));
    request.on("end", () => {
      const content = Buffer.concat(chunks);
      if (request.url === "/api/v1/agent/images" && request.method === "POST") {
        record.body = `<${content.length} image bytes>`;
        if (nextImageFailure) {
          const failure = nextImageFailure;
          nextImageFailure = null;
          sendJSON(response, record, failure.status, { error: failure.error });
          return;
        }
        const state = providerState(request.headers["x-agent-whiteboard-provider"]);
        const imageID = protocolID(imageSequence++);
        const mediaType = request.headers["content-type"];
        fixtureImages.set(imageID, {
          id: imageID, content, mediaType, provider: state.provider, conversationID: request.headers["x-agent-whiteboard-conversation-id"],
          clientID: request.headers["x-agent-whiteboard-client-id"], state: "staged", name: "", messageID: "",
        });
        sendJSON(response, record, 201, { image_id: imageID, media_type: mediaType, bytes: content.length });
        return;
      }
      const imageMatch = /^\/api\/v1\/agent\/images\/([A-Za-z0-9_-]{32})$/u.exec(request.url);
      if (imageMatch && (request.method === "GET" || request.method === "DELETE")) {
        const image = fixtureImages.get(imageMatch[1]);
        const authorized = image && image.conversationID === request.headers["x-agent-whiteboard-conversation-id"] && (image.state === "claimed" || image.clientID === request.headers["x-agent-whiteboard-client-id"]);
        if (!authorized) {
          sendJSON(response, record, 404, { error: { code: "image_missing", message: "The selected image is no longer available.", action: "none" } });
          return;
        }
        if (request.method === "DELETE") {
          if (image.state === "staged") fixtureImages.delete(image.id);
          record.status = 204;
          record.responseHeaders = corsHeaders();
          response.writeHead(204, record.responseHeaders);
          response.end();
          return;
        }
        record.status = 200;
        record.responseHeaders = { ...corsHeaders(), "Content-Type": image.mediaType, "Content-Length": String(image.content.length), "Cache-Control": "no-store" };
        response.writeHead(200, record.responseHeaders);
        response.end(image.content);
        return;
      }
      record.body = content.toString("utf8");
      let command;
      try { command = JSON.parse(record.body); }
      catch {
        sendJSON(response, record, 400, { error: { code: "invalid_command", message: "The broker rejected an invalid command.", action: "none" } });
        return;
      }
      if (request.method === "POST" && request.url === "/api/v1/agent/connect") {
        const state = stateForCommand(command);
        if (!state.available) {
          sendJSON(response, record, 503, { error: { code: "provider_missing", message: "The selected provider executable is not available.", action: "install_provider" } });
          return;
        }
        if (state.provider === "codex" && state.applyConnectSettings && command.payload.settings !== null) {
          state.effectiveSettings = presentedSettings(command.payload.settings);
          state.model = state.effectiveSettings.model_display_name;
          state.supportsImages = state.catalog.find(({ model }) => model === command.payload.settings.model).supports_images;
          state.applyConnectSettings = false;
        }
        const events = bootstrapEvents(state, command.payload.replay_after);
        if (events === null) {
          sendJSON(response, record, 409, { error: { code: "replay_window_unavailable", message: "The requested replay window is no longer available.", action: "reload_conversation" } });
          return;
        }
        record.status = 200;
        record.responseHeaders = { ...corsHeaders(), "Content-Type": "application/x-ndjson", "Cache-Control": "no-store" };
        response.writeHead(200, record.responseHeaders);
        const stream = { provider: state.provider, clientID: command.client_id, response };
        streams.add(stream);
        response.once("close", () => streams.delete(stream));
        for (const event of events) response.write(`${JSON.stringify(event)}\n`);
        return;
      }
      if (request.method !== "POST" || request.url !== "/api/v1/agent/commands") {
        sendJSON(response, record, 404, { error: { code: "invalid_command", message: "The broker rejected an invalid command.", action: "none" } });
        return;
      }

      sendJSON(response, record, 200, handleCommand(command));
    });
  });
  server.on("upgrade", (request, socket) => {
    const record = requestRecord(request);
    requests.push(record);
    if (!webSocketEnabled || request.headers.origin !== allowedOrigin || request.headers["sec-websocket-protocol"] !== "agent-whiteboard.v4") {
      record.status = 503;
      socket.end("HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
      return;
    }
    const key = request.headers["sec-websocket-key"];
    const accept = createHash("sha1").update(`${key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest("base64");
    record.status = 101;
    record.responseHeaders = { Upgrade: "websocket", Connection: "Upgrade", "Sec-WebSocket-Accept": accept, "Sec-WebSocket-Protocol": "agent-whiteboard.v4" };
    socket.write([
      "HTTP/1.1 101 Switching Protocols",
      "Upgrade: websocket",
      "Connection: Upgrade",
      `Sec-WebSocket-Accept: ${accept}`,
      "Sec-WebSocket-Protocol: agent-whiteboard.v4",
      "",
      "",
    ].join("\r\n"));
    const connection = { provider: null, clientID: null, socket };
    webSockets.add(connection);
    socket.once("close", () => webSockets.delete(connection));
    let buffered = Buffer.alloc(0);
    let connected = false;
    socket.on("data", (chunk) => {
      buffered = Buffer.concat([buffered, chunk]);
      try {
        buffered = consumeClientWebSocketFrames(buffered, (payload) => {
          const command = JSON.parse(payload);
          webSocketCommands.push(command);
          if (!connected) {
            const state = stateForCommand(command);
            if (!state.available) { socket.destroy(); return; }
            const events = bootstrapEvents(state, command.payload.replay_after);
            if (events === null) { socket.destroy(); return; }
            connected = true;
            connection.provider = state.provider;
            connection.clientID = command.client_id;
            for (const event of events) socket.write(webSocketFrame(JSON.stringify(event)));
            return;
          }
          handleCommand(command, connection.provider);
        });
      } catch {
        socket.destroy();
      }
    });
  });

  return {
    server,
    requests,
    webSocketCommands,
    setAllowedOrigin(origin) { allowedOrigin = origin; },
    setWebSocketEnabled(value) { webSocketEnabled = value; },
    setHoldResponses(value, provider = "pi") { providerState(provider).holdResponses = value; },
    setHoldInterruptCompletion(value, provider = "pi") { providerState(provider).holdInterruptCompletion = value; },
    releaseInterruptCompletion(provider = "pi") {
      const state = providerState(provider);
      if (!state.pendingInterruptCompletion) throw new Error(`no pending ${provider} interrupt completion`);
      const completion = state.pendingInterruptCompletion;
      state.pendingInterruptCompletion = null;
      completion();
    },
    setPhaseResponses(value, provider = "pi") { providerState(provider).phaseResponses = value; },
    preparePendingResponse(provider = "pi") {
      const state = providerState(provider);
      if (state.activeTurn === null || state.pendingResponse !== null) throw new Error(`cannot prepare ${provider} sidebar response`);
      state.pendingResponse = { turnID: state.activeTurn, assistantID: protocolID(state.identitySequence++), createdAt: "2026-07-27T03:04:05Z", phase: "responding" };
    },
    setResponseText(value, provider = "pi") { providerState(provider).responseText = value; },
    releaseResponsePhase: emitResponsePhase,
    setProviderAvailable(provider, value) { providerState(provider).available = value; },
    setSupportsImages(provider, value) { providerState(provider).supportsImages = value; },
    setArchiveMode(value, provider = "pi") { providerState(provider).archiveMode = value; },
    setArchiveDelay(value, provider = "pi") { providerState(provider).archiveDelay = value; },
    releaseArchiveList(provider = "pi") {
      const state = providerState(provider);
      if (!state.pendingArchiveResponse) throw new Error(`no delayed ${provider} archive response`);
      const pending = state.pendingArchiveResponse;
      state.pendingArchiveResponse = null;
      pending();
    },
    refreshSkills(skills, provider = "codex") {
      const state = providerState(provider);
      state.skills = structuredClone(skills);
      state.skillsState = "ready";
      emit(state, "skill_catalog", { state: state.skillsState, skills: structuredClone(state.skills) });
    },
    completeCompact(status = "completed", provider = "codex") {
      const state = providerState(provider);
      if (state.activeCompact === null) throw new Error(`no active ${provider} compact work`);
      emit(state, "compaction", { work_id: state.activeCompact, status });
      state.activeCompact = null;
      emit(state, "lifecycle", { state: status === "interrupted" ? "interrupted" : "ready", active_work: null });
    },
    rejectNextSettings(provider = "codex") {
      const state = providerState(provider);
      state.rejectNextSettings = true;
      state.rejectionCatalog = structuredClone(state.catalog);
      state.rejectionCatalog.push({
        model: "gpt-5.6-nova", model_display_name: "5.6 Nova", description: "Newly refreshed model.", default_effort: "medium",
        supported_reasoning_efforts: [{ effort: "medium", description: "Balanced reasoning." }], supports_images: true, default: false, supports_fast: false,
      });
    },
    setNewMapping(provider = "codex") { providerState(provider).applyConnectSettings = true; },
    refreshCatalog(catalog, provider = "codex") {
      const state = providerState(provider);
      state.catalog = structuredClone(catalog);
      emit(state, "settings", {
        settings_state: state.settingsState,
        effective_settings: state.effectiveSettings === null ? null : { ...state.effectiveSettings },
        catalog: structuredClone(state.catalog),
        accepted_turn_id: null,
      });
    },
    restoreSettings(settings, provider = "codex") {
      const state = providerState(provider);
      state.effectiveSettings = presentedSettings(settings);
      state.model = state.effectiveSettings.model_display_name;
      state.supportsImages = state.catalog.find(({ model }) => model === settings.model).supports_images;
      emit(state, "archive", { action: "restored", archive_id: state.archiveID });
      emit(state, "settings", {
        settings_state: "verified",
        effective_settings: { ...state.effectiveSettings },
        catalog: structuredClone(state.catalog),
        accepted_turn_id: null,
      });
      emit(state, "provider", { provider: state.provider, state: "ready", model: state.model, supports_images: state.supportsImages });
    },
    acceptedSettings(provider = "codex") { return structuredClone(providerState(provider).acceptedSettings); },
    createdSettings(provider = "codex") { return structuredClone(providerState(provider).createdSettings); },
    failNextImageUpload() {
      nextImageFailure = { status: 500, error: { code: "image_storage_failure", message: "The selected image could not be stored safely.", action: "try_again" } };
    },
    setHoldInteractionResolution(provider, value) { providerState(provider).holdInteractionResolution = value; },
    releaseInteraction(provider, requestID) {
      const state = providerState(provider);
      const interaction = state.interactions.get(requestID);
      if (!interaction?.resolved || !interaction.response) throw new Error(`no resolved ${provider} interaction ${requestID}`);
      emit(state, "interaction_resolved", { request_id: requestID, kind: interaction.kind, option_id: interaction.response.option_id });
    },
    disconnectProvider(provider) {
      for (const stream of [...streams]) {
        if (stream.provider === provider) stream.response.end();
      }
      for (const connection of [...webSockets]) {
        if (connection.provider === provider) connection.socket.destroy();
      }
    },
    emitBlocked(kind, provider = "pi") {
      const messages = {
        tool: "A provider tool request was blocked by content-only policy.",
        permission: "A provider permission request was blocked by content-only policy.",
      };
      if (!messages[kind]) throw new Error(`unsupported blocked fixture kind: ${kind}`);
      const state = providerState(provider);
      emit(state, "blocked", { kind, message: messages[kind] });
    },
    emitActivity(kind, summary, provider = "pi") {
      const state = providerState(provider);
      emit(state, "activity", { kind, summary });
    },
    emitToolActivity(provider, payload) {
      const state = providerState(provider);
      const activityID = payload.activity_id ?? protocolID(state.identitySequence++);
      emit(state, "tool_activity", { ...payload, activity_id: activityID });
      return activityID;
    },
    emitInteraction(provider, payload) {
      const state = providerState(provider);
      const requestID = payload.request_id ?? protocolID(state.identitySequence++);
      const request = {
        turn_id: payload.turn_id,
        request_id: requestID,
        kind: payload.kind,
        title: payload.title,
        summary: payload.summary ?? "",
        command: payload.command ?? "",
        working_directory: payload.working_directory ?? "",
        options: payload.options ?? [],
        questions: payload.questions ?? [],
        fields: payload.fields ?? [],
      };
      if (request.turn_id === undefined) delete request.turn_id;
      state.interactions.set(requestID, { requestID, kind: request.kind, resolved: false, response: null });
      emit(state, "interaction_request", request);
      return requestID;
    },
    resetState() {
      for (const provider of Object.keys(providerDefinitions)) providers.set(provider, createProviderState(provider));
      fixtureImages.clear();
      imageSequence = 300;
      nextImageFailure = null;
      interactionResults.splice(0);
    },
    resetRequests() { requests.splice(0); webSocketCommands.splice(0); interactionResults.splice(0); webSocketEnabled = false; },
    interactionResults,
  };
}

function createPiModelServer() {
  const requests = [];
  let pendingStream = null;
  let pendingWaiters = [];

  const waitForPendingStream = () => {
    if (pendingStream) return Promise.resolve(pendingStream);
    return new Promise((resolve) => pendingWaiters.push(resolve));
  };
  const setPendingStream = (stream) => {
    pendingStream = stream;
    const waiters = pendingWaiters;
    pendingWaiters = [];
    for (const resolve of waiters) resolve(stream);
  };
  const releasePhase = async (phase) => {
    const stream = await waitForPendingStream();
    if (stream.phase === "closed") throw new Error(`model stream closed before ${phase}`);
    if (phase === "first_delta" && stream.phase === "responding") {
      stream.write({ id: "chatcmpl-browser", object: "chat.completion.chunk", created: 1, model: "agent-whiteboard-browser", choices: [{ index: 0, delta: { content: "Real Pi fixture " }, finish_reason: null }] });
      stream.phase = "first_delta";
      return;
    }
    if (phase === "later_delta" && stream.phase === "first_delta") {
      stream.write({ id: "chatcmpl-browser", object: "chat.completion.chunk", created: 1, model: "agent-whiteboard-browser", choices: [{ index: 0, delta: { content: "reply" }, finish_reason: null }] });
      stream.phase = "later_delta";
      return;
    }
    if (phase === "completion" && stream.phase === "later_delta") {
      stream.write({ id: "chatcmpl-browser", object: "chat.completion.chunk", created: 1, model: "agent-whiteboard-browser", choices: [{ index: 0, delta: {}, finish_reason: "stop" }] });
      stream.write({ id: "chatcmpl-browser", object: "chat.completion.chunk", created: 1, model: "agent-whiteboard-browser", choices: [], usage: { prompt_tokens: 10, completion_tokens: 4, total_tokens: 14 } });
      stream.phase = "completion";
      stream.response.end("data: [DONE]\n\n");
      pendingStream = null;
      return;
    }
    throw new Error(`model stream cannot release ${phase} from ${stream.phase}`);
  };

  const server = http.createServer((request, response) => {
    const chunks = [];
    request.on("data", (chunk) => chunks.push(chunk));
    request.on("end", () => {
      const body = Buffer.concat(chunks).toString("utf8");
      requests.push({ method: request.method, url: request.url, headers: { ...request.headers }, body });
      if (request.method !== "POST" || request.url !== "/v1/chat/completions") {
        response.writeHead(404, { "Content-Type": "application/json" });
        response.end('{"error":{"message":"not found"}}');
        return;
      }
      response.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
      const write = (value) => response.write(`data: ${JSON.stringify(value)}\n\n`);
      write({ id: "chatcmpl-browser", object: "chat.completion.chunk", created: 1, model: "agent-whiteboard-browser", choices: [{ index: 0, delta: { role: "assistant" }, finish_reason: null }] });
      const stream = { response, write, phase: "responding" };
      response.once("close", () => {
        stream.phase = "closed";
        if (pendingStream === stream) pendingStream = null;
      });
      setPendingStream(stream);
    });
  });
  return {
    server,
    requests,
    releaseFirstDelta: () => releasePhase("first_delta"),
    releaseLaterDelta: () => releasePhase("later_delta"),
    releaseCompletion: () => releasePhase("completion"),
    closePending() {
      if (pendingStream && !pendingStream.response.writableEnded) pendingStream.response.end();
      pendingStream = null;
      pendingWaiters = [];
    },
  };
}

async function reserveLoopbackPort() {
  const temporary = http.createServer();
  const port = await listen(temporary, "127.0.0.1");
  await new Promise((resolve, reject) => temporary.close((error) => (error ? reject(error) : resolve())));
  return port;
}

async function waitForAgentStatus(port, origin, child, output) {
  const deadline = Date.now() + processTimeout;
  let lastError;
  while (Date.now() < deadline) {
    if (child.exitCode !== null || child.signalCode !== null) {
      const captured = output();
      throw new Error(`agent service exited before readiness\nstdout:\n${captured.stdout}\nstderr:\n${captured.stderr}`);
    }
    try {
      const response = await fetch(`http://127.0.0.1:${port}/api/v1/agent/status`, { headers: { Origin: origin }, signal: AbortSignal.timeout(500) });
      const body = await response.json();
      if (response.status === 200 && body.available === true && body.origin_trusted === true) return;
      lastError = new Error(`unexpected agent status ${response.status}: ${JSON.stringify(body)}`);
    } catch (error) {
      lastError = error;
    }
    await new Promise((resolve) => setTimeout(resolve, pollInterval));
  }
  const captured = output();
  throw new Error(`agent service readiness timed out: ${lastError}\nstdout:\n${captured.stdout}\nstderr:\n${captured.stderr}`);
}

function createLoopbackStub() {
  const requests = [];
  const server = http.createServer((request, response) => {
    requests.push(requestRecord(request));
    response.writeHead(204, { "Cache-Control": "no-store" });
    response.end();
  });
  return { server, requests };
}

function createStandaloneCaptureServer(upstreamURL, captureSelfNavigation = false) {
  const upstream = new URL(upstreamURL);
  const requests = [];
  const server = http.createServer((request, response) => {
    const record = requestRecord(request);
    requests.push(record);
    const chunks = [];
    request.on("data", (chunk) => chunks.push(chunk));
    request.on("end", () => {
      record.body = Buffer.concat(chunks).toString("utf8");
      if (captureSelfNavigation && request.url?.startsWith("/self-navigation?")) {
        record.status = 200;
        record.responseHeaders = { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" };
        response.writeHead(record.status, record.responseHeaders);
        response.end("<!doctype html><meta charset=utf-8><title>capture received</title><p>capture received</p>");
        return;
      }
      const proxyRequest = http.request(
        {
          hostname: upstream.hostname,
          port: upstream.port,
          method: request.method,
          path: request.url,
          headers: { ...request.headers, host: upstream.host },
        },
        (proxyResponse) => {
          record.status = proxyResponse.statusCode;
          record.responseHeaders = { ...proxyResponse.headers };
          response.writeHead(proxyResponse.statusCode ?? 502, proxyResponse.headers);
          proxyResponse.pipe(response);
        },
      );
      proxyRequest.once("error", (error) => {
        record.status = 502;
        if (!response.headersSent) response.writeHead(502, { "Content-Type": "text/plain; charset=utf-8" });
        response.end(`capture proxy failed: ${error.message}`);
      });
      proxyRequest.end(record.body);
    });
  });
  server.on("upgrade", (request, socket) => {
    const record = requestRecord(request);
    record.status = 426;
    requests.push(record);
    socket.end("HTTP/1.1 426 Upgrade Required\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
  });
  return { server, requests };
}

export const test = base.extend({
  server: [
    async ({}, use) => {
      const root = await fs.mkdtemp(path.join(os.tmpdir(), "agent-whiteboard-browser-"));
      const binary = path.join(root, process.platform === "win32" ? "agent-whiteboard.exe" : "agent-whiteboard");
      const storage = path.join(root, "storage");
      const home = path.join(root, "home");
      const env = isolatedEnvironment(home);
      let running;
      try {
        await fs.mkdir(storage, { recursive: true });
        await fs.mkdir(home, { recursive: true });
        await runProcess("go", ["build", "-trimpath", "-o", binary, "./cmd/agent-whiteboard"]);
        running = startServer(binary, storage, env);
        const listening = await running.listening;
        await waitForReady(listening.url, running.child, running.output);
        await use({ ...listening, binary, child: running.child, env, root, storage });
      } finally {
        try {
          await stopServer(running?.child);
        } finally {
          await fs.rm(root, { recursive: true, force: true });
        }
      }
    },
    { scope: "worker" },
  ],

  localAgentTransport: [
    async ({ server }, use) => {
      const credentials = await createTestCertificate(server.root);
      const source = createHTTPSSource(credentials, server.url);
      const sourceSockets = trackConnections(source.server);
      const broker = createLoopbackBroker("");
      const brokerSockets = trackConnections(broker.server);
      const stub = createLoopbackStub();
      const stubSockets = trackConnections(stub.server);
      try {
        const sourcePort = await listen(source.server, "::1");
        const sourceOrigin = `https://[::1]:${sourcePort}`;
        broker.setAllowedOrigin(sourceOrigin);
        const brokerPort = await listen(broker.server, "127.0.0.1");
        const stubPort = await listen(stub.server, "127.0.0.1");
        await use({
          source: {
            origin: sourceOrigin,
            url: `${sourceOrigin}/__local-agent-transport`,
            proxyURL: sourceOrigin,
            requests: source.requests,
          },
          broker: {
            origin: `http://127.0.0.1:${brokerPort}`,
            requests: broker.requests,
            reset: broker.reset,
            setWebSocketFailure: broker.setWebSocketFailure,
          },
          stub: { origin: `http://127.0.0.1:${stubPort}`, requests: stub.requests },
        });
      } finally {
        await Promise.all([
          closeNodeServer(stub.server, stubSockets),
          closeNodeServer(broker.server, brokerSockets),
          closeNodeServer(source.server, sourceSockets),
        ]);
      }
    },
    { scope: "worker" },
  ],

  localAgentSidebar: [
    async ({ server }, use) => {
      const configPath = path.join(server.root, "sidebar-config.yaml");
      const storage = path.join(server.root, "sidebar-storage");
      await fs.writeFile(configPath, "version: 1\nviewer:\n  local_agent:\n    enabled: true\n", { mode: 0o600 });
      await fs.mkdir(storage, { recursive: true });
      const running = startServer(server.binary, storage, server.env, ["--config", configPath]);
      let source;
      let sourceSockets;
      let broker;
      let brokerSockets;
      try {
        const listening = await running.listening;
        await waitForReady(listening.url, running.child, running.output);
        const credentials = await createTestCertificate(server.root);
        source = createHTTPSSource(credentials, listening.url);
        sourceSockets = trackConnections(source.server);
        const sourcePort = await listen(source.server, "::1");
        const sourceOrigin = `https://[::1]:${sourcePort}`;
        broker = createSidebarBroker(sourceOrigin);
        brokerSockets = trackConnections(broker.server);
        const brokerPort = await listen(broker.server, "127.0.0.1");
        let sequence = 0;
        const publishAtOrigin = async (origin, markdown, creatorContext = "Creator context for the local Pi agent.\n") => {
          const fixturePath = path.join(server.root, `sidebar-${sequence}.md`);
          const contextPath = path.join(server.root, `sidebar-${sequence++}-context.md`);
          await Promise.all([
            fs.writeFile(fixturePath, markdown, { mode: 0o600 }),
            fs.writeFile(contextPath, creatorContext, { mode: 0o600 }),
          ]);
          const { stdout, stderr } = await runProcess(
            server.binary,
            ["--server", listening.url, "--json", "create", "markdown", "--context", contextPath, "--expires-in", "0", fixturePath],
            { env: server.env, timeout: processTimeout },
          );
          if (stderr !== "") throw new Error(`CLI wrote unexpected stderr: ${stderr}`);
          const envelope = JSON.parse(stdout);
          const pathName = new URL(envelope.resource.url).pathname;
          return { ...envelope.resource, url: `${origin}${pathName}`, markdown, context: creatorContext };
        };
        await use({
          origin: sourceOrigin,
          loopbackOrigin: listening.url,
          brokerPort,
          brokerRequests: broker.requests,
          webSocketCommands: broker.webSocketCommands,
          resetBrokerRequests: broker.resetRequests,
          resetBrokerState: broker.resetState,
          setWebSocketEnabled: broker.setWebSocketEnabled,
          setHoldResponses: broker.setHoldResponses,
          setHoldInterruptCompletion: broker.setHoldInterruptCompletion,
          releaseInterruptCompletion: broker.releaseInterruptCompletion,
          setPhaseResponses: broker.setPhaseResponses,
          preparePendingResponse: broker.preparePendingResponse,
          setResponseText: broker.setResponseText,
          releaseResponsePhase: broker.releaseResponsePhase,
          setProviderAvailable: broker.setProviderAvailable,
          setSupportsImages: broker.setSupportsImages,
          setArchiveMode: broker.setArchiveMode,
          setArchiveDelay: broker.setArchiveDelay,
          releaseArchiveList: broker.releaseArchiveList,
          refreshSkills: broker.refreshSkills,
          completeCompact: broker.completeCompact,
          rejectNextSettings: broker.rejectNextSettings,
          setNewMapping: broker.setNewMapping,
          refreshCatalog: broker.refreshCatalog,
          restoreSettings: broker.restoreSettings,
          acceptedSettings: broker.acceptedSettings,
          createdSettings: broker.createdSettings,
          failNextImageUpload: broker.failNextImageUpload,
          disconnectProvider: broker.disconnectProvider,
          emitToolActivity: broker.emitToolActivity,
          emitInteraction: broker.emitInteraction,
          setHoldInteractionResolution: broker.setHoldInteractionResolution,
          releaseInteraction: broker.releaseInteraction,
          interactionResults: broker.interactionResults,
          emitBlocked: broker.emitBlocked,
          emitActivity: broker.emitActivity,
          publish: (markdown, creatorContext) => publishAtOrigin(sourceOrigin, markdown, creatorContext),
          publishLoopback: (markdown, creatorContext) => publishAtOrigin(listening.url, markdown, creatorContext),
        });
      } finally {
        await Promise.all([
          broker ? closeNodeServer(broker.server, brokerSockets) : Promise.resolve(),
          source ? closeNodeServer(source.server, sourceSockets) : Promise.resolve(),
        ]);
        await stopServer(running.child);
      }
    },
    { scope: "worker" },
  ],

  realAgentSidebar: [
    async ({ server, localAgentSidebar }, use) => {
      const root = path.join(server.root, "real-agent-sidebar");
      const home = path.join(root, "home");
      const piConfig = path.join(home, ".pi", "agent");
      const temporary = path.join(root, "tmp");
      await Promise.all([
        fs.mkdir(piConfig, { recursive: true, mode: 0o700 }),
        fs.mkdir(temporary, { recursive: true, mode: 0o700 }),
      ]);
      const model = createPiModelServer();
      const modelSockets = trackConnections(model.server);
      const modelPort = await listen(model.server, "127.0.0.1");
      const providerName = "agent-whiteboard-browser";
      await Promise.all([
        fs.writeFile(path.join(piConfig, "models.json"), `${JSON.stringify({ providers: {
          [providerName]: {
            baseUrl: `http://127.0.0.1:${modelPort}/v1`, api: "openai-completions", apiKey: "browser-placeholder-key",
            models: [{ id: providerName, name: "Agent Whiteboard Browser", reasoning: false, input: ["text"], contextWindow: 32768, maxTokens: 1024, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }],
          },
        } }, null, 2)}\n`, { mode: 0o600 }),
        fs.writeFile(path.join(piConfig, "settings.json"), `${JSON.stringify({
          defaultProvider: providerName, defaultModel: providerName, defaultThinkingLevel: "off", defaultProjectTrust: "never",
          enableInstallTelemetry: false, compaction: { enabled: false }, retry: { enabled: false, provider: { timeoutMs: 5000, maxRetries: 0 } },
        }, null, 2)}\n`, { mode: 0o600 }),
      ]);
      const configPath = path.join(root, "config.yaml");
      await fs.writeFile(configPath, `version: 1\nagent:\n  trusted_origins:\n    - "${localAgentSidebar.origin}"\n  provider_idle_timeout: 10m\n  shutdown_timeout: 10s\n  default_access: configured\n`, { mode: 0o600 });
      const agentPort = await reserveLoopbackPort();
      const piExecutable = path.join(projectRoot, "node_modules", ".bin", "pi");
      const env = { ...isolatedEnvironment(home), TMPDIR: temporary };
      const child = spawn(server.binary, ["--config", configPath, "agent", "serve", "--port", String(agentPort), "--pi-executable", piExecutable], {
        cwd: root, env, stdio: ["ignore", "pipe", "pipe"],
      });
      child.stdout.setEncoding("utf8");
      child.stderr.setEncoding("utf8");
      let stdout = "";
      let stderr = "";
      child.stdout.on("data", (chunk) => { stdout += chunk; });
      child.stderr.on("data", (chunk) => { stderr += chunk; });
      const output = () => ({ stdout, stderr });
      try {
        await waitForAgentStatus(agentPort, localAgentSidebar.origin, child, output);
        await use({
          origin: localAgentSidebar.origin,
          loopbackOrigin: localAgentSidebar.loopbackOrigin,
          brokerPort: agentPort,
          publish: localAgentSidebar.publish,
          publishLoopback: localAgentSidebar.publishLoopback,
          modelRequests: model.requests,
          resetModelRequests: () => model.requests.splice(0),
          releaseModelFirstDelta: model.releaseFirstDelta,
          releaseModelLaterDelta: model.releaseLaterDelta,
          releaseModelCompletion: model.releaseCompletion,
          agentOutput: output,
        });
      } finally {
        model.closePending();
        await stopServer(child);
        await closeNodeServer(model.server, modelSockets);
      }
    },
    { scope: "worker" },
  ],

  publish: async ({ server }, use) => {
    let sequence = 0;
    await use(async (markdown, creatorContext = "# Browser test context\n\nHermetic rendering fixture.\n") => {
      const fixtureNumber = sequence++;
      const fixturePath = path.join(server.root, `fixture-${fixtureNumber}.md`);
      const contextPath = path.join(server.root, `fixture-${fixtureNumber}-context.md`);
      await Promise.all([
        fs.writeFile(fixturePath, markdown, { mode: 0o600 }),
        fs.writeFile(contextPath, creatorContext, { mode: 0o600 }),
      ]);
      const { stdout, stderr } = await runProcess(
        server.binary,
        ["--server", server.url, "--json", "create", "markdown", "--context", contextPath, "--expires-in", "0", fixturePath],
        { env: server.env, timeout: processTimeout },
      );
      if (stderr !== "") throw new Error(`CLI wrote unexpected stderr: ${stderr}`);
      const envelope = JSON.parse(stdout);
      if (envelope.schema_version !== 1 || typeof envelope.resource?.url !== "string") {
        throw new Error(`invalid CLI JSON: ${stdout}`);
      }
      return envelope.resource;
    });
  },

  publishHTML: async ({ server }, use) => {
    let sequence = 0;
    await use(async (html) => {
      const fixturePath = path.join(server.root, `standalone-${sequence++}.html`);
      await fs.writeFile(fixturePath, html, { mode: 0o600 });
      const { stdout, stderr } = await runProcess(
        server.binary,
        ["--server", server.url, "--json", "create", "html", "--expires-in", "0", fixturePath],
        { env: server.env, timeout: processTimeout },
      );
      if (stderr !== "") throw new Error(`CLI wrote unexpected stderr: ${stderr}`);
      const envelope = JSON.parse(stdout);
      if (envelope.schema_version !== 1 || typeof envelope.resource?.url !== "string") {
        throw new Error(`invalid CLI JSON: ${stdout}`);
      }
      return envelope.resource;
    });
  },

  standaloneCapture: [
    async ({ server }, use) => {
      const publishingOrigin = createStandaloneCaptureServer(server.url, true);
      const publishingSockets = trackConnections(publishingOrigin.server);
      const crossOrigin = createStandaloneCaptureServer(server.url);
      const crossSockets = trackConnections(crossOrigin.server);
      try {
        const publishingPort = await listen(publishingOrigin.server, "127.0.0.1");
        const crossPort = await listen(crossOrigin.server, "127.0.0.1");
        await use({
          origin: `http://127.0.0.1:${publishingPort}`,
          requests: publishingOrigin.requests,
          reset: () => publishingOrigin.requests.splice(0),
          crossOrigin: {
            origin: `http://127.0.0.1:${crossPort}`,
            requests: crossOrigin.requests,
          },
        });
      } finally {
        await Promise.all([
          closeNodeServer(crossOrigin.server, crossSockets),
          closeNodeServer(publishingOrigin.server, publishingSockets),
        ]);
      }
    },
    { scope: "worker" },
  ],

  browserRequestInterception: [true, { option: true }],

  networkRequests: [
    async ({ page, server, browserRequestInterception }, use) => {
      const all = [];
      const external = [];
      if (browserRequestInterception) {
        await page.route("**/*", async (route) => {
          const requestURL = route.request().url();
          all.push(requestURL);
          if (new URL(requestURL).origin !== new URL(server.url).origin) {
            external.push(requestURL);
            await route.abort("blockedbyclient");
            return;
          }
          await route.continue();
        });
      }
      await use({ all, external });
      if (browserRequestInterception) expect(external, "external browser requests").toEqual([]);
    },
    { auto: true },
  ],
});

export { expect };
