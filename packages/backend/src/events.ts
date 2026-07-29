import type { WebSocket } from "ws";
import { getDockerInstance, LABEL_ENABLE } from "./docker.js";

const eventClients = new Set<WebSocket>();
let eventStreamActive = false;
let eventStream: (NodeJS.ReadableStream & { destroy?: () => void }) | null = null;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;

export function addEventClient(ws: WebSocket): void {
  eventClients.add(ws);

  const removeClient = () => {
    eventClients.delete(ws);
    if (eventClients.size === 0) stopEventStream();
  };
  ws.on("close", removeClient);
  ws.on("error", removeClient);

  if (!eventStreamActive) {
    void startEventStream();
  }
}

export function stopEventStream(): void {
  if (reconnectTimer) {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
  eventStream?.destroy?.();
  eventStream = null;
  eventStreamActive = false;
}

async function startEventStream(): Promise<void> {
  if (eventStreamActive) return;
  eventStreamActive = true;

  try {
    // Resolving the client is part of the attempt: when Docker is unreachable
    // this throws, and outside the try it would reject the returned promise
    // with no handler, taking the process down.
    const docker = getDockerInstance();

    const stream = await docker.getEvents({
      filters: {
        type: ["container"],
        label: [`${LABEL_ENABLE}=true`],
      },
    });
    eventStream = stream;

    stream.on("data", (chunk: Buffer) => {
      try {
        const event = JSON.parse(chunk.toString());
        const payload = JSON.stringify({
          type: "container_event",
          action: event.Action,
          containerId: event.Actor?.ID || event.id || "",
          name: event.Actor?.Attributes?.name || "",
          time: event.time,
        });

        for (const client of eventClients) {
          if (client.readyState === client.OPEN) {
            client.send(payload);
          }
        }
      } catch {
        return;
      }
    });

    stream.on("error", (error) => {
      if (eventStream !== stream) return;
      console.error("[Events] Docker event stream error:", error.message);
      scheduleReconnect(5000);
    });

    stream.on("end", () => {
      if (eventStream !== stream) return;
      console.warn("[Events] Docker event stream ended, reconnecting...");
      scheduleReconnect(2000);
    });
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : String(error);
    console.error("[Events] Failed to start event stream:", message);
    scheduleReconnect(5000);
  }
}

function scheduleReconnect(delay: number): void {
  if (reconnectTimer) return;

  eventStream = null;
  eventStreamActive = false;
  if (eventClients.size === 0) return;

  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    void startEventStream();
  }, delay);
}
