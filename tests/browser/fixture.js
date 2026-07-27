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

function startServer(binary, storage, env) {
  const child = spawn(
    binary,
    ["serve", "--host", "127.0.0.1", "--port", "0", "--storage", storage, "--log-mode", "json"],
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
  if (body.length >= 126) throw new Error("test WebSocket payload is unexpectedly large");
  return Buffer.concat([Buffer.from([0x81, body.length]), body]);
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
