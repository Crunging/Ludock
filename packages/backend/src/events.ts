import type { WebSocket } from "ws";
import { getDockerInstance, LABEL_ENABLE } from "./docker.js";
import { createLogger, errorMessage } from "./logger.js";

const eventClients = new Set<WebSocket>();
let eventStreamActive = false;
let eventStream: (NodeJS.ReadableStream & { destroy?: () => void }) | null = null;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
const logger = createLogger("events");

interface DockerEvent {
  Action?: string;
  Actor?: {
    ID?: string;
    Attributes?: { name?: string };
  };
  id?: string;
  time?: number;
}

function parseDockerEvent(chunk: Buffer): DockerEvent | null {
  const value = JSON.parse(chunk.toString()) as unknown;
  return typeof value === "object" && value !== null ? value : null;
}

export function addEventClient(ws: WebSocket): void {
  eventClients.add(ws);
  logger.debug("Event WebSocket client connected", {
    clients: eventClients.size,
  });

  const removeClient = () => {
    eventClients.delete(ws);
    logger.debug("Event WebSocket client disconnected", {
      clients: eventClients.size,
    });
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
    const docker = getDockerInstance();

    const stream = await docker.getEvents({
      filters: {
        type: ["container"],
        label: [`${LABEL_ENABLE}=true`],
      },
    });
    eventStream = stream;
    logger.debug("Docker event stream attached", {
      clients: eventClients.size,
    });

    stream.on("data", (chunk: Buffer) => {
      try {
        const event = parseDockerEvent(chunk);
        if (!event) return;
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

    stream.on("error", (error: Error) => {
      if (eventStream !== stream) return;
      logger.warn("Docker event stream failed", { error: error.message });
      scheduleReconnect(5000);
    });

    stream.on("end", () => {
      if (eventStream !== stream) return;
      logger.warn("Docker event stream ended; reconnecting");
      scheduleReconnect(2000);
    });
  } catch (error: unknown) {
    logger.warn("Failed to start Docker event stream", {
      error: errorMessage(error),
    });
    scheduleReconnect(5000);
  }
}

function scheduleReconnect(delay: number): void {
  if (reconnectTimer) return;

  eventStream = null;
  eventStreamActive = false;
  if (eventClients.size === 0) return;
  logger.debug("Scheduled Docker event stream reconnect", { delayMs: delay });

  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    void startEventStream();
  }, delay);
}
