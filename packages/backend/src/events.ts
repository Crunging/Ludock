import { SERVER_STATE_ACTIONS, serverEventSchema } from "@ludock/shared";
import { StringDecoder } from "node:string_decoder";
import type { WebSocket } from "ws";
import type { WebSocketAuth } from "./auth.js";
import { currentActor, hasServerCapability } from "./authorization.js";
import { getDockerInstance } from "./docker.js";
import { listLogicalServers } from "./identity.js";
import { refreshServers } from "./servers.js";
import { createLogger } from "./logger.js";

const eventClients = new Map<WebSocket, WebSocketAuth>();
let eventStreamActive = false;
let eventStream: (NodeJS.ReadableStream & { destroy?: () => void }) | null =
  null;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
let streamGeneration = 0;
const logger = createLogger("events");

export interface DockerEvent {
  Action?: string;
  Actor?: { ID?: string };
  id?: string;
  time?: number;
}

const STATE_ACTIONS = new Set<string>(SERVER_STATE_ACTIONS);

/** Docker JSON events can span chunks or share a chunk. Bound incomplete input. */
export function dockerEventDecoder(
  deliver: (event: DockerEvent) => void,
): (chunk: Buffer) => void {
  const decoder = new StringDecoder("utf8");
  let pending = "";
  return (chunk) => {
    pending += decoder.write(chunk);
    if (Buffer.byteLength(pending) > 1_048_576)
      throw new Error("Docker event buffer exceeded its limit");
    let newline: number;
    while ((newline = pending.indexOf("\n")) !== -1) {
      const line = pending.slice(0, newline);
      pending = pending.slice(newline + 1);
      if (!line.trim()) continue;
      try {
        const value: unknown = JSON.parse(line);
        if (value && typeof value === "object" && !Array.isArray(value))
          deliver(value);
      } catch {
        /* Ignore malformed lines without recording their contents. */
      }
    }
  };
}

export function addEventClient(ws: WebSocket, auth: WebSocketAuth): void {
  eventClients.set(ws, auth);
  const removeClient = () => {
    eventClients.delete(ws);
    if (eventClients.size === 0) stopEventStream();
  };
  ws.on("close", removeClient);
  ws.on("error", removeClient);
  if (!eventStreamActive) void startEventStream();
}

export function stopEventStream(): void {
  streamGeneration++;
  if (reconnectTimer) clearTimeout(reconnectTimer);
  reconnectTimer = null;
  const stream = eventStream;
  eventStream = null;
  eventStreamActive = false;
  stream?.destroy?.();
}

/** Send only logical identifiers after a fresh eligible snapshot and grant check. */
export async function dispatchDockerEvent(event: DockerEvent): Promise<void> {
  if (typeof event.Action !== "string" || !STATE_ACTIONS.has(event.Action))
    return;
  const containerId = event.Actor?.ID || event.id;
  if (
    typeof containerId !== "string" ||
    !/^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/.test(containerId)
  )
    return;
  const previous = listLogicalServers().find(
    (server) => server.containerId === containerId,
  );
  const previouslyVisible = new Set<WebSocket>();
  if (previous) {
    for (const [client, auth] of eventClients) {
      if (hasServerCapability(auth.validate(), previous.id, "server.view"))
        previouslyVisible.add(client);
    }
  }
  await refreshServers();
  const current = listLogicalServers().find(
    (server) => server.containerId === containerId,
  );
  for (const [client, auth] of eventClients) {
    if (client.readyState !== client.OPEN) continue;
    const user = currentActor(auth.validate());
    if (!user) {
      client.close(1008, "Authentication expired");
      continue;
    }
    const visible =
      current && hasServerCapability(user, current.id, "server.view");
    // A former viewer needs a content-free invalidation to remove a disappeared
    // or suspended server. It reveals nothing about any other server.
    if (!visible && !previouslyVisible.has(client)) continue;
    client.send(
      JSON.stringify(
        serverEventSchema.parse({
          type: "container_event",
          action: visible ? event.Action : "refresh",
          ...(visible ? { serverId: current.id } : {}),
          time:
            typeof event.time === "number" && Number.isFinite(event.time)
              ? event.time
              : Math.floor(Date.now() / 1000),
        }),
      ),
    );
  }
}

async function startEventStream(): Promise<void> {
  if (eventStreamActive) return;
  eventStreamActive = true;
  const generation = streamGeneration;
  try {
    const stream = await getDockerInstance().getEvents({
      filters: { type: ["container"] },
    });
    if (generation !== streamGeneration || eventClients.size === 0) {
      (stream as NodeJS.ReadableStream & { destroy?: () => void }).destroy?.();
      return;
    }
    eventStream = stream;
    // Coalesce Docker bursts; dashboard updates need the latest state, not an
    // unbounded queue of health/exec events. Only state actions enter the queue.
    const pending = new Map<string, DockerEvent>();
    let draining = false;
    const drain = async () => {
      if (draining) return;
      draining = true;
      try {
        while (pending.size && eventStream === stream) {
          const [id, event] = pending.entries().next().value!;
          pending.delete(id);
          try {
            await dispatchDockerEvent(event);
          } catch {
            logger.warn("Unable to refresh Docker event state");
          }
        }
      } finally {
        draining = false;
      }
    };
    const decode = dockerEventDecoder((event) => {
      if (!event.Action || !STATE_ACTIONS.has(event.Action)) return;
      const id = event.Actor?.ID || event.id;
      if (typeof id !== "string" || pending.size >= 1024) return;
      pending.set(id, event);
      void drain();
    });
    stream.on("data", (chunk: Buffer) => {
      try {
        decode(chunk);
      } catch {
        scheduleReconnect(5000);
      }
    });
    stream.on("error", () => {
      if (eventStream === stream) scheduleReconnect(5000);
    });
    stream.on("end", () => {
      if (eventStream === stream) scheduleReconnect(2000);
    });
  } catch {
    if (generation === streamGeneration) scheduleReconnect(5000);
  }
}

function scheduleReconnect(delay: number): void {
  if (reconnectTimer) return;
  stopEventStream();
  if (eventClients.size === 0) return;
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    void startEventStream();
  }, delay);
  reconnectTimer.unref();
}
