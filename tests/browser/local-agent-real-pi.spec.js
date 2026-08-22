import { expect, test } from "./fixture.js";

const portKey = "agent-whiteboard-agent-port";
const nativeEnvelopeHeader = "agent-whiteboard-turn-v4\n";
const nativeEnvelopeFooter = "end-agent-whiteboard-turn-v4\n";

function parseProviderEnvelope(text) {
  const start = text.indexOf(nativeEnvelopeHeader);
  expect(start).toBeGreaterThanOrEqual(0);
  let envelope = text.slice(start);
  if (!envelope.endsWith("\n")) envelope += "\n";
  const labels = [
    "revision", "turn-id", "message-id", "application-instructions", "page-title-untrusted", "page-url-untrusted",
    "resource-kind-untrusted", "resource-id-untrusted", "resource-created-at-untrusted", "resource-updated-at-untrusted",
    "resource-expires-at-untrusted", "creator-context-untrusted", "page-source-untrusted", "reader-content-untrusted",
  ];
  let cursor = nativeEnvelopeHeader.length;
  const fields = {};
  for (const label of labels) {
    const prefix = `${label} `;
    expect(envelope.slice(cursor, cursor + prefix.length)).toBe(prefix);
    cursor += prefix.length;
    const end = envelope.indexOf("\n", cursor);
    const length = Number(envelope.slice(cursor, end));
    expect(Number.isSafeInteger(length)).toBe(true);
    cursor = end + 1;
    fields[label] = envelope.slice(cursor, cursor + length);
    cursor += length;
    expect(envelope[cursor]).toBe("\n");
    cursor += 1;
  }
  expect(envelope.slice(cursor)).toBe(nativeEnvelopeFooter);
  return { fields, envelope };
}

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

test("streams exact HTML context through the real publishing server, broker, and pinned Pi", async ({
  context,
  page,
  realAgentSidebar,
}) => {
  test.setTimeout(30_000);
  realAgentSidebar.resetModelRequests();
  const html = "<!doctype html><html><head><title>Real HTML agent path</title><style>body{font-family:system-ui;background:#f5efe3;color:#17324d}main{margin:3rem;padding:2rem;background:white;border-radius:1rem}</style></head><body><main><h1>Published dashboard</h1><p>Exact browser-to-provider HTML.</p></main></body></html>";
  const creatorContext = "Exact HTML browser-to-provider creator context.\n";
  const resource = await realAgentSidebar.publishHTML(html, creatorContext);
  await context.grantPermissions(["local-network-access"], { origin: realAgentSidebar.origin });
  await page.addInitScript(
    ({ key, port }) => localStorage.setItem(key, String(port)),
    { key: portKey, port: realAgentSidebar.brokerPort },
  );

  await page.goto(resource.url);
  await expect(page.locator(".agent-live-status")).toHaveText("Pi ready");
  await expect(page.frameLocator("#agent-whiteboard-html-content").getByText("Published dashboard")).toBeVisible();
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await expect(page.getByRole("button", { name: /Add selected text|Add section|Add image/u })).toHaveCount(0);
  await page.getByRole("button", { name: "Connect to Pi", exact: true }).click();
  const composer = page.getByLabel("Message Pi about this whiteboard");
  await expect(composer).toBeEnabled({ timeout: 15_000 });

  const pill = page.locator(".agent-model-pill");
  await pill.click();
  const menu = page.locator(".agent-model-menu");
  await expect(menu.locator('[data-settings-section="speed"]')).toHaveCount(0);
  await menu.locator('[data-settings-section="model"]').click();
  await expect(menu.locator('[data-settings-value]')).toHaveCount(2);
  await menu.locator('[data-settings-value="agent-whiteboard-browser/agent-whiteboard-browser"]').click();
  await menu.locator('[data-settings-section="effort"]').click();
  await menu.locator('[data-settings-value="high"]').click();

  await composer.fill("$");
  const suggestions = page.getByRole("listbox", { name: "Composer suggestions" });
  await expect(suggestions).toContainText("whiteboard-review", { timeout: 15_000 });
  await composer.fill("$white");
  await composer.press("Enter");
  await expect(composer.locator(".agent-message-skill")).toHaveText("Skill: whiteboard-review");
  await composer.press("End");
  await composer.pressSequentially(" M3_REAL_PI_INTERACTION Describe the published HTML dashboard.");
  await composer.press("Enter");
  const interaction = page.locator('.agent-interaction[data-kind="mcp_elicitation"]');
  await expect(interaction).toBeVisible({ timeout: 15_000 });
  const multiline = interaction.getByLabel("Real Pi multiline response");
  await expect(multiline).toHaveAttribute("rows");
  await multiline.fill("First line\nSecond line");
  await interaction.getByRole("button", { name: "Submit", exact: true }).click();
  await expect(interaction).toHaveAttribute("data-state", "resolved");
  await expect(page.locator(".agent-status-notice").filter({ hasText: "Real Pi passive notice" })).toHaveCount(1);
  await expect(page.locator(".agent-status-notice").filter({ hasText: "Real Pi multiline exact" })).toHaveCount(1);
  await expect(page.locator(".agent-status-notice").filter({ hasText: "First line" })).toHaveCount(0);
  await expect(page.locator(".agent-status-notice").filter({ hasText: "Second line" })).toHaveCount(0);
  await expect(multiline).toHaveValue("");
  await expect(interaction).not.toContainText("First line");
  await expect(interaction).not.toContainText("Second line");
  await expect(page.locator(".agent-drawer")).not.toContainText("Real Pi multiline mismatch");
  await expect(page.locator(".agent-drawer")).not.toContainText("private status must stay hidden");
  await expect(page.locator(".agent-response-loading")).toHaveAccessibleName("Pi is responding", { timeout: 15_000 });

  await realAgentSidebar.releaseModelFirstDelta();
  await expect(page.locator(".agent-message-assistant")).toContainText("Real Pi fixture");
  await realAgentSidebar.releaseModelLaterDelta();
  await realAgentSidebar.releaseModelCompletion();
  await expect(page.locator(".agent-live-status")).toHaveText("Connected", { timeout: 15_000 });
  await expect.poll(() => realAgentSidebar.modelRequests.length).toBe(1);

  const modelRequest = realAgentSidebar.modelRequests[0];
  expect(modelRequest.method).toBe("POST");
  expect(modelRequest.url).toBe("/v1/chat/completions");
  const modelBody = JSON.parse(modelRequest.body);
  expect(modelBody.model).toBe("agent-whiteboard-browser");
  expect(modelBody.reasoning_effort).toBe("high");
  const providerContent = modelBody.messages.at(-1).content[0].text;
  expect(providerContent).toContain("REAL_PI_SKILL_FRAME");
  const parsed = parseProviderEnvelope(providerContent);
  expect(parsed.fields["resource-kind-untrusted"]).toBe("html");
  expect(parsed.fields["page-source-untrusted"]).toBe(html);
  expect(parsed.fields["creator-context-untrusted"]).toBe(creatorContext);
  expect(JSON.parse(parsed.fields["reader-content-untrusted"])).toEqual({ parts: [
    { type: "skill", skill: { id: expect.any(String), name: "whiteboard-review" } },
    { type: "text", text: "  M3_REAL_PI_INTERACTION Describe the published HTML dashboard." },
  ] });
  expect(providerContent.endsWith(nativeEnvelopeFooter.trimEnd())).toBe(true);
  expect(`${parsed.envelope}`).toContain(nativeEnvelopeFooter);
  expect(modelRequest.body).not.toContain("browser-placeholder-key");

  await composer.fill("/compact");
  await composer.press("Enter");
  await expect.poll(() => realAgentSidebar.modelRequests.length).toBe(2);
  const compactRow = page.locator(".agent-compaction-row");
  await expect(compactRow).toContainText("Compacting context…", { timeout: 15_000 });
  await expect(page.locator(".agent-live-status")).toHaveText("Compacting");
  await expect(page.locator(".agent-drawer")).not.toContainText("The provider returned a malformed event stream.");

  await realAgentSidebar.releaseCompactionSummary();
  await expect(compactRow).toContainText("Context compacted", { timeout: 15_000 });
  await expect(page.locator(".agent-live-status")).toHaveText("Connected");
  await expect(page.locator(".agent-drawer")).not.toContainText("The provider returned a malformed event stream.");
  await expect(page.frameLocator("#agent-whiteboard-html-content").getByText("Published dashboard")).toBeVisible();
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
