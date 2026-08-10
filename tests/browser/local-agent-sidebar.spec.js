import { expect, test } from "./fixture.js";

const drawerKey = "agent-whiteboard-agent-drawer-open";
const portKey = "agent-whiteboard-agent-port";
const widthKey = "agent-whiteboard-agent-drawer-width";

test.use({ browserRequestInterception: false, ignoreHTTPSErrors: true });

async function openSidebarPage({ context, page, fixture, markdown, creatorContext, preferences = {} }) {
  const resource = await fixture.publish(markdown, creatorContext);
  await context.grantPermissions(["local-network-access"], { origin: fixture.origin });
  await page.addInitScript(
    ({ port, preferences: initialPreferences }) => {
      localStorage.setItem("agent-whiteboard-agent-port", String(port));
      for (const [key, value] of Object.entries(initialPreferences)) localStorage.setItem(key, String(value));
    },
    { port: fixture.brokerPort, preferences },
  );
  fixture.resetBrokerRequests();
  fixture.resetBrokerState();
  await page.goto(resource.url);
  await expect(page.locator(".agent-live-status")).toHaveText("Pi ready");
  return resource;
}

async function connectSidebar(page, provider = "pi") {
  const providerName = provider === "codex" ? "Codex" : "Pi";
  const model = provider === "codex" ? "fixture-codex-model" : "fixture-model";
  const launcher = page.getByRole("button", { name: "Open Page agent", exact: true });
  if (await launcher.isVisible()) await launcher.click();
  await page.getByRole("button", { name: `Connect to ${providerName}`, exact: true }).click();
  await expect(page.locator(".agent-provider-label")).toContainText(model);
  await expect(page.locator('.agent-composer button[type="submit"]')).toBeDisabled();
}

function parsedCommands(requests) {
  return requests
    .filter((request) => request.method === "POST" && ["/api/v1/agent/connect", "/api/v1/agent/commands"].includes(request.url) && typeof request.body === "string" && request.body !== "")
    .map((request) => JSON.parse(request.body));
}

test("keeps page context behind explicit consent and uses the HTTP fallback", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  const markdown = "# Sidebar authority proof\n\nExact page Markdown.\n";
  const creatorContext = "Exact creator context.\n";
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown, creatorContext });

  expect(localAgentSidebar.brokerRequests.map(({ method, url }) => ({ method, url }))).toEqual([
    { method: "GET", url: "/api/v1/agent/status" },
  ]);
  expect(JSON.stringify(localAgentSidebar.brokerRequests)).not.toContain(markdown);
  expect(JSON.stringify(localAgentSidebar.brokerRequests)).not.toContain(creatorContext);

  await page.getByRole("button", { name: "Open Page agent" }).click();
  await expect(page.locator(".agent-drawer-header")).toContainText("Pi ready");
  await expect(page.locator(".agent-status-bar")).toHaveCount(0);
  await expect(page.locator(".agent-provider-label")).toBeHidden();
  await expect(page.locator(".agent-consent")).toContainText("sends no page content");
  await expect(page.locator(".agent-consent-list")).toContainText("Complete Markdown and creator notes");
  await expect(page.locator(".agent-context-disclosure")).toContainText("Markdown + creator notes");
  await expect(page.locator(".agent-context-disclosure")).not.toContainText(/shared|uncertain/iu);
  await page.getByRole("button", { name: "Connect to Pi", exact: true }).click();

  await expect(page.locator(".agent-provider-label")).toContainText("fixture-model");
  await expect(page.locator('.agent-composer button[type="submit"]')).toBeDisabled();
  const connect = parsedCommands(localAgentSidebar.brokerRequests).find((command) => command.type === "connect");
  expect(connect).toBeDefined();
  expect(connect.payload).not.toHaveProperty("context");
  expect(JSON.stringify(connect)).not.toContain(markdown);
  expect(JSON.stringify(connect)).not.toContain(creatorContext);
  expect(localAgentSidebar.brokerRequests.some((request) => request.status === 503 && request.url === "/api/v1/agent/connect")).toBe(true);
  expect(localAgentSidebar.brokerRequests.some((request) => request.status === 200 && request.method === "POST" && request.url === "/api/v1/agent/connect")).toBe(true);
});

test("uses the versioned WebSocket for connect and subsequent commands", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# WebSocket sidebar\n", creatorContext: "WebSocket context.\n" });
  localAgentSidebar.setWebSocketEnabled(true);
  await connectSidebar(page);
  const priorReplies = await page.locator(".agent-message-assistant").count();
  await page.getByLabel("Message Pi about this whiteboard").fill("Use the socket.");
  await page.getByLabel("Message Pi about this whiteboard").press("Enter");
  await expect(page.locator(".agent-message-assistant")).toHaveCount(priorReplies + 1);

  expect(localAgentSidebar.brokerRequests.some((request) => request.status === 101)).toBe(true);
  expect(localAgentSidebar.brokerRequests.some((request) => request.method === "POST" && request.url === "/api/v1/agent/connect")).toBe(false);
  expect(localAgentSidebar.brokerRequests.some((request) => request.method === "POST" && request.url === "/api/v1/agent/commands")).toBe(false);
  expect(localAgentSidebar.webSocketCommands.map((command) => command.type)).toEqual(["connect", "history_page", "submit"]);
  expect(localAgentSidebar.webSocketCommands[0].payload).not.toHaveProperty("context");
});

test("docks without covering the page and persists pointer and keyboard widths", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await openSidebarPage({
    context,
    page,
    fixture: localAgentSidebar,
    markdown: "# Resizable workspace\n\n| Column | Value |\n| --- | --- |\n| wide | `abcdefghijklmnopqrstuvwxyz` |\n\n```text\nA deliberately long line that must remain inside the available whiteboard region without page-level overflow.\n```\n",
    creatorContext: "Layout context.\n",
  });

  const launcher = page.getByRole("button", { name: "Open Page agent", exact: true });
  await launcher.click();
  const drawer = page.locator(".agent-drawer");
  const content = page.locator("#agent-whiteboard-content");
  await expect(drawer).toHaveAttribute("role", "complementary");
  await expect(page.locator(".agent-drawer-separator")).toBeVisible();
  await expect(launcher).toBeHidden();
  await expect.poll(async () => Math.round((await drawer.boundingBox()).x)).toBe(1020);

  const [drawerBox, contentBox] = await Promise.all([drawer.boundingBox(), content.boundingBox()]);
  expect(contentBox.x + contentBox.width).toBeLessThanOrEqual(drawerBox.x + 1);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);

  const separator = page.locator(".agent-drawer-separator");
  const separatorBox = await separator.boundingBox();
  await page.mouse.move(separatorBox.x + separatorBox.width / 2, separatorBox.y + 80);
  await page.mouse.down();
  await page.mouse.move(separatorBox.x - 96, separatorBox.y + 120);
  await page.mouse.up();
  const pointerWidth = Number(await page.evaluate((key) => localStorage.getItem(key), widthKey));
  expect(pointerWidth).toBeGreaterThan(420);
  await expect(drawer).toHaveCSS("width", `${pointerWidth}px`);

  await page.reload();
  await expect(drawer).toHaveClass(/is-open/u);
  await expect(drawer).toHaveCSS("width", `${pointerWidth}px`);
  await separator.focus();
  await page.keyboard.press("ArrowLeft");
  expect(Number(await page.evaluate((key) => localStorage.getItem(key), widthKey))).toBe(pointerWidth + 8);
  await page.keyboard.press("Shift+ArrowRight");
  expect(Number(await page.evaluate((key) => localStorage.getItem(key), widthKey))).toBe(pointerWidth - 24);
  await page.keyboard.press("Home");
  expect(await page.evaluate((key) => localStorage.getItem(key), widthKey)).toBe("420");
});

test("keeps the ChatGPT-like header, transcript, context, and composer in stable regions", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await page.setViewportSize({ width: 1200, height: 700 });
  await openSidebarPage({
    context,
    page,
    fixture: localAgentSidebar,
    markdown: "# Chat shell\n\nThe context summary belongs before the conversation.\n",
    creatorContext: "Chat shell context.\n",
  });
  await connectSidebar(page);

  const timeline = page.locator(".agent-timeline");
  const contextSummary = timeline.locator(":scope > .agent-context-summary");
  await expect(contextSummary).toHaveCount(1);
  await expect(timeline.locator(":scope > *").first()).toHaveClass(/agent-context-summary/u);
  await expect(contextSummary).toContainText("Chat shell");

  const overflowBox = await page.locator(".agent-overflow-button").boundingBox();
  const closeBox = await page.getByRole("button", { name: "Close local agent", exact: true }).boundingBox();
  expect(overflowBox.x + overflowBox.width).toBeLessThanOrEqual(closeBox.x);

  const longMessage = Array.from({ length: 32 }, (_, index) => `Paragraph ${index + 1} keeps the transcript tall enough to scroll independently.`).join("\n\n");
  await page.getByLabel("Message Pi about this whiteboard").fill(longMessage);
  await page.locator('.agent-composer button[type="submit"]').click();
  await expect(page.locator(".agent-message-user")).toContainText("Paragraph 32");
  await expect.poll(() => timeline.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true);

  localAgentSidebar.setPhaseResponses(true);
  await page.getByLabel("Message Pi about this whiteboard").fill("Keep my reading position while this response streams.");
  await page.getByLabel("Message Pi about this whiteboard").press("Enter");
  await expect(page.locator(".agent-response-loading")).toBeVisible();

  const composer = page.locator(".agent-composer-wrap");
  const before = await composer.boundingBox();
  const readingPosition = await timeline.evaluate((element) => {
    element.scrollTop = Math.floor((element.scrollHeight - element.clientHeight) / 2);
    return element.scrollTop;
  });
  localAgentSidebar.releaseResponsePhase("first_delta");
  await expect(page.locator(".agent-message-assistant").last()).toContainText("Fixture");
  await expect.poll(() => timeline.evaluate((element) => element.scrollTop)).toBe(readingPosition);
  localAgentSidebar.releaseResponsePhase("later_delta");
  localAgentSidebar.releaseResponsePhase("completion");
  await expect(page.locator(".agent-live-status")).toHaveText("Connected");
  await expect.poll(() => timeline.evaluate((element) => element.scrollTop)).toBe(readingPosition);
  const after = await composer.boundingBox();
  expect(Math.round(after.y + after.height)).toBe(Math.round(before.y + before.height));

  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await expect(page.getByRole("menuitem", { name: "New conversation" })).toBeFocused();
  await page.keyboard.press("End");
  await expect(page.getByRole("menuitem", { name: "Inspect page context" })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("button", { name: "Back to conversation" })).toBeFocused();
  await page.getByRole("button", { name: "Back to conversation" }).click();
  await expect(page.getByLabel("Message Pi about this whiteboard")).toBeFocused();
  await expect.poll(() => timeline.evaluate((element) => element.scrollTop)).toBe(readingPosition);

  await expect(page.locator(".agent-drawer")).toHaveCSS("overflow", "hidden");
  await expect(timeline).toHaveCSS("overflow-y", "auto");
  const drawerBox = await page.locator(".agent-drawer").boundingBox();
  expect(after.y + after.height).toBeLessThanOrEqual(drawerBox.y + drawerBox.height + 1);
});

test("temporarily clamps a wider preference and switches every narrow layout to a modal", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await page.setViewportSize({ width: 1100, height: 800 });
  await openSidebarPage({
    context,
    page,
    fixture: localAgentSidebar,
    markdown: "# Responsive pane\n",
    creatorContext: "Responsive context.\n",
    preferences: { [widthKey]: 700 },
  });
  const launcher = page.getByRole("button", { name: "Open Page agent", exact: true });
  await launcher.click();
  await expect(page.locator(".agent-drawer")).toHaveCSS("width", "605px");
  expect(await page.evaluate((key) => localStorage.getItem(key), widthKey)).toBe("700");

  await page.setViewportSize({ width: 800, height: 800 });
  const drawer = page.locator(".agent-drawer");
  await expect(drawer).toHaveAttribute("role", "dialog");
  await expect(drawer).toHaveAttribute("aria-modal", "true");
  await expect(page.locator(".agent-overlay")).toBeVisible();
  await expect(page.locator(".agent-drawer-separator")).toBeHidden();
  await expect(page.locator("body")).toHaveCSS("overflow", "hidden");
  expect((await drawer.boundingBox()).width).toBeLessThan(800);

  await page.setViewportSize({ width: 600, height: 800 });
  expect(Math.round((await drawer.boundingBox()).width)).toBe(600);
  await page.setViewportSize({ width: 1200, height: 800 });
  await expect(drawer).toHaveAttribute("role", "complementary");
  await expect(page.locator(".agent-overlay")).toBeHidden();
  await expect(page.locator(".agent-drawer-separator")).toBeVisible();

  await page.setViewportSize({ width: 800, height: 800 });
  await page.keyboard.press("Escape");
  await expect(drawer).not.toHaveClass(/is-open/u);
  await expect(launcher).toBeFocused();
});

test("aligns the mobile viewer controls on one inset toolbar line", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await page.setViewportSize({ width: 390, height: 800 });
  await openSidebarPage({
    context,
    page,
    fixture: localAgentSidebar,
    markdown: "# Mobile toolbar\n",
    creatorContext: "Mobile toolbar context.\n",
  });

  const [themeControl, agentLauncher] = await Promise.all([
    page.getByRole("button", { name: /^Appearance:/u }).boundingBox(),
    page.getByRole("button", { name: "Open Page agent", exact: true }).boundingBox(),
  ]);
  const themeCenter = themeControl.y + themeControl.height / 2;
  const agentCenter = agentLauncher.y + agentLauncher.height / 2;
  const themeInset = themeControl.x;
  const agentInset = 390 - agentLauncher.x - agentLauncher.width;

  expect(Math.abs(themeCenter - agentCenter)).toBeLessThanOrEqual(1);
  expect(Math.abs(themeInset - agentInset)).toBeLessThanOrEqual(1);
});

test("keeps queue submission and Stop available with Enter and Shift+Enter", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Queue controls\n", creatorContext: "Queue context.\n" });
  localAgentSidebar.setHoldResponses(true);
  await connectSidebar(page);

  const composer = page.getByLabel("Message Pi about this whiteboard");
  await composer.fill("Line one");
  await composer.press("Shift+Enter");
  await expect(composer).toHaveValue("Line one\n");
  await composer.fill("Keep this turn active.");
  await composer.press("Enter");
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeEnabled();
  await composer.fill("Queued follow-up.");
  await composer.press("Enter");
  const queued = page.getByLabel("Edit queued message");
  await expect(queued).toHaveValue("Queued follow-up.");
  await queued.fill("Edited follow-up.");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByLabel("Edit queued message")).toHaveValue("Edited follow-up.");
  await page.getByRole("button", { name: "Remove", exact: true }).click();
  await expect(page.getByLabel("Edit queued message")).toHaveCount(0);
  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await expect(page.locator(".agent-activity-interruption")).toContainText("not replayed automatically");

  const types = parsedCommands(localAgentSidebar.brokerRequests).map((command) => command.type);
  expect(types).toEqual(expect.arrayContaining(["submit", "queue_edit", "queue_remove", "interrupt"]));
  expect(types.filter((type) => type === "submit")).toHaveLength(2);
});

test("picks and pastes multiple images with previews, retry, queue, and styled focus", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Image composer\n", creatorContext: "Image context.\n" });
  localAgentSidebar.setHoldResponses(true);
  await connectSidebar(page);

  const composer = page.getByLabel("Message Pi about this whiteboard");
  await composer.focus();
  expect(await composer.evaluate((element) => getComputedStyle(element).outlineStyle)).toBe("none");

  const picker = page.locator(".agent-image-picker");
  await picker.setInputFiles([
    { name: "diagram.png", mimeType: "image/png", buffer: Buffer.from([137, 80, 78, 71]) },
    { name: "photo.jpg", mimeType: "image/jpeg", buffer: Buffer.from([255, 216, 255, 217]) },
  ]);
  await expect(page.locator('.agent-attachment-preview[data-state="ready"]')).toHaveCount(2);
  await expect(page.locator('.agent-composer button[type="submit"]')).toBeEnabled();
  await page.locator('.agent-composer button[type="submit"]').click();
  await expect(page.locator(".agent-message-user .agent-message-images img")).toHaveCount(2);
  await expect(page.locator(".agent-attachment-preview")).toHaveCount(0);

  await composer.fill("Queued with a pasted image.");
  await composer.evaluate((element) => {
    const clipboard = new DataTransfer();
    clipboard.items.add(new File([new Uint8Array([82, 73, 70, 70])], "paste.webp", { type: "image/webp" }));
    element.dispatchEvent(new ClipboardEvent("paste", { bubbles: true, cancelable: true, clipboardData: clipboard }));
  });
  await expect(composer).toHaveValue("Queued with a pasted image.");
  await expect(page.locator('.agent-attachment-preview[data-state="ready"]')).toHaveCount(1);
  await composer.press("Enter");
  await expect(page.getByLabel("Edit queued message")).toHaveValue("Queued with a pasted image.");
  await expect(page.locator(".agent-queue .agent-message-images img")).toHaveCount(1);

  localAgentSidebar.failNextImageUpload();
  await picker.setInputFiles({ name: "retry.png", mimeType: "image/png", buffer: Buffer.from([137, 80, 78, 71]) });
  const failed = page.locator('.agent-attachment-preview[data-state="failed"]');
  await expect(failed).toContainText("could not be stored safely");
  await failed.getByRole("button", { name: "Retry retry.png" }).click();
  await expect(page.locator('.agent-attachment-preview[data-state="ready"]')).toHaveCount(1);
  await page.getByRole("button", { name: "Remove retry.png" }).click();
  await expect(page.locator(".agent-attachment-preview")).toHaveCount(0);
  await expect.poll(() => localAgentSidebar.brokerRequests.some((request) => request.method === "DELETE" && request.url.startsWith("/api/v1/agent/images/"))).toBe(true);

  const submits = parsedCommands(localAgentSidebar.brokerRequests).filter((command) => command.type === "submit");
  expect(submits[0].payload).toMatchObject({ message: "", images: [{ name: "diagram.png" }, { name: "photo.jpg" }] });
  expect(submits[1].payload).toMatchObject({ message: "Queued with a pasted image.", images: [{ name: "paste.webp" }] });
});

test("explains when the selected model cannot accept images", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Text model\n", creatorContext: "Text only.\n" });
  localAgentSidebar.setSupportsImages("pi", false);
  await connectSidebar(page);
  const imageButton = page.getByRole("button", { name: "Add images" });
  await expect(imageButton).toBeDisabled();
  await expect(imageButton).toHaveAttribute("title", /does not support image input/u);
});

test("shows authoritative loading, progressive streaming, alternate views, and sanitized activity", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const markdown = `# Controlled response\n\n${Array.from({ length: 80 }, (_, index) => `Markdown line ${index + 1} for context inspection.`).join("\n")}\n`;
  const creatorContext = `${Array.from({ length: 60 }, (_, index) => `Creator note ${index + 1} for context inspection.`).join("\n")}\n`;
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown, creatorContext });
  localAgentSidebar.setPhaseResponses(true);
  await connectSidebar(page);

  const composer = page.getByLabel("Message Pi about this whiteboard");
  await composer.fill("Stream this response.");
  await composer.press("Enter");
  await expect(page.locator(".agent-response-loading")).toHaveAccessibleName("Pi is responding");
  await expect(page.locator(".agent-response-dot")).toHaveCount(3);
  await expect(page.locator(".agent-response-dot").first()).not.toHaveCSS("animation-name", "none");
  await expect(page.locator(".agent-live-status")).toHaveText("Responding");
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeEnabled();

  localAgentSidebar.releaseResponsePhase("first_delta");
  const assistant = page.locator(".agent-message-assistant .agent-message-body");
  await expect(assistant).toContainText("Fixture");
  await expect(assistant).not.toContainText("reply");
  await expect(page.locator(".agent-response-loading")).toHaveCount(0);

  localAgentSidebar.releaseResponsePhase("later_delta");
  await expect(assistant).toContainText("Fixture reply");
  await expect(page.locator(".agent-live-status")).toHaveText("Responding");
  localAgentSidebar.emitActivity("visible_summary", "Checked the published headings.");
  localAgentSidebar.emitBlocked("tool");
  localAgentSidebar.emitBlocked("permission");
  await expect(page.locator(".agent-activity-visible_summary summary")).toHaveText("Work summary");
  await expect(page.locator(".agent-activity-visible_summary")).not.toHaveAttribute("open", "");
  await expect(page.locator(".agent-activity-blocked summary")).toHaveText(["Tool request blocked", "Permission request blocked"]);
  await expect(page.locator(".agent-activity-blocked").first()).toContainText("content-only policy");
  await expect(page.locator(".agent-timeline")).not.toContainText("thinking_delta");

  localAgentSidebar.releaseResponsePhase("completion");
  await expect(page.locator(".agent-live-status")).toHaveText("Connected");
  const conversationHeaderBox = await page.locator(".agent-drawer-header").boundingBox();
  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "Inspect page context" }).click();
  await expect(page.locator(".agent-context")).toBeVisible();
  const contextHeaderBox = await page.locator(".agent-drawer-header").boundingBox();
  expect(Math.round(contextHeaderBox.height)).toBe(Math.round(conversationHeaderBox.height));
  await expect(page.locator(".agent-drawer-header h2")).toHaveText("Page context");
  await expect(page.getByLabel("Conversation provider")).toBeHidden();
  await expect(page.getByRole("button", { name: "Open Page agent menu" })).toBeHidden();
  const contextCards = page.locator(".agent-context-card");
  await expect(contextCards).toHaveCount(2);
  await expect(contextCards.first()).toHaveAttribute("open", "");
  await expect(contextCards.nth(1)).not.toHaveAttribute("open", "");
  await contextCards.nth(1).locator("summary").click();
  const markdownContext = page.getByLabel("Page Markdown");
  const creatorContextBlock = page.getByLabel("Creator notes");
  await expect(markdownContext).toHaveText(markdown);
  await expect(creatorContextBlock).toHaveText(creatorContext);
  await expect(markdownContext).toHaveCSS("overflow-y", "scroll");
  await expect(creatorContextBlock).toHaveCSS("overflow-y", "scroll");
  await expect(markdownContext).toHaveCSS("touch-action", "pan-y");
  await expect(creatorContextBlock).toHaveCSS("touch-action", "pan-y");
  const markdownContextBox = await markdownContext.boundingBox();
  const creatorContextBox = await creatorContextBlock.boundingBox();
  expect(markdownContextBox.height).toBeGreaterThanOrEqual(288);
  expect(creatorContextBox.height).toBeGreaterThanOrEqual(288);
  expect(Math.abs(markdownContextBox.height - creatorContextBox.height)).toBeLessThanOrEqual(1);
  expect(await markdownContext.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true);
  expect(await creatorContextBlock.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true);
  await markdownContext.evaluate((element) => { element.scrollTop = 240; });
  await creatorContextBlock.evaluate((element) => { element.scrollTop = 240; });
  expect(await markdownContext.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
  expect(await creatorContextBlock.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
  await expect(page.locator(".agent-timeline")).toBeHidden();
  await page.getByRole("button", { name: "Back to conversation" }).click();

  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "Archives" }).click();
  await expect(page.locator(".agent-archives")).toBeVisible();
  await expect(page.locator(".agent-archives article")).toContainText("fixture-model");
  await expect(page.locator(".agent-archives article")).not.toContainText("Fixture reply");
});

test("sends exact initial context once and resumes without replaying it", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  const markdown = "# Exact context\n\nUTF-8: café\n";
  const creatorContext = "Creator says: preserve this exactly.\n";
  const resource = await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown, creatorContext });
  await connectSidebar(page);

  await page.getByLabel("Message Pi about this whiteboard").fill("What does this page say?");
  await page.getByLabel("Message Pi about this whiteboard").press("Enter");
  await expect(page.locator(".agent-message-assistant")).toContainText("Fixture reply");

  const firstSubmit = parsedCommands(localAgentSidebar.brokerRequests).find((command) => command.type === "submit");
  expect(firstSubmit.payload.message).toBe("What does this page say?");
  expect(firstSubmit.payload.context).toMatchObject({
    revision: "initial",
    markdown,
    creator_context: creatorContext,
    title: "Exact context",
    url: resource.url,
  });

  localAgentSidebar.resetBrokerRequests();
  await page.reload();
  await expect(page.locator(".agent-live-status")).toHaveText("Pi ready");
  await expect(page.locator(".agent-drawer")).toHaveClass(/is-open/u);
  await page.getByRole("button", { name: "Connect to Pi", exact: true }).click();
  await expect(page.locator(".agent-message-assistant")).toContainText("Fixture reply");
  await page.getByLabel("Message Pi about this whiteboard").fill("Continue without repeating context.");
  await page.getByLabel("Message Pi about this whiteboard").press("Enter");
  await expect(page.locator(".agent-message-assistant")).toHaveCount(2);

  const resumedSubmit = parsedCommands(localAgentSidebar.brokerRequests).find((command) => command.type === "submit");
  expect(resumedSubmit.payload.message).toBe("Continue without repeating context.");
  expect(resumedSubmit.payload).not.toHaveProperty("context");
  const stored = await page.evaluate(() => Object.fromEntries(Object.entries(localStorage)));
  expect(stored[portKey]).toBe(String(localAgentSidebar.brokerPort));
  expect(stored[drawerKey]).toBe("true");
  const allowedPreferenceKeys = new Set(["agent-whiteboard-theme", drawerKey, portKey, widthKey]);
  expect(Object.keys(stored).every((key) => allowedPreferenceKeys.has(key))).toBe(true);
  expect(JSON.stringify(stored)).not.toContain("What does this page say?");
  expect(JSON.stringify(stored)).not.toContain(creatorContext);
});

test("switches providers silently and isolates active Pi and Codex conversations", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  const markdown = "# Provider isolation\n\nDo not send this while switching.\n";
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown, creatorContext: "Provider context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();

  await page.getByLabel("Conversation provider").selectOption("codex");
  await expect(page.locator(".agent-drawer-header")).toContainText("Codex ready");
  await expect(page.getByRole("button", { name: "Connect to Codex", exact: true })).toBeVisible();
  expect(parsedCommands(localAgentSidebar.brokerRequests)).toEqual([]);
  expect(JSON.stringify(localAgentSidebar.brokerRequests)).not.toContain(markdown);
  expect(await page.evaluate(() => localStorage.getItem("agent-whiteboard-agent-provider"))).toBe("codex");

  localAgentSidebar.setHoldResponses(true, "codex");
  await connectSidebar(page, "codex");
  await page.getByLabel("Message Codex about this whiteboard").fill("Keep the Codex turn active.");
  await page.getByLabel("Message Codex about this whiteboard").press("Enter");
  await expect(page.locator(".agent-live-status")).toHaveText("Responding");
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeEnabled();

  const commandCountBeforeSwitch = parsedCommands(localAgentSidebar.brokerRequests).length;
  await page.getByLabel("Conversation provider").selectOption("pi");
  await expect(page.locator(".agent-live-status")).toHaveText("Pi ready");
  await expect(page.locator(".agent-provider-label")).toBeHidden();
  expect(parsedCommands(localAgentSidebar.brokerRequests)).toHaveLength(commandCountBeforeSwitch);
  await connectSidebar(page, "pi");
  await page.getByLabel("Message Pi about this whiteboard").fill("Answer only in the Pi conversation.");
  await page.getByLabel("Message Pi about this whiteboard").press("Enter");
  await expect(page.locator(".agent-message-assistant")).toContainText("Fixture reply");
  await expect(page.locator(".agent-timeline")).not.toContainText("Keep the Codex turn active.");

  await page.getByLabel("Conversation provider").selectOption("codex");
  await expect(page.locator(".agent-provider-label")).toContainText("fixture-codex-model");
  await expect(page.locator(".agent-message-user")).toContainText("Keep the Codex turn active.");
  await expect(page.locator(".agent-timeline")).not.toContainText("Answer only in the Pi conversation.");
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeEnabled();

  const commands = parsedCommands(localAgentSidebar.brokerRequests);
  const connects = commands.filter((command) => command.type === "connect");
  expect(connects.map((command) => command.payload.provider)).toEqual(["codex", "pi"]);
  const codexSubmit = commands.find((command) => command.type === "submit" && command.payload.message.includes("Codex"));
  const piSubmit = commands.find((command) => command.type === "submit" && command.payload.message.includes("Pi"));
  expect(codexSubmit.conversation_id).not.toBe(piSubmit.conversation_id);
});

test("renders Codex tool lifecycle and stable approval and interaction families", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Codex interactions\n", creatorContext: "Interaction context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("codex");
  await connectSidebar(page, "codex");

  const activityID = localAgentSidebar.emitToolActivity("codex", {
    kind: "command",
    status: "running",
    title: "Inspect browser tests",
    summary: "Running a focused search.",
    detail: "rg -n provider tests/browser",
  });
  await expect(page.locator(".agent-tool-activity")).toHaveCount(1);
  await expect(page.locator(".agent-tool-activity")).toHaveAttribute("data-status", "running");
  localAgentSidebar.emitToolActivity("codex", {
    activity_id: activityID,
    kind: "command",
    status: "completed",
    title: "Inspect browser tests",
    summary: "Focused search completed.",
    detail: "tests/browser/local-agent-sidebar.spec.js",
  });
  await expect(page.locator(".agent-tool-activity")).toHaveCount(1);
  await expect(page.locator(".agent-tool-activity")).toHaveAttribute("data-status", "completed");
  await expect(page.locator(".agent-tool-activity")).toContainText("Focused search completed.");

  localAgentSidebar.emitInteraction("codex", {
    kind: "command_approval",
    title: "Run command",
    summary: "Codex wants to inspect the focused browser files.",
    command: "rg -n provider tests/browser",
    working_directory: "/workspace/agent-whiteboard",
    options: [
      { id: "accept", label: "Accept", description: "Run once." },
      { id: "acceptForSession", label: "Accept for session", description: "Allow matching commands for this session." },
      { id: "decline", label: "Decline", description: "Do not run it." },
    ],
  });
  const commandCard = page.locator('.agent-interaction[data-kind="command_approval"]');
  await expect(commandCard).toHaveAccessibleName(/Run command · Response requested/u);
  await expect(commandCard.getByLabel("Requested command")).toHaveText("rg -n provider tests/browser");
  await commandCard.getByRole("button", { name: "Accept for session", exact: true }).click();
  await expect(commandCard).toHaveAttribute("data-state", "resolved");

  localAgentSidebar.emitInteraction("codex", {
    kind: "file_change_approval",
    title: "Apply file changes",
    summary: "Update tests/browser/example.spec.js with the displayed diff.",
    working_directory: "/workspace/agent-whiteboard",
    options: [
      { id: "accept", label: "Apply", description: "Apply this change." },
      { id: "decline", label: "Decline", description: "Keep the file unchanged." },
    ],
  });
  const fileCard = page.locator('.agent-interaction[data-kind="file_change_approval"]');
  await fileCard.getByRole("button", { name: "Decline", exact: true }).click();
  await expect(fileCard).toHaveAttribute("data-state", "resolved");

  localAgentSidebar.emitInteraction("codex", {
    kind: "permission_approval",
    title: "Grant permissions",
    summary: "Choose the requested permission subset and scope.",
    options: [
      { id: "grantTurn", label: "Allow for turn", description: "Grant the selected permissions for this turn." },
      { id: "grantSession", label: "Allow for session", description: "Grant the selected permissions for this session." },
      { id: "decline", label: "Decline", description: "Grant nothing." },
    ],
    fields: [
      {
        id: "permissions",
        label: "Permissions",
        description: "Select the capabilities Codex may use.",
        type: "multi_select",
        required: true,
        secret: false,
        options: [
          { id: "workspace_read", label: "Workspace read", description: "Read workspace files." },
          { id: "workspace_write", label: "Workspace write", description: "Modify workspace files." },
          { id: "network", label: "Network", description: "Reach approved network resources." },
        ],
      },
    ],
  });
  const permissionCard = page.locator('.agent-interaction[data-kind="permission_approval"]');
  await permissionCard.getByLabel("Permissions").selectOption(["workspace_write", "network"]);
  await permissionCard.getByRole("button", { name: "Allow for session", exact: true }).click();
  await expect(permissionCard).toHaveAttribute("data-state", "resolved");

  localAgentSidebar.emitInteraction("codex", {
    kind: "mcp_elicitation",
    title: "Configure MCP action",
    summary: "The local MCP server needs structured values.",
		options: [
			{ id: "accept", label: "Accept", description: "Provide the requested input." },
			{ id: "decline", label: "Decline", description: "Decline this request." },
			{ id: "cancel", label: "Cancel", description: "Cancel this request." },
		],
    fields: [
      { id: "project_name", label: "Project name", description: "Project to inspect.", type: "text", required: true, secret: false, options: [] },
      { id: "mode", label: "Mode", description: "Requested operation.", type: "select", required: true, secret: false, options: [
        { id: "inspect", label: "Inspect", description: "Read only." },
        { id: "update", label: "Update", description: "Apply changes." },
      ] },
      { id: "confirmed", label: "Confirmed", description: "Confirm the request.", type: "boolean", required: false, secret: false, options: [] },
    ],
  });
  const mcpCard = page.locator('.agent-interaction[data-kind="mcp_elicitation"]');
  await mcpCard.getByLabel("Project name").fill("agent-whiteboard");
  await mcpCard.getByLabel("Mode").selectOption("inspect");
  await mcpCard.getByLabel("Confirmed").check();
  await mcpCard.getByRole("button", { name: "Accept", exact: true }).click();
  await expect(mcpCard).toHaveAttribute("data-state", "resolved");

  const responses = parsedCommands(localAgentSidebar.brokerRequests).filter((command) => command.type === "interaction_respond");
  expect(responses.map((command) => command.payload.kind)).toEqual([
    "command_approval",
    "file_change_approval",
    "permission_approval",
    "mcp_elicitation",
  ]);
  expect(responses[0].payload.option_id).toBe("acceptForSession");
  expect(responses[1].payload.option_id).toBe("decline");
  expect(responses[2].payload).toMatchObject({ option_id: "grantSession", answers: { permissions: ["workspace_write", "network"] } });
  expect(responses[3].payload).toMatchObject({ option_id: "accept", answers: { project_name: ["agent-whiteboard"], mode: ["inspect"], confirmed: ["true"] } });
});

test("the first valid Codex interaction response wins across tabs", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  const resource = await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Cross-tab approval\n", creatorContext: "Cross-tab context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("codex");
  await connectSidebar(page, "codex");

  const otherPage = await context.newPage();
  await otherPage.goto(resource.url);
  await expect(otherPage.locator(".agent-live-status")).toHaveText("Codex ready");
  await connectSidebar(otherPage, "codex");
  localAgentSidebar.setHoldInteractionResolution("codex", true);
  const requestID = localAgentSidebar.emitInteraction("codex", {
    kind: "command_approval",
    title: "Choose once",
    summary: "Only the first valid browser response may resolve this request.",
    options: [
      { id: "accept", label: "Accept", description: "Approve once." },
      { id: "decline", label: "Decline", description: "Reject once." },
    ],
  });
  const firstCard = page.locator('.agent-interaction[data-kind="command_approval"]');
  const secondCard = otherPage.locator('.agent-interaction[data-kind="command_approval"]');
  await expect(firstCard).toBeVisible();
  await expect(secondCard).toBeVisible();

  await firstCard.getByRole("button", { name: "Accept", exact: true }).click();
  await expect.poll(() => localAgentSidebar.interactionResults.filter(({ requestID: id }) => id === requestID)).toHaveLength(1);
  await secondCard.getByRole("button", { name: "Decline", exact: true }).click();
  await expect.poll(() => localAgentSidebar.interactionResults.filter(({ requestID: id }) => id === requestID)).toHaveLength(2);
  expect(localAgentSidebar.interactionResults.filter(({ requestID: id, status }) => id === requestID && status === "accepted")).toEqual([
    expect.objectContaining({ optionID: "accept" }),
  ]);
  expect(localAgentSidebar.interactionResults.filter(({ requestID: id, status }) => id === requestID && status === "rejected")).toHaveLength(1);

  localAgentSidebar.releaseInteraction("codex", requestID);
  await expect(firstCard).toHaveAttribute("data-state", "resolved");
  await expect(secondCard).toHaveAttribute("data-state", "resolved");
  await expect(firstCard.locator(".agent-interaction-status")).toHaveText("Resolved · Accept");
  await expect(secondCard.locator(".agent-interaction-status")).toHaveText("Resolved · Accept");
  await otherPage.close();
});

test("replays Codex interaction activity after reconnect and isolates an unavailable provider", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Recovery and isolation\n", creatorContext: "Recovery context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("codex");
  await connectSidebar(page, "codex");

  localAgentSidebar.disconnectProvider("codex");
  await expect(page.locator(".agent-live-status")).toHaveText("Broker unavailable");
  localAgentSidebar.emitToolActivity("codex", {
    kind: "mcp",
    status: "completed",
    title: "Recovered MCP call",
    summary: "This activity was emitted while the tab was disconnected.",
    detail: "replayed detail",
  });
  localAgentSidebar.emitInteraction("codex", {
    kind: "mcp_elicitation",
    title: "Recovered elicitation",
    summary: "This request must replay after reconnect.",
		options: [
			{ id: "accept", label: "Accept", description: "Provide the requested input." },
			{ id: "decline", label: "Decline", description: "Decline this request." },
			{ id: "cancel", label: "Cancel", description: "Cancel this request." },
		],
    fields: [
      { id: "answer", label: "Recovered answer", description: "Replay proof.", type: "text", required: true, secret: false, options: [] },
    ],
  });
  await expect(page.locator(".agent-tool-activity")).toContainText("Recovered MCP call", { timeout: 5_000 });
  await expect(page.locator('.agent-interaction[data-kind="mcp_elicitation"]')).toContainText("Recovered elicitation");
  const replayConnect = parsedCommands(localAgentSidebar.brokerRequests)
    .filter((command) => command.type === "connect" && command.payload.provider === "codex")
    .find((command) => Object.hasOwn(command.payload, "replay_after"));
  expect(replayConnect).toBeDefined();

  await page.getByLabel("Conversation provider").selectOption("pi");
  await connectSidebar(page, "pi");
  await page.getByLabel("Message Pi about this whiteboard").fill("Pi remains available.");
  await page.getByLabel("Message Pi about this whiteboard").press("Enter");
  await expect(page.locator(".agent-message-assistant")).toContainText("Fixture reply");

  localAgentSidebar.setProviderAvailable("codex", false);
  await page.getByLabel("Conversation provider").selectOption("codex");
  await expect(page.locator(".agent-tool-activity")).toContainText("Recovered MCP call");
  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "Reconnect" }).click();
  await expect.poll(() => localAgentSidebar.brokerRequests.some((request) => request.status === 503 && request.url === "/api/v1/agent/connect")).toBe(true);
  await expect(page.locator(".agent-live-status")).toHaveText("Codex ready");
  await expect(page.locator(".agent-provider-label")).toBeHidden();
  await page.getByLabel("Conversation provider").selectOption("pi");
  await expect(page.locator(".agent-provider-label")).toContainText("fixture-model");
  await expect(page.locator(".agent-message-assistant")).toContainText("Fixture reply");
  await expect(page.locator('.agent-composer button[type="submit"]')).toBeDisabled();
});
