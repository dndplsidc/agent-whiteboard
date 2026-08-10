import { expect, test } from "./fixture.js";

const portKey = "agent-whiteboard-agent-port";

test.use({ browserRequestInterception: false, ignoreHTTPSErrors: true });

test("streams a consented Enter submission through the real broker and pinned Pi", async ({
  context,
  page,
  realAgentSidebar,
}) => {
  test.setTimeout(30_000);
  realAgentSidebar.resetModelRequests();
  const markdown = "# Real Pi browser path\n\nExact browser-to-provider Markdown.\n";
  const creatorContext = "Exact browser-to-provider creator context.\n";
  const resource = await realAgentSidebar.publish(markdown, creatorContext);
  await context.grantPermissions(["local-network-access"], { origin: realAgentSidebar.origin });
  await page.addInitScript(
    ({ key, port }) => localStorage.setItem(key, String(port)),
    { key: portKey, port: realAgentSidebar.brokerPort },
  );
  const browserRequests = [];
  page.on("request", (request) => browserRequests.push(request.url()));

  await page.goto(resource.url);
  await expect(page.locator(".agent-live-status")).toHaveText("Pi ready");
  expect(realAgentSidebar.modelRequests).toHaveLength(0);
  await page.getByRole("button", { name: "Open Page agent" }).click();
  await page.getByRole("button", { name: "Connect to Pi", exact: true }).click();
  const composer = page.getByLabel("Message Pi about this whiteboard");
  const send = page.locator('.agent-composer button[type="submit"]');
  await expect(composer).toBeEnabled({ timeout: 15_000 });
  await expect(send).toBeDisabled();
  expect(realAgentSidebar.modelRequests).toHaveLength(0);

  await composer.fill("Answer from the supplied content only.");
  await expect(send).toBeEnabled();
  await composer.press("Enter");
  await expect(page.locator(".agent-response-loading")).toHaveAccessibleName("Pi is responding", { timeout: 15_000 });
  await expect(page.locator(".agent-live-status")).toHaveText("Responding");

  await realAgentSidebar.releaseModelFirstDelta();
  const assistant = page.locator(".agent-message-assistant .agent-message-body");
  await expect(assistant).toContainText("Real Pi fixture");
  await expect(assistant).not.toContainText("reply");
  await expect(page.locator(".agent-response-loading")).toHaveCount(0);

  await realAgentSidebar.releaseModelLaterDelta();
  await expect(assistant).toContainText("Real Pi fixture reply");
  await expect(page.locator(".agent-live-status")).toHaveText("Responding");
  await realAgentSidebar.releaseModelCompletion();
  await expect(page.locator(".agent-live-status")).toHaveText("Connected", { timeout: 15_000 });
  await expect.poll(() => realAgentSidebar.modelRequests.length).toBe(1);

  const modelRequest = realAgentSidebar.modelRequests[0];
  expect(modelRequest.method).toBe("POST");
  expect(modelRequest.url).toBe("/v1/chat/completions");
  expect(modelRequest.body).toContain("Exact browser-to-provider Markdown.");
  expect(modelRequest.body).toContain("Exact browser-to-provider creator context.");
  expect(modelRequest.body).toContain("Answer from the supplied content only.");
  expect(modelRequest.body).not.toContain("browser-placeholder-key");

  const allowedOrigins = new Set([realAgentSidebar.origin, `http://127.0.0.1:${realAgentSidebar.brokerPort}`]);
  for (const requestURL of browserRequests) expect(allowedOrigins.has(new URL(requestURL).origin)).toBe(true);
});

test("automatically connects a literal loopback HTTP Markdown viewer", async ({
  page,
  realAgentSidebar,
}) => {
  test.setTimeout(30_000);
  realAgentSidebar.resetModelRequests();
  const markdown = "# Local HTTP proof\n\nThis board is served directly from literal loopback HTTP.\n";
  const creatorContext = "Prove automatic literal-loopback broker admission.\n";
  const resource = await realAgentSidebar.publishLoopback(markdown, creatorContext);
  expect(new URL(resource.url).origin).toBe(realAgentSidebar.loopbackOrigin);
  await page.addInitScript(
    ({ key, port }) => localStorage.setItem(key, String(port)),
    { key: portKey, port: realAgentSidebar.brokerPort },
  );
  const browserRequests = [];
  page.on("request", (request) => browserRequests.push(request.url()));

  await page.goto(resource.url);
  await expect(page.locator(".agent-live-status")).toHaveText("Pi ready");
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByRole("button", { name: "Connect to Pi", exact: true }).click();
  const composer = page.getByLabel("Message Pi about this whiteboard");
  const send = page.locator('.agent-composer button[type="submit"]');
  await expect(composer).toBeEnabled({ timeout: 15_000 });
  await expect(send).toBeDisabled();
  await composer.fill("Confirm the local HTTP board content.");
  await expect(send).toBeEnabled();
  await composer.press("Enter");
  await expect(page.locator(".agent-response-loading")).toHaveAccessibleName("Pi is responding", { timeout: 15_000 });
  await realAgentSidebar.releaseModelFirstDelta();
  await expect(page.locator(".agent-message-assistant")).toContainText("Real Pi fixture");
  await realAgentSidebar.releaseModelLaterDelta();
  await realAgentSidebar.releaseModelCompletion();
  await expect(page.locator(".agent-live-status")).toHaveText("Connected", { timeout: 15_000 });
  await expect.poll(() => realAgentSidebar.modelRequests.length).toBe(1);

  const modelRequest = realAgentSidebar.modelRequests[0];
  expect(modelRequest.body).toContain("This board is served directly from literal loopback HTTP.");
  expect(modelRequest.body).toContain("Prove automatic literal-loopback broker admission.");
  expect(modelRequest.body).toContain("Confirm the local HTTP board content.");
  const allowedOrigins = new Set([realAgentSidebar.loopbackOrigin, `http://127.0.0.1:${realAgentSidebar.brokerPort}`]);
  for (const requestURL of browserRequests) expect(allowedOrigins.has(new URL(requestURL).origin)).toBe(true);
});
