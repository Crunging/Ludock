import type { Server, ServerCapability } from "@ludock/shared";
import type { AuthUser } from "./auth-context";

export const CAPABILITY_LABELS = {
  "server.view": "View server",
  "server.start": "Start",
  "server.stop": "Stop",
  "server.restart": "Restart",
  "logs.read": "Read logs",
  "console.execute": "Send console commands",
  "files.read": "Read and download files",
  "files.write": "Change files",
  "backups.create": "Create backups",
  "schedules.manage": "Manage own schedules",
} satisfies Partial<Record<ServerCapability, string>>;
export type GrantCapability = keyof typeof CAPABILITY_LABELS;

export const VIEWER_CAPABILITIES: ServerCapability[] = [
  "server.view",
  "logs.read",
  "files.read",
];

// Effective permissions come from the API. Role ceilings also protect against
// stale or malformed responses; backend authorization remains authoritative.
export function can(
  user: AuthUser | null,
  server: Server | null,
  capability: ServerCapability,
): boolean {
  if (!user || !server?.permissions.includes("server.view") || !server.permissions.includes(capability))
    return false;
  if (user.role === "admin") return true;
  if (!(capability in CAPABILITY_LABELS)) return false;
  return user.role !== "viewer" || VIEWER_CAPABILITIES.includes(capability);
}

export function toggleGrant(
  current: ServerCapability[],
  capability: ServerCapability,
  checked: boolean,
): ServerCapability[] {
  const selected = new Set(current);
  if (checked) {
    selected.add(capability);
    selected.add("server.view");
    if (capability === "files.write") selected.add("files.read");
  } else {
    selected.delete(capability);
    if (capability === "server.view") selected.clear();
    if (capability === "files.read") selected.delete("files.write");
  }
  return Object.keys(CAPABILITY_LABELS).filter((key) =>
    selected.has(key as ServerCapability),
  ) as ServerCapability[];
}
