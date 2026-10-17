import { SERVER_STATE_ACTIONS, serverEventSchema } from "@ludock/shared";
import { JsonLineDecoder } from "./json-lines.js";
import type { SocketChannel } from "./socket-channel.js";
import type { WebSocketAuth } from "./auth.js";
import { currentActor, hasServerCapability } from "./authorization.js";
import { docker } from "./docker-client.js";
import { listLogicalServers } from "./identity.js";
import { refreshServers } from "./servers.js";
import { createLogger } from "./logger.js";

const eventClients = new Map<SocketChannel, WebSocketAuth>();
const eventDeliveries = new Set<Promise<void>>();
let eventStreamActive = false;
let eventStream: ReturnType<ReadableStream<Uint8Array>["getReader"]> | null = null;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
let streamGeneration = 0;
const logger = createLogger("events");

interface DockerEvent {
  Action?: string;
  Actor?: { ID?: string };
  id?: string;
  time?: number;
}

const STATE_ACTIONS = new Set<string>(SERVER_STATE_ACTIONS);

/** Docker JSON events can span chunks or share a chunk. Bound incomplete input. */
export function dockerEventDecoder(
  deliver: (event: DockerEvent) => void,
): (chunk: Uint8Array) => void {
  const decoder = new JsonLineDecoder(value => {
    if (value && typeof value === "object" && !Array.isArray(value)) deliver(value);
  }, true);
  return chunk => decoder.push(chunk);
}

export function addEventClient(ws: SocketChannel, auth: WebSocketAuth): void {
  if (!ws.isOpen) return;
  eventClients.set(ws, auth);
  const removeClient = () => {
    eventClients.delete(ws);
    if (eventClients.size === 0) void stopEventStream();
  };
  ws.onClose(removeClient);
  if (!eventStreamActive) void startEventStream();
}

export async function stopEventStream(): Promise<void> {
  streamGeneration++;
  if (reconnectTimer) clearTimeout(reconnectTimer);
  reconnectTimer = null;
  const stream = eventStream;
  eventStream = null;
  eventStreamActive = false;
  await stream?.cancel().catch(() => {});
  // A decoded event may still be awaiting a Docker refresh. It must finish
  // before shutdown closes the database used by its authorization checks.
  await Promise.allSettled(eventDeliveries);
}

/** Send only logical identifiers after a fresh eligible snapshot and grant check. */
export function dispatchDockerEvent(event: DockerEvent): Promise<void> {
  return dispatchDockerEvents([event]);
}

/** One discovery refresh covers a whole burst of container events. */
function dispatchDockerEvents(events: readonly DockerEvent[]): Promise<void> {
  const delivery = deliverDockerEvents(events).finally(() => eventDeliveries.delete(delivery));
  eventDeliveries.add(delivery);
  return delivery;
}

function stateChange(event: DockerEvent): { action: string; containerId: string } | null {
  if (typeof event.Action !== "string" || !STATE_ACTIONS.has(event.Action))
    return null;
  const containerId = event.Actor?.ID || event.id;
  if (
    typeof containerId !== "string" ||
    !/^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/.test(containerId)
  )
    return null;
  return { action: event.Action, containerId };
}

async function deliverDockerEvents(events: readonly DockerEvent[]): Promise<void> {
  const changes = events.flatMap((event) => {
    const change = stateChange(event);
    return change ? [{ ...change, event }] : [];
  });
  if (!changes.length) return;
  const before = listLogicalServers();
  const previouslyVisible = changes.map(({ containerId }) => {
    const previous = before.find((server) => server.containerId === containerId);
    const clients = new Set<SocketChannel>();
    if (previous) {
      for (const [client, auth] of eventClients) {
        if (hasServerCapability(auth.validate(), previous.id, "server.view"))
          clients.add(client);
      }
    }
    return clients;
  });
  await refreshServers();
  const after = listLogicalServers();
  changes.forEach(({ action, containerId, event }, index) => {
    const current = after.find((server) => server.containerId === containerId);
    for (const [client, auth] of eventClients) {
      if (!client.isOpen) continue;
      const user = currentActor(auth.validate());
      if (!user) {
        client.close(1008, "Authentication expired");
        continue;
      }
      const visible =
        current && hasServerCapability(user, current.id, "server.view");
      // A former viewer needs a content-free invalidation to remove a disappeared
      // or suspended server. It reveals nothing about any other server.
      if (!visible && !previouslyVisible[index].has(client)) continue;
      client.send(
        JSON.stringify(
          serverEventSchema.parse({
            type: "container_event",
            action: visible ? action : "refresh",
            ...(visible ? { serverId: current.id } : {}),
            time:
              typeof event.time === "number" && Number.isFinite(event.time)
                ? event.time
                : Math.floor(Date.now() / 1000),
          }),
        ),
      );
    }
  });
}

async function startEventStream(): Promise<void> {
  if (eventStreamActive) return;
  eventStreamActive = true;
  const generation = streamGeneration;
  try {
    const stream = await docker.getEvents({
      filters: { type: ["container"] },
    });
    if (generation !== streamGeneration || eventClients.size === 0) {
      await stream.cancel().catch(() => {});
      return;
    }
    const reader = stream.getReader();
    eventStream = reader;
    // Coalesce Docker bursts; dashboard updates need the latest state, not an
    // unbounded queue of health/exec events. Only state actions enter the queue,
    // and each drained batch shares one discovery refresh.
    const pending = new Map<string, DockerEvent>();
    let draining = false;
    const drain = async () => {
      if (draining) return;
      draining = true;
      try {
        while (pending.size && eventStream === reader) {
          const batch = [...pending.values()];
          pending.clear();
          try {
            await dispatchDockerEvents(batch);
          } catch {
            logger.warn("Unable to refresh Docker event state");
          }
        }
      } finally {
        draining = false;
      }
    };
    const decode = dockerEventDecoder((event) => {
      const change = stateChange(event);
      if (!change || pending.size >= 1024) return;
      pending.set(change.containerId, event);
      void drain();
    });
    void (async () => {
      try {
        while (eventStream === reader) {
          const { value, done } = await reader.read();
          if (eventStream !== reader) return;
          if (done) { scheduleReconnect(2000); return; }
          decode(value);
        }
      } catch {
        if (eventStream === reader) scheduleReconnect(5000);
      } finally {
        await reader.cancel().catch(() => {});
        reader.releaseLock();
      }
    })();
  } catch {
    if (generation === streamGeneration) scheduleReconnect(5000);
  }
}

function scheduleReconnect(delay: number): void {
  if (reconnectTimer) return;
  void stopEventStream();
  if (eventClients.size === 0) return;
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    void startEventStream();
  }, delay);
  reconnectTimer.unref();
}
