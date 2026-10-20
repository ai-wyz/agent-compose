import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { preparePiAgentDir } from "../src/runners/pi-mcp.js";

describe("Pi MCP configuration", () => {
  const roots: string[] = [];
  afterEach(async () => Promise.all(roots.splice(0).map((root) => fs.rm(root, { recursive: true, force: true }))));

  it("mirrors the managed agent dir and writes a native mcp.json", async () => {
    const root = await fs.mkdtemp(path.join(os.tmpdir(), "pi-mcp-test-"));
    roots.push(root);
    const managed = path.join(root, "managed");
    const invocation = path.join(root, "invocation");
    await fs.mkdir(managed, { recursive: true });
    await fs.mkdir(invocation);
    await fs.writeFile(path.join(managed, "models.json"), "{}\n");
    await fs.writeFile(path.join(managed, "mcp.json"), '{"mcpServers":{"stale":{}}}\n');

    const isolated = await preparePiAgentDir(managed, invocation, {
      local: { type: "local", command: "mcp-server", args: ["--stdio"], cwd: "/workspace", env: { TOKEN: { value: "secret", secret: true } } },
      remote: { type: "remote", transport: "http", url: "https://mcp.example", headers: { Authorization: { value: "Bearer token", secret: true } } },
    });

    expect(isolated).toBe(path.join(invocation, "agent"));
    // The model catalog stays shared by reference, so the daemon's atomic
    // catalog replacement remains visible to every invocation.
    expect((await fs.lstat(path.join(isolated, "models.json"))).isSymbolicLink()).toBe(true);
    expect(await fs.readFile(path.join(isolated, "models.json"), "utf8")).toBe("{}\n");

    const configPath = path.join(isolated, "mcp.json");
    expect((await fs.stat(configPath)).mode & 0o777).toBe(0o600);
    expect(JSON.parse(await fs.readFile(configPath, "utf8"))).toEqual({
      mcpServers: {
        local: { command: "mcp-server", args: ["--stdio"], env: { TOKEN: "secret" }, cwd: "/workspace" },
        remote: { url: "https://mcp.example", headers: { Authorization: "Bearer token" } },
      },
    });
    // The persistent catalog is never rewritten by a prompt.
    expect(JSON.parse(await fs.readFile(path.join(managed, "mcp.json"), "utf8"))).toEqual({ mcpServers: { stale: {} } });
  });

  it("returns the managed dir and writes nothing when no supported server is configured", async () => {
    const root = await fs.mkdtemp(path.join(os.tmpdir(), "pi-mcp-test-"));
    roots.push(root);
    const managed = path.join(root, "managed");
    const invocation = path.join(root, "invocation");
    await fs.mkdir(managed);
    await fs.mkdir(invocation);

    await expect(preparePiAgentDir(managed, invocation, { invalid: { type: "unknown" } })).resolves.toBe(managed);
    await expect(fs.readdir(invocation)).resolves.toEqual([]);
  });

  it("writes a config when the managed agent dir does not exist yet", async () => {
    const root = await fs.mkdtemp(path.join(os.tmpdir(), "pi-mcp-test-"));
    roots.push(root);
    const managed = path.join(root, "missing");
    const invocation = path.join(root, "invocation");
    await fs.mkdir(invocation);

    const isolated = await preparePiAgentDir(managed, invocation, {
      remote: { type: "remote", url: "https://mcp.example" },
    });
    expect(await fs.readFile(path.join(isolated, "mcp.json"), "utf8")).toContain('"https://mcp.example"');
  });
});
