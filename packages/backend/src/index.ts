import { createServer } from "node:http";
import { WebSocketServer, type WebSocket } from "ws";
import { handleConsoleConnection } from "./console.js";
import { addEventClient, stopEventStream } from "./events.js";
import {
  authenticateWsRequest,
  logSetupInstructions,
  ludockApiToken,
  type WebSocketAuth,
} from "./auth.js";
import { createApp } from "./app.js";
import { closeDatabase } from "./database.js";
import {
  createLogger,
  errorMessage,
  getLogLevelConfiguration,
} from "./logger.js";

const PORT = parseInt(process.env.PORT || "3001", 10);
const MAX_WEBSOCKET_CONNECTIONS = 100;
const WEBSOCKET_SESSION_CHECK_MS = 15_000;
const logger = createLogger("server");

const app = createApp();
const server = createServer(app);
const wss = new WebSocketServer({
  noServer: true,
  maxPayload: 64 * 1024,
  perMessageDeflate: false,
});

server.on("upgrade", (req, socket, head) => {
  let pathname: string;
  try {
    pathname = new URL(req.url || "", `http://${req.headers.host}`).pathname;
  } catch {
    logger.warn("Rejected malformed WebSocket upgrade", {
      remoteAddress: req.socket.remoteAddress,
    });
    socket.write("HTTP/1.1 400 Bad Request\r\n\r\n");
    socket.destroy();
    return;
  }

  logger.debug("WebSocket upgrade requested", {
    path: pathname,
    remoteAddress: req.socket.remoteAddress,
    activeConnections: wss.clients.size,
  });

  const auth = authenticateWsRequest(req);
  if (!auth) {
    logger.debug("Rejected unauthenticated WebSocket upgrade", {
      path: pathname,
      remoteAddress: req.socket.remoteAddress,
    });
    socket.write("HTTP/1.1 401 Unauthorized\r\n\r\n");
    socket.destroy();
    return;
  }
  if (wss.clients.size >= MAX_WEBSOCKET_CONNECTIONS) {
    logger.warn("Rejected WebSocket upgrade at connection limit", {
      path: pathname,
      activeConnections: wss.clients.size,
    });
    socket.write("HTTP/1.1 503 Service Unavailable\r\n\r\n");
    socket.destroy();
    return;
  }

  if (
    pathname.startsWith("/ws/game-console/") ||
    pathname.startsWith("/ws/console/")
  ) {
    wss.handleUpgrade(req, socket, head, (ws) => {
      monitorWebSocketSession(ws, auth);
      void handleConsoleConnection(ws, req, auth, "game").catch((error) => {
        logger.error("Unexpected game console connection failure", {
          path: pathname,
          error: errorMessage(error),
        });
        ws.close(1011, "Console connection failed");
      });
    });
  } else if (pathname.startsWith("/ws/shell/")) {
    if (auth.user.role !== "admin") {
      logger.warn("Rejected unauthorized shell WebSocket upgrade", {
        path: pathname,
        role: auth.user.role,
      });
      socket.write("HTTP/1.1 403 Forbidden\r\n\r\n");
      socket.destroy();
      return;
    }
    wss.handleUpgrade(req, socket, head, (ws) => {
      monitorWebSocketSession(ws, auth, true);
      void handleConsoleConnection(ws, req, auth, "shell").catch((error) => {
        logger.error("Unexpected shell connection failure", {
          path: pathname,
          error: errorMessage(error),
        });
        ws.close(1011, "Shell connection failed");
      });
    });
  } else if (pathname === "/ws/events") {
    wss.handleUpgrade(req, socket, head, (ws) => {
      monitorWebSocketSession(ws, auth);
      addEventClient(ws);
    });
  } else {
    logger.debug("Rejected unknown WebSocket route", { path: pathname });
    socket.write("HTTP/1.1 404 Not Found\r\n\r\n");
    socket.destroy();
  }
});

server.on("clientError", (error, socket) => {
  logger.debug("HTTP client connection error", {
    error: error.message,
  });
  socket.destroy();
});

server.on("error", (error) => {
  logger.error("HTTP server error", { error: error.message });
});

wss.on("error", (error) => {
  logger.error("WebSocket server error", { error: error.message });
});

function monitorWebSocketSession(
  ws: WebSocket,
  auth: WebSocketAuth,
  adminRequired = false
): void {
  const interval = setInterval(() => {
    const user = auth.validate();
    if (!user || (adminRequired && user.role !== "admin")) {
      logger.info("Closing WebSocket after session validation failed", {
        adminRequired,
      });
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
  const logConfiguration = getLogLevelConfiguration();
  logger.info("Ludock listening", {
    address: `http://localhost:${PORT}`,
    logLevel: logConfiguration.level,
  });
  if (logConfiguration.invalidValue) {
    logger.warn("Invalid LOG_LEVEL; using info", {
      configuredValue: logConfiguration.invalidValue,
      supportedValues: "error,warn,info,debug",
    });
  }
  // Surface a rejected API token now rather than on the first request that
  // happens to present a bearer credential.
  ludockApiToken();
  logSetupInstructions();
});

function shutdown(signal: string): void {
  logger.info("Shutting down", { signal });
  stopEventStream();
  for (const client of wss.clients) {
    client.close(1001, "Server shutting down");
  }
  wss.close();
  server.close((error) => {
    closeDatabase();
    if (error) {
      logger.error("Failed to shut down cleanly", {
        error: errorMessage(error),
      });
      process.exitCode = 1;
    } else {
      logger.info("Shutdown complete");
    }
  });
}

process.once("SIGINT", () => shutdown("SIGINT"));
process.once("SIGTERM", () => shutdown("SIGTERM"));
