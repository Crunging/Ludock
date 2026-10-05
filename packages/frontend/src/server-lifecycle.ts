import type { Server } from "@ludock/shared";
import type { PipTone } from "./components/StatusPip";
import { formatRelativeTime, formatRelativeTimeSentence } from "./format";

export function lifecycleActionForState(state: string): "start" | "stop" | null {
  if (state === "running") return "stop";
  if (state === "created" || state === "exited") return "start";
  return null;
}

export function lifecycleStateGuidance(state: string): string | null {
  if (lifecycleActionForState(state)) return null;
  if (state === "paused")
    return "Paused in Docker. Resume it through Docker or its owning manager.";
  if (state === "restarting")
    return "Restart in progress. Controls will be available when it finishes.";
  if (state === "removing") return "Removal in progress.";
  return `Container state: ${state}. Check it in Docker or its owning manager before using server controls.`;
}

// Exit codes from a clean shutdown, SIGINT, or SIGTERM.
const CLEAN_EXITS = new Set([0, 130, 143]);
const SIGKILL_EXIT = 137;

/** Names Docker's state the way players would, using its health check and exit code. */
export function serverStatus(
  server: Pick<Server, "state" | "health"> & Partial<Pick<Server, "exit">>,
): { label: string; tone: PipTone } {
  switch (server.state) {
    case "running":
      if (server.health === "starting") return { label: "Starting", tone: "active" };
      if (server.health === "unhealthy") return { label: "Unhealthy", tone: "attention" };
      return { label: "Running", tone: "ok" };
    case "exited":
      if (server.exit?.oomKilled) return { label: "Out of memory", tone: "failed" };
      // Docker kills a server that doesn't stop within its grace period.
      if (server.exit?.code === SIGKILL_EXIT) return { label: "Force-stopped", tone: "attention" };
      if (server.exit && !CLEAN_EXITS.has(server.exit.code)) return { label: "Crashed", tone: "failed" };
      return { label: "Stopped", tone: "idle" };
    case "created": return { label: "Not started", tone: "idle" };
    case "restarting": return { label: "Restarting", tone: "active" };
    case "paused": return { label: "Paused", tone: "active" };
    case "removing": return { label: "Removing", tone: "active" };
    case "dead": return { label: "Dead", tone: "failed" };
    default:
      return { label: server.state.charAt(0).toUpperCase() + server.state.slice(1), tone: "idle" };
  }
}

/** Why an exited server ended, when it wasn't a clean stop. */
export function exitDetail(server: Partial<Pick<Server, "exit">>): string | null {
  if (!server.exit) return null;
  if (server.exit.oomKilled) return "Ran out of memory";
  return CLEAN_EXITS.has(server.exit.code) ? null : `Exit code ${server.exit.code}`;
}

/** When the current state began. A stop or crash is already named by its label. */
export function stateSinceText(
  server: Pick<Server, "state" | "stateSince">,
  { standalone = false, now = Date.now() } = {},
): string | null {
  if (server.stateSince === null) return null;
  if (server.state === "running") return `Started ${formatRelativeTime(server.stateSince, now)}`;
  return standalone
    ? formatRelativeTimeSentence(server.stateSince, now)
    : formatRelativeTime(server.stateSince, now);
}

/** The address players type, with IPv6 hosts bracketed. Unset hosts use the address of this page. */
export function connectAddress(server: Pick<Server, "connection">): string | null {
  if (!server.connection) return null;
  const host = server.connection.host ?? window.location.hostname;
  return `${host.includes(":") ? `[${host}]` : host}:${server.connection.port}`;
}
