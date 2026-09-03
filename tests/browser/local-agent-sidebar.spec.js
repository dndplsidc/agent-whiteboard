import { expect, test } from "./fixture.js";

const drawerKey = "agent-whiteboard-agent-drawer-open";
const portKey = "agent-whiteboard-agent-port";
const widthKey = "agent-whiteboard-agent-drawer-width";
const codexSettingsKey = "agent-whiteboard-codex-settings-v1";
const providerFixtures = {
  pi: { label: "Pi", model: "fixture-model" },
  codex: { label: "Codex", model: "5.6 Sol" },
  cursor: { label: "Cursor", model: "Cursor Small" },
};

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
  const selected = preferences["agent-whiteboard-agent-provider"] ?? "pi";
  await expect(page.locator(".agent-live-status")).toHaveText(`${providerFixtures[selected].label} ready`);
  return resource;
}

async function connectSidebar(page, provider = "pi", expectedModel = null) {
  const providerName = providerFixtures[provider].label;
  const model = expectedModel ?? providerFixtures[provider].model;
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

async function recordedCommand(fixture, type) {
  await expect.poll(() => parsedCommands(fixture.brokerRequests).some((command) => command.type === type)).toBe(true);
  return parsedCommands(fixture.brokerRequests).find((command) => command.type === type);
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
  const contextDisclosure = page.locator(".agent-context-disclosure");
  await expect(contextDisclosure).toContainText("Markdown + creator notes");
  await expect(contextDisclosure).not.toContainText(/shared|uncertain/iu);
  await expect(contextDisclosure.locator(".agent-context-disclosure-icon svg")).toHaveCount(1);
  await expect(contextDisclosure.locator(".agent-context-disclosure-chevron svg")).toHaveCount(1);
  await expect(contextDisclosure.locator(":scope > div > span")).toHaveCSS("font-size", "10.56px");
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

test("adds multiple document selections inline with surrounding message text", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({
    context,
    page,
    fixture: localAgentSidebar,
    markdown: "# Selection board\n\nThe first selected sentence explains the premise.\n\n## Evidence\n\nA complete section supports it.\n",
    creatorContext: "Selection context.\n",
  });
  const paragraph = page.locator("#agent-whiteboard-content p").first();
  await paragraph.evaluate((element) => {
    const range = document.createRange();
    range.setStart(element.firstChild, 4);
    range.setEnd(element.firstChild, 27);
    const selection = getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    document.dispatchEvent(new Event("selectionchange"));
  });
  const popup = page.getByRole("button", { name: "Add selected text to message" });
  await expect(popup).toBeVisible();
  await popup.click();
  const composer = page.getByLabel("Message Pi about this whiteboard");
  const toast = page.locator(".agent-toast");
  await expect(composer.locator(".agent-message-reference")).toHaveCount(1);
  await expect(page.locator(".agent-attachment-status")).toBeEmpty();
  await expect(toast).toContainText(/Added .* to the message/u);
  await expect(toast).toBeVisible();
  await expect(toast).toBeHidden({ timeout: 4_000 });
  await page.getByRole("button", { name: "Connect to Pi", exact: true }).click();
  await expect(page.locator(".agent-provider-label")).toContainText("fixture-model");
  await composer.press("End");
  await composer.pressSequentially(" and ");
  await expect(composer).toContainText("and");

  const evidence = page.getByRole("heading", { name: /Evidence/u });
  await evidence.hover();
  await evidence.getByRole("button", { name: "Add section: Evidence" }).click();
  await expect(composer.locator(".agent-message-reference")).toHaveCount(2);
  await expect(composer).toContainText("and");
  await composer.press("End");
  await composer.pressSequentially(" explain the relationship.");

  await composer.press("Enter");
  const sent = page.locator(".agent-message-user").last();
  await expect(sent.locator(".agent-message-reference")).toHaveCount(2);
  await expect(sent).toContainText("and");
  await expect(sent).toContainText("explain the relationship");

  const submit = await recordedCommand(localAgentSidebar, "submit");
  const references = submit.payload.content.parts.filter((part) => part.type === "reference");
  expect(references).toHaveLength(2);
  expect(references.map((part) => part.reference)).toEqual(expect.arrayContaining([
    expect.objectContaining({ kind: "text", quote: "first selected sentence" }),
    expect.objectContaining({ kind: "section", label: "Evidence" }),
  ]));
  expect(submit.payload.content.parts.filter((part) => part.type === "text").map((part) => part.text).join(""))
    .toMatch(/and\s+explain the relationship\./u);
});

test("adds exact Mermaid source as existing section context", async ({ context, page, localAgentSidebar }) => {
  const diagram = "```mermaid\nflowchart LR\n  Browser --> Broker\n```";
  await page.setViewportSize({ width: 360, height: 800 });
  await openSidebarPage({
    context,
    page,
    fixture: localAgentSidebar,
    markdown: `# Architecture\n\nThis diagram shows how Page Agent passes context.\n\n${diagram}\n\nThe diagram shows the Page Agent path.\n`,
    creatorContext: "Mermaid component context.\n",
  });

  const source = page.locator(".agent-source-diagram");
  await source.hover();
  const addDiagram = source.getByRole("button", { name: "Add diagram: Architecture — Mermaid diagram 1" });
  const [introBox, buttonBox, diagramBox] = await Promise.all([
    page.locator("#agent-whiteboard-content > p").first().boundingBox(),
    addDiagram.boundingBox(),
    source.locator(".mermaid-placeholder").boundingBox(),
  ]);
  expect(introBox.y + introBox.height).toBeLessThanOrEqual(buttonBox.y);
  expect(buttonBox.y + buttonBox.height).toBeLessThanOrEqual(diagramBox.y);
  await page.mouse.move(1, 1);
  await addDiagram.focus();
  await expect.poll(() => addDiagram.evaluate((element) => getComputedStyle(element).opacity)).toBe("1");
  await addDiagram.click();
  const composer = page.getByLabel("Message Pi about this whiteboard");
  const token = composer.locator('[data-reference-kind="section"]');
  await expect(token).toHaveText("Architecture — Mermaid diagram 1");
  await page.getByRole("button", { name: "Connect to Pi", exact: true }).click();
  await expect(page.locator(".agent-provider-label")).toContainText("fixture-model");
  await composer.press("End");
  await composer.pressSequentially(" explain this flow.");
  await composer.press("Enter");

  const sent = page.locator(".agent-message-user").last();
  await expect(sent.locator('[data-reference-kind="section"]')).toHaveText("Architecture — Mermaid diagram 1");
  const submit = await recordedCommand(localAgentSidebar, "submit");
  const submittedReference = submit.payload.content.parts.find((part) => part.type === "reference").reference;
  expect(submittedReference).toMatchObject({
    kind: "section",
    label: "Architecture — Mermaid diagram 1",
    markdown: diagram,
    section_lines: { start: 5, end: 9 },
    source: { resource_kind: "markdown", anchor: { markdown: { start: { line: 5 }, end: { line: 9 } } } },
  });
  expect(localAgentSidebar.brokerRequests.some((request) => request.url === "/api/v1/agent/images")).toBe(false);
});

test("ellipsizes a long page reference inside the composer", async ({ context, page, localAgentSidebar }) => {
  const title = "A deliberately long whiteboard heading that must stay inside the compact message composer";
  await openSidebarPage({
    context,
    page,
    fixture: localAgentSidebar,
    markdown: `# ${title}\n\nReference token overflow regression.\n`,
    creatorContext: "Long page-reference context.\n",
  });
  await connectSidebar(page);
  const heading = page.locator("#agent-whiteboard-content h1");
  await heading.hover();
  await heading.getByRole("button", { name: `Add page: ${title}` }).click();

  const composer = page.getByLabel("Message Pi about this whiteboard");
  const token = composer.locator(".agent-message-reference");
  const label = token.locator(".agent-message-reference-label");
  await expect(token).toHaveCount(1);
  await expect(label).toHaveText(title);
  const metrics = await label.evaluate((element) => ({
    clientWidth: element.clientWidth,
    scrollWidth: element.scrollWidth,
    overflow: getComputedStyle(element).overflow,
    textOverflow: getComputedStyle(element).textOverflow,
    whiteSpace: getComputedStyle(element).whiteSpace,
  }));
  expect(metrics.scrollWidth).toBeGreaterThan(metrics.clientWidth);
  expect(metrics).toMatchObject({ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" });
  const [composerBox, tokenBox] = await Promise.all([composer.boundingBox(), token.boundingBox()]);
  expect(tokenBox.x + tokenBox.width).toBeLessThanOrEqual(composerBox.x + composerBox.width);
  expect(tokenBox.width).toBeLessThanOrEqual(196);
  expect(tokenBox.height).toBeLessThanOrEqual(20);
});

test("adds a rendered raster as a private inline image reference", async ({ context, page, localAgentSidebar }) => {
  const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M/wHwAF/gL+V3x7WQAAAABJRU5ErkJggg==";
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: `# Visual board\n\n![Architecture](data:image/png;base64,${onePixelPNG})\n`, creatorContext: "Visual context.\n" });
  await connectSidebar(page);
  const image = page.getByRole("img", { name: "Architecture" });
  await image.locator("..").hover();
  await page.getByRole("button", { name: "Add image: Architecture" }).click();
  const composer = page.getByLabel("Message Pi about this whiteboard");
  await expect(composer.locator('[data-reference-kind="image"]')).toHaveText("Architecture");
  await composer.press("End");
  await composer.pressSequentially(" explain this visual.");
  await composer.press("Enter");

  const sent = page.locator(".agent-message-user").last();
  await expect(sent.locator('[data-reference-kind="image"]')).toHaveText("Architecture");
  await expect(sent.locator(".agent-message-images")).toHaveCount(0);
  const upload = localAgentSidebar.brokerRequests.find((request) => request.method === "POST" && request.url === "/api/v1/agent/images");
  expect(upload.headers["x-agent-whiteboard-image-purpose"]).toBe("inline_reference");
  const submit = await recordedCommand(localAgentSidebar, "submit");
  expect(submit.payload.images).toBeUndefined();
  expect(submit.payload.content.parts[0].reference).toMatchObject({ kind: "image", visual: { name: "image-1.png", alt: "Architecture" } });
});

test("keeps header pointer focus borderless while retaining keyboard focus", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Header focus\n", creatorContext: "Focus context.\n", preferences: { "agent-whiteboard-agent-provider": "codex" } });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  const provider = page.getByLabel("Conversation provider");
  const providerStyle = await provider.evaluate((element) => {
    const style = getComputedStyle(element);
    return { appearance: style.appearance, backgroundImage: style.backgroundImage, paddingRight: Number.parseFloat(style.paddingRight) };
  });
  expect(providerStyle.appearance).toBe("none");
  expect(providerStyle.backgroundImage).not.toBe("none");
  expect(providerStyle.paddingRight).toBeGreaterThanOrEqual(26);
  await provider.click();
  expect(await provider.evaluate((element) => getComputedStyle(element).outlineStyle)).toBe("none");
  await page.getByRole("button", { name: "Close local agent", exact: true }).focus();
  await page.keyboard.press("Shift+Tab");
  await expect(provider).toBeFocused();
  expect(await provider.evaluate((element) => getComputedStyle(element).outlineStyle)).not.toBe("none");
});

test("shows connection recheck success and an explicit wrong-port failure in settings", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Connection feedback\n", creatorContext: "Connection context.\n", preferences: { "agent-whiteboard-agent-provider": "codex" } });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await expect(page.locator(".agent-setup-icon svg")).toBeVisible();
  const connectBox = await page.getByRole("button", { name: "Connect to Codex", exact: true }).boundingBox();
  expect(connectBox).not.toBeNull();
  expect(connectBox.height).toBeLessThanOrEqual(34);
  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "Connection settings" }).click();
  const check = page.locator(".agent-connection-retry");
  const status = page.locator(".agent-connection-status-text");
  await check.click();
  await expect(status).toHaveText("Broker available");
  await expect(page.locator(".agent-guidance")).toContainText("local broker is available");
  const portInput = page.getByLabel("Local agent broker port");
  await portInput.click();
  expect(await portInput.evaluate((element) => getComputedStyle(element).outlineStyle)).toBe("none");
  await portInput.fill("1");
  await portInput.press("Tab");
  await expect(status).toHaveText("No broker on port 1");
  await expect(check).toHaveText("Try again");
  await check.click();
  expect(await check.evaluate((element) => getComputedStyle(element).outlineStyle)).toBe("none");
  await expect(page.locator(".agent-guidance")).toContainText("No compatible broker responded on port 1");
  await page.getByRole("button", { name: "Back to conversation" }).click();
  await expect(page.locator(".agent-setup-icon svg")).toBeVisible();
  for (const label of ["Check again", "Connection settings"]) {
    const box = await page.getByRole("button", { name: label, exact: true }).boundingBox();
    expect(box).not.toBeNull();
    expect(box.height).toBeLessThanOrEqual(34);
  }
});

test("invokes Codex skills and compacts without queueing a busy draft", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Codex skills and compact\n", creatorContext: "Codex feature context.\n", preferences: { "agent-whiteboard-agent-provider": "codex" } });
  await connectSidebar(page, "codex");
  const composer = page.getByLabel("Message Codex about this whiteboard");

  await composer.fill("$");
  const suggestions = page.getByRole("listbox", { name: "Composer suggestions" });
  await expect(suggestions).toContainText("Review helper");
  await expect(suggestions).toContainText("Personal helper with an intentionally long name");
  await expect(suggestions.getByText("User", { exact: true })).toBeVisible();
  const longSkill = suggestions.locator(".agent-completion-option").filter({ hasText: "Personal helper" });
  const longSkillBox = await longSkill.boundingBox();
  const longSkillScrollWidth = await longSkill.evaluate((element) => element.scrollWidth);
  expect(longSkillBox).not.toBeNull();
  expect(longSkillScrollWidth).toBeLessThanOrEqual(Math.ceil(longSkillBox.width));
  const suggestionsBox = await suggestions.boundingBox();
  const composerBox = await page.locator(".agent-composer").boundingBox();
  expect(suggestionsBox).not.toBeNull();
  expect(composerBox).not.toBeNull();
  expect(suggestionsBox.y + suggestionsBox.height).toBeLessThanOrEqual(composerBox.y);
  await composer.fill("$rev");
  await expect(suggestions).not.toContainText("Personal helper");
  await composer.press("Enter");
  const skillPill = composer.locator(".agent-message-skill");
  await expect(skillPill).toHaveText("Skill: Review helper");
  await expect(skillPill).toHaveCSS("margin-left", "0px");
  await expect(skillPill).toHaveCSS("margin-right", "0px");
  await expect(composer).not.toContainText("$");
  await composer.press("End");
  await composer.pressSequentially(" check this page");
  await composer.press("Enter");
  await expect.poll(() => parsedCommands(localAgentSidebar.brokerRequests).some(({ type }) => type === "submit")).toBe(true);
  const skillSubmit = parsedCommands(localAgentSidebar.brokerRequests).find(({ type }) => type === "submit");
  expect(skillSubmit.payload.content.parts.some((part) => part.type === "skill" && part.skill.name === "review-helper")).toBe(true);
  expect(JSON.stringify(skillSubmit)).not.toMatch(/\/Users\/|SKILL\.md/u);

  await expect(page.locator(".agent-live-status")).toHaveText("Connected");
  await composer.fill("/co");
  await expect(suggestions).toContainText("/compact");
  await composer.fill("/nope");
  await expect(suggestions).toBeHidden();
  await composer.fill("/compact");
  await composer.press("Enter");
  await expect(page.locator(".agent-compaction-row")).toContainText("Compacting context…");
  await expect(page.locator(".agent-compaction-row .agent-status-notice-icon svg")).toBeVisible();
  expect(parsedCommands(localAgentSidebar.brokerRequests).some(({ type }) => type === "compact")).toBe(true);
  await composer.fill("preserved next draft");
  await composer.press("Enter");
  expect(parsedCommands(localAgentSidebar.brokerRequests).filter(({ type }) => type === "submit")).toHaveLength(1);
  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await expect(page.locator(".agent-compaction-row")).toContainText("Compaction stopped");
  await expect(composer).toHaveText("preserved next draft");
  expect(parsedCommands(localAgentSidebar.brokerRequests).at(-1)).toMatchObject({ type: "interrupt" });
});

for (const provider of ["pi", "codex", "cursor"]) {
  test(`starts a fresh ${provider} conversation with exact /new`, async ({ context, page, localAgentSidebar }) => {
    await openSidebarPage({
      context,
      page,
      fixture: localAgentSidebar,
      markdown: `# ${provider} new command\n`,
      creatorContext: `${provider} new-command context.\n`,
      preferences: { "agent-whiteboard-agent-provider": provider },
    });
    await connectSidebar(page, provider);
    const composer = page.getByLabel(`Message ${providerFixtures[provider].label} about this whiteboard`);
    const suggestions = page.getByRole("listbox", { name: "Composer suggestions" });

    await composer.fill("/n");
    await expect(suggestions).toContainText("/new");
    await composer.press("Enter");
    await expect(composer).toHaveText("/new");
    await composer.press("Enter");
    let confirmation = page.getByRole("dialog", { name: "Start a new conversation?" });
    await expect(confirmation.getByRole("button", { name: "Cancel" })).toBeFocused();
    expect(parsedCommands(localAgentSidebar.brokerRequests).filter(({ type }) => type === "new")).toHaveLength(0);

    await confirmation.getByRole("button", { name: "Cancel" }).click();
    await expect(composer).toHaveText("/new");
    await composer.press("Enter");
    confirmation = page.getByRole("dialog", { name: "Start a new conversation?" });
    await confirmation.getByRole("button", { name: "Start new" }).click();
    await expect.poll(() => parsedCommands(localAgentSidebar.brokerRequests).filter(({ type }) => type === "new")).toHaveLength(1);
    await expect(page.locator(".agent-live-status")).toHaveText("Connected", { timeout: 5_000 });
    await expect(page.locator(".agent-message-user")).toHaveCount(0);
  });
}

test("keeps Codex command activity at its native transcript position", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Stable skill stream\n", creatorContext: "Stable stream context.\n" });
  localAgentSidebar.setSkills([{ id: "W7W7W7W7W7W7W7W7W7W7W7W7W7W7W7W7", name: "devflow", display_name: "devflow", description: "Software change workflow.", scope: "user" }]);
  localAgentSidebar.setPhaseResponses(true, "codex");
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("codex");
  await connectSidebar(page, "codex");

  const composer = page.getByLabel("Message Codex about this whiteboard");
  await composer.fill("$dev");
  await composer.press("Enter");
  await composer.press("End");
  await composer.pressSequentially(" tell me about this skill");
  await composer.press("Enter");
  await expect(page.locator(".agent-live-status")).toHaveText("Responding");
  const turnID = parsedCommands(localAgentSidebar.brokerRequests).find(({ type }) => type === "submit").payload.turn_id;
  localAgentSidebar.emitAssistantMessage("codex", {
    turn_id: turnID,
    message_id: "bW1tbW1tbW1tbW1tbW1tbW1tbW1tbW1t",
    text: "I will inspect the skill.",
    created_at: "2026-07-27T03:04:05Z",
  });
  localAgentSidebar.emitToolActivity("codex", {
    turn_id: turnID,
    kind: "command",
    status: "completed",
    title: "Command",
    summary: "Loaded the devflow skill.",
    detail: "Read SKILL.md",
  });
  localAgentSidebar.releaseResponsePhase("first_delta", "codex");

  const streamedItems = page.locator(".agent-timeline > .agent-tool-activity, .agent-timeline > .agent-message-assistant");
  await expect(streamedItems).toHaveCount(3);
  await expect(streamedItems.nth(0)).toHaveClass(/agent-message-assistant/u);
  await expect(streamedItems.nth(1)).toHaveClass(/agent-tool-activity/u);
  await expect(streamedItems.nth(2)).toHaveClass(/agent-message-assistant/u);

  localAgentSidebar.releaseResponsePhase("later_delta", "codex");
  await expect(streamedItems.nth(1)).toHaveClass(/agent-tool-activity/u);
  localAgentSidebar.releaseResponsePhase("completion", "codex");
  await expect(page.locator(".agent-live-status")).toHaveText("Connected");
  await expect(streamedItems.nth(0)).toHaveClass(/agent-message-assistant/u);
  await expect(streamedItems.nth(1)).toHaveClass(/agent-tool-activity/u);
  await expect(streamedItems.nth(2)).toHaveClass(/agent-message-assistant/u);
});

test("keeps completed compaction in its original transcript position", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Compaction order\n", creatorContext: "Compaction order context.\n", preferences: { "agent-whiteboard-agent-provider": "codex" } });
  await connectSidebar(page, "codex");
  const composer = page.getByLabel("Message Codex about this whiteboard");
  await composer.fill("/compact");
  await composer.press("Enter");
  await expect(page.locator(".agent-compaction-row")).toContainText("Compacting context…");
  localAgentSidebar.completeCompact("completed", "codex");
  await expect(page.locator(".agent-compaction-row")).toContainText("Context compacted");
  await composer.fill("What is this page about?");
  await composer.press("Enter");
  await expect(page.locator(".agent-message-assistant")).toContainText(/fixture reply/iu);
  const order = await page.locator(".agent-timeline").evaluate((timeline) => [...timeline.children].map((element) => {
    if (element.classList.contains("agent-compaction-row")) return "compaction";
    if (element.classList.contains("agent-message-user")) return "user";
    if (element.classList.contains("agent-message-assistant")) return "assistant";
    return "other";
  }));
  expect(order.indexOf("compaction")).toBeLessThan(order.indexOf("user"));
  expect(order.indexOf("compaction")).toBeLessThan(order.indexOf("assistant"));
});

test("keeps the keyboard-selected skill visible while navigating the full list", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Skill navigation\n", creatorContext: "Skill navigation context.\n", preferences: { "agent-whiteboard-agent-provider": "codex" } });
  localAgentSidebar.setSkills(Array.from({ length: 14 }, (_, index) => ({
    id: `${String(index).padStart(31, "A")}B`,
    name: `skill-${String(index + 1).padStart(2, "0")}`,
    display_name: `Skill ${String(index + 1).padStart(2, "0")}`,
    description: `Description for skill ${index + 1}`,
    scope: "user",
  })));
  await connectSidebar(page, "codex");
  const composer = page.getByLabel("Message Codex about this whiteboard");
  await composer.fill("$");
  const suggestions = page.getByRole("listbox", { name: "Composer suggestions" });
  await expect(suggestions).toBeVisible();
  await expect(suggestions.locator(".agent-completion-option")).toHaveCount(14);
  for (let index = 0; index < 13; index += 1) await composer.press("ArrowDown");
  const active = suggestions.locator('[aria-selected="true"]');
  await expect(active).toContainText("Skill 14");
  const [suggestionsBox, activeBox, scrollTop] = await Promise.all([
    suggestions.boundingBox(),
    active.boundingBox(),
    suggestions.evaluate((element) => element.scrollTop),
  ]);
  expect(suggestionsBox).not.toBeNull();
  expect(activeBox).not.toBeNull();
  expect(activeBox.y).toBeGreaterThanOrEqual(suggestionsBox.y);
  expect(activeBox.y + activeBox.height).toBeLessThanOrEqual(suggestionsBox.y + suggestionsBox.height + 1);
  expect(scrollTop).toBeGreaterThan(0);
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

test("keeps queue submission and keyboard Stop available with Enter and Shift+Enter", async ({
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
  await expect(composer).toHaveText("Line one\n");
  await composer.fill("Keep this turn active.");
  await composer.press("Enter");
  const stop = page.getByRole("button", { name: "Stop", exact: true });
  await expect(stop).toBeEnabled();
  await expect(stop.locator(".agent-stop-icon")).toBeVisible();
  await expect(stop).toHaveText("");
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
  await expect(page.locator(".agent-drawer")).toBeVisible();
  const interrupted = page.locator(".agent-activity-interruption");
  await expect(interrupted).toContainText("Response stopped");
  await expect(interrupted).toContainText("not replayed automatically");
  await expect(interrupted.locator(".agent-status-notice-icon svg")).toBeVisible();
  expect(await interrupted.evaluate((element) => element.tagName)).toBe("DIV");

  const types = parsedCommands(localAgentSidebar.brokerRequests).map((command) => command.type);
  expect(types).toEqual(expect.arrayContaining(["submit", "queue_edit", "queue_remove", "interrupt"]));
  expect(types.filter((type) => type === "submit")).toHaveLength(2);
  expect(types.filter((type) => type === "interrupt")).toHaveLength(1);
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
  await expect(composer).toHaveText("Queued with a pasted image.");
  await expect(page.locator('.agent-attachment-preview[data-state="ready"]')).toHaveCount(1);
  await composer.press("Enter");
  await expect(page.getByLabel("Edit queued message")).toHaveText("Queued with a pasted image.");
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
  expect(submits[0].payload).toMatchObject({ content: { parts: [] }, images: [{ name: "diagram.png" }, { name: "photo.jpg" }] });
  expect(submits[1].payload).toMatchObject({ content: { parts: [{ type: "text", text: "Queued with a pasted image." }] }, images: [{ name: "paste.webp" }] });
});

test("keeps assistant Markdown emphasis inline and separate from the author label", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({
    context,
    page,
    fixture: localAgentSidebar,
    markdown: "# Inline assistant Markdown\n",
    creatorContext: "Assistant Markdown structure context.\n",
  });
  localAgentSidebar.setResponseText("This uses **Page agent**, **Pi**, and **Codex** inline.\n\n```go\nfunc ready() bool { return true }\n```");
  await connectSidebar(page);
  const composer = page.getByLabel("Message Pi about this whiteboard");
  await composer.fill("Render inline emphasis.");
  await composer.press("Enter");

  const assistant = page.locator(".agent-message-assistant").last();
  const author = assistant.locator(":scope > .agent-message-author");
  const emphasis = assistant.locator(".agent-message-body strong");
  await expect(author).toHaveText("Pi");
  await expect(emphasis).toHaveText(["Page agent", "Pi", "Codex"]);
  await expect(author).toHaveCSS("display", "block");
  for (const element of await emphasis.all()) await expect(element).toHaveCSS("display", "inline");
  await expect(assistant.locator(".agent-message-body")).toContainText("This uses Page agent, Pi, and Codex inline.");
  const code = assistant.locator("pre code.language-go");
  await expect(code).toHaveClass(/hljs/u);
  await expect(code.locator(".hljs-keyword").first()).toContainText("func");
});

test("explains when the selected model cannot accept images", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Text model\n", creatorContext: "Text only.\n" });
  localAgentSidebar.setSupportsImages("pi", false);
  await connectSidebar(page);
  const imageButton = page.getByRole("button", { name: "Add images" });
  await expect(imageButton.locator("svg")).toHaveCount(1);
  await expect(imageButton.locator("svg")).toHaveAttribute("aria-hidden", "true");
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
  const responseDots = page.locator(".agent-response-dot");
  await expect(responseDots).toHaveCount(3);
  await page.emulateMedia({ reducedMotion: "reduce" });
  await expect(responseDots.first()).toHaveCSS("animation-name", "agent-response-dance");
  const dotMotion = await responseDots.first().evaluate((element) => element.getAnimations()[0].effect.getKeyframes().map(({ transform }) => new DOMMatrixReadOnly(transform).m42));
  expect(Math.min(...dotMotion)).toBeLessThan(-1);
  expect(Math.max(...dotMotion)).toBeGreaterThan(1);
  await expect(responseDots.nth(1)).toHaveCSS("animation-delay", "-0.16s");
  await expect(responseDots.nth(2)).toHaveCSS("animation-delay", "-0.32s");
  const sampledMotion = await responseDots.evaluateAll(async (elements) => {
    const samples = elements.map(() => []);
    for (let frame = 0; frame < 18; frame += 1) {
      elements.forEach((element, index) => samples[index].push(element.getBoundingClientRect().y));
      await new Promise((resolve) => setTimeout(resolve, 40));
    }
    return samples.map((values) => Math.max(...values) - Math.min(...values));
  });
  expect(sampledMotion.every((distance) => distance > 3)).toBe(true);
  await expect(page.locator(".agent-response-text")).toHaveCSS("margin-right", "8px");
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
  await expect(page.locator(".agent-activity-blocked strong")).toHaveText(["Tool request blocked", "Permission request blocked"]);
  await expect(page.locator(".agent-activity-blocked").first()).toContainText("content-only policy");
  await expect(page.locator(".agent-activity-blocked").first()).toHaveAttribute("data-tone", "warning");
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

for (const provider of ["pi", "cursor"]) {
  test(`${provider} sends exact initial context once and resumes without replaying it`, async ({
    context,
    page,
    localAgentSidebar,
  }) => {
  const markdown = "# Exact context\n\nUTF-8: café\n";
  const creatorContext = "Creator says: preserve this exactly.\n";
  const resource = await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown, creatorContext, preferences: { "agent-whiteboard-agent-provider": provider } });
  await connectSidebar(page, provider);
  const label = providerFixtures[provider].label;
  const reply = provider === "cursor" ? "Cursor fixture reply" : "Fixture reply";

  await page.getByLabel(`Message ${label} about this whiteboard`).fill("What does this page say?");
  await page.getByLabel(`Message ${label} about this whiteboard`).press("Enter");
  await expect(page.locator(".agent-message-assistant")).toContainText(reply);

  const firstSubmit = parsedCommands(localAgentSidebar.brokerRequests).find((command) => command.type === "submit");
  expect(firstSubmit.payload.content).toEqual({ parts: [{ type: "text", text: "What does this page say?" }] });
  expect(firstSubmit.payload.context).toMatchObject({
    revision: "initial",
    source: markdown,
    creator_context: creatorContext,
    title: "Exact context",
    url: resource.url,
  });

  localAgentSidebar.resetBrokerRequests();
  await page.reload();
  await expect(page.locator(".agent-live-status")).toHaveText(`${label} ready`);
  await expect(page.locator(".agent-drawer")).toHaveClass(/is-open/u);
  await page.getByRole("button", { name: `Connect to ${label}`, exact: true }).click();
  await expect(page.locator(".agent-message-assistant")).toContainText(reply);
  await page.getByLabel(`Message ${label} about this whiteboard`).fill("Continue without repeating context.");
  await page.getByLabel(`Message ${label} about this whiteboard`).press("Enter");
  await expect(page.locator(".agent-message-assistant")).toHaveCount(2);

  const resumedSubmit = parsedCommands(localAgentSidebar.brokerRequests).find((command) => command.type === "submit");
  expect(resumedSubmit.payload.content).toEqual({ parts: [{ type: "text", text: "Continue without repeating context." }] });
  expect(resumedSubmit.payload).not.toHaveProperty("context");
  const stored = await page.evaluate(() => Object.fromEntries(Object.entries(localStorage)));
  expect(stored[portKey]).toBe(String(localAgentSidebar.brokerPort));
  expect(stored[drawerKey]).toBe("true");
  const allowedPreferenceKeys = new Set(["agent-whiteboard-theme", drawerKey, portKey, widthKey, "agent-whiteboard-agent-provider", `agent-whiteboard-${provider}-settings-v1`]);
  expect(Object.keys(stored).every((key) => allowedPreferenceKeys.has(key))).toBe(true);
  expect(JSON.stringify(stored)).not.toContain("What does this page say?");
  expect(JSON.stringify(stored)).not.toContain(creatorContext);
  });
}

for (const [provider, metadata] of Object.entries(providerFixtures)) {
  test(`selects, connects, and verifies isolated ${metadata.label} model state`, async ({ context, page, localAgentSidebar }) => {
    await openSidebarPage({
      context, page, fixture: localAgentSidebar, markdown: `# ${metadata.label} provider`, creatorContext: "Shared provider fixture context.\n",
    });
    await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
    await page.getByLabel("Conversation provider").selectOption(provider);
    await expect(page.locator(".agent-drawer-header")).toContainText(`${metadata.label} ready`);
    await connectSidebar(page, provider);
    await expect(page.locator(".agent-provider-label")).toContainText(metadata.model);
    const connect = parsedCommands(localAgentSidebar.brokerRequests).find((command) => command.type === "connect");
    expect(connect.payload.provider).toBe(provider);
    expect(await page.evaluate(() => localStorage.getItem("agent-whiteboard-agent-provider"))).toBe(provider === "pi" ? null : provider);
  });
}

test("switches providers silently and isolates Pi, Codex, and Cursor conversations", async ({
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

  await page.getByLabel("Conversation provider").selectOption("cursor");
  await expect(page.locator(".agent-live-status")).toHaveText("Cursor ready");
  await connectSidebar(page, "cursor");
  await page.getByLabel("Message Cursor about this whiteboard").fill("Answer only in the Cursor conversation.");
  await page.getByLabel("Message Cursor about this whiteboard").press("Enter");
  await expect(page.locator(".agent-message-assistant")).toContainText("Cursor fixture reply");
  await expect(page.locator(".agent-timeline")).not.toContainText("Answer only in the Pi conversation.");
  await expect(page.locator(".agent-timeline")).not.toContainText("Keep the Codex turn active.");

  await page.getByLabel("Conversation provider").selectOption("codex");
  await expect(page.locator(".agent-provider-label")).toContainText("5.6 Sol");
  await expect(page.locator(".agent-message-user")).toContainText("Keep the Codex turn active.");
  await expect(page.locator(".agent-timeline")).not.toContainText("Answer only in the Pi conversation.");
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeEnabled();

  const commands = parsedCommands(localAgentSidebar.brokerRequests);
  const connects = commands.filter((command) => command.type === "connect");
  expect(connects.map((command) => command.payload.provider)).toEqual(["codex", "pi", "cursor"]);
  const commandText = (command) => command.payload.content.parts.filter((part) => part.type === "text").map((part) => part.text).join("");
  const codexSubmit = commands.find((command) => command.type === "submit" && commandText(command).includes("Codex"));
  const piSubmit = commands.find((command) => command.type === "submit" && commandText(command).includes("Pi"));
  const cursorSubmit = commands.find((command) => command.type === "submit" && commandText(command).includes("Cursor"));
  expect(new Set([codexSubmit.conversation_id, piSubmit.conversation_id, cursorSubmit.conversation_id]).size).toBe(3);
});

test("hides archive deletion for Cursor while preserving restore and list", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Cursor archives\n", creatorContext: "Cursor archive context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("cursor");
  await connectSidebar(page, "cursor");
  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "Archives" }).click();
  const archive = page.locator(".agent-archive-card");
  await expect(archive).toContainText("Cursor · Cursor Small");
  await expect(archive.getByRole("button", { name: "Restore" })).toBeVisible();
  await expect(archive.getByRole("button", { name: "Delete" })).toHaveCount(0);
  expect(parsedCommands(localAgentSidebar.brokerRequests).filter(({ type }) => type === "archive_delete")).toHaveLength(0);
});

test("uses Cursor capabilities without copied skill, compact, or archive-delete controls", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Cursor capabilities\n", creatorContext: "Cursor capability context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("cursor");
  await connectSidebar(page, "cursor");
  const composer = page.getByLabel("Message Cursor about this whiteboard");
  const suggestions = page.getByRole("listbox", { name: "Composer suggestions" });

  await composer.fill("$");
  await expect(suggestions).toBeHidden();
  await composer.fill("/co");
  await expect(suggestions).toBeHidden();
  await composer.fill("");

  const picker = page.locator(".agent-image-picker");
  await expect(page.getByRole("button", { name: "Add images" })).toBeEnabled();
  await picker.setInputFiles({ name: "cursor.png", mimeType: "image/png", buffer: Buffer.from([137, 80, 78, 71]) });
  await expect(page.locator('.agent-attachment-preview[data-state="ready"]')).toHaveCount(1);
  await page.locator('.agent-composer button[type="submit"]').click();
  await expect(page.locator(".agent-message-user .agent-message-images img")).toHaveCount(1);
  const upload = localAgentSidebar.brokerRequests.find((request) => request.method === "POST" && request.url === "/api/v1/agent/images");
  expect(upload.headers["x-agent-whiteboard-provider"]).toBe("cursor");
  expect((await recordedCommand(localAgentSidebar, "submit")).payload.images).toEqual([{ image_id: expect.any(String), name: "cursor.png" }]);
  expect(parsedCommands(localAgentSidebar.brokerRequests).some(({ type }) => type === "compact" || type === "archive_delete")).toBe(false);
});

test("shows Cursor working after transport send and before correlated acceptance", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Cursor delivery\n", creatorContext: "Cursor delivery context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("cursor");
  localAgentSidebar.setWebSocketEnabled(true);
  await connectSidebar(page, "cursor");
  localAgentSidebar.holdNextSubmit("cursor");

  const composer = page.getByLabel("Message Cursor about this whiteboard");
  await composer.fill("Show this immediately.");
  await composer.press("Enter");

  await expect(composer).toBeEmpty();
  const pending = page.locator('.agent-message-pending[data-state="waiting"]');
  await expect(pending).toContainText("Show this immediately.");
  await expect(pending.locator(".agent-message-delivery")).toHaveCount(0);
  await expect(page.locator(".agent-live-status")).toHaveText("Waiting for Cursor");
  await expect(page.locator(".agent-response-loading")).toContainText("Cursor is working");
  await expect(page.locator(".agent-response-dot")).toHaveCount(3);
  expect(localAgentSidebar.webSocketCommands.filter(({ type }) => type === "submit")).toHaveLength(1);

  localAgentSidebar.resolveHeldSubmit("accepted", "cursor");
  await expect(page.locator(".agent-message-pending")).toHaveCount(0);
  await expect(page.locator(".agent-message-user").last()).toContainText("Show this immediately.");
  expect(localAgentSidebar.webSocketCommands.filter(({ type }) => type === "submit")).toHaveLength(1);
});

test("retains a rejected Cursor submit without overwriting a newer draft", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Cursor rejection\n", creatorContext: "Cursor rejection context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("cursor");
  localAgentSidebar.setWebSocketEnabled(true);
  await connectSidebar(page, "cursor");
  localAgentSidebar.holdNextSubmit("cursor");

  const composer = page.getByLabel("Message Cursor about this whiteboard");
  await composer.fill("Rejected exact payload.");
  await composer.press("Enter");
  await composer.fill("Newer draft stays here.");
  localAgentSidebar.resolveHeldSubmit("rejected", "cursor");

  await expect(composer).toHaveText("Newer draft stays here.");
  const rejected = page.locator('.agent-message-pending[data-state="rejected"]');
  await expect(rejected).toContainText("Rejected exact payload.");
  await expect(rejected.locator(".agent-message-delivery > span")).toHaveText("Not sent");
  const restore = rejected.getByRole("button", { name: "Restore draft" });
  await expect(restore).toBeVisible();
  await composer.fill("");
  await restore.click();
  await expect(composer).toHaveText("Rejected exact payload.");
  await expect(page.locator(".agent-message-pending")).toHaveCount(0);
});

test("confirms indeterminate Cursor delivery and recovers without resubmitting", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Cursor reconnect\n", creatorContext: "Cursor reconnect context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("cursor");
  localAgentSidebar.setWebSocketEnabled(true);
  await connectSidebar(page, "cursor");
  localAgentSidebar.holdNextSubmit("cursor");

  const composer = page.getByLabel("Message Cursor about this whiteboard");
  await composer.fill("Recover this once.");
  await composer.press("Enter");
  localAgentSidebar.setProviderAvailable("cursor", false);
  localAgentSidebar.disconnectProvider("cursor");

  const confirming = page.locator('.agent-message-pending[data-state="confirming"]');
  await expect(page.locator(".agent-live-status")).toHaveText("Broker unavailable");
  await expect(confirming).toBeVisible();
  await expect(confirming).toContainText("Recover this once.");
  await expect(confirming.locator(".agent-message-delivery")).toHaveText("Confirming delivery…");
  expect(localAgentSidebar.webSocketCommands.filter(({ type }) => type === "submit")).toHaveLength(1);

  localAgentSidebar.resolveHeldSubmit("accepted", "cursor");
  localAgentSidebar.setProviderAvailable("cursor", true);
  await expect(page.locator(".agent-live-status")).toHaveText("Connected", { timeout: 10_000 });
  await expect(page.locator(".agent-message-pending")).toHaveCount(0);
  await expect(page.locator(".agent-message-user").last()).toContainText("Recover this once.");
  expect(localAgentSidebar.webSocketCommands.filter(({ type }) => type === "submit")).toHaveLength(1);
});

test("uses accessible Codex model controls and captures accepted exact settings", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Codex controls\n", creatorContext: "Model controls context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("codex");
  await connectSidebar(page, "codex");

  const pill = page.locator(".agent-model-pill");
  await expect(pill).toHaveAccessibleName("Model 5.6 Sol, effort High, speed Fast");
  await expect(pill.locator(".agent-model-pill-fast")).toHaveText("⚡");
  await pill.focus();
  await pill.press("Enter");
  const menu = page.locator(".agent-model-menu");
  await expect(menu).toBeVisible();
  const [pillBox, menuBox, drawerBox] = await Promise.all([
    pill.boundingBox(),
    menu.boundingBox(),
    page.locator(".agent-drawer").boundingBox(),
  ]);
  expect(pillBox).not.toBeNull();
  expect(menuBox).not.toBeNull();
  expect(drawerBox).not.toBeNull();
  const pillCenter = pillBox.x + pillBox.width / 2;
  expect(pillCenter).toBeGreaterThanOrEqual(menuBox.x);
  expect(pillCenter).toBeLessThanOrEqual(menuBox.x + menuBox.width);
  expect(menuBox.x).toBeGreaterThanOrEqual(drawerBox.x);
  expect(menuBox.x + menuBox.width).toBeLessThanOrEqual(drawerBox.x + drawerBox.width + 1);
  await expect(menu.locator('[data-settings-section="model"]')).toBeFocused();
  await menu.locator('[data-settings-section="model"]').press("ArrowRight");
  const luna = menu.locator('[data-settings-value="gpt-5.6-luna"]');
  await expect(luna).toHaveAttribute("aria-disabled", "true");
  await expect(luna).toContainText("Choose a supported effort and Standard speed first.");
  await menu.press("Escape");
  await expect(pill).toBeFocused();

  await pill.click();
  await menu.locator('[data-settings-section="effort"]').click();
  await menu.locator('[data-settings-value="xhigh"]').click();
  await menu.locator('[data-settings-section="speed"]').click();
  await menu.locator('[data-settings-value="standard"]').click();
  await expect(pill).toHaveAccessibleName("Model 5.6 Sol, effort Extra high, speed Standard");
  const composer = page.getByLabel("Message Codex about this whiteboard");
  await composer.fill("Accept this tuple.");
  await composer.press("Enter");

  await expect.poll(() => localAgentSidebar.acceptedSettings()).toEqual([{
    turn_id: expect.any(String),
    settings: { model: "gpt-5.6-sol", effort: "xhigh", speed: "standard" },
  }]);
  expect(parsedCommands(localAgentSidebar.brokerRequests).find((command) => command.type === "submit")?.payload.settings).toEqual({ model: "gpt-5.6-sol", effort: "xhigh", speed: "standard" });
  await expect.poll(() => page.evaluate((key) => JSON.parse(localStorage.getItem(key)), codexSettingsKey)).toEqual({ model: "gpt-5.6-sol", effort: "xhigh", speed: "standard" });
});

test("searches a large Cursor variant catalog with keyboard focus and contained layout", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Cursor variants\n", creatorContext: "Large catalog context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("cursor");
  await connectSidebar(page, "cursor");
  const catalog = Array.from({ length: 155 }, (_, index) => ({
    model: index === 0 ? "cursor-small" : `cursor-variant-${index}`,
    model_display_name: index === 0 ? "Cursor Small" : index === 87 ? "Native Fast High 87" : `Cursor Variant ${index}`,
    description: `Cursor catalog variant ${index}.`,
    default_effort: "default",
    supported_reasoning_efforts: [{ effort: "default", description: "Provider-managed reasoning." }],
    supports_images: true,
    default: index === 0,
    supports_fast: false,
  })).reverse();
  localAgentSidebar.refreshCatalog(catalog, "cursor");

  const pill = page.locator(".agent-model-pill");
  await expect(pill).toHaveAccessibleName("Model Cursor Small");
  await pill.click();
  const menu = page.locator(".agent-model-menu");
  await menu.locator('[data-settings-section="model"]').press("ArrowRight");
  const filter = page.getByLabel("Filter models");
  await expect(filter).toBeFocused();
  await expect(filter.locator("..")).toHaveClass(/agent-model-filter-shell/);
  await expect(filter).toHaveCSS("outline-style", "none");
  const sortedValues = ["cursor-small", ...Array.from({ length: 154 }, (_, index) => index + 1).filter((index) => index !== 87).map((index) => `cursor-variant-${index}`), "cursor-variant-87"];
  await expect.poll(() => menu.locator('[role="menuitemradio"]').evaluateAll((rows) => rows.map((row) => row.dataset.settingsValue))).toEqual(sortedValues);
  await filter.fill("fast high");
  await expect(menu.locator('[role="menuitemradio"]:visible')).toHaveCount(1);
  await expect(menu.locator('[role="menuitemradio"]:visible')).toContainText("Native Fast High 87");
  await filter.press("ArrowDown");
  await expect(menu.locator('[data-settings-value="cursor-variant-87"]')).toBeFocused();
  await filter.focus();
  await filter.fill("cursor-variant-154");
  await expect(menu.locator('[role="menuitemradio"]:visible')).toHaveCount(1);
  await filter.fill("does-not-exist");
  await expect(menu.getByRole("status")).toHaveText("No models found.");
  const [menuBox, drawerBox] = await Promise.all([menu.boundingBox(), page.locator(".agent-drawer").boundingBox()]);
  expect(menuBox.x).toBeGreaterThanOrEqual(drawerBox.x);
  expect(menuBox.x + menuBox.width).toBeLessThanOrEqual(drawerBox.x + drawerBox.width + 1);
  await page.setViewportSize({ width: 390, height: 720 });
  const narrowBox = await menu.boundingBox();
  expect(narrowBox.x).toBeGreaterThanOrEqual(0);
  expect(narrowBox.x + narrowBox.width).toBeLessThanOrEqual(390);
  await filter.press("Escape");
  await expect(pill).toBeFocused();
});

for (const provider of ["codex", "cursor"]) {
  test(`preserves a busy ${provider} draft without queue admission`, async ({ context, page, localAgentSidebar }) => {
    const label = providerFixtures[provider].label;
    await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: `# ${label} queue\n`, creatorContext: "Queue settings context.\n" });
    await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
    await page.getByLabel("Conversation provider").selectOption(provider);
    localAgentSidebar.setHoldResponses(true, provider);
    await connectSidebar(page, provider);
    const pill = page.locator(".agent-model-pill");
    const menu = page.locator(".agent-model-menu");
    const composer = page.getByLabel(`Message ${label} about this whiteboard`);

    await composer.fill("Active turn.");
    await composer.press("Enter");
    await pill.click();
    if (provider === "codex") {
      await menu.locator('[data-settings-section="speed"]').click();
      await menu.locator('[data-settings-value="standard"]').click();
    } else {
      await menu.locator('[data-settings-section="model"]').click();
      await menu.locator('[data-settings-value="cursor-large"]').click();
    }
    await composer.fill("Preserved next turn.");
    await composer.press("Enter");
    if (provider === "codex") {
      if (await menu.isVisible()) await menu.press("Escape");
      await pill.click();
      await menu.locator('[data-settings-section="effort"]').click();
      await menu.locator('[data-settings-value="xhigh"]').click();
      await composer.fill("Preserved xhigh turn.");
      await composer.press("Enter");
    }

    await expect(page.locator(".agent-queue-settings")).toHaveCount(0);
    expect(parsedCommands(localAgentSidebar.brokerRequests).filter((command) => command.type === "submit")).toHaveLength(1);
    const preservedDraft = provider === "codex" ? "Preserved xhigh turn." : "Preserved next turn.";
    await expect(composer).toHaveText(preservedDraft);
    await page.getByRole("button", { name: "Stop", exact: true }).click();
    await expect(composer).toHaveText(preservedDraft);
    await expect(pill).toBeEnabled();
    await expect(pill).toHaveAccessibleName(provider === "codex"
      ? "Model 5.6 Sol, effort Extra high, speed Standard"
      : "Model Cursor Large");
    if (await menu.isVisible()) await menu.press("Escape");
  });
}

test("renders archive loading, populated, and empty states without false emptiness", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Archive states\n", creatorContext: "Archive context.\n" });
  await connectSidebar(page);
  localAgentSidebar.setArchiveDelay(true);
  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "Archives" }).click();
  await expect(page.getByRole("status")).toContainText("Loading archived conversations");
  await expect(page.getByText("No archived conversations")).toHaveCount(0);
  localAgentSidebar.releaseArchiveList();
  const archive = page.locator(".agent-archive-card");
  await expect(archive).toContainText("Pi · fixture-model");
  await expect(archive).toContainText("Updated Jul 26, 2026");
  const [restoreBox, deleteBox] = await Promise.all([
    archive.getByRole("button", { name: "Restore" }).boundingBox(),
    archive.getByRole("button", { name: "Delete" }).boundingBox(),
  ]);
  expect(restoreBox).not.toBeNull();
  expect(deleteBox).not.toBeNull();
  expect(restoreBox.height).toBeLessThanOrEqual(34);
  expect(deleteBox.height).toBeLessThanOrEqual(34);

  await page.getByRole("button", { name: "Back to conversation" }).click();
  localAgentSidebar.setArchiveMode("empty");
  localAgentSidebar.setArchiveDelay(false);
  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "Archives" }).click();
  await expect(page.getByText("No archived conversations")).toBeVisible();
  await expect(page.locator(".agent-archives")).toContainText("Conversations you replace or restore will appear here.");

  await page.getByRole("button", { name: "Back to conversation" }).click();
  localAgentSidebar.setArchiveMode("paginated");
  localAgentSidebar.setArchiveDelay(false);
  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "Archives" }).click();
  await expect(page.locator(".agent-archive-card")).toHaveCount(1);
  localAgentSidebar.setArchiveDelay(true);
  await page.getByRole("button", { name: "Load more archives" }).click();
  await expect(page.locator(".agent-archive-card")).toHaveCount(1);
  await expect(page.getByRole("button", { name: "Loading…" })).toBeDisabled();
  localAgentSidebar.releaseArchiveList();
  await expect(page.locator(".agent-archive-card")).toHaveCount(2);
});

test("uses styled confirmations for New, Restore, and Delete with exact commands", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Confirmations\n", creatorContext: "Confirmation context.\n" });
  await connectSidebar(page);
  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "New conversation" }).click();
  let dialog = page.getByRole("dialog", { name: "Start a new conversation?" });
  await expect(dialog.getByRole("button", { name: "Cancel" })).toBeFocused();
  await dialog.press("Escape");
  expect(parsedCommands(localAgentSidebar.brokerRequests).filter(({ type }) => type === "new")).toHaveLength(0);

  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "Archives" }).click();
  const archive = page.locator(".agent-archive-card");
  await archive.getByRole("button", { name: "Restore" }).click();
  dialog = page.getByRole("dialog", { name: "Restore this conversation?" });
  await dialog.getByRole("button", { name: "Cancel" }).click();
  expect(parsedCommands(localAgentSidebar.brokerRequests).filter(({ type }) => type === "archive_restore")).toHaveLength(0);
  await archive.getByRole("button", { name: "Restore" }).click();
  await page.getByRole("dialog", { name: "Restore this conversation?" }).getByRole("button", { name: "Restore" }).click();
  await expect.poll(() => parsedCommands(localAgentSidebar.brokerRequests).filter(({ type }) => type === "archive_restore")).toHaveLength(1);

  await page.getByRole("button", { name: "Back to conversation" }).click();
  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "Archives" }).click();
  await page.locator(".agent-archive-card").getByRole("button", { name: "Delete" }).click();
  dialog = page.getByRole("dialog", { name: "Delete this archive permanently?" });
  await expect(dialog).toContainText("cannot be recovered");
  await dialog.getByRole("button", { name: "Delete" }).click();
  await expect.poll(() => parsedCommands(localAgentSidebar.brokerRequests).filter(({ type }) => type === "archive_delete")).toHaveLength(1);
});

test("keeps confirmation focus exclusive in the docked drawer", async ({ context, page, localAgentSidebar }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Docked confirmation\n", creatorContext: "Focus context.\n" });
  await connectSidebar(page);
  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "New conversation" }).click();
  const dialog = page.getByRole("dialog", { name: "Start a new conversation?" });
  const cancel = dialog.getByRole("button", { name: "Cancel" });
  const confirm = dialog.getByRole("button", { name: "Start new" });
  await expect(cancel).toBeFocused();
  await cancel.press("Shift+Tab");
  await expect(confirm).toBeFocused();
  await confirm.press("Tab");
  await expect(cancel).toBeFocused();
  await expect(page.locator(".agent-drawer > [inert]")).not.toHaveCount(0);
  await cancel.press("Escape");
  await expect(dialog).toBeHidden();
  await expect(page.locator(".agent-drawer")).toHaveClass(/is-open/u);
  expect(parsedCommands(localAgentSidebar.brokerRequests).filter(({ type }) => type === "new")).toHaveLength(0);
});

test("keeps confirmation focus exclusive in the narrow modal drawer", async ({ context, page, localAgentSidebar }) => {
  await page.setViewportSize({ width: 390, height: 720 });
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Narrow confirmation\n", creatorContext: "Focus context.\n" });
  await connectSidebar(page);
  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "New conversation" }).click();
  const dialog = page.getByRole("dialog", { name: "Start a new conversation?" });
  const cancel = dialog.getByRole("button", { name: "Cancel" });
  const confirm = dialog.getByRole("button", { name: "Start new" });
  await expect(cancel).toBeFocused();
  await cancel.press("Shift+Tab");
  await expect(confirm).toBeFocused();
  await confirm.press("Tab");
  await expect(cancel).toBeFocused();
  await expect(page.locator(".agent-drawer > [inert]")).not.toHaveCount(0);
  await cancel.press("Escape");
  await expect(dialog).toBeHidden();
  await expect(page.locator(".agent-drawer")).toHaveClass(/is-open/u);
  expect(parsedCommands(localAgentSidebar.brokerRequests).filter(({ type }) => type === "new")).toHaveLength(0);
});

test("uses the visible Codex pill for New without updating accepted preference", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Codex New\n", creatorContext: "New conversation context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("codex");
  await connectSidebar(page, "codex");
  const pill = page.locator(".agent-model-pill");
  const menu = page.locator(".agent-model-menu");
  await pill.click();
  await menu.locator('[data-settings-section="effort"]').click();
  await menu.locator('[data-settings-value="xhigh"]').click();
  await menu.locator('[data-settings-section="speed"]').click();
  await menu.locator('[data-settings-value="standard"]').click();
  await menu.press("Escape");
  await page.getByRole("button", { name: "Open Page agent menu" }).click();
  await page.getByRole("menuitem", { name: "New conversation" }).click();
  const confirmation = page.getByRole("dialog", { name: "Start a new conversation?" });
  await expect(confirmation).toContainText("remain available in Archives");
  await confirmation.getByRole("button", { name: "Start new" }).click();
  await expect.poll(() => localAgentSidebar.createdSettings()).toEqual([{ model: "gpt-5.6-sol", effort: "xhigh", speed: "standard" }]);
  await expect(page.locator(".agent-live-status")).toHaveText("Connected", { timeout: 5_000 });
  await expect(page.locator(".agent-provider-label")).toContainText("5.6 Sol");
  await expect(page.locator(".agent-model-pill")).toHaveAccessibleName("Model 5.6 Sol, effort Extra high, speed Standard");
  const connects = parsedCommands(localAgentSidebar.brokerRequests).filter((command) => command.type === "connect" && command.payload.provider === "codex");
  expect(connects.at(-1).conversation_id).toBeNull();
  await expect.poll(() => page.evaluate((key) => localStorage.getItem(key), codexSettingsKey)).toBeNull();
});

test("discards an unsent Codex draft on reload", async ({ context, page, localAgentSidebar }) => {
  await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Codex reload\n", creatorContext: "Reload context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("codex");
  await connectSidebar(page, "codex");
  const pill = page.locator(".agent-model-pill");
  const menu = page.locator(".agent-model-menu");
  await pill.click();
  await menu.locator('[data-settings-section="speed"]').click();
  await menu.locator('[data-settings-value="standard"]').click();
  await expect(pill).toHaveAccessibleName("Model 5.6 Sol, effort High, speed Standard");
  await page.reload();
  await expect(page.locator(".agent-live-status")).toHaveText("Codex ready");
  await connectSidebar(page, "codex");
  await expect(page.locator(".agent-model-pill")).toHaveAccessibleName("Model 5.6 Sol, effort High, speed Fast");
  await expect.poll(() => page.evaluate((key) => localStorage.getItem(key), codexSettingsKey)).toBeNull();
});

test("synchronizes accepted Codex settings across tabs without overwriting a later draft", async ({ context, page, localAgentSidebar }) => {
  const resource = await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Codex tabs\n", creatorContext: "Cross-tab settings context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("codex");
  await connectSidebar(page, "codex");
  const otherPage = await context.newPage();
  await otherPage.goto(resource.url);
  await expect(otherPage.locator(".agent-live-status")).toHaveText("Codex ready");
  await connectSidebar(otherPage, "codex");

  const otherPill = otherPage.locator(".agent-model-pill");
  const otherMenu = otherPage.locator(".agent-model-menu");
  await otherPill.click();
  await otherMenu.locator('[data-settings-section="speed"]').click();
  await otherMenu.locator('[data-settings-value="standard"]').click();
  const composer = page.getByLabel("Message Codex about this whiteboard");
  await composer.fill("Accept fast in both tabs.");
  await composer.press("Enter");
  await expect(otherPill).toHaveAccessibleName("Model 5.6 Sol, effort High, speed Standard");
  await expect.poll(() => otherPage.evaluate((key) => JSON.parse(localStorage.getItem(key)), codexSettingsKey)).toEqual({ model: "gpt-5.6-sol", effort: "high", speed: "fast" });
  await otherPage.close();
});

test("applies accepted browser preference only to a new Codex mapping", async ({ context, page, localAgentSidebar }) => {
  const preference = JSON.stringify({ model: "gpt-5.6-luna", effort: "medium", speed: "standard" });
  await openSidebarPage({
    context,
    page,
    fixture: localAgentSidebar,
    markdown: "# Codex preference\n",
    creatorContext: "Preference context.\n",
    preferences: { [codexSettingsKey]: preference },
  });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("codex");
  await connectSidebar(page, "codex");
  await expect(page.locator(".agent-model-pill")).toHaveAccessibleName("Model 5.6 Sol, effort High, speed Fast");
  expect(parsedCommands(localAgentSidebar.brokerRequests).find((command) => command.type === "connect")?.payload.settings).toEqual({ model: "gpt-5.6-luna", effort: "medium", speed: "standard" });

  localAgentSidebar.restoreSettings({ model: "gpt-5.6-luna", effort: "medium", speed: "standard" });
  await expect(page.locator(".agent-model-pill")).toHaveAccessibleName("Model 5.6 Luna, effort Medium, speed Standard");
  await expect(page.getByRole("button", { name: "Add images" })).toBeDisabled();
  await expect.poll(() => page.evaluate((key) => localStorage.getItem(key), codexSettingsKey)).toBe(preference);

  const newResource = await localAgentSidebar.publish("# New Codex mapping\n", "New mapping context.\n");
  localAgentSidebar.resetBrokerRequests();
  localAgentSidebar.resetBrokerState();
  localAgentSidebar.setNewMapping();
  const otherPage = await context.newPage();
  await otherPage.goto(newResource.url);
  await expect(otherPage.locator(".agent-live-status")).toHaveText("Codex ready");
  await connectSidebar(otherPage, "codex", "5.6 Luna");
  await expect(otherPage.locator(".agent-model-pill")).toHaveAccessibleName("Model 5.6 Luna, effort Medium, speed Standard");
  await otherPage.close();
});

test("contains the Codex menu in mobile dark mode and leaves Pi unchanged", async ({ context, page, localAgentSidebar }) => {
  await page.setViewportSize({ width: 390, height: 720 });
  await openSidebarPage({
    context,
    page,
    fixture: localAgentSidebar,
    markdown: "# Mobile model controls\n",
    creatorContext: "Mobile context.\n",
    preferences: { "agent-whiteboard-theme": "dark" },
  });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption("codex");
  await connectSidebar(page, "codex");
  await page.locator(".agent-model-pill").click();
  const menu = page.locator(".agent-model-menu");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  const box = await menu.boundingBox();
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(390);
  expect(box.y).toBeGreaterThanOrEqual(0);
  expect(box.y + box.height).toBeLessThanOrEqual(720);
  await page.getByLabel("Conversation provider").selectOption("pi");
  await expect(page.locator(".agent-model-control")).toBeHidden();
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

for (const provider of ["codex", "cursor"]) {
  test(`the first valid ${provider} interaction response wins across tabs`, async ({
    context,
    page,
    localAgentSidebar,
  }) => {
  const resource = await openSidebarPage({ context, page, fixture: localAgentSidebar, markdown: "# Cross-tab approval\n", creatorContext: "Cross-tab context.\n" });
  await page.getByRole("button", { name: "Open Page agent", exact: true }).click();
  await page.getByLabel("Conversation provider").selectOption(provider);
  await connectSidebar(page, provider);

  const otherPage = await context.newPage();
  await otherPage.goto(resource.url);
  await expect(otherPage.locator(".agent-live-status")).toHaveText(`${providerFixtures[provider].label} ready`);
  await connectSidebar(otherPage, provider);
  localAgentSidebar.setHoldInteractionResolution(provider, true);
  const requestID = localAgentSidebar.emitInteraction(provider, {
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

  localAgentSidebar.releaseInteraction(provider, requestID);
  await expect(firstCard).toHaveAttribute("data-state", "resolved");
  await expect(secondCard).toHaveAttribute("data-state", "resolved");
  await expect(firstCard.locator(".agent-interaction-status")).toHaveText("Resolved · Accept");
  await expect(secondCard.locator(".agent-interaction-status")).toHaveText("Resolved · Accept");
  await otherPage.close();
  });
}

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

test("keeps the page context card and its inspect action inside a narrow drawer", async ({
  context,
  page,
  localAgentSidebar,
}) => {
  // 360 matches the viewer's minimum drawer width, where a long nowrap page
  // title used to push the whole card past the timeline's clip edge.
  const markdown = "# Existing Home-Screen Guide \u2192 PWA Redirect \u2192 Android Launch Plan Appendix\n\nBody text.\n";
  await openSidebarPage({
    context,
    page,
    fixture: localAgentSidebar,
    markdown,
    creatorContext: "Creator context.\n",
    preferences: { [widthKey]: 360 },
  });
  await connectSidebar(page);

  const card = page.locator(".agent-context-summary");
  await expect(card).toBeVisible();
  const timelineBox = await page.locator(".agent-timeline").boundingBox();
  const cardBox = await card.boundingBox();
  const inspectBox = await card.getByRole("button", { name: "Inspect context" }).boundingBox();
  expect(timelineBox).not.toBeNull();
  expect(cardBox).not.toBeNull();
  expect(inspectBox).not.toBeNull();
  const timelineRight = timelineBox.x + timelineBox.width;
  expect(cardBox.x).toBeGreaterThanOrEqual(timelineBox.x - 1);
  expect(cardBox.x + cardBox.width).toBeLessThanOrEqual(timelineRight + 1);
  expect(inspectBox.x + inspectBox.width).toBeLessThanOrEqual(timelineRight + 1);
});
