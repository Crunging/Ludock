import type { AttentionItem, AttentionResponse } from "@ludock/shared";
import { currentActor, getEffectiveCapabilities } from "./authorization.js";
import type { SessionUser } from "./database.js";
import { listLogicalServers } from "./identity.js";
import { getAvailabilityProblem } from "./monitoring.js";
import { listOperations } from "./operations.js";
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
    // Match the operation history screen's most recent 100 operations. Only
    // summary fields are copied: inputs, results and errors stay on that screen.
    for (const operation of listOperations(server.id)) {
      if (operation.status !== "failed" && operation.status !== "interrupted") continue;
      items.push({
        ...base,
        id: `operation:${operation.id}`,
        kind: "operation",
        operationId: operation.id,
        operationKind: operation.kind,
        status: operation.status,
        updatedAt: operation.updatedAt,
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
