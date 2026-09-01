import { expect, test } from "./fixture.js";

const portKey = "agent-whiteboard-agent-port";
const providerKey = "agent-whiteboard-agent-provider";

test.use({ browserRequestInterception: false, ignoreHTTPSErrors: true });

test("runs a Cursor turn through the real broker and standalone ACP child", async ({ context, page, realCursorSidebar }) => {
  test.setTimeout(30_000);
  const markdown = "# Scripted Cursor browser path\n\nOnly the real local fixture sees this page content.\n";
  const creatorContext = "Private creator context for the browser path.\n";
  const resource = await realCursorSidebar.publish(markdown, creatorContext);
  await context.grantPermissions(["local-network-access"], { origin: realCursorSidebar.origin });
  await page.addInitScript(
    ({ port, portStorageKey, providerStorageKey }) => {
      localStorage.setItem(portStorageKey, String(port));
      localStorage.setItem(providerStorageKey, "cursor");
    },
    { port: realCursorSidebar.brokerPort, portStorageKey: portKey, providerStorageKey: providerKey },
  );
  const browserRequests = [];
  page.on("request", (request) => browserRequests.push(request.url()));

  await page.goto(resource.url);
  await expect(page.locator(".agent-live-status")).toHaveText("Cursor ready");
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByRole("button", { name: "Connect to Cursor", exact: true }).click();
  await expect(page.locator(".agent-model-pill")).toContainText("Cursor Small", { timeout: 15_000 });

  const composer = page.getByLabel("Message Cursor about this whiteboard");
  await expect(composer).toBeEnabled({ timeout: 15_000 });
  await composer.fill("Summarize the supplied content using the scripted provider.");
  await composer.press("Enter");

  await expect(page.locator(".agent-tool-activity")).toContainText("Run fixture", { timeout: 15_000 });
  await expect(page.locator(".agent-tool-activity")).toHaveAttribute("data-status", "completed");
  await expect(page.locator(".agent-message-assistant")).toContainText("scripted answer");
  await expect(page.locator(".agent-live-status")).toHaveText("Connected", { timeout: 15_000 });

  await expect.poll(async () => (await realCursorSidebar.semanticEvidence()).some(({ method }) => method === "session/prompt.semantic")).toBe(true);
  const evidence = await realCursorSidebar.semanticEvidence();
  expect(evidence).toEqual(expect.arrayContaining([
    expect.objectContaining({ method: "session/new.semantic", workspace_identity: expect.stringMatching(/^[a-f0-9]{64}$/u), model_option: "cursor-small" }),
    expect.objectContaining({ method: "session/prompt.semantic", workspace_identity: expect.stringMatching(/^[a-f0-9]{64}$/u), content_block_types: ["text"], content_block_count: 1, provider_envelope_valid: true }),
  ]));
  const encodedEvidence = JSON.stringify(evidence);
  expect(encodedEvidence).not.toContain(markdown);
  expect(encodedEvidence).not.toContain(creatorContext);
  expect(encodedEvidence).not.toContain(resource.id);
  expect(encodedEvidence).not.toMatch(/sessionId|turn_id|message_id|image|creator/iu);

  const allowedOrigins = new Set([realCursorSidebar.origin, `http://127.0.0.1:${realCursorSidebar.brokerPort}`]);
  for (const requestURL of browserRequests) expect(allowedOrigins.has(new URL(requestURL).origin)).toBe(true);
});
