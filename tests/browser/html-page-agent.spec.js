import { expect, test } from "./fixture.js";

test.use({ browserRequestInterception: false, ignoreHTTPSErrors: true });

function parsedCommands(requests) {
  return requests
    .filter((request) => request.method === "POST" && ["/api/v1/agent/connect", "/api/v1/agent/commands"].includes(request.url) && typeof request.body === "string" && request.body !== "")
    .map((request) => JSON.parse(request.body));
}

async function openHTMLPage({ context, page, fixture, html, creatorContext, provider = "pi", viewport }) {
  const resource = await fixture.publishHTML(html, creatorContext);
  await context.grantPermissions(["local-network-access"], { origin: fixture.origin });
  await page.addInitScript(
    ({ port, selectedProvider }) => {
      localStorage.setItem("agent-whiteboard-agent-port", String(port));
      localStorage.setItem("agent-whiteboard-agent-provider", selectedProvider);
    },
    { port: fixture.brokerPort, selectedProvider: provider },
  );
  if (viewport) await page.setViewportSize(viewport);
  fixture.resetBrokerRequests();
  fixture.resetBrokerState();
  await page.goto(resource.url);
  await expect(page.locator(".agent-live-status")).toHaveText(`${provider === "codex" ? "Codex" : "Pi"} ready`);
  return resource;
}

async function connect(page, provider) {
  const launcher = page.getByRole("button", { name: "Open Page agent", exact: true });
  if (await launcher.isVisible()) await launcher.click();
  await page.getByRole("button", { name: `Connect to ${provider === "codex" ? "Codex" : "Pi"}`, exact: true }).click();
  await expect(page.locator(".agent-provider-label")).toBeVisible();
}

test("keeps exact HTML in an opaque child while shared trusted chrome owns consent, theme, and context", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  const marker = "OPAQUE_CHILD_MARKER_4f6d";
  const html = `<!doctype html><html><head><title>HTML agent board</title><style>body{background:rgb(20,30,40);color:white}</style></head><body><p>${marker}</p><script>parent.postMessage({type:"forged-agent-event",api_version:"4"},"*")</script></body></html>`;
  const creatorContext = "Exact HTML creator context.\n";
  const resource = await openHTMLPage({ context, page, fixture: localAgentSidebar, html, creatorContext, viewport: { width: 1280, height: 800 } });

  expect(localAgentSidebar.brokerRequests.map(({ method, url }) => ({ method, url }))).toEqual([{ method: "GET", url: "/api/v1/agent/status" }]);
  expect(JSON.stringify(localAgentSidebar.brokerRequests)).not.toContain(marker);
  expect(JSON.stringify(localAgentSidebar.brokerRequests)).not.toContain(creatorContext);

  const appBar = page.locator("#agent-whiteboard-app-bar");
  const frameElement = page.locator("#agent-whiteboard-html-content");
  await expect(appBar).toBeVisible();
  await expect(appBar).toContainText("HTML agent board");
  await expect(frameElement).toHaveCount(1);
  await expect(frameElement).toHaveAttribute("sandbox", "allow-scripts");
  await expect(frameElement).toHaveAttribute("referrerpolicy", "no-referrer");
  await expect(frameElement).toHaveAttribute("credentialless", "");
  await expect(page.frameLocator("#agent-whiteboard-html-content").getByText(marker)).toBeVisible();
  expect(await page.frameLocator("#agent-whiteboard-html-content").locator("body").evaluate(() => window.origin)).toBe("null");
  expect(await page.frameLocator("#agent-whiteboard-html-content").locator("body").evaluate(() => {
    try { return parent.document.body.textContent; } catch (error) { return error.name; }
  })).toBe("SecurityError");

  const theme = page.getByRole("button", { name: /Appearance:/u });
  await theme.click();
  await page.getByRole("menuitemradio", { name: /Dark/u }).click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  expect(await page.frameLocator("#agent-whiteboard-html-content").locator("body").evaluate((body) => getComputedStyle(body).backgroundColor)).toBe("rgb(20, 30, 40)");

  const frameBefore = await frameElement.boundingBox();
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await expect(page.locator(".agent-context-disclosure")).toContainText("HTML source + creator notes");
  await expect(page.locator(".agent-consent-list")).toContainText("Complete HTML source and creator notes");
  await page.locator(".agent-context-disclosure").click();
  await expect(page.locator(".agent-context-card").first()).toContainText("HTML source");
  await expect(page.locator(".agent-context-card").first().locator("pre")).toHaveText(html);
  await expect(page.locator(".agent-context-card").nth(1).locator("pre")).toHaveText(creatorContext);
  await expect(page.getByRole("button", { name: /Add selected text|Add section|Add image/u })).toHaveCount(0);
  const frameAfter = await frameElement.boundingBox();
  expect(frameAfter.width).toBeLessThan(frameBefore.width);

  await page.getByRole("button", { name: "Back to conversation", exact: true }).click();
  await page.getByRole("button", { name: "Connect to Pi", exact: true }).click();
  await expect(page.locator(".agent-provider-label")).toBeVisible();
  await expect.poll(() => parsedCommands(localAgentSidebar.brokerRequests).some(({ type }) => type === "connect")).toBe(true);
  const connectCommand = parsedCommands(localAgentSidebar.brokerRequests).find(({ type }) => type === "connect");
  expect(connectCommand.payload.resource.kind).toBe("html");
  expect(JSON.stringify(connectCommand)).not.toContain(marker);
  expect(JSON.stringify(connectCommand)).not.toContain(creatorContext);

  const composer = page.getByLabel("Message Pi about this whiteboard");
  await composer.fill("What is this page?");
  await composer.press("Enter");
  await expect.poll(() => parsedCommands(localAgentSidebar.brokerRequests).some(({ type }) => type === "submit")).toBe(true);
  const submit = parsedCommands(localAgentSidebar.brokerRequests).find(({ type }) => type === "submit");
  expect(submit.payload.context).toMatchObject({
    revision: "initial",
    source: html,
    creator_context: creatorContext,
    resource: { kind: "html", id: resource.id },
  });

  await page.setViewportSize({ width: 320, height: 700 });
  await expect(frameElement).toHaveCount(1);
  await expect(page.locator(".agent-drawer")).toHaveClass(/is-modal/u);
  await expect(page.locator(".agent-overlay")).toHaveClass(/is-open/u);
});

test("keeps sandbox inheritance credentialless when a hostile child self-navigates to the enabled wrapper", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await context.addCookies([{ name: "publishing_secret", value: "must-not-leak", url: localAgentSidebar.origin }]);
  const resource = await localAgentSidebar.publishHTML(`<!doctype html><html><head><title>Self navigation</title></head><body><script>
    setTimeout(() => { location.href = location.pathname.replace(/\\/content$/u, ""); }, 100);
  </script></body></html>`, "Self-navigation context.\n");
  await context.grantPermissions(["local-network-access"], { origin: localAgentSidebar.origin });
  await page.addInitScript((port) => localStorage.setItem("agent-whiteboard-agent-port", String(port)), localAgentSidebar.brokerPort);
  localAgentSidebar.publishingRequests.splice(0);
  localAgentSidebar.resetBrokerRequests();
  localAgentSidebar.resetBrokerState();

  await page.goto(resource.url);
  await expect(page.locator(".agent-live-status")).toHaveText("Pi ready");
  const wrapperPath = new URL(resource.url).pathname;
  await expect.poll(() => localAgentSidebar.publishingRequests.filter(({ url }) => url === wrapperPath).length).toBe(2);

  const wrapperRequests = localAgentSidebar.publishingRequests.filter(({ url }) => url === wrapperPath);
  expect(wrapperRequests[0].headers.cookie).toContain("publishing_secret=must-not-leak");
  expect(wrapperRequests[1].headers.cookie).toBeUndefined();
  expect(wrapperRequests[1].headers.referer).toBeUndefined();
  expect(wrapperRequests[1].headers.origin).not.toBe(localAgentSidebar.origin);
  await expect(page.locator("#agent-whiteboard-html-content")).toHaveCount(1);

  const trustedAccepted = localAgentSidebar.brokerRequests.filter((request) => request.headers.origin === localAgentSidebar.origin && request.status >= 200 && request.status < 300);
  expect(trustedAccepted.map(({ method, url }) => ({ method, url }))).toEqual([{ method: "GET", url: "/api/v1/agent/status" }]);
});

test("leaves HTML context pending through compact and sends it on a Codex skill-only first turn", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  const html = "<!doctype html><html><head><title>Compact first</title></head><body>exact compact source</body></html>";
  const creatorContext = "Compact-first HTML notes.\n";
  await openHTMLPage({ context, page, fixture: localAgentSidebar, html, creatorContext, provider: "codex" });
  await connect(page, "codex");
  const composer = page.getByLabel("Message Codex about this whiteboard");

  await composer.fill("/compact");
  await composer.press("Enter");
  await expect(page.locator(".agent-compaction-row")).toContainText("Compacting context…");
  const compact = parsedCommands(localAgentSidebar.brokerRequests).find(({ type }) => type === "compact");
  expect(compact).toBeDefined();
  expect(JSON.stringify(compact)).not.toContain("exact compact source");
  expect(JSON.stringify(compact)).not.toContain("Compact-first HTML notes");
  localAgentSidebar.completeCompact("completed", "codex");
  await expect(page.locator(".agent-compaction-row")).toContainText("Context compacted");

  await composer.fill("$rev");
  await composer.press("Enter");
  await expect(composer.locator(".agent-message-skill")).toHaveText("Skill: Review helper");
  await composer.press("Enter");
  await expect.poll(() => parsedCommands(localAgentSidebar.brokerRequests).some(({ type }) => type === "submit")).toBe(true);
  const submit = parsedCommands(localAgentSidebar.brokerRequests).find(({ type }) => type === "submit");
  expect(submit.payload.content.parts).toEqual([{ type: "skill", skill: { id: expect.any(String), name: "review-helper" } }]);
  expect(submit.payload.context).toMatchObject({
    revision: "initial",
    source: html,
    creator_context: creatorContext,
    resource: { kind: "html" },
  });
});

test("shares Codex settings, busy draft, activity, interactions, provider isolation, archives, and confirmation focus", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await openHTMLPage({
    context,
    page,
    fixture: localAgentSidebar,
    html: "<!doctype html><html><head><title>Codex parity</title></head><body>Codex host parity</body></html>",
    creatorContext: "Codex parity creator notes.\n",
  });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("codex");
  await expect(page.locator(".agent-live-status")).toHaveText("Codex ready");
  localAgentSidebar.setHoldResponses(true, "codex");
  await page.getByRole("button", { name: "Connect to Codex", exact: true }).click();
  await expect(page.locator(".agent-provider-label")).toBeVisible();

  const pill = page.locator(".agent-model-pill");
  const menu = page.locator(".agent-model-menu");
  await pill.click();
  await menu.locator('[data-settings-section="effort"]').click();
  await menu.locator('[data-settings-value="xhigh"]').click();
  await menu.locator('[data-settings-section="speed"]').click();
  await menu.locator('[data-settings-value="standard"]').click();
  const composer = page.getByLabel("Message Codex about this whiteboard");
  await composer.fill("Codex active turn.");
  await composer.press("Enter");
  await expect(page.locator(".agent-live-status")).toHaveText("Responding");
  await composer.fill("Preserved Codex busy draft.");
  await composer.press("Enter");
  expect(parsedCommands(localAgentSidebar.brokerRequests).filter(({ type }) => type === "submit")).toHaveLength(1);
  await expect(composer).toHaveText("Preserved Codex busy draft.");
  const submit = parsedCommands(localAgentSidebar.brokerRequests).find(({ type }) => type === "submit");
  expect(submit.payload.settings).toEqual({ model: "gpt-5.6-sol", effort: "xhigh", speed: "standard" });

  localAgentSidebar.emitToolActivity("codex", {
    turn_id: submit.payload.turn_id,
    activity_id: "VVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVV",
    kind: "command",
    status: "running",
    title: "Run focused verification",
    summary: "Browser checks are running.",
    detail: "pnpm test",
  });
  await expect(page.locator(".agent-tool-activity")).toContainText("Browser checks are running.");
  localAgentSidebar.emitInteraction("codex", {
    kind: "command_approval",
    title: "Run command",
    summary: "Approve the focused browser command.",
    command: "pnpm test",
    working_directory: "/workspace/agent-whiteboard",
    options: [
      { id: "accept", label: "Accept", description: "Run once." },
      { id: "decline", label: "Decline", description: "Do not run it." },
    ],
  });
  const interaction = page.locator('.agent-interaction[data-kind="command_approval"]');
  await expect(interaction).toBeVisible();
  await interaction.getByRole("button", { name: "Decline", exact: true }).click();
  await expect(interaction).toHaveAttribute("data-state", "resolved");

  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await expect(page.locator(".agent-live-status")).toHaveText("Connected");
  localAgentSidebar.setHoldResponses(false, "codex");

  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "New conversation" }).click();
  const confirmation = page.getByRole("dialog");
  await expect(confirmation).toBeVisible();
  const cancelNew = confirmation.getByRole("button", { name: "Cancel", exact: true });
  await expect(cancelNew).toBeFocused();
  await cancelNew.press("Shift+Tab");
  await expect(confirmation.getByRole("button", { name: "Start new", exact: true })).toBeFocused();
  await confirmation.press("Escape");
  await expect(page.locator(".agent-drawer")).toHaveClass(/is-open/u);

  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "Archives" }).click();
  const archive = page.locator(".agent-archive-card");
  await expect(archive).toBeVisible();
  await archive.getByRole("button", { name: "Restore" }).click();
  await expect(confirmation).toBeVisible();
  await confirmation.getByRole("button", { name: "Cancel", exact: true }).click();
  await archive.getByRole("button", { name: "Delete" }).click();
  await expect(confirmation).toBeVisible();
  await confirmation.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByRole("button", { name: "Back to conversation" }).click();

  await page.getByLabel("Conversation provider").selectOption("pi");
  await expect(page.locator(".agent-live-status")).toHaveText("Pi ready");
  await expect(page.locator(".agent-message-user")).toHaveCount(0);
  await page.getByLabel("Conversation provider").selectOption("codex");
  await expect(page.locator(".agent-message-user")).toContainText("Codex active turn.");

  await page.reload();
  await expect(page.locator(".agent-live-status")).toHaveText("Pi ready");
  await page.getByLabel("Conversation provider").selectOption("codex");
  await expect(page.locator(".agent-live-status")).toHaveText("Codex ready");
  await connect(page, "codex");
  await expect(page.locator(".agent-message-user")).toContainText("Codex active turn.");
});

test("shares ordinary images plus Pi queue edit, remove, and Stop behavior without disturbing the HTML child", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  const marker = "PI_QUEUE_CHILD_STAYS_VISIBLE";
  await openHTMLPage({
    context,
    page,
    fixture: localAgentSidebar,
    html: `<!doctype html><html><head><title>Pi parity</title></head><body>${marker}</body></html>`,
    creatorContext: "Pi queue and image parity notes.\n",
  });
  localAgentSidebar.setHoldResponses(true, "pi");
  await connect(page, "pi");

  const picker = page.locator(".agent-image-picker");
  await picker.setInputFiles([
    { name: "diagram.png", mimeType: "image/png", buffer: Buffer.from([137, 80, 78, 71]) },
    { name: "photo.jpg", mimeType: "image/jpeg", buffer: Buffer.from([255, 216, 255, 217]) },
  ]);
  await expect(page.locator('.agent-attachment-preview[data-state="ready"]')).toHaveCount(2);
  await page.locator('.agent-composer button[type="submit"]').click();
  await expect(page.locator(".agent-message-user .agent-message-images img")).toHaveCount(2);

  const composer = page.getByLabel("Message Pi about this whiteboard");
  await composer.fill("Queued follow-up.");
  await composer.press("Enter");
  const queued = page.getByLabel("Edit queued message");
  await expect(queued).toHaveText("Queued follow-up.");
  await queued.fill("Edited follow-up.");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByLabel("Edit queued message")).toHaveText("Edited follow-up.");
  await page.getByRole("button", { name: "Remove", exact: true }).click();
  await expect(page.getByLabel("Edit queued message")).toHaveCount(0);
  await composer.focus();
  await page.keyboard.press("Escape");
  await expect(page.locator(".agent-activity-interruption")).toContainText("Response stopped");
  await expect(page.frameLocator("#agent-whiteboard-html-content").getByText(marker)).toBeVisible();

  const commands = parsedCommands(localAgentSidebar.brokerRequests);
  expect(commands.map(({ type }) => type)).toEqual(expect.arrayContaining(["submit", "queue_edit", "queue_remove", "interrupt"]));
  expect(commands.find(({ type }) => type === "submit").payload).toMatchObject({ content: { parts: [] }, images: [{ name: "diagram.png" }, { name: "photo.jpg" }] });
});

test("uses HTTP fallback and sends an exact replacement once after a real HTML update and reconnect", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  const firstHTML = "<!doctype html><html><head><title>First revision</title></head><body>first exact source</body></html>";
  const firstContext = "First exact creator notes.\n";
  const resource = await openHTMLPage({ context, page, fixture: localAgentSidebar, html: firstHTML, creatorContext: firstContext });
  localAgentSidebar.setWebSocketEnabled(false);
  await connect(page, "pi");
  const composer = page.getByLabel("Message Pi about this whiteboard");
  await composer.fill("Accept the first revision.");
  await composer.press("Enter");
  await expect(page.locator(".agent-message-assistant")).toContainText(/fixture reply/iu);
  const firstSubmit = parsedCommands(localAgentSidebar.brokerRequests).find(({ type }) => type === "submit");
  expect(firstSubmit.payload.context).toMatchObject({ revision: "initial", source: firstHTML, creator_context: firstContext });
  expect(localAgentSidebar.brokerRequests.some((request) => request.status === 503 && request.url === "/api/v1/agent/connect")).toBe(true);
  expect(localAgentSidebar.brokerRequests.some((request) => request.status === 200 && request.method === "POST" && request.url === "/api/v1/agent/connect")).toBe(true);

  const secondHTML = "<!doctype html><html><head><title>Second revision</title></head><body>second exact source</body></html>";
  const secondContext = "Second exact creator notes.\n";
  await localAgentSidebar.updateHTML(resource.id, secondHTML, secondContext);
  localAgentSidebar.setContextState("pending", "pi");
  localAgentSidebar.resetBrokerRequests();
  await page.reload();
  await expect(page.locator(".agent-live-status")).toHaveText("Pi ready");
  await connect(page, "pi");
  await composer.fill("Accept the replacement.");
  await expect(page.locator('.agent-composer button[type="submit"]')).toBeEnabled();
  await composer.press("Enter");
  await expect.poll(() => parsedCommands(localAgentSidebar.brokerRequests).some(({ type }) => type === "submit")).toBe(true);
  const replacement = parsedCommands(localAgentSidebar.brokerRequests).find(({ type }) => type === "submit");
  expect(replacement.payload.context).toMatchObject({ revision: "replacement", source: secondHTML, creator_context: secondContext });
  expect(parsedCommands(localAgentSidebar.brokerRequests).filter((command) => command.type === "submit" && Object.hasOwn(command.payload, "context"))).toHaveLength(1);
  await expect(page.frameLocator("#agent-whiteboard-html-content").getByText("second exact source")).toBeVisible();
});

test("preserves the server-rendered iframe and performs no broker probe when agent bootstrap rejects the payload", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  const marker = "BOOTSTRAP_FAILURE_CHILD_SURVIVES";
  const resource = await localAgentSidebar.publishHTML(`<!doctype html><html><head><title>Failure isolation</title></head><body>${marker}</body></html>`, "Failure context.\n");
  await context.grantPermissions(["local-network-access"], { origin: localAgentSidebar.origin });
  localAgentSidebar.resetBrokerRequests();
  await page.route(resource.url, async (route) => {
    const response = await route.fetch();
    const body = (await response.text()).replace('"kind":"html"', '"kind":"invalid"');
    await route.fulfill({ response, body });
  });

  await page.goto(resource.url);

  await expect(page.locator("#agent-whiteboard-html-content")).toHaveCount(1);
  await expect(page.frameLocator("#agent-whiteboard-html-content").getByText(marker)).toBeVisible();
  await expect(page.locator("body")).toHaveAttribute("data-agent-bootstrap", "failed");
  await expect(page.locator("#agent-whiteboard-agent-drawer")).toHaveCount(0);
  expect(localAgentSidebar.brokerRequests).toEqual([]);
});
