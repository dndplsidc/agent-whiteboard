import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { buildAssets } from "./build.mjs";

const assetsDirectory = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const generatedFiles = ["dist/viewer.min.js", "dist/viewer.min.css", "manifest.json"];
const temporaryDirectory = await mkdtemp(join(tmpdir(), "agent-whiteboard-assets-"));

try {
  await buildAssets(temporaryDirectory);
  const differing = [];
  for (const filename of generatedFiles) {
    let committed;
    try {
      committed = await readFile(resolve(assetsDirectory, filename));
    } catch (error) {
      if (error.code === "ENOENT") {
        differing.push(filename);
        continue;
      }
      throw error;
    }
    const regenerated = await readFile(resolve(temporaryDirectory, filename));
    if (!committed.equals(regenerated)) differing.push(filename);
  }

  if (differing.length > 0) {
    throw new Error(`generated browser assets differ: ${differing.join(", ")}`);
  }
  process.stdout.write("browser assets match deterministic build\n");
} finally {
  await rm(temporaryDirectory, { recursive: true, force: true });
}
