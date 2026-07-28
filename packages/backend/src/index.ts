import { createServer } from "node:http";
import { WebSocketServer, type WebSocket } from "ws";
import { handleConsoleConnection } from "./console.js";
import { addEventClient, stopEventStream } from "./events.js";
import {
  authenticateWsRequest,
  logSetupInstructions,
  panelApiToken,
  type WebSocketAuth,
} from "./auth.js";
import { createApp } from "./app.js";
import { closeDatabase } from "./database.js";

const PORT = parseInt(process.env.PORT || "3001", 10);
const MAX_WEBSOCKET_CONNECTIONS = 100;
const WEBSOCKET_SESSION_CHECK_MS = 15_000;

const app = createApp();
const server = createServer(app);
const wss = new WebSocketServer({
  noServer: true,
  maxPayload: 64 * 1024,
  perMessageDeflate: false,
});

server.on("upgrade", (req, socket, head) => {
  const auth = authenticateWsRequest(req);
  if (!auth) {
    socket.write("HTTP/1.1 401 Unauthorized\r\n\r\n");
    socket.destroy();
    return;
  }
  if (wss.clients.size >= MAX_WEBSOCKET_CONNECTIONS) {
    socket.write("HTTP/1.1 503 Service Unavailable\r\n\r\n");
    socket.destroy();
    return;
  }

  let pathname: string;
  try {
    pathname = new URL(req.url || "", `http://${req.headers.host}`).pathname;
  } catch {
    socket.write("HTTP/1.1 400 Bad Request\r\n\r\n");
    socket.destroy();
    return;
  }

  if (
    pathname.startsWith("/ws/game-console/") ||
    pathname.startsWith("/ws/console/")
  ) {
    wss.handleUpgrade(req, socket, head, (ws) => {
      monitorWebSocketSession(ws, auth);
      void handleConsoleConnection(ws, req, auth, "game");
    });
  } else if (pathname.startsWith("/ws/shell/")) {
    if (auth.user.role !== "admin") {
      socket.write("HTTP/1.1 403 Forbidden\r\n\r\n");
      socket.destroy();
      return;
    }
    wss.handleUpgrade(req, socket, head, (ws) => {
      monitorWebSocketSession(ws, auth, true);
      void handleConsoleConnection(ws, req, auth, "shell");
    });
  } else if (pathname === "/ws/events") {
    wss.handleUpgrade(req, socket, head, (ws) => {
      monitorWebSocketSession(ws, auth);
      addEventClient(ws);
    });
  } else {
    socket.write("HTTP/1.1 404 Not Found\r\n\r\n");
    socket.destroy();
  }
});

function monitorWebSocketSession(
  ws: WebSocket,
  auth: WebSocketAuth,
  adminRequired = false
): void {
  const interval = setInterval(() => {
    const user = auth.validate();
    if (!user || (adminRequired && user.role !== "admin")) {
      ws.close(1008, "Session expired or access revoked");
      return;
    }
    auth.user = user;
  }, WEBSOCKET_SESSION_CHECK_MS);
  interval.unref();
  const stop = () => clearInterval(interval);
  ws.once("close", stop);
  ws.once("error", stop);
}

server.listen(PORT, () => {
  console.log(`Docker Game Manager listening on http://localhost:${PORT}`);
  // Surface a rejected API token now rather than on the first request that
  // happens to present a bearer credential.
  panelApiToken();
  logSetupInstructions();
});

function shutdown(signal: string): void {
  console.log(`Received ${signal}, shutting down...`);
  stopEventStream();
  for (const client of wss.clients) {
    client.close(1001, "Server shutting down");
  }
  wss.close();
  server.close((error) => {
    closeDatabase();
    if (error) {
      console.error("Failed to shut down cleanly:", error);
      process.exitCode = 1;
    }
  });
}

process.once("SIGINT", () => shutdown("SIGINT"));
process.once("SIGTERM", () => shutdown("SIGTERM"));
