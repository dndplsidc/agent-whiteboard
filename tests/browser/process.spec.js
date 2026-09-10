import { expect, test } from "@playwright/test";
import http from "node:http";
import { runProcess } from "./fixture.js";

for (const code of [0, 7]) {
  test(`catalog process capture drains inherited output pipes after exit ${code}`, async () => {
    let release;
    let parentPID;
    let reached;
    const pipeHeld = new Promise((resolve) => { reached = resolve; });
    const gate = http.createServer((request, response) => {
      parentPID = Number(request.headers["x-parent-pid"]);
      release = () => response.end();
      reached();
    });
    await new Promise((resolve, reject) => {
      gate.once("error", reject);
      gate.listen(0, "127.0.0.1", resolve);
    });
    const payload = "output line αβγ\n".repeat(100_000);
    const writer = `
      const http = require("node:http");
      process.on("disconnect", () => {
        const request = http.get({ hostname: "127.0.0.1", port: ${gate.address().port}, headers: { "x-parent-pid": process.argv[1] } }, (response) => {
          response.resume();
          response.on("end", () => {
            const payload = "output line αβγ\\n".repeat(100_000);
            process.stdout.write(payload);
            process.stderr.write(payload);
          });
        });
        request.setTimeout(5000, () => request.destroy(new Error("gate timeout")));
        request.on("error", () => process.exit(1));
      });
      process.send("ready");
    `;
    const parent = `
      const { spawn } = require("node:child_process");
      const child = spawn(process.execPath, ["-e", ${JSON.stringify(writer)}, String(process.pid)], { stdio: ["ignore", 1, 2, "ipc"] });
      child.once("message", () => process.exit(${code}));
    `;
    const result = runProcess(process.execPath, ["-e", parent], { timeout: 10_000 })
      .then((value) => ({ value }), (error) => ({ error }));
    try {
      await Promise.race([
        pipeHeld,
        result.then(({ error }) => { throw error ?? new Error("process settled before reaching its output gate"); }),
      ]);
      // The descendant holds both pipes open after the direct child has been
      // reaped. Only then release its large output; no timing sleep is needed.
      await expect.poll(() => {
        try { process.kill(parentPID, 0); return false; }
        catch (error) { if (error.code === "ESRCH") return true; throw error; }
      }).toBe(true);
      release();
      const captured = await result;
      if (code === 0) {
        expect(captured.error).toBeUndefined();
        expect(captured.value.stdout.length).toBe(payload.length);
        expect(captured.value.stderr.length).toBe(payload.length);
        expect(captured.value.stdout).toBe(payload);
        expect(captured.value.stderr).toBe(payload);
      } else {
        expect(captured.error.message).toContain(`process failed (${code})`);
        expect(captured.error.message.endsWith(`\nstdout:\n${payload}\nstderr:\n${payload}`)).toBe(true);
      }
    } finally {
      release?.();
      await result;
      await new Promise((resolve) => gate.close(resolve));
    }
  });
}

test("catalog process capture retains timeout and spawn failures", async () => {
  await expect(runProcess(process.execPath, ["-e", "setInterval(() => {}, 1000)"], { timeout: 50 })).rejects.toThrow("process timed out");
  await expect(runProcess("/nonexistent/catalog-test-program", [])).rejects.toThrow("ENOENT");
});
