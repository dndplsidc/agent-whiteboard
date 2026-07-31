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

async function connectSidebar(page) {
  await page.getByRole("button", { name: "Open Page agent" }).click();
  await page.getByRole("button", { name: "Connect to Pi", exact: true }).click();
  await expect(page.locator(".agent-provider-label")).toContainText("fixture-model");
  await expect(page.locator('.agent-composer button[type="submit"]')).toBeEnabled();
}

function parsedCommands(requests) {
  return requests
    .filter((request) => request.method === "POST" && typeof request.body === "string" && request.body !== "")
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
  await expect(page.locator(".agent-drawer-header")).toContainText("Content-only · Local Pi");
  await expect(page.locator(".agent-drawer-header")).not.toContainText("Pi ready");
  await expect(page.locator(".agent-status-bar")).toContainText("Pi ready");
  await expect(page.locator(".agent-status-bar")).toContainText("Not connected");
  await expect(page.locator(".agent-consent")).toContainText("sends no page content");
  await expect(page.locator(".agent-consent-list")).toContainText("Complete Markdown and creator notes");
  await expect(page.locator(".agent-context-disclosure")).toContainText("Not shared");
  await page.getByRole("button", { name: "Connect to Pi", exact: true }).click();

  await expect(page.locator(".agent-provider-label")).toContainText("fixture-model");
  await expect(page.locator('.agent-composer button[type="submit"]')).toBeEnabled();
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

test("shows authoritative loading, progressive streaming, alternate views, and sanitized activity", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  const markdown = "# Controlled response\n\nExact Markdown for context inspection.\n";
  const creatorContext = "Exact creator context for inspection.\n";
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
  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "Inspect page context" }).click();
  await expect(page.locator(".agent-context")).toBeVisible();
  await expect(page.getByLabel("Page Markdown")).toHaveText(markdown);
  await expect(page.getByLabel("Creator context")).toHaveText(creatorContext);
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
