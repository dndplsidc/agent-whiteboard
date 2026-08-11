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

export async function buildAssets(outputDirectory = assetsDirectory) {
  const distributionDirectory = resolve(outputDirectory, "dist");
  const javascriptOutput = resolve(distributionDirectory, "viewer.min.js");
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

  const bundledJavaScript = await readFile(javascriptOutput, "utf8");
  const selfContainedJavaScript = bundledJavaScript
    .replaceAll("https://", "https:\\x2f\\x2f")
    .replaceAll("http://", "http:\\x2f\\x2f");
  await writeFile(javascriptOutput, selfContainedJavaScript);

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

  const [javascript, stylesheet] = await Promise.all([
    readFile(javascriptOutput),
    readFile(stylesheetOutput),
  ]);
  const manifest = {
    versions: await packageVersions(),
    sha256: {
      "dist/viewer.min.js": sha256(javascript),
      "dist/viewer.min.css": sha256(stylesheet),
    },
  };
  await writeFile(resolve(outputDirectory, "manifest.json"), `${JSON.stringify(manifest, null, 2)}\n`);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await buildAssets();
}
