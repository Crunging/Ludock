import type { Operation } from "@ludock/shared";

export const operationActive = (operation: Operation) =>
  operation.status === "queued" || operation.status === "running";
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
