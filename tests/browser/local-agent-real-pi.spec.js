import { expect, test } from "./fixture.js";

const portKey = "agent-whiteboard-agent-port";

test.use({ browserRequestInterception: false, ignoreHTTPSErrors: true });

test("runs the consented browser flow through the real broker and pinned Pi", async ({
  context,
  page,
  realAgentSidebar,
}) => {
  test.setTimeout(30_000);
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
  await expect(page.locator(".agent-live-status")).toContainText("Local broker available");
  expect(realAgentSidebar.modelRequests).toHaveLength(0);
  await page.getByRole("button", { name: "Open local agent" }).click();
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(page.locator('.agent-composer button[type="submit"]')).toBeEnabled({ timeout: 15_000 });
  expect(realAgentSidebar.modelRequests).toHaveLength(0);

  await page.getByLabel("Message Pi about this whiteboard").fill("Answer from the supplied content only.");
  await page.locator('.agent-composer button[type="submit"]').click();
  await expect(page.locator(".agent-message-assistant")).toContainText("Real Pi fixture reply", { timeout: 15_000 });
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
