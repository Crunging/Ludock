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
