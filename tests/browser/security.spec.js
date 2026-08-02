import { expect, test } from "./fixture.js";

test("sanitizes hostile Markdown and preserves ordinary code", async ({ page, publish, networkRequests }) => {
  const source = [
    "# Security",
    "",
    "<script>window.__rawScriptExecuted = true</script>",
    "",
    "<style>body { display: none }</style>",
    "",
    '<img src="x" onerror="window.__eventExecuted = true">',
    "",
    "[unsafe](javascript:window.__javascriptLinkExecuted=true)",
    "",
    '<svg onload="window.__svgExecuted = true"><script>window.__svgScriptExecuted = true</script></svg>',
    "",
    "</script><script>window.__breakoutExecuted = true</script>",
    "",
    "Ordinary code remains: `const safe = true;`",
  ].join("\n");
  const resource = await publish(source);
  const response = await page.goto(resource.url);
  expect(response).not.toBeNull();

  const content = page.locator("#agent-whiteboard-content");
  await expect(content.locator("script, style, img")).toHaveCount(0);
  await expect(content.locator("svg:not(.theme-control-icon)")).toHaveCount(0);
  await expect(content.locator(".theme-control .theme-control-icon")).toHaveCount(4);
  await expect(content.locator('a[href^="javascript:"]')).toHaveCount(0);
  expect(
    await content.locator("*").evaluateAll((nodes) =>
      nodes.flatMap((node) => [...node.attributes].filter((attribute) => attribute.name.toLowerCase().startsWith("on"))),
    ),
  ).toEqual([]);
  expect(
    await page.evaluate(() => ({
      breakout: globalThis.__breakoutExecuted,
      event: globalThis.__eventExecuted,
      javascript: globalThis.__javascriptLinkExecuted,
      rawScript: globalThis.__rawScriptExecuted,
      svg: globalThis.__svgExecuted,
      svgScript: globalThis.__svgScriptExecuted,
    })),
  ).toEqual({});
  await expect(content.locator("code")).toHaveText("const safe = true;");

  await expect(page.locator('meta[name="robots"]')).toHaveAttribute("content", "noindex, nofollow, noarchive");
  expect(response.headers()["x-robots-tag"]).toBe("noindex, nofollow, noarchive");
  expect(response.headers()["x-content-type-options"]).toBe("nosniff");
  await expect(page.locator('#agent-whiteboard-source[type="application/json"]')).toHaveCount(1);
  await expect(page.locator("script")).toHaveCount(2);
  expect(await page.locator("#agent-whiteboard-source").textContent()).not.toContain("</script>");
  expect(networkRequests.external).toEqual([]);
});

test("denies same-origin framing of a published Markdown viewer", async ({ page, publish, server }) => {
  const marker = "MARKDOWN_FRAME_DENIAL_12c896cc";
  const resource = await publish(`# Framing denied\n\n${marker}`);
  const attemptedRequests = [];
  page.on("request", (request) => {
    if (request.url() === resource.url) attemptedRequests.push(request.url());
  });

  await page.goto(`${server.url}/healthz`);
  expect(new URL(page.url()).origin).toBe(new URL(resource.url).origin);
  const frameResponsePromise = page.waitForResponse((response) => response.url() === resource.url);
  await page.evaluate(({ url }) => {
    globalThis.__markdownFrameHostExecuted = true;
    globalThis.__markdownFrameLoadEvents = 0;
    const frame = document.createElement("iframe");
    frame.id = "markdown-frame-attempt";
    frame.addEventListener("load", () => globalThis.__markdownFrameLoadEvents++);
    frame.src = url;
    document.body.append(frame);
  }, { url: resource.url });

  const frameResponse = await frameResponsePromise;
  expect(await page.evaluate(() => globalThis.__markdownFrameHostExecuted)).toBe(true);
  expect(attemptedRequests).toEqual([resource.url]);
  expect(frameResponse.headers()["x-frame-options"]).toBe("DENY");
  expect(frameResponse.headers()["content-security-policy"]).toContain("frame-ancestors 'none'");
  await expect.poll(() => page.evaluate(() => globalThis.__markdownFrameLoadEvents)).toBe(1);

  const frameState = await page.locator("#markdown-frame-attempt").evaluate(
    (frame, expectedMarker) => ({
      contentDocumentAvailable: frame.contentDocument !== null,
      markerReadable: frame.contentDocument?.body?.textContent?.includes(expectedMarker) ?? false,
    }),
    marker,
  );
  expect(frameState).toEqual({ contentDocumentAvailable: false, markerReadable: false });
  expect(page.frames().map((frame) => frame.url())).not.toContain(resource.url);
});
