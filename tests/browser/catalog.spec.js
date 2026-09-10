import { expect, test } from "./fixture.js";
import { promises as fs } from "node:fs";

test("catalog discovers Markdown and HTML without changing rendered titles or content", async ({ page, catalogClient }) => {
  const markdown = await catalogClient.create({
    kind: "markdown",
    source: "# Rendered Markdown heading\n\nVisible document body.\n",
    context: "# Private creator context\n\nNot catalog metadata.\n",
    title: "Catalog-only Markdown title",
    summary: "Search phrase alpha markdown",
  });
  const html = await catalogClient.create({
    kind: "html",
    source: "<!doctype html><html><head><title>Inner HTML title</title></head><body><main>Visible HTML body</main></body></html>",
    context: "# Private HTML creator context\n",
    title: "Catalog-only HTML title",
    summary: "Search phrase beta html",
  });

  const markdownSearch = await catalogClient.run(["--json", "catalog", "list", "--query", "alpha markdown"]);
  expect(markdownSearch.stderr).toBe("");
  expect(markdownSearch.json.total).toBe(1);
  expect(markdownSearch.json.records[0].id).toBe(markdown.id);
  await page.goto(markdownSearch.json.records[0].url);
  await expect(page.locator("#agent-whiteboard-content h1")).toHaveText("Rendered Markdown heading");
  await expect(page.locator("body")).toContainText("Visible document body");
  await expect(page.locator("body")).not.toContainText("Catalog-only Markdown title");
  await expect(page.locator("body")).not.toContainText("Search phrase alpha markdown");
  await expect(page).toHaveTitle("Rendered Markdown heading");

  const htmlSearch = await catalogClient.run(["--json", "catalog", "list", "--kind", "html", "--query", "beta html"]);
  expect(htmlSearch.stderr).toBe("");
  expect(htmlSearch.json.total).toBe(1);
  expect(htmlSearch.json.records[0].id).toBe(html.id);
  await page.goto(htmlSearch.json.records[0].url);
  await expect(page).toHaveTitle("Standalone whiteboard");
  const frame = page.frameLocator('iframe[title="Standalone whiteboard content"]');
  await expect(frame.locator("main")).toHaveText("Visible HTML body");
  await expect(frame.locator("body")).not.toContainText("Catalog-only HTML title");
  await expect(frame.locator("body")).not.toContainText("Search phrase beta html");
});

test("catalog preserves and replaces tracked metadata through update and deletion", async ({ page, catalogClient, server }) => {
  const created = await catalogClient.create({
    kind: "markdown",
    source: "# Original rendered heading\n\nOriginal body.\n",
    context: "Original creator context\n",
    title: "Original catalog title",
    summary: "Preserved lifecycle summary",
  });
  const updatedSource = "# Updated rendered heading\n\nUpdated body at the same capability.\n";
  await fs.writeFile(created.sourcePath, updatedSource, { mode: 0o600 });
  await fs.writeFile(created.contextPath, "Updated creator context\n", { mode: 0o600 });
  const update = await catalogClient.run([
    "--server", server.url, "--json", "update", "markdown", created.id, created.sourcePath,
    "--context", created.contextPath, "--title", "Replacement catalog title",
  ]);
  expect(update.stderr).toBe("");
  expect(update.json.resource.url).toBe(created.url);

  const search = await catalogClient.run(["--json", "catalog", "list", "--query", "replacement preserved"]);
  expect(search.json.total).toBe(1);
  expect(search.json.records[0].title).toBe("Replacement catalog title");
  expect(search.json.records[0].summary).toBe("Preserved lifecycle summary");
  await page.goto(search.json.records[0].url);
  await expect(page.locator("#agent-whiteboard-content h1")).toHaveText("Updated rendered heading");
  await expect(page.locator("body")).toContainText("Updated body at the same capability");

  const deletion = await catalogClient.run(["--server", server.url, "--json", "delete", "markdown", created.id]);
  expect(deletion.stderr).toBe("");
  const deleted = await catalogClient.run(["--json", "catalog", "list", "--query", "replacement preserved"]);
  expect(deleted.json.total).toBe(1);
  expect(deleted.json.records[0].state).toBe("deleted");
  expect(deleted.json.records[0].deleted_at).not.toBeNull();
  const response = await page.request.get(created.url);
  expect(response.status()).toBe(404);
});

test("catalog remains local, paginated, multi-origin, and offline", async ({ catalogClient, server }) => {
  const additional = await catalogClient.startAdditionalServer();
  const first = await catalogClient.create({
    kind: "markdown",
    source: "# First origin\n",
    context: "First context\n",
    title: "First local record",
    summary: "Shared pagination term",
  });
  const second = await catalogClient.create({
    kind: "html",
    source: "<!doctype html><html><head><title>Second</title></head><body>Second origin</body></html>",
    context: "Second context\n",
    title: "Second local record",
    summary: "Shared pagination term",
    origin: additional.url,
  });

  const firstPage = await catalogClient.run(["--json", "catalog", "list", "--query", "shared term", "--limit", "1", "--offset", "0"]);
  const secondPage = await catalogClient.run(["--json", "catalog", "list", "--query", "shared term", "--limit", "1", "--offset", "1"]);
  expect(firstPage.json.total).toBe(2);
  expect(secondPage.json.total).toBe(2);
  expect(new Set([firstPage.json.records[0].id, secondPage.json.records[0].id])).toEqual(new Set([first.id, second.id]));
  const htmlOnly = await catalogClient.run(["--json", "catalog", "list", "--kind", "html"]);
  expect(htmlOnly.json.records.map(({ id }) => id)).toEqual([second.id]);

  const freshHome = await catalogClient.listFromFreshHome();
  expect(freshHome.json.total).toBe(0);
  expect(freshHome.json.records).toEqual([]);

  await additional.stop();
  const offline = await catalogClient.run(["--server", additional.url, "--config", "/missing/catalog-config.yaml", "--json", "catalog", "list", "--query", "second local"]);
  expect(offline.stderr).toBe("");
  expect(offline.json.total).toBe(1);
  expect(offline.json.records[0].id).toBe(second.id);
  expect(offline.json.records[0].server).toBe(additional.url);
  expect(offline.json.records[0].server).not.toBe(server.url);
});
