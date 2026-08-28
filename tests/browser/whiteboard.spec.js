import { expect, test } from "./fixture.js";

const completeMarkdown = [
  "# Browser Whiteboard",
  "",
  "## Rendered structures",
  "",
  "> quoted content",
  "",
  "| Name | Value |",
  "| --- | --- |",
  "| Alpha | Beta |",
  "",
  "- [ ] pending task",
  "- [x] completed task",
  "",
  "[safe link](https://example.com/path)",
  "",
  "[unsafe link](javascript:window.__unsafeLinkExecuted=true)",
  "",
  "```go",
  "package main",
  "func main() {}",
  "```",
  "",
  "```mermaid",
  "flowchart LR",
  "  A[First] --> B[Second]",
  "```",
  "",
  "```mermaid",
  "sequenceDiagram",
  "  Alice->>Bob: Hello",
  "```",
].join("\n");

async function waitForDiagrams(page, count) {
  await expect(page.locator(".mermaid-placeholder svg")).toHaveCount(count);
}

test("renders the complete Markdown contract without external requests", async ({
  page,
  publish,
  networkRequests,
  server,
}) => {
  const resource = await publish(completeMarkdown);
  await page.goto(resource.url);

  await expect(page.locator("#agent-whiteboard-content h1").first()).toHaveText("Browser Whiteboard");
  await expect(page.locator("blockquote")).toContainText("quoted content");
  await expect(page.locator("table tbody tr")).toHaveCount(1);
  await expect(page.locator("table tbody td").first()).toHaveText("Alpha");
  const tasks = page.locator('input.task-list-item-checkbox[type="checkbox"]');
  await expect(tasks).toHaveCount(2);
  await expect(tasks.nth(0)).toBeDisabled();
  await expect(tasks.nth(1)).toBeDisabled();
  await expect(tasks.nth(1)).toBeChecked();
  await expect(page.locator("pre code.language-go.hljs")).toContainText("package main");
  await expect(page.locator('a[href="https://example.com/path"]')).toHaveText("safe link");
  await expect(page.locator('a[href^="javascript:"]')).toHaveCount(0);

  const themeControl = page.locator("[data-theme-control]");
  await expect(themeControl).toHaveAttribute("aria-label", "Appearance: System");
  await themeControl.click();
  await page.locator('[data-theme-option="dark"]').click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expect(themeControl).toHaveAttribute("aria-label", "Appearance: Dark");
  await expect(themeControl.locator('[data-theme-icon="dark"]')).toHaveCount(1);
  await page.reload();
  await expect(themeControl).toHaveAttribute("aria-label", "Appearance: Dark");
  await themeControl.click();
  await page.locator('[data-theme-option="system"]').click();
  await expect.poll(() => page.evaluate(() => localStorage.getItem("agent-whiteboard-theme"))).toBe("system");

  await waitForDiagrams(page, 2);
  await expect(page.locator(".mermaid-placeholder").nth(0)).toContainText("First");
  await expect(page.locator(".mermaid-placeholder").nth(0)).toContainText("Second");
  for (const diagram of await page.locator(".mermaid-placeholder").all()) {
    await expect(diagram.locator("svg")).toHaveCount(1);
    await expect(diagram.locator("script")).toHaveCount(0);
    expect(await diagram.locator("[onload], [onclick], [onerror]").count()).toBe(0);
  }

  await expect(page).toHaveTitle("Browser Whiteboard");
  await expect(page.locator("#agent-whiteboard-source")).toBeHidden();
  const visibleText = await page.locator("body").innerText();
  expect(visibleText).not.toContain("| Name | Value |");
  expect(visibleText).not.toContain("```mermaid");
  expect(visibleText).not.toContain("flowchart LR");

  expect(networkRequests.all.length).toBeGreaterThan(0);
  expect(networkRequests.external).toEqual([]);
  expect(networkRequests.all.every((requestURL) => new URL(requestURL).origin === new URL(server.url).origin)).toBe(true);
  expect(networkRequests.all.some((requestURL) => /cdn|jsdelivr|unpkg/iu.test(requestURL))).toBe(false);
});

test("persists light dark and system themes and re-renders Mermaid", async ({ page, publish }) => {
  const resource = await publish("# Themes\n\n```mermaid\nflowchart LR\n  A --> B\n```\n");
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto(resource.url);

  const setThemeAndReload = async (theme) => {
    await page.evaluate((value) => localStorage.setItem("agent-whiteboard-theme", value), theme);
    await page.reload();
    await waitForDiagrams(page, 1);
    await expect.poll(() => page.evaluate(() => localStorage.getItem("agent-whiteboard-theme"))).toBe(theme);
    return page.locator(".mermaid-placeholder svg").evaluate((node) => node.outerHTML);
  };

  const lightSVG = await setThemeAndReload("light");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  const darkSVG = await setThemeAndReload("dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  expect(darkSVG).not.toBe(lightSVG);

  const systemLightSVG = await setThemeAndReload("system");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  expect(systemLightSVG).not.toBe(darkSVG);
  await page.reload();
  await waitForDiagrams(page, 1);
  await expect.poll(() => page.evaluate(() => localStorage.getItem("agent-whiteboard-theme"))).toBe("system");

  const beforeSystemChange = await page.locator(".mermaid-placeholder svg").evaluate((node) => node.outerHTML);
  await page.emulateMedia({ colorScheme: "dark" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expect
    .poll(() => page.locator(".mermaid-placeholder svg").evaluate((node) => node.outerHTML))
    .not.toBe(beforeSystemChange);
});

test("isolates an invalid Mermaid diagram from a valid diagram", async ({ page, publish }) => {
  const resource = await publish(
    "# Isolation\n\n```mermaid\nflowchart LR\n  A --> B\n```\n\n```mermaid\nthis is not valid {\n```\n",
  );
  await page.goto(resource.url);

  const placeholders = page.locator(".mermaid-placeholder");
  await expect(placeholders).toHaveCount(2);
  await expect(placeholders.nth(0).locator("svg")).toHaveCount(1);
  await expect(placeholders.nth(0).locator(".mermaid-error")).toHaveCount(0);
  await expect(placeholders.nth(1).locator(".mermaid-error")).toHaveText("Unable to render diagram");
  await expect(placeholders.nth(1).locator("svg")).toHaveCount(0);
  await expect(page.locator(".mermaid-error")).toHaveCount(1);
  await expect(placeholders.nth(1)).not.toContainText("parse");
});

test("renders wide markdown tables with natural columns and horizontal scrolling", async ({
  page,
  publish,
}) => {
  const header = [
    "Case", "Kind", "RPS", "N", "Min ms", "Avg ms", "P50 ms", "P75 ms", "P90 ms", "P95 ms", "P99 ms", "P999 ms", "Max ms", "Rate rps", "Drain ms", "Result", "Sampling",
  ];
  const row = (n) => [
    `\`capacity-64-Throughput-Exploratory-rep-${n}\``, "capacity", "64", "600", "0.911", "2.445", "2.492", "3.004", "3.312", "3.395", "3.414", "5.207", "3.414", "213.7", "0", "PASS", "none",
  ];
  const markdown = [
    "| " + header.join(" | ") + " |",
    "| " + header.map(() => "---").join(" | ") + " |",
    ...[0, 1].map((n) => "| " + row(n).join(" | ") + " |"),
  ].join("\n");
  const resource = await publish(markdown);
  await page.goto(resource.url);

  const table = page.locator("#agent-whiteboard-content table");
  await expect(table).toBeVisible();
  const metrics = await table.evaluate((element) => ({
    scrollWidth: element.scrollWidth,
    clientWidth: element.clientWidth,
    headerHeights: [...element.querySelectorAll("thead th")].map((th) => th.getBoundingClientRect().height),
    codeCellHeight: element.querySelector("tbody td code").getBoundingClientRect().height,
    headerBackground: getComputedStyle(element.querySelector("thead th")).backgroundColor,
  }));
  // Natural column widths make the table wider than the content column, so the
  // table scrolls horizontally instead of squeezing cells mid-word.
  expect(metrics.clientWidth).toBeLessThan(metrics.scrollWidth);
  expect(metrics.scrollWidth).toBeGreaterThan(900);
  // Inline code identifiers are single unbreakable tokens.
  expect(metrics.codeCellHeight).toBeLessThan(40);
  // Uniform header heights prove every header renders on a single line.
  const shortestHeader = Math.min(...metrics.headerHeights);
  const tallestHeader = Math.max(...metrics.headerHeights);
  expect(tallestHeader).toBeLessThan(shortestHeader * 1.35);
  expect(metrics.headerBackground).not.toBe("rgba(0, 0, 0, 0)");
});
