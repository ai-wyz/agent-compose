import fs from "node:fs/promises";
import type { Dirent } from "node:fs";
import path from "node:path";
import { flattenEnvMap } from "../mcp-config.js";

/**
 * Pi 1.x has built-in MCP support but only reads servers from the agent
 * directory `<agent-dir>/mcp.json` or a trusted project's `.pi/mcp.json`; it has
 * no flag for an invocation-scoped config file. Mirror the managed agent
 * directory into this invocation's temporary directory and add the run's
 * `mcp.json` there, so a prompt sees exactly its own servers and never mutates
 * or races on the persistent model catalog.
 */
export async function preparePiAgentDir(
  managedAgentDir: string,
  invocationDir: string,
  configured?: Record<string, unknown>,
): Promise<string> {
  const servers = piMCPServers(configured);
  if (!servers) return managedAgentDir;

  const isolated = path.join(invocationDir, "agent");
  await fs.mkdir(isolated, { recursive: true, mode: 0o700 });
  await linkAgentDir(managedAgentDir, isolated);
  await fs.writeFile(path.join(isolated, "mcp.json"), `${JSON.stringify({ mcpServers: servers }, null, 2)}\n`, {
    encoding: "utf8",
    mode: 0o600,
  });
  return isolated;
}

function piMCPServers(configured?: Record<string, unknown>): Record<string, Record<string, unknown>> | undefined {
  if (!configured || Object.keys(configured).length === 0) return undefined;

  const servers: Record<string, Record<string, unknown>> = {};
  for (const [name, value] of Object.entries(configured)) {
    if (!value || typeof value !== "object" || Array.isArray(value)) continue;
    const server = value as Record<string, unknown>;
    if (server.type === "local") {
      if (typeof server.command !== "string" || server.command === "") continue;
      const entry: Record<string, unknown> = {
        command: server.command,
        args: Array.isArray(server.args) ? server.args : [],
      };
      const env = flattenEnvMap(server.env as Record<string, { value: string }> | undefined);
      if (env) entry.env = env;
      if (typeof server.cwd === "string" && server.cwd !== "") entry.cwd = server.cwd;
      servers[name] = entry;
    } else if (server.type === "remote") {
      if (typeof server.url !== "string" || server.url === "") continue;
      if (server.transport === "sse") {
        throw new Error(`pi runner does not support MCP transport "sse" (server "${name}")`);
      }
      const entry: Record<string, unknown> = { url: server.url };
      const headers = flattenEnvMap(server.headers as Record<string, { value: string }> | undefined);
      if (headers) entry.headers = headers;
      servers[name] = entry;
    }
  }
  return Object.keys(servers).length === 0 ? undefined : servers;
}

async function linkAgentDir(managedAgentDir: string, isolated: string): Promise<void> {
  // Symlink targets must be absolute: a relative PI_CODING_AGENT_DIR would
  // otherwise resolve against the invocation directory and break.
  const source = path.resolve(managedAgentDir);
  let entries: Dirent[];
  try {
    entries = await fs.readdir(source, { withFileTypes: true });
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return;
    throw error;
  }
  for (const entry of entries) {
    // The invocation's own mcp.json replaces any persistent one.
    if (entry.name === "mcp.json") continue;
    await fs.symlink(path.join(source, entry.name), path.join(isolated, entry.name));
  }
}
