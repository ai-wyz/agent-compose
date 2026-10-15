import fs from "node:fs/promises";

/**
 * Reads the version of this runtime package from its own manifest.
 *
 * The package publishes `dist/` as a sibling of `package.json`, and Vitest runs
 * this module from `src/`, so `../package.json` resolves to the installed
 * package manifest in both layouts. This avoids a hand-maintained version
 * literal and a JSON import that would move `rootDir` above `src/`.
 */
export async function readRuntimeVersion(): Promise<string> {
  const manifestURL = new URL("../package.json", import.meta.url);
  const parsed: unknown = JSON.parse(await fs.readFile(manifestURL, "utf8"));
  if (!isRecord(parsed) || typeof parsed.version !== "string" || parsed.version === "") {
    throw new Error(`runtime package manifest has no version: ${manifestURL.pathname}`);
  }
  return parsed.version;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
