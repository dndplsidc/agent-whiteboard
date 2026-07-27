import { expect, test } from "./fixture.js";

const drawerKey = "agent-whiteboard-agent-drawer-open";
const portKey = "agent-whiteboard-agent-port";

test.use({ browserRequestInterception: false, ignoreHTTPSErrors: true });

async function openSidebarPage({ context, page, fixture, markdown, creatorContext }) {
  const resource = await fixture.publish(markdown, creatorContext);
  await context.grantPermissions(["local-network-access"], { origin: fixture.origin });
  await page.addInitScript(
    ({ key, port }) => localStorage.setItem(key, String(port)),
    { key: portKey, port: fixture.brokerPort },
  );
  fixture.resetBrokerRequests();
  fixture.resetBrokerState();
  await page.goto(resource.url);
  await expect(page.locator(".agent-live-status")).toContainText("Local broker available");
  return resource;
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

  await page.getByRole("button", { name: "Open local agent" }).click();
  await expect(page.locator(".agent-consent")).toContainText("No page content is sent when you connect");
  await expect(page.locator(".agent-consent")).toContainText("complete Page Markdown and Creator context");
  await page.getByRole("button", { name: "Connect", exact: true }).click();

  await expect(page.locator(".agent-live-status")).toContainText("fixture-model");
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
  await page.getByRole("button", { name: "Open local agent" }).click();
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(page.locator(".agent-live-status")).toContainText("fixture-model");
  const priorReplies = await page.locator(".agent-message-assistant").count();
  await page.getByLabel("Message Pi about this whiteboard").fill("Use the socket.");
  await page.locator('.agent-composer button[type="submit"]').click();
  await expect(page.locator(".agent-message-assistant")).toHaveCount(priorReplies + 1);

  expect(localAgentSidebar.brokerRequests.some((request) => request.status === 101)).toBe(true);
  expect(localAgentSidebar.brokerRequests.some((request) => request.method === "POST" && request.url === "/api/v1/agent/connect")).toBe(false);
  expect(localAgentSidebar.brokerRequests.some((request) => request.method === "POST" && request.url === "/api/v1/agent/commands")).toBe(false);
  expect(localAgentSidebar.webSocketCommands.map((command) => command.type)).toEqual(["connect", "history_page", "submit"]);
  expect(localAgentSidebar.webSocketCommands[0].payload).not.toHaveProperty("context");
});

test("renders the shared queue and stops without replaying the active turn", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Queue controls\n", creatorContext: "Queue context.\n" });
  localAgentSidebar.setHoldResponses(true);
  await page.getByRole("button", { name: "Open local agent" }).click();
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(page.locator('.agent-composer button[type="submit"]')).toBeEnabled();

  const composer = page.getByLabel("Message Pi about this whiteboard");
  await composer.fill("Keep this turn active.");
  await page.locator('.agent-composer button[type="submit"]').click();
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeEnabled();
  await composer.fill("Queued follow-up.");
  await page.locator('.agent-composer button[type="submit"]').click();
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

test("uses modal focus and Escape restoration at the mobile viewport", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  await page.setViewportSize({ width: 390, height: 800 });
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Mobile drawer\n", creatorContext: "Mobile context.\n" });
  const toggle = page.getByRole("button", { name: "Open local agent" });
  await toggle.click();
  await expect(page.locator(".agent-drawer")).toHaveAttribute("role", "dialog");
  await expect(page.locator(".agent-drawer")).toHaveAttribute("aria-modal", "true");
  await expect(page.locator("body")).toHaveCSS("overflow", "hidden");
  await page.keyboard.press("Escape");
  await expect(page.locator(".agent-drawer")).not.toHaveClass(/is-open/u);
  await expect(toggle).toBeFocused();
});

test("sends exact initial context once, streams Pi output, and resumes without replaying it", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  const markdown = "# Exact context\n\nUTF-8: café\n";
  const creatorContext = "Creator says: preserve this exactly.\n";
  const resource = await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown, creatorContext });
  await page.getByRole("button", { name: "Open local agent" }).click();
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(page.locator('.agent-composer button[type="submit"]')).toBeEnabled();

  await page.getByLabel("Message Pi about this whiteboard").fill("What does this page say?");
  await page.locator('.agent-composer button[type="submit"]').click();
  await expect(page.locator(".agent-message-assistant")).toContainText("Fixture reply");
  await expect(page.locator(".agent-context")).toContainText("context accepted");

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
  await expect(page.locator(".agent-live-status")).toContainText("Local broker available");
  await expect(page.locator(".agent-drawer")).toHaveClass(/is-open/u);
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(page.locator(".agent-message-assistant")).toContainText("Fixture reply");
  await page.getByLabel("Message Pi about this whiteboard").fill("Continue without repeating context.");
  await page.locator('.agent-composer button[type="submit"]').click();
  await expect(page.locator(".agent-message-assistant")).toHaveCount(2);

  const resumedSubmit = parsedCommands(localAgentSidebar.brokerRequests).find((command) => command.type === "submit");
  expect(resumedSubmit.payload.message).toBe("Continue without repeating context.");
  expect(resumedSubmit.payload).not.toHaveProperty("context");
  const stored = await page.evaluate(() => Object.fromEntries(Object.entries(localStorage)));
  expect(stored[portKey]).toBe(String(localAgentSidebar.brokerPort));
  expect(stored[drawerKey]).toBe("true");
  expect(JSON.stringify(stored)).not.toContain("What does this page say?");
  expect(JSON.stringify(stored)).not.toContain(creatorContext);
});
