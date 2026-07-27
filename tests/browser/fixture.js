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
        '{"available":true,"api_version":"1"}',
      );
      return;
    }
    if (request.method === "POST" && request.url === "/api/v1/agent/connect") {
      if (request.headers["x-agent-whiteboard-api-version"] !== "1") {
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
    if (!originAllowed(request) || request.url !== "/api/v1/agent/connect") {
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
    record.responseHeaders = { Upgrade: "websocket", Connection: "Upgrade", "Sec-WebSocket-Accept": accept };
    socket.write(
      [
        "HTTP/1.1 101 Switching Protocols",
        "Upgrade: websocket",
        "Connection: Upgrade",
        `Sec-WebSocket-Accept: ${accept}`,
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
  const conversationID = protocolID(201);
  let allowedOrigin = initialAllowedOrigin;
  let webSocketEnabled = false;
  let sequence = 1;
  let contextState = "pending";
  let contextDigest = "0".repeat(64);
  let holdResponses = false;
  let activeTurn = null;
  const queue = [];
  const history = [];

  const nextEvent = (type, payload) => ({
    api_version: "1",
    event_id: protocolID(sequence++),
    conversation_id: conversationID,
    type,
    timestamp: "2026-07-27T03:04:05Z",
    payload,
  });
  const snapshotPayload = () => ({ lifecycle: activeTurn === null ? "ready" : "responding", queue: queue.map((item) => ({ ...item })), context_state: contextState, active_turn_id: activeTurn });
  const corsHeaders = () => ({ "Access-Control-Allow-Origin": allowedOrigin, Vary: "Origin" });
  const sendJSON = (response, record, status, value) => {
    const body = `${JSON.stringify(value)}\n`;
    record.status = status;
    record.responseHeaders = { ...corsHeaders(), "Content-Type": "application/json", "Cache-Control": "no-store" };
    response.writeHead(status, record.responseHeaders);
    response.end(body);
  };
  const emit = (event) => {
    const encoded = JSON.stringify(event);
    for (const response of streams) response.write(`${encoded}\n`);
    for (const socket of webSockets) socket.write(webSocketFrame(encoded));
  };
  const commandResult = (command, error) => nextEvent("command_result", error
    ? { command_id: command.command_id, status: "rejected", error }
    : { command_id: command.command_id, status: "succeeded" });
  const handleCommand = (command) => {
    if (command.type === "history_page") {
      emit(nextEvent("timeline", { command_id: command.command_id, items: [...history].reverse(), next_cursor: null }));
    } else if (command.type === "submit") {
      if (activeTurn !== null) {
        queue.push({ turn_id: command.payload.turn_id, message_id: command.payload.message_id, message: command.payload.message });
        emit(nextEvent("queue", { items: queue.map((item) => ({ ...item })) }));
      } else {
        if (command.payload.context) {
          contextState = "accepted";
          contextDigest = command.payload.context.digest;
          emit(nextEvent("context", { digest: contextDigest, state: "accepted" }));
        }
        const createdAt = "2026-07-27T03:04:05Z";
        const user = { item_id: command.payload.message_id, kind: "user", turn_id: command.payload.turn_id, message_id: command.payload.message_id, text: command.payload.message, created_at: createdAt };
        history.push(user);
        activeTurn = command.payload.turn_id;
        emit(nextEvent("user_message", { turn_id: user.turn_id, message_id: user.message_id, text: user.text, created_at: createdAt }));
        emit(nextEvent("lifecycle", { state: "responding", turn_id: command.payload.turn_id }));
        if (!holdResponses) {
          const assistantID = protocolID(150 + history.length);
          emit(nextEvent("assistant_delta", { turn_id: command.payload.turn_id, message_id: assistantID, text: "Fixture " }));
          emit(nextEvent("assistant_delta", { turn_id: command.payload.turn_id, message_id: assistantID, text: "reply" }));
          const assistant = { item_id: assistantID, kind: "assistant", turn_id: command.payload.turn_id, message_id: assistantID, text: "Fixture reply", created_at: createdAt };
          history.push(assistant);
          emit(nextEvent("assistant_message", { turn_id: assistant.turn_id, message_id: assistant.message_id, text: assistant.text, created_at: createdAt }));
          emit(nextEvent("completion", { turn_id: command.payload.turn_id }));
          activeTurn = null;
        }
      }
    } else if (command.type === "queue_edit") {
      const item = queue.find((candidate) => candidate.message_id === command.payload.message_id);
      if (item) item.message = command.payload.message;
      emit(nextEvent("queue", { items: queue.map((candidate) => ({ ...candidate })) }));
    } else if (command.type === "queue_remove") {
      const index = queue.findIndex((candidate) => candidate.message_id === command.payload.message_id);
      if (index >= 0) queue.splice(index, 1);
      emit(nextEvent("queue", { items: queue.map((candidate) => ({ ...candidate })) }));
    } else if (command.type === "interrupt" && activeTurn === command.payload.turn_id) {
      emit(nextEvent("interruption", { turn_id: activeTurn, reason: "requested" }));
      activeTurn = null;
    }
    const result = commandResult(command);
    emit(result);
    return result;
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
        "Access-Control-Allow-Headers": "content-type, x-agent-whiteboard-api-version",
      };
      response.writeHead(204, record.responseHeaders);
      response.end();
      return;
    }
    if (request.method === "GET" && request.url === "/api/v1/agent/status") {
      sendJSON(response, record, 200, { available: true, api_version: "1", origin_trusted: true });
      return;
    }
    const chunks = [];
    request.on("data", (chunk) => chunks.push(chunk));
    request.on("end", () => {
      record.body = Buffer.concat(chunks).toString("utf8");
      let command;
      try { command = JSON.parse(record.body); }
      catch {
        sendJSON(response, record, 400, { error: { code: "invalid_command", message: "The broker rejected an invalid command.", action: "none" } });
        return;
      }
      if (request.method === "POST" && request.url === "/api/v1/agent/connect") {
        record.status = 200;
        record.responseHeaders = { ...corsHeaders(), "Content-Type": "application/x-ndjson", "Cache-Control": "no-store" };
        response.writeHead(200, record.responseHeaders);
        streams.add(response);
        response.once("close", () => streams.delete(response));
        response.write(`${JSON.stringify(nextEvent("snapshot", snapshotPayload()))}\n`);
        response.write(`${JSON.stringify(nextEvent("provider", { provider: "pi", state: "ready", model: "fixture-model" }))}\n`);
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
    if (!webSocketEnabled || request.headers.origin !== allowedOrigin || request.headers["sec-websocket-protocol"] !== "agent-whiteboard.v1") {
      record.status = 503;
      socket.end("HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
      return;
    }
    const key = request.headers["sec-websocket-key"];
    const accept = createHash("sha1").update(`${key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest("base64");
    record.status = 101;
    record.responseHeaders = { Upgrade: "websocket", Connection: "Upgrade", "Sec-WebSocket-Accept": accept, "Sec-WebSocket-Protocol": "agent-whiteboard.v1" };
    socket.write([
      "HTTP/1.1 101 Switching Protocols",
      "Upgrade: websocket",
      "Connection: Upgrade",
      `Sec-WebSocket-Accept: ${accept}`,
      "Sec-WebSocket-Protocol: agent-whiteboard.v1",
      "",
      "",
    ].join("\r\n"));
    webSockets.add(socket);
    socket.once("close", () => webSockets.delete(socket));
    let buffered = Buffer.alloc(0);
    let connected = false;
    socket.on("data", (chunk) => {
      buffered = Buffer.concat([buffered, chunk]);
      try {
        buffered = consumeClientWebSocketFrames(buffered, (payload) => {
          const command = JSON.parse(payload);
          webSocketCommands.push(command);
          if (!connected) {
            connected = true;
            socket.write(webSocketFrame(JSON.stringify(nextEvent("snapshot", snapshotPayload()))));
            socket.write(webSocketFrame(JSON.stringify(nextEvent("provider", { provider: "pi", state: "ready", model: "fixture-model" }))));
            return;
          }
          handleCommand(command);
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
    setHoldResponses(value) { holdResponses = value; },
    resetState() {
      contextState = "pending";
      contextDigest = "0".repeat(64);
      holdResponses = false;
      activeTurn = null;
      queue.splice(0);
      history.splice(0);
      sequence = 1;
    },
    resetRequests() { requests.splice(0); webSocketCommands.splice(0); webSocketEnabled = false; },
  };
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
        await use({
          origin: sourceOrigin,
          brokerPort,
          brokerRequests: broker.requests,
          webSocketCommands: broker.webSocketCommands,
          resetBrokerRequests: broker.resetRequests,
          resetBrokerState: broker.resetState,
          setWebSocketEnabled: broker.setWebSocketEnabled,
          setHoldResponses: broker.setHoldResponses,
          publish: async (markdown, creatorContext = "Creator context for the local Pi agent.\n") => {
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
            return { ...envelope.resource, url: `${sourceOrigin}${pathName}`, markdown, context: creatorContext };
          },
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
