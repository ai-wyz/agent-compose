import fs from "node:fs/promises";
import { readRuntimeVersion } from "./runtime-version.js";

/** Where a guest image bakes the manifest that describes its agent capabilities. */
export const GUEST_ABI_MANIFEST_PATH = "/etc/agent-compose/guest-abi.json";

/**
 * Reported when no readable manifest describes the guest. It is deliberately
 * not a valid ABI number: callers treat `0` as an unknown/legacy guest and
 * degrade to their fallback instead of trusting invented capabilities.
 */
export const UNKNOWN_GUEST_ABI = 0;

export interface GuestAbiProvider {
  version: string;
  features: string[];
}

export interface GuestAbiReport {
  guestAbi: number;
  providers: Record<string, GuestAbiProvider>;
  manifestPath: string;
  runtimeVersion: string;
}

export interface ReadGuestAbiReportOptions {
  manifestPath?: string;
}

interface GuestAbiManifest {
  guestAbi: number;
  providers: Record<string, GuestAbiProvider>;
}

/**
 * Describes the guest this runtime runs in.
 *
 * This is an operator-facing diagnostic: an absent, unreadable, or malformed
 * manifest is reported as an unknown guest rather than an error, so the caller
 * can still print a report and fall back. Only a broken installation (a missing
 * package manifest) fails.
 */
export async function readGuestAbiReport(options: ReadGuestAbiReportOptions = {}): Promise<GuestAbiReport> {
  const manifestPath = options.manifestPath ?? GUEST_ABI_MANIFEST_PATH;
  const manifest = await readGuestAbiManifest(manifestPath);
  return {
    guestAbi: manifest?.guestAbi ?? UNKNOWN_GUEST_ABI,
    providers: manifest?.providers ?? {},
    manifestPath,
    runtimeVersion: await readRuntimeVersion(),
  };
}

async function readGuestAbiManifest(manifestPath: string): Promise<GuestAbiManifest | undefined> {
  let text: string;
  try {
    text = await fs.readFile(manifestPath, "utf8");
  } catch {
    // A guest without a manifest (or without read access to it) is still a
    // usable legacy guest; the command reports it as unknown instead of failing.
    return undefined;
  }
  return parseGuestAbiManifest(text);
}

function parseGuestAbiManifest(text: string): GuestAbiManifest | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (!isRecord(parsed)) {
    return undefined;
  }
  const guestAbi = parsed.guestAbi;
  if (typeof guestAbi !== "number" || !Number.isInteger(guestAbi)) {
    return undefined;
  }
  const providers = parseProviders(parsed.providers);
  if (providers === undefined) {
    return undefined;
  }
  return { guestAbi, providers };
}

function parseProviders(raw: unknown): Record<string, GuestAbiProvider> | undefined {
  if (!isRecord(raw)) {
    return undefined;
  }
  const providers: Record<string, GuestAbiProvider> = {};
  for (const [name, value] of Object.entries(raw)) {
    if (!isRecord(value) || typeof value.version !== "string") {
      return undefined;
    }
    const { features } = value;
    if (!Array.isArray(features) || !features.every((feature) => typeof feature === "string")) {
      return undefined;
    }
    providers[name] = { version: value.version, features: [...features] };
  }
  return providers;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
