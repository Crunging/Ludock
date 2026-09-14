import type { Operation, OperationStatus } from "@ludock/shared";

export const operationStatusLabels: Record<OperationStatus, string> = {
  queued: "Queued",
  running: "Running",
  succeeded: "Succeeded",
  already_current: "Already current",
  failed: "Failed",
  interrupted: "Interrupted",
};

export const operationActive = (operation: Operation) =>
  operation.status === "queued" || operation.status === "running";

export function historyActorLabel(actor: { id: string | null; name: string | null } | null | undefined): string {
  if (actor?.name) return actor.name;
  if (actor?.id === "api-token") return "API token";
  if (actor?.id) return `User ${actor.id} (name unavailable)`;
  return "Not recorded";
}
export function operationLabel(operation: Operation): string {
  if (operation.status === "already_current")
    return "Configured image is current";
  if (operation.status === "failed" || operation.status === "interrupted")
    return operation.status === "interrupted"
      ? "Interrupted — review required"
      : "Failed";
  if (operation.status === "succeeded") return "Completed";
  return operation.phase.replaceAll("_", " ");
}
