import fs from "node:fs/promises";
import path from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import { createProgram } from "../src/cli.js";
import { GUEST_ABI_MANIFEST_PATH, readGuestAbiReport } from "../src/guest-abi.js";
import { captureStdio, withTempSession } from "./helpers.js";

const VALID_MANIFEST = {
  guestAbi: 1,
  providers: {
    codex: {
      version: "0.146.0",
      features: ["structured_output", "session_resume", "streaming"],
    },
  },
};

const MALFORMED_MANIFESTS: Array<[string, string]> = [
  ["truncated JSON", "{"],
  ["a JSON array", "[]"],
  ["a missing guestAbi", JSON.stringify({ providers: {} })],
  ["a non-numeric guestAbi", JSON.stringify({ guestAbi: "1", providers: {} })],
  ["a provider without features", JSON.stringify({ guestAbi: 1, providers: { codex: { version: "1.0.0" } } })],
  ["a provider with non-string features", JSON.stringify({ guestAbi: 1, providers: { codex: { version: "1.0.0", features: [1] } } })],
];

async function installedRuntimeVersion(): Promise<string> {
  const raw = await fs.readFile(new URL("../package.json", import.meta.url), "utf8");
  return (JSON.parse(raw) as { version: string }).version;
}

async function writeManifest(root: string, contents: string): Promise<string> {
  const manifestPath = path.join(root, "guest-abi.json");
  await fs.writeFile(manifestPath, contents, "utf8");
  return manifestPath;
}

async function runAbiCommand(args: string[]): Promise<{ stdout: string; stderr: string }> {
  const stdio = captureStdio();
  try {
    await createProgram({ exitOverride: true }).parseAsync(["node", "cli", "abi", ...args]);
  } finally {
    stdio.restore();
  }
  return { stdout: stdio.stdout, stderr: stdio.stderr };
}

describe("readGuestAbiReport", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("reports a valid manifest verbatim", async () => {
    await withTempSession(async (root) => {
      const manifestPath = await writeManifest(root, JSON.stringify(VALID_MANIFEST, null, 2));

      const report = await readGuestAbiReport({ manifestPath });

      expect(report).toEqual({
        guestAbi: 1,
        providers: {
          codex: {
            version: "0.146.0",
            features: ["structured_output", "session_resume", "streaming"],
          },
        },
        manifestPath,
        runtimeVersion: await installedRuntimeVersion(),
      });
    });
  });

  it("defaults to the manifest path baked into the guest image", async () => {
    const report = await readGuestAbiReport();

    expect(report.manifestPath).toBe(GUEST_ABI_MANIFEST_PATH);
    expect(GUEST_ABI_MANIFEST_PATH).toBe("/etc/agent-compose/guest-abi.json");
  });

  it("reports an unknown guest without throwing when the manifest is missing", async () => {
    await withTempSession(async (root) => {
      const manifestPath = path.join(root, "absent", "guest-abi.json");

      const report = await readGuestAbiReport({ manifestPath });

      expect(report.guestAbi).toBe(0);
      expect(report.providers).toEqual({});
      expect(report.manifestPath).toBe(manifestPath);
      expect(report.runtimeVersion).toBe(await installedRuntimeVersion());
    });
  });

  it.each(MALFORMED_MANIFESTS)("reports an unknown guest when the manifest contains %s", async (_name, contents) => {
    await withTempSession(async (root) => {
      const manifestPath = await writeManifest(root, contents);

      const report = await readGuestAbiReport({ manifestPath });

      expect(report.guestAbi).toBe(0);
      expect(report.providers).toEqual({});
      expect(report.manifestPath).toBe(manifestPath);
      expect(report.runtimeVersion).toBe(await installedRuntimeVersion());
    });
  });
});

describe("abi command", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("prints a single line of parseable JSON for --json", async () => {
    await withTempSession(async (root) => {
      const manifestPath = await writeManifest(root, JSON.stringify(VALID_MANIFEST));

      const { stdout, stderr } = await runAbiCommand(["--json", "--manifest", manifestPath]);

      expect(stderr).toBe("");
      expect(stdout.trim().split("\n")).toHaveLength(1);
      expect(JSON.parse(stdout)).toEqual({
        guestAbi: 1,
        providers: {
          codex: {
            version: "0.146.0",
            features: ["structured_output", "session_resume", "streaming"],
          },
        },
        manifestPath,
        runtimeVersion: await installedRuntimeVersion(),
      });
    });
  });

  it("succeeds with an unknown guest when the manifest is absent", async () => {
    await withTempSession(async (root) => {
      const manifestPath = path.join(root, "absent.json");

      const { stdout, stderr } = await runAbiCommand(["--json", "--manifest", manifestPath]);

      expect(stderr).toBe("");
      expect(JSON.parse(stdout)).toMatchObject({
        guestAbi: 0,
        providers: {},
        manifestPath,
      });
    });
  });

  it("reads the manifest from --manifest instead of the default path", async () => {
    await withTempSession(async (root) => {
      const manifestPath = await writeManifest(root, JSON.stringify({
        guestAbi: 7,
        providers: { pi: { version: "9.9.9", features: ["streaming"] } },
      }));

      const { stdout } = await runAbiCommand(["--json", "--manifest", manifestPath]);

      const payload = JSON.parse(stdout);
      expect(payload.manifestPath).toBe(manifestPath);
      expect(payload.guestAbi).toBe(7);
      expect(payload.providers).toEqual({ pi: { version: "9.9.9", features: ["streaming"] } });
    });
  });

  it("prints a human-readable summary without --json", async () => {
    await withTempSession(async (root) => {
      const manifestPath = await writeManifest(root, JSON.stringify(VALID_MANIFEST));

      const { stdout, stderr } = await runAbiCommand(["--manifest", manifestPath]);

      expect(stderr).toBe("");
      expect(stdout).toContain("guest ABI: 1");
      expect(stdout).toContain(`manifest: ${manifestPath}`);
      expect(stdout).toContain("  codex 0.146.0 (3 features)");
    });
  });

  it("summarizes an unknown guest without listing providers", async () => {
    await withTempSession(async (root) => {
      const manifestPath = path.join(root, "absent.json");

      const { stdout } = await runAbiCommand(["--manifest", manifestPath]);

      expect(stdout).toContain("guest ABI: 0");
      expect(stdout).toContain("providers: none");
    });
  });
});
