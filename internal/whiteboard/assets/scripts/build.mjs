import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { build } from "esbuild";

const scriptDirectory = dirname(fileURLToPath(import.meta.url));
const assetsDirectory = resolve(scriptDirectory, "..");
const repositoryRoot = resolve(assetsDirectory, "../../..");
const sourceDirectory = resolve(assetsDirectory, "src");

async function packageVersions() {
  const packageJSON = JSON.parse(await readFile(resolve(repositoryRoot, "package.json"), "utf8"));
  return {
    "markdown-it": packageJSON.dependencies["markdown-it"],
    dompurify: packageJSON.dependencies.dompurify,
    mermaid: packageJSON.dependencies.mermaid,
    "highlight.js": packageJSON.dependencies["highlight.js"],
    esbuild: packageJSON.devDependencies.esbuild,
  };
}

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

async function makeInlineScriptSafe(filename) {
  const bundled = await readFile(filename, "utf8");
  const safe = bundled
    .replaceAll("https://", "https:\\x2f\\x2f")
    .replaceAll("http://", "http:\\x2f\\x2f")
    .replace(/<\/script/giu, "<\\x2fscript");
  await writeFile(filename, safe);
}

export async function buildAssets(outputDirectory = assetsDirectory) {
  const distributionDirectory = resolve(outputDirectory, "dist");
  const javascriptOutput = resolve(distributionDirectory, "viewer.min.js");
  const bridgeOutput = resolve(distributionDirectory, "html-bridge.min.js");
  const stylesheetOutput = resolve(distributionDirectory, "viewer.min.css");
  await mkdir(distributionDirectory, { recursive: true });

  await build({
    entryPoints: [resolve(sourceDirectory, "viewer.js")],
    outfile: javascriptOutput,
    bundle: true,
    platform: "browser",
    format: "iife",
    minify: true,
    legalComments: "none",
    charset: "utf8",
    sourcemap: false,
    target: ["es2022"],
    supported: { "template-literal": false },
  });

  await build({
    entryPoints: [resolve(sourceDirectory, "html-bridge.js")],
    outfile: bridgeOutput,
    bundle: true,
    platform: "browser",
    format: "iife",
    minify: true,
    legalComments: "none",
    charset: "utf8",
    sourcemap: false,
    target: ["es2022"],
    supported: { "template-literal": false },
  });

  await Promise.all([makeInlineScriptSafe(javascriptOutput), makeInlineScriptSafe(bridgeOutput)]);

  await build({
    entryPoints: [resolve(sourceDirectory, "viewer.css")],
    outfile: stylesheetOutput,
    bundle: true,
    platform: "browser",
    minify: true,
    legalComments: "none",
    charset: "utf8",
    sourcemap: false,
    target: ["es2022"],
  });

  const [javascript, bridge, stylesheet] = await Promise.all([
    readFile(javascriptOutput),
    readFile(bridgeOutput),
    readFile(stylesheetOutput),
  ]);
  const manifest = {
    versions: await packageVersions(),
    sha256: {
      "dist/viewer.min.js": sha256(javascript),
      "dist/html-bridge.min.js": sha256(bridge),
      "dist/viewer.min.css": sha256(stylesheet),
    },
  };
  await writeFile(resolve(outputDirectory, "manifest.json"), `${JSON.stringify(manifest, null, 2)}\n`);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await buildAssets();
}
