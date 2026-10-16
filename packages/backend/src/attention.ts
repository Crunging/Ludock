import type { AttentionItem, AttentionResponse } from "@ludock/shared";
import { currentActor, getEffectiveCapabilities } from "./authorization.js";
import { getDatabase, type SessionUser } from "./database.js";
import { listLogicalServers } from "./identity.js";
import { getAvailabilityProblem } from "./monitoring.js";
import { listSchedules } from "./schedules.js";
import { refreshServers } from "./servers.js";

const priority: Record<AttentionItem["kind"], number> = {
  binding: 0,
  availability: 1,
  schedule: 2,
  operation: 3,
};

export async function listAttention(
  actor: SessionUser,
  now = Date.now(),
): Promise<AttentionResponse> {
  let discoveryUnavailable = false;
  await refreshServers().catch(() => { discoveryUnavailable = true; });

  const items: AttentionItem[] = [];
  let hasVisibleServer = false;
  for (const server of listLogicalServers()) {
    // Read current account, role, grant and binding authority after discovery.
    // A stale request principal never exposes a revoked server's saved history.
    const permissions = getEffectiveCapabilities(actor, server);
    if (!permissions.includes("server.view")) continue;
    hasVisibleServer = true;
    const base = { serverId: server.id, serverName: server.displayName };
    if (server.status !== "active") {
      items.push({
        ...base,
        id: `binding:${server.id}`,
        kind: "binding",
        bindingStatus: server.status,
      });
    }
    const availability = getAvailabilityProblem(server.id, now);
    if (availability) {
      items.push({
        ...base,
        ...availability,
        id: `availability:${server.id}`,
        kind: "availability",
      });
    }
    if (permissions.includes("schedules.manage")) {
      for (const schedule of listSchedules(actor, server.id)) {
        if (!schedule.enabled || !schedule.nextRunUnavailableReason) continue;
        items.push({
          ...base,
          id: `schedule:${schedule.id}`,
          kind: "schedule",
          scheduleId: schedule.id,
          action: schedule.action,
          reason: schedule.nextRunUnavailableReason,
        });
      }
    }
    // Authority was checked above. Limit recent work before filtering issues,
    // and read only summary fields; history payloads and cursors are unnecessary.
    const issues = getDatabase().query(`
      SELECT id,kind,status,updated_at AS updatedAt,
        CASE WHEN json_valid(result_json) THEN json_extract(result_json,'$.forceStopped') = 1 ELSE 0 END AS forceStopped
      FROM (
        SELECT id,kind,status,updated_at,result_json FROM operations
        WHERE server_id=? ORDER BY created_at DESC,id DESC LIMIT 100
      ) WHERE status IN ('failed','interrupted') OR (
        status='succeeded' AND kind IN ('stop','restart') AND
        CASE WHEN json_valid(result_json) THEN json_extract(result_json,'$.forceStopped') = 1 ELSE 0 END
      )
    `).all(server.id) as Array<{
      id: string; kind: string; status: "failed" | "interrupted" | "succeeded"; updatedAt: number; forceStopped: number;
    }>;
    for (const operation of issues) {
      items.push({
        ...base,
        id: `operation:${operation.id}`,
        kind: "operation",
        operationId: operation.id,
        operationKind: operation.kind,
        status: operation.status,
        updatedAt: operation.updatedAt,
        ...(operation.forceStopped ? { forceStopped: true } : {}),
      });
    }
  }
  items.sort((left, right) => priority[left.kind] - priority[right.kind] ||
    (left.kind === "operation" && right.kind === "operation"
      ? right.updatedAt - left.updatedAt : 0) ||
    left.serverName.localeCompare(right.serverName) || left.id.localeCompare(right.id));
  return {
    items,
    discoveryUnavailable: discoveryUnavailable &&
      (hasVisibleServer || currentActor(actor)?.role === "admin"),
  };
}
