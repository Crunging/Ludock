import type { WebSocket } from "ws";
import { getDockerInstance, LABEL_ENABLE } from "./docker.js";

const eventClients = new Set<WebSocket>();
let eventStreamActive = false;

export function addEventClient(ws: WebSocket): void {
  eventClients.add(ws);

  ws.on("close", () => eventClients.delete(ws));
  ws.on("error", () => eventClients.delete(ws));

  if (!eventStreamActive) {
    startEventStream();
  }
}

async function startEventStream(): Promise<void> {
  if (eventStreamActive) return;
  eventStreamActive = true;

  const docker = getDockerInstance();

  try {
    const stream = await docker.getEvents({
      filters: {
        type: ["container"],
        label: [`${LABEL_ENABLE}=true`],
      },
    });

    stream.on("data", (chunk: Buffer) => {
      try {
        const event = JSON.parse(chunk.toString());
        const payload = JSON.stringify({
          type: "container_event",
          action: event.Action,
          containerId: event.id,
          name: event.Actor?.Attributes?.name || "",
          time: event.time,
        });

        for (const client of eventClients) {
          if (client.readyState === client.OPEN) {
            client.send(payload);
          }
        }
      } catch {
        // malformed event
      }
    });

    stream.on("error", (err) => {
      console.error("[Events] Docker event stream error:", err.message);
      eventStreamActive = false;
      setTimeout(() => {
        if (eventClients.size > 0) startEventStream();
      }, 5000);
    });

    stream.on("end", () => {
      console.warn("[Events] Docker event stream ended, reconnecting...");
      eventStreamActive = false;
      setTimeout(() => {
        if (eventClients.size > 0) startEventStream();
      }, 2000);
    });
  } catch (err: any) {
    console.error("[Events] Failed to start event stream:", err.message);
    eventStreamActive = false;
  }
}
