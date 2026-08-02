import { expect, test } from "./fixture.js";

const outerCSP =
  "default-src 'none'; base-uri 'none'; connect-src 'none'; font-src 'none'; form-action 'none'; frame-ancestors 'none'; frame-src 'self'; img-src 'none'; manifest-src 'none'; media-src 'none'; object-src 'none'; script-src 'none'; style-src 'sha256-Tn/hKQI0ISMV0qjQCZd0Gif536vvizgJ1ukIP+PYoJ8='; worker-src 'none'";
const innerCSP =
  "sandbox allow-scripts; default-src 'none'; base-uri 'none'; connect-src 'none'; font-src 'none'; form-action 'none'; frame-ancestors 'self'; frame-src 'none'; img-src data: blob:; manifest-src 'none'; media-src data: blob:; object-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; worker-src 'none'";

function standaloneHTML(script, body = "") {
  return `<!doctype html><html><head><meta charset="utf-8"><title>hostile standalone proof</title></head><body>${body}<script>${script}</script></body></html>`;
}

function headerProof(response, csp) {
  const headers = response.headers();
  expect(headers["content-security-policy"]).toBe(csp);
  expect(headers["referrer-policy"]).toBe("no-referrer");
  expect(headers["permissions-policy"]).toContain("camera=()");
  expect(headers["x-content-type-options"]).toBe("nosniff");
  expect(headers["x-robots-tag"]).toBe("noindex, nofollow, noarchive");
}

async function seedPublishingState(page, server) {
  await page.context().addCookies([
    { name: "publishing_secret", value: "cookie-secret", url: server.url },
  ]);
  await page.goto(`${server.url}/healthz`);
  await page.evaluate(async () => {
    localStorage.setItem("publishing-secret", "local-storage-secret");
    await new Promise((resolve, reject) => {
      const request = indexedDB.open("publishing-secret", 1);
      request.onupgradeneeded = () => request.result.createObjectStore("secrets").put("indexed-db-secret", "value");
      request.onsuccess = () => {
        request.result.close();
        resolve();
      };
      request.onerror = () => reject(request.error);
    });
  });
}

test.describe("standalone HTML authority boundary", () => {
  test.use({ browserRequestInterception: false });

  test("keeps hostile bytes out of the outer wrapper and denies active authority", async ({
    page,
    publishHTML,
    server,
    standaloneCapture,
  }) => {
    await seedPublishingState(page, server);
    const marker = "HOSTILE_SOURCE_BYTES_9e537dae";
    const target = standaloneCapture.crossOrigin.origin;
    const script = `
      const result = { marker: ${JSON.stringify(marker)}, executed: true };
      const attempt = (name, operation) => {
        try { result[name] = { value: String(operation()) }; }
        catch (error) { result[name] = { error: error.name }; }
      };
      attempt("parentDocument", () => parent.document.body.textContent);
      attempt("cookie", () => document.cookie);
      attempt("localStorage", () => localStorage.getItem("publishing-secret"));
      attempt("indexedDB", () => indexedDB.open("publishing-secret"));
      attempt("topNavigation", () => { top.location.href = ${JSON.stringify(`${target}/top-navigation`)}; return "assigned"; });
      result.popup = String(window.open(${JSON.stringify(`${target}/popup`)}, "_blank"));

      const download = document.createElement("a");
      download.href = ${JSON.stringify(`${target}/download`)};
      download.download = "proof.txt";
      download.target = "_blank";
      document.body.append(download);
      download.click();

      const form = document.createElement("form");
      form.method = "POST";
      form.action = ${JSON.stringify(`${target}/form`)};
      form.target = "_blank";
      document.body.append(form);
      attempt("form", () => { form.submit(); return "submitted"; });

      const child = document.createElement("iframe");
      child.src = ${JSON.stringify(`${target}/child-frame`)};
      child.onload = () => { result.childLoadEvent = true; };
      document.body.append(child);
      result.childElementCreated = child.isConnected;

      const bounded = (operation) => Promise.race([
        operation,
        new Promise((resolve) => setTimeout(() => resolve("blocked-timeout"), 500)),
      ]);
      const network = async () => {
        try { await fetch(${JSON.stringify(`${target}/fetch`)}); result.fetch = "resolved"; }
        catch (error) { result.fetch = error.name; }
        result.xhr = await bounded(new Promise((resolve) => {
          const request = new XMLHttpRequest();
          request.onload = () => resolve("load");
          request.onerror = () => resolve("error");
          request.open("GET", ${JSON.stringify(`${target}/xhr`)});
          request.send();
        }));
        result.websocket = await bounded(new Promise((resolve) => {
          try {
            const socket = new WebSocket(${JSON.stringify(target.replace("http:", "ws:") + "/websocket")});
            socket.onopen = () => resolve("open");
            socket.onerror = () => resolve("error");
          } catch (error) {
            resolve(error.name);
          }
        }));
        result.image = await bounded(new Promise((resolve) => {
          const image = new Image();
          image.onload = () => resolve("load");
          image.onerror = () => resolve("error");
          image.src = ${JSON.stringify(`${target}/image`)};
        }));
        try { await fetch(${JSON.stringify(`${target}/api/v1/agent/status`)}); result.broker = "resolved"; }
        catch (error) { result.broker = error.name; }
        await new Promise((resolve) => setTimeout(resolve, 250));
      };
      network()
        .catch((error) => { result.networkError = error.name + ":" + error.message; })
        .finally(() => parent.postMessage({ type: "standalone-proof", result }, "*"));
    `;
    const source = standaloneHTML(script, `<p>${marker}</p>`);
    const resource = await publishHTML(source);
    const innerURL = `${resource.url}/content`;

    const outerSourceResponse = await fetch(resource.url);
    expect(outerSourceResponse.status).toBe(200);
    const outerSource = await outerSourceResponse.text();
    expect(outerSource).not.toContain(marker);
    expect(outerSource).not.toContain(script);

    await page.addInitScript(() => {
      if (window === window.top) {
        globalThis.__standaloneMessages = [];
        addEventListener("message", (event) => {
          globalThis.__standaloneMessages.push({ data: event.data, origin: event.origin });
        });
      }
    });
    const innerRequestHeaders = [];
    page.on("request", (request) => {
      if (request.url() === innerURL) innerRequestHeaders.push(request.allHeaders());
    });
    let popupCount = 0;
    let downloadCount = 0;
    page.on("popup", () => popupCount++);
    page.on("download", () => downloadCount++);

    const outerResponse = await page.goto(resource.url);
    expect(outerResponse).not.toBeNull();
    headerProof(outerResponse, outerCSP);
    const frame = page.locator("iframe");
    await expect(frame).toHaveCount(1);
    await expect(frame).toHaveAttribute("src", new URL(innerURL).pathname);
    expect((await frame.getAttribute("sandbox")).trim().split(/\s+/)).toEqual(["allow-scripts"]);
    await expect(frame).toHaveAttribute("referrerpolicy", "no-referrer");
    await expect(frame).toHaveAttribute("credentialless", "");

    await expect.poll(() => page.evaluate(() => globalThis.__standaloneMessages.length)).toBe(1);
    const message = await page.evaluate(() => globalThis.__standaloneMessages[0]);
    expect(message.origin).toBe("null");
    expect(message.data.type).toBe("standalone-proof");
    const result = message.data.result;
    expect(result.executed).toBe(true);
    expect(result.parentDocument.error).toBe("SecurityError");
    expect(result.cookie.error).toBe("SecurityError");
    expect(result.localStorage.error).toBe("SecurityError");
    expect(result.indexedDB.error).toBe("SecurityError");
    expect(result.topNavigation.error).toBe("SecurityError");
    expect(result.popup).toBe("null");
    expect(result.form.value).toBe("submitted");
    expect(result.childElementCreated).toBe(true);
    expect(result.fetch).toBe("TypeError");
    expect(result.xhr).toBe("error");
    expect(result.websocket).not.toBe("open");
    expect(result.image).toBe("error");
    expect(result.broker).toBe("TypeError");
    expect(page.url()).toBe(resource.url);
    expect(popupCount).toBe(0);
    expect(downloadCount).toBe(0);
    expect(standaloneCapture.crossOrigin.requests).toEqual([]);
    expect(innerRequestHeaders).toHaveLength(1);
    expect((await innerRequestHeaders[0]).cookie).toBeUndefined();
  });

  test("direct content navigation remains opaque and independently blocks storage and network", async ({
    page,
    publishHTML,
    server,
    standaloneCapture,
  }) => {
    await seedPublishingState(page, server);
    const script = `
      (async () => {
        const result = {};
        for (const [name, operation] of [
          ["cookie", () => document.cookie],
          ["localStorage", () => localStorage.getItem("publishing-secret")],
          ["indexedDB", () => indexedDB.open("publishing-secret")],
        ]) {
          try { result[name] = { value: String(operation()) }; }
          catch (error) { result[name] = { error: error.name }; }
        }
        try { await fetch(${JSON.stringify(`${standaloneCapture.origin}/direct-fetch`)}); result.fetch = "resolved"; }
        catch (error) { result.fetch = error.name; }
        const image = new Image();
        result.image = await new Promise((resolve) => {
          image.onload = () => resolve("load");
          image.onerror = () => resolve("error");
          image.src = ${JSON.stringify(`${standaloneCapture.origin}/direct-image`)};
        });
        postMessage({ type: "direct-proof", result }, "*");
      })();
    `;
    const resource = await publishHTML(standaloneHTML(script));
    const innerURL = `${resource.url}/content`;
    await page.addInitScript(() => {
      addEventListener("message", (event) => {
        if (event.data?.type === "direct-proof") globalThis.__directProof = { data: event.data, origin: event.origin };
      });
    });

    const response = await page.goto(innerURL);
    expect(response).not.toBeNull();
    headerProof(response, innerCSP);
    expect(response.headers()["x-frame-options"]).toBe("SAMEORIGIN");
    await expect.poll(() => page.evaluate(() => globalThis.__directProof)).not.toBeUndefined();
    const proof = await page.evaluate(() => globalThis.__directProof);
    expect(proof.origin).toBe("null");
    expect(proof.data.result).toEqual({
      cookie: { error: "SecurityError" },
      localStorage: { error: "SecurityError" },
      indexedDB: { error: "SecurityError" },
      fetch: "TypeError",
      image: "error",
    });
    expect(standaloneCapture.requests).toEqual([]);
  });

  test("only the exact content route serves bytes and error responses retain inner security headers", async ({
    page,
    publish,
    publishHTML,
  }) => {
    const html = await publishHTML(standaloneHTML("globalThis.__exactRoute = true"));
    const markdown = await publish("# Wrong kind");
    const malformed = html.url.replace(html.id, "malformed");
    const missing = html.url.replace(html.id, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA");
    const wrongKind = markdown.url.replace("/markdown/", "/html/");

    for (const endpoint of [`${malformed}/content`, `${missing}/content`, `${wrongKind}/content`]) {
      const response = await page.request.get(endpoint);
      expect(response.status()).toBe(404);
      headerProof(response, innerCSP);
      expect(response.headers()["x-frame-options"]).toBe("SAMEORIGIN");
    }
    for (const endpoint of [`${html.url}/raw`, `${html.url}/content/extra`, `${html.url}/source`]) {
      const response = await page.request.get(endpoint);
      expect(response.status()).toBe(404);
      expect(await response.text()).not.toContain("__exactRoute");
    }
    const exact = await page.request.get(`${html.url}/content`);
    expect(exact.status()).toBe(200);
    expect(await exact.text()).toContain("__exactRoute");
    headerProof(exact, innerCSP);
  });

  test("accepts child self-navigation while stripping referrer and credentials", async ({
    page,
    publishHTML,
    standaloneCapture,
  }) => {
    await page.context().addCookies([
      { name: "capture_secret", value: "must-not-leak", url: standaloneCapture.origin },
    ]);
    const script = `
      setTimeout(() => {
        location.href = "/self-navigation?capability=" + encodeURIComponent(location.href);
      }, 50);
    `;
    const resource = await publishHTML(standaloneHTML(script));
    const proxiedURL = resource.url.replace(new URL(resource.url).origin, standaloneCapture.origin);
    await page.goto(proxiedURL);

    await expect
      .poll(() => standaloneCapture.requests.filter((request) => request.url.startsWith("/self-navigation?")).length)
      .toBe(1);
    const outerRequest = standaloneCapture.requests.find((request) => request.url === new URL(proxiedURL).pathname);
    expect(outerRequest.headers.cookie).toContain("capture_secret=must-not-leak");
    const captured = standaloneCapture.requests.find((request) => request.url.startsWith("/self-navigation?"));
    const capturedURL = new URL(captured.url, standaloneCapture.origin);
    expect(capturedURL.pathname).toBe("/self-navigation");
    expect(capturedURL.searchParams.get("capability")).toBe(`${proxiedURL}/content`);
    expect(captured.headers.referer).toBeUndefined();
    expect(captured.headers.cookie).toBeUndefined();
  });

  test("the wrapper CSP limits child self-navigation to the publishing origin", async ({
    page,
    publishHTML,
    standaloneCapture,
  }) => {
    const target = `${standaloneCapture.crossOrigin.origin}/cross-origin-navigation`;
    const markerType = "cross-origin-navigation-attempt";
    const resource = await publishHTML(
      standaloneHTML(`
        parent.postMessage({ type: ${JSON.stringify(markerType)}, target: ${JSON.stringify(target)} }, "*");
        location.href = ${JSON.stringify(target)};
      `),
    );
    const proxiedURL = resource.url.replace(new URL(resource.url).origin, standaloneCapture.origin);
    await page.addInitScript((type) => {
      if (window === window.top) {
        addEventListener("message", (event) => {
          if (event.data?.type === type) globalThis.__crossOriginNavigationMarker = {
            data: event.data,
            origin: event.origin,
          };
        });
      }
    }, markerType);
    await page.goto(proxiedURL);

    await expect.poll(() => page.evaluate(() => globalThis.__crossOriginNavigationMarker)).not.toBeUndefined();
    const marker = await page.evaluate(() => globalThis.__crossOriginNavigationMarker);
    expect(marker).toEqual({ data: { type: markerType, target }, origin: "null" });
    await page.waitForTimeout(250);
    expect(standaloneCapture.crossOrigin.requests).toEqual([]);
    expect(page.frames().map((frame) => frame.url())).not.toContain(target);
  });
});
