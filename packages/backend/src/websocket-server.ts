import type { Server, ServerWebSocket, WebSocketHandler } from "bun";
import { authenticateWsRequest, type WebSocketAuth } from "./auth.js";
import { handleConsoleConnection } from "./console.js";
import { handleContainerLogsConnection } from "./container-logs.js";
import { addEventClient } from "./events.js";
import { matchesDevelopmentInstance } from "./development-instance.js";
import { createLogger } from "./logger.js";
import { MAX_SOCKET_BUFFER_BYTES, NativeSocketChannel } from "./socket-channel.js";

export const MAX_WEBSOCKET_CONNECTIONS = 100;
export const MAX_WEBSOCKET_CONNECTIONS_PER_USER = 12;
export const MAX_WEBSOCKET_CONNECTIONS_PER_SESSION = 8;
export const RESERVED_ADMIN_WEBSOCKET_CONNECTIONS = 10;
export const MAX_WEBSOCKET_PAYLOAD_BYTES = 64 * 1024;
const logger = createLogger("websocket");

export interface SocketSession {
  request: Request;
  remoteAddress?: string;
  auth: WebSocketAuth;
  kind: "events" | "logs" | "game" | "shell";
  socket?: ServerWebSocket<SocketSession>;
  channel?: NativeSocketChannel;
  admissionTimer?: ReturnType<typeof setTimeout>;
}

export function createWebSocketGateway() {
  const sessions = new Set<SocketSession>();
  const opening = new Set<Promise<void>>();
  let stopping = false;
  let closing: Promise<void> | undefined;
  let socketsClosed: (() => void) | undefined;

  const remove = (session: SocketSession) => {
    if (session.admissionTimer) clearTimeout(session.admissionTimer);
    sessions.delete(session);
    if (sessions.size === 0) socketsClosed?.();
  };
  const countSessions = (matches: (session: SocketSession) => boolean) => {
    let count = 0;
    for (const session of sessions) if (matches(session)) count++;
    return count;
  };
  const valid = (session: SocketSession) => {
    const user = session.auth.validate();
    if (!user || (session.kind === "shell" && user.role !== "admin")) {
      session.channel?.close(1008, "Session expired or access revoked");
      return false;
    }
    session.auth.user = user;
    return true;
  };
  const sessionTimer = setInterval(() => {
    for (const session of sessions) if (session.channel?.isOpen) valid(session);
  }, 15_000);
  sessionTimer.unref();

  const websocket: WebSocketHandler<SocketSession> = {
    maxPayloadLength: MAX_WEBSOCKET_PAYLOAD_BYTES,
    backpressureLimit: MAX_SOCKET_BUFFER_BYTES,
    closeOnBackpressureLimit: true,
    perMessageDeflate: false,
    open(socket) {
      const session = socket.data;
      if (session.admissionTimer) clearTimeout(session.admissionTimer);
      session.socket = socket;
      session.channel = new NativeSocketChannel(socket);
      if (stopping) {
        session.channel.close(1001, "Server shutting down");
        return;
      }
      if (!sessions.has(session)) {
        session.channel.close(1008, "WebSocket upgrade expired");
        return;
      }
      if (!valid(session)) return;
      const channel = session.channel;
      const start = async () => {
        if (session.kind === "events") addEventClient(channel, session.auth);
        else if (session.kind === "logs")
          await handleContainerLogsConnection(channel, session.request, session.auth, session.remoteAddress);
        else
          await handleConsoleConnection(channel, session.request, session.auth, session.kind, session.remoteAddress);
      };
      const work = start().catch(() => {
        logger.warn("WebSocket connection could not open", { kind: session.kind });
        channel.close(1011, "Connection failed");
      }).finally(() => opening.delete(work));
      opening.add(work);
    },
    message(socket, message) {
      const session = socket.data;
      if (stopping || !session.channel?.isOpen || !valid(session)) return;
      try {
        session.channel.receive(message);
      } catch {
        logger.warn("WebSocket message could not be processed", { kind: session.kind });
        session.channel.close(1011, "Message processing failed");
      }
    },
    close(socket, code) {
      remove(socket.data);
      socket.data.channel?.finish(code);
    },
  };

  return {
    websocket,
    get connectionCount() { return sessions.size; },
    upgrade(request: Request, server: Pick<Server<SocketSession>, "upgrade" | "requestIP">): Response | undefined {
      if (!matchesDevelopmentInstance(request.headers.get("x-ludock-dev-instance") ?? undefined))
        return new Response("Development instance mismatch", { status: 409 });
      if (stopping) return new Response("Server shutting down", { status: 503 });
      const pathname = new URL(request.url).pathname;
      const auth = authenticateWsRequest(request);
      if (!auth) return new Response("Authentication required", { status: 401 });
      const kind = pathname === "/ws/v1/events" ? "events"
        : pathname.startsWith("/ws/v1/logs/") ? "logs"
          : pathname.startsWith("/ws/v1/game-console/") ? "game"
            : pathname.startsWith("/ws/v1/shell/") ? "shell" : null;
      if (!kind) return new Response("Unknown WebSocket endpoint", { status: 404 });
      if (kind === "shell" && auth.user.role !== "admin")
        return new Response("Administrator access required", { status: 403 });
      if (countSessions((session) => session.auth.user.id === auth.user.id) >=
        MAX_WEBSOCKET_CONNECTIONS_PER_USER)
        return new Response("WebSocket user connection limit reached", { status: 429 });
      if (auth.sessionTokenHash && countSessions((session) =>
        session.auth.sessionTokenHash === auth.sessionTokenHash) >=
          MAX_WEBSOCKET_CONNECTIONS_PER_SESSION)
        return new Response("WebSocket session connection limit reached", { status: 429 });
      if (sessions.size >= MAX_WEBSOCKET_CONNECTIONS)
        return new Response("WebSocket connection limit reached", { status: 503 });
      if (auth.user.role !== "admin" && countSessions((session) =>
        session.auth.user.role !== "admin") >=
          MAX_WEBSOCKET_CONNECTIONS - RESERVED_ADMIN_WEBSOCKET_CONNECTIONS)
        return new Response("WebSocket capacity reserved for administrators", { status: 503 });
      const session: SocketSession = {
        request, auth, kind, remoteAddress: server.requestIP(request)?.address,
      };
      // Count an accepted upgrade before its open callback so concurrent
      // handshakes cannot pass the connection cap together.
      sessions.add(session);
      session.admissionTimer = setTimeout(() => {
        if (!session.socket) remove(session);
      }, 5000);
      session.admissionTimer.unref();
      try {
        if (server.upgrade(request, { data: session })) return undefined;
      } catch {
        // Invalid upgrade headers are client data, not diagnostics to expose.
      }
      remove(session);
      return new Response("WebSocket upgrade failed", { status: 400 });
    },
    close(): Promise<void> {
      if (closing) return closing;
      stopping = true;
      clearInterval(sessionTimer);
      closing = (async () => {
        const closed = new Promise<void>((resolve) => {
          socketsClosed = resolve;
          if (!sessions.size) resolve();
        });
        for (const session of sessions) {
          if (session.channel) session.channel.close(1001, "Server shutting down");
          else remove(session);
        }
        const force = setTimeout(() => {
          for (const session of sessions) {
            session.socket?.terminate();
            session.channel?.finish(1001);
            remove(session);
          }
        }, 5000);
        force.unref();
        try {
          await closed;
          await Promise.allSettled(opening);
        } finally {
          clearTimeout(force);
          socketsClosed = undefined;
        }
      })();
      return closing;
    },
  };
}
