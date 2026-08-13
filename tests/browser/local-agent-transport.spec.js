import { expect, test } from "./fixture.js";

test.use({ browserRequestInterception: false, ignoreHTTPSErrors: true });

async function grantLocalNetworkAccess(context, origin) {
  await context.grantPermissions(["local-network-access"], { origin });
}

async function fetchStatus(page, brokerOrigin) {
  return page.evaluate(async (origin) => {
    try {
      const response = await fetch(`${origin}/api/v1/agent/status`);
      return { ok: response.ok, status: response.status, body: await response.json() };
    } catch (error) {
      return { ok: false, error: String(error) };
    }
  }, brokerOrigin);
}

async function readWebSocketStream(page, brokerOrigin) {
  return page.evaluate(
    ({ origin }) =>
      new Promise((resolve, reject) => {
        const messages = [];
        const socket = new WebSocket(`${origin.replace("http:", "ws:")}/api/v1/agent/connect`, "agent-whiteboard.v4");
        const timer = setTimeout(() => {
          socket.close();
          reject(new Error("WebSocket stream timed out"));
        }, 5_000);
        socket.onmessage = (event) => {
          messages.push(JSON.parse(event.data));
          if (messages.some((message) => message.type === "delta")) {
            clearTimeout(timer);
            socket.close();
            resolve(messages);
          }
        };
        socket.onerror = () => {
          clearTimeout(timer);
          reject(new Error("WebSocket handshake failed"));
        };
      }),
    { origin: brokerOrigin },
  );
}

async function connectWithFallback(page, brokerOrigin) {
  return page.evaluate(async (origin) => {
    const webSocketURL = `${origin.replace("http:", "ws:")}/api/v1/agent/connect`;
    try {
      await new Promise((resolve, reject) => {
        const socket = new WebSocket(webSocketURL, "agent-whiteboard.v4");
        const timer = setTimeout(() => {
          socket.close();
          reject(new Error("WebSocket attempt timed out"));
        }, 2_000);
        socket.onopen = () => {
          clearTimeout(timer);
          socket.close();
          resolve();
        };
        socket.onerror = () => {
          clearTimeout(timer);
          reject(new Error("WebSocket unavailable"));
        };
      });
      return { transport: "websocket", messages: [] };
    } catch {
      const response = await fetch(`${origin}/api/v1/agent/connect`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-Agent-Whiteboard-API-Version": "4",
        },
        body: "{}",
      });
      if (!response.ok || !response.body) throw new Error(`HTTP fallback failed: ${response.status}`);
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffered = "";
      const chunks = [];
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        chunks.push(decoder.decode(value, { stream: true }));
      }
      buffered = chunks.join("") + decoder.decode();
      return {
        transport: "http",
        chunks: chunks.length,
        messages: buffered
          .trim()
          .split("\n")
          .map((line) => JSON.parse(line)),
      };
    }
  }, brokerOrigin);
}

test("requires exact-origin Local Network Access permission before loopback status discovery", async ({
  context,
  page,
  localAgentTransport,
}) => {
  const { source, broker } = localAgentTransport;
  broker.reset();
  await context.clearPermissions();
  await page.goto(source.url);
  expect(await page.evaluate(() => location.origin)).toBe(source.origin);

  const denied = await fetchStatus(page, broker.origin);
  expect(denied.ok).toBe(false);
  expect(denied.error).toContain("Failed to fetch");
  await expect(readWebSocketStream(page, broker.origin)).rejects.toThrow("WebSocket handshake failed");
  await new Promise((resolve) => setTimeout(resolve, 200));
  expect(broker.requests, "loopback HTTP or WebSocket requests before LNA permission").toEqual([]);

  await grantLocalNetworkAccess(context, source.origin);
  const permission = await page.evaluate(async () =>
    navigator.permissions.query({ name: "local-network-access" }).then(({ state }) => state),
  );
  expect(permission).toBe("granted");

  const status = await fetchStatus(page, broker.origin);
  expect(status).toEqual({ ok: true, status: 200, body: { available: true, api_version: "4" } });
  expect(broker.requests).toHaveLength(1);
  expect(broker.requests[0]).toMatchObject({
    method: "GET",
    url: "/api/v1/agent/status",
    headers: { origin: source.origin },
    status: 200,
  });
  expect(broker.requests[0].responseHeaders["Access-Control-Allow-Origin"]).toBe(source.origin);
  expect(broker.requests[0].responseHeaders["Access-Control-Allow-Origin"]).not.toBe("*");
});

test("streams over a real WebSocket and falls back to HTTP streaming after handshake failure", async ({
  context,
  page,
  localAgentTransport,
}) => {
  const { source, broker } = localAgentTransport;
  broker.reset();
  broker.setWebSocketFailure(false);
  await page.goto(source.url);
  await grantLocalNetworkAccess(context, source.origin);

  const webSocketMessages = await readWebSocketStream(page, broker.origin);
  expect(webSocketMessages).toEqual([
    { type: "ready", transport: "websocket" },
    { type: "delta", text: "websocket stream" },
  ]);
  expect(broker.requests.some((request) => request.status === 101 && request.headers.origin === source.origin)).toBe(true);

  broker.setWebSocketFailure(true);
  const fallback = await connectWithFallback(page, broker.origin);
  expect(fallback).toMatchObject({
    transport: "http",
    messages: [
      { type: "ready", transport: "http" },
      { type: "delta", text: "fallback stream" },
    ],
  });
  expect(fallback.chunks).toBeGreaterThanOrEqual(2);
  expect(broker.requests.some((request) => request.status === 503 && request.headers.origin === source.origin)).toBe(true);
  expect(
    broker.requests.some(
      (request) =>
        request.method === "POST" &&
        request.url === "/api/v1/agent/connect" &&
        request.headers.origin === source.origin &&
        request.headers["x-agent-whiteboard-api-version"] === "4" &&
        request.status === 200,
    ),
  ).toBe(true);
});

test("enforces exact-origin ordinary CORS and legacy PNA preflight contracts", async ({
  context,
  page,
  localAgentTransport,
}) => {
  const { source, broker } = localAgentTransport;
  broker.reset();
  await page.goto(source.url);
  await grantLocalNetworkAccess(context, source.origin);

  const fallback = await page.evaluate(async (origin) => {
    const response = await fetch(`${origin}/api/v1/agent/connect`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Agent-Whiteboard-API-Version": "4",
      },
      body: "{}",
    });
    return { status: response.status };
  }, broker.origin);
  expect(fallback).toEqual({ status: 200 });

  const ordinaryPreflight = broker.requests.find((request) => request.method === "OPTIONS");
  expect(ordinaryPreflight).toMatchObject({
    url: "/api/v1/agent/connect",
    headers: {
      origin: source.origin,
      "access-control-request-method": "POST",
    },
    status: 204,
  });
  expect(ordinaryPreflight.headers["access-control-request-headers"].split(",").map((value) => value.trim()).sort()).toEqual([
    "content-type",
    "x-agent-whiteboard-api-version",
  ]);
  expect(ordinaryPreflight.headers["access-control-request-private-network"]).toBeUndefined();
  expect(ordinaryPreflight.responseHeaders).toMatchObject({
    "Access-Control-Allow-Origin": source.origin,
    "Access-Control-Allow-Methods": "POST",
    "Access-Control-Allow-Headers": "content-type, x-agent-whiteboard-api-version",
  });
  expect(ordinaryPreflight.responseHeaders["Access-Control-Allow-Origin"]).not.toBe("*");

  const deniedOrigin = await fetch(`${broker.origin}/api/v1/agent/status`, {
    headers: { Origin: "https://attacker.invalid" },
  });
  expect(deniedOrigin.status).toBe(403);
  expect(deniedOrigin.headers.get("access-control-allow-origin")).toBeNull();

  const legacyPNA = await fetch(`${broker.origin}/api/v1/agent/status`, {
    method: "OPTIONS",
    headers: {
      Origin: source.origin,
      "Access-Control-Request-Method": "GET",
      "Access-Control-Request-Private-Network": "true",
    },
  });
  expect(legacyPNA.status).toBe(204);
  expect(legacyPNA.headers.get("access-control-allow-origin")).toBe(source.origin);
  expect(legacyPNA.headers.get("access-control-allow-methods")).toBe("GET");
  expect(legacyPNA.headers.get("access-control-allow-private-network")).toBe("true");
  expect(legacyPNA.headers.get("access-control-allow-origin")).not.toBe("*");

  const legacyRecord = broker.requests.at(-1);
  expect(legacyRecord).toMatchObject({
    method: "OPTIONS",
    url: "/api/v1/agent/status",
    headers: {
      origin: source.origin,
      "access-control-request-method": "GET",
      "access-control-request-private-network": "true",
    },
    status: 204,
  });
  expect(legacyRecord.responseHeaders["Access-Control-Allow-Private-Network"]).toBe("true");
});
