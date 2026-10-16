import type { Operation, OperationStatus } from "@ludock/shared";
import type { PipTone } from "./components/StatusPip";

export const operationStatusLabels: Record<OperationStatus, string> = {
  queued: "Queued",
  running: "Running",
  succeeded: "Succeeded",
  already_current: "Already current",
  failed: "Failed",
  interrupted: "Interrupted",
};

export const operationTones: Record<OperationStatus, PipTone> = {
  queued: "active",
  running: "active",
  succeeded: "ok",
  already_current: "ok",
  failed: "failed",
  interrupted: "attention",
};

export const operationActive = (operation: Operation) =>
  operation.status === "queued" || operation.status === "running";

export function historyActorLabel(actor: { id: string | null; name: string | null } | null | undefined): string {
  if (actor?.name) return actor.name;
  if (actor?.id === "api-token") return "API token";
  if (actor?.id) return `User ${actor.id} (name unavailable)`;
  return "Not recorded";
}
export function operationLabel(operation: Pick<Operation, "status" | "phase">): string {
  if (operation.status === "already_current")
    return "Configured image is current";
  if (operation.status === "failed" || operation.status === "interrupted")
    return operation.status === "interrupted"
      ? "Interrupted — review required"
      : "Failed";
  if (operation.status === "succeeded") return "Completed";
  const phases: Record<string, string> = {
    saving_world: "Saving world",
    waiting_for_save: "Waiting for save",
    save_skipped: "Save skipped",
    stopping: "Waiting for server to stop",
    verifying_stop: "Checking shutdown",
    starting: "Starting server",
  };
  if (phases[operation.phase]) return phases[operation.phase];
  return operation.phase.replaceAll("_", " ");
}

export function operationSaveDetail(operation: Operation): string | null {
  if (operation.result?.saveStatus === "sent") return "Save command sent before stopping.";
  if (operation.result?.saveStatus !== "skipped") return null;
  switch (operation.result.saveReason) {
    case "timed_out": return "Save command timed out; shutdown continued.";
    case "console_unavailable": return "Save skipped: console unavailable. Shutdown continued.";
    case "not_running": return "Save skipped: server was already stopped.";
    case "unsupported": return "No save command is configured for this game.";
    default: return "Save skipped; shutdown continued.";
  }
}
