import type { ManagedContainer } from "./docker.js";
import { availabilitySchema } from "@ludock/shared";
import type { AvailabilityPolicy } from "@ludock/shared";
import { getDatabase } from "./database.js";
import { listLogicalServers } from "./identity.js";
import { refreshServers } from "./servers.js";
import { notifyEvent } from "./notifications.js";
import { isServerBusy } from "./operation-locks.js";

interface AvailabilityRow {
  server_id: string;
  policy_json: string;
  outage_started_at: number | null;
  notified: number;
  suppressed_until: number;
  intentionally_stopped: number;
  last_state: string | null;
}
function rowFor(serverId: string): AvailabilityRow | null {
  return getDatabase()
    .query("SELECT * FROM availability WHERE server_id=?")
    .get(serverId) as AvailabilityRow | null;
}
export function getAvailability(serverId: string) {
  const row = rowFor(serverId);
  return {
    policy: row
      ? availabilitySchema.parse(JSON.parse(row.policy_json))
      : { enabled: false, maintenance: false, graceSeconds: 120 },
    state: {
      outageStartedAt: row?.outage_started_at ?? null,
      notified: Boolean(row?.notified),
      suppressedUntil: row?.suppressed_until ?? 0,
      intentionallyStopped: Boolean(row?.intentionally_stopped),
      lastState: row?.last_state ?? null,
    },
  };
}
export function configureAvailability(serverId: string, input: unknown) {
  const policy = availabilitySchema.parse(input);
  getDatabase()
    .query(
      `INSERT INTO availability(server_id,policy_json,updated_at) VALUES(?,?,?) ON CONFLICT(server_id) DO UPDATE SET policy_json=excluded.policy_json,updated_at=excluded.updated_at, outage_started_at=NULL, notified=0`,
    )
    .run(serverId, JSON.stringify(policy), Date.now());
  return getAvailability(serverId);
}
function ensureRow(serverId: string) {
  if (!rowFor(serverId)) configureAvailability(serverId, { enabled: false });
}
export function setIntentionalStop(serverId: string, stopped: boolean): void {
  ensureRow(serverId);
  getDatabase()
    .query(
      "UPDATE availability SET intentionally_stopped=?,outage_started_at=NULL,notified=0,updated_at=? WHERE server_id=?",
    )
    .run(stopped ? 1 : 0, Date.now(), serverId);
}
export function suppressMonitoring(serverId: string): void {
  ensureRow(serverId);
  const { policy } = getAvailability(serverId);
  getDatabase()
    .query(
      "UPDATE availability SET suppressed_until=?,outage_started_at=NULL,notified=0 WHERE server_id=?",
    )
    .run(Date.now() + policy.graceSeconds * 1000, serverId);
}

function monitoringSuppressed(
  serverId: string,
  policy: AvailabilityPolicy,
  suppressedUntil: number,
  now: number,
): boolean {
  return !policy.enabled || policy.maintenance || suppressedUntil > now ||
    isServerBusy(serverId) || Boolean(getDatabase()
      .query("SELECT id FROM operations WHERE server_id=? AND status IN ('queued','running') LIMIT 1")
      .get(serverId));
}

/** Read the monitor's persisted outage without triggering checks or notifications.
 * Policy and operation suppression are current even between monitor ticks. */
export function getAvailabilityProblem(serverId: string, now = Date.now()) {
  const { policy, state } = getAvailability(serverId);
  if (monitoringSuppressed(serverId, policy, state.suppressedUntil, now) ||
    state.intentionallyStopped || state.outageStartedAt === null ||
    now - state.outageStartedAt < policy.graceSeconds * 1000) return null;
  return {
    state: state.lastState ?? "unavailable",
    outageStartedAt: state.outageStartedAt,
  };
}

export async function checkAvailability(now = Date.now()): Promise<void> {
  // A daemon outage is not evidence that containers vanished. Preserve their
  // bindings, but report that monitored availability cannot be verified.
  let dockerUnavailable = false;
  const current = await refreshServers().catch(() => {
    dockerUnavailable = true;
    return new Map<string, ManagedContainer>();
  });
  for (const server of listLogicalServers()) {
    const row = rowFor(server.id);
    if (!row) continue;
    const policy = availabilitySchema.parse(JSON.parse(row.policy_json));
    if (monitoringSuppressed(server.id, policy, row.suppressed_until, now)) continue;
    const container = current.get(server.containerId ?? "");
    const healthy =
      server.status === "active" &&
      container?.state === "running" &&
      !/\((?:unhealthy|starting)\)/i.test(container.status);
    if (healthy) {
      if (row.notified)
        notifyEvent(
          `recovery:${server.id}:${row.outage_started_at}`,
          `${server.displayName} recovered.`,
        );
      getDatabase()
        .query(
          "UPDATE availability SET outage_started_at=NULL,notified=0,intentionally_stopped=0,last_state='running',updated_at=? WHERE server_id=?",
        )
        .run(now, server.id);
    } else if (!row.intentionally_stopped) {
      const started = row.outage_started_at ?? now;
      const notify = now - started >= policy.graceSeconds * 1000;
      if (notify && !row.notified)
        notifyEvent(
          `outage:${server.id}:${started}`,
          dockerUnavailable
            ? `Ludock cannot reach Docker to verify ${server.displayName}.`
            : `${server.displayName} is unavailable.`,
        );
      getDatabase()
        .query(
          "UPDATE availability SET outage_started_at=?,notified=?,last_state=?,updated_at=? WHERE server_id=?",
        )
        .run(
          started,
          notify || row.notified ? 1 : 0,
          dockerUnavailable
            ? "docker_unavailable"
            : (container?.state ?? server.status),
          now,
          server.id,
        );
    }
  }
}
