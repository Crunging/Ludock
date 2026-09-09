import { createServer } from "node:http";
import { WebSocketServer, type WebSocket } from "ws";
import { handleConsoleConnection } from "./console.js";
import { handleContainerLogsConnection } from "./container-logs.js";
import { addEventClient, stopEventStream } from "./events.js";
import {
  authenticateWsRequest,
  logSetupInstructions,
  ludockApiToken,
  type WebSocketAuth,
} from "./auth.js";
import { createApp } from "./app.js";
import { closeDatabase } from "./database.js";
import { registerBackgroundJobs } from "./jobs.js";
import { startOperationRunner, stopOperationRunner } from "./operations.js";
import { checkAvailability } from "./monitoring.js";
import { runSchedules } from "./schedules.js";
import { deliverNotifications } from "./notifications.js";
import { refreshServers } from "./servers.js";
import { waitForLocksReleased } from "./operation-locks.js";
import { matchesDevelopmentInstance } from "./development-instance.js";
import {
  createLogger,
  errorMessage,
  getLogLevelConfiguration,
} from "./logger.js";

const PORT = parseInt(process.env.PORT || "3001", 10);
const HOST = process.env.HOST;
const MAX_WEBSOCKET_CONNECTIONS = 100;
const WEBSOCKET_SESSION_CHECK_MS = 15_000;
const logger = createLogger("server");

registerBackgroundJobs();
const app = createApp();
let backgroundTask: Promise<void> | undefined;
let shuttingDown = false;
let backgroundTimer: ReturnType<typeof setInterval> | undefined;
function backgroundTick(): Promise<void> {
  if (backgroundTask || shuttingDown)
    return backgroundTask ?? Promise.resolve();
  backgroundTask = (async () => {
    try {
      await refreshServers();
      runSchedules();
    } catch {
      logger.warn("Discovery and schedules are temporarily unavailable");
    }
    try {
      await checkAvailability();
    } catch {
      logger.warn("Availability checks are temporarily unavailable");
    }
    try {
      await deliverNotifications();
    } catch {
      logger.warn("Notification delivery is temporarily unavailable");
    }
  })().finally(() => {
    backgroundTask = undefined;
  });
  return backgroundTask;
}
const server = createServer(app);
const wss = new WebSocketServer({
  noServer: true,
  maxPayload: 64 * 1024,
  perMessageDeflate: false,
});

server.on("upgrade", (req, socket, head) => {
  if (!matchesDevelopmentInstance(req.headers["x-ludock-dev-instance"])) {
    socket.write("HTTP/1.1 409 Conflict\r\n\r\n");
    socket.destroy();
    return;
  }
  if (shuttingDown) {
    socket.write("HTTP/1.1 503 Service Unavailable\r\n\r\n");
    socket.destroy();
    return;
  }
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

  if (pathname.startsWith("/ws/v1/logs/")) {
    wss.handleUpgrade(req, socket, head, (ws) => {
      monitorWebSocketSession(ws, auth);
      void handleContainerLogsConnection(ws, req, auth).catch((error) => {
        logger.error("Unexpected Docker log connection failure", {
          path: pathname,
          error: errorMessage(error),
        });
        ws.close(1011, "Log connection failed");
      });
    });
  } else if (
    pathname.startsWith("/ws/v1/game-console/") ||
    pathname.startsWith("/ws/v1/console/")
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
  } else if (pathname.startsWith("/ws/v1/shell/")) {
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
  } else if (pathname === "/ws/v1/events") {
    wss.handleUpgrade(req, socket, head, (ws) => {
      monitorWebSocketSession(ws, auth);
      addEventClient(ws, auth);
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
  process.exitCode = 1;
});

wss.on("error", (error) => {
  logger.error("WebSocket server error", { error: error.message });
});

function monitorWebSocketSession(
  ws: WebSocket,
  auth: WebSocketAuth,
  adminRequired = false,
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

server.listen(PORT, HOST, () => {
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
  void startOperationRunner()
    .then(() => {
      if (shuttingDown) return;
      void backgroundTick();
      backgroundTimer = setInterval(() => void backgroundTick(), 15_000);
      backgroundTimer.unref();
    })
    .catch(() =>
      logger.error("Operation recovery requires administrator attention"),
    );
});

async function shutdown(signal: string): Promise<void> {
  if (shuttingDown) return;
  shuttingDown = true;
  logger.info("Shutting down", { signal });
  // Stop admission first. Active requests may finish, but a long-running job
  // must not leave the listener accepting new work throughout shutdown.
  const httpClosed = new Promise<Error | undefined>((resolve) =>
    server.close(resolve),
  );
  for (const client of wss.clients) client.close(1001, "Server shutting down");
  const socketsClosed = new Promise<void>((resolve) =>
    wss.close(() => resolve()),
  );
  const terminateSockets = setTimeout(() => {
    for (const client of wss.clients) client.terminate();
  }, 5000);
  terminateSockets.unref();
  stopEventStream();
  if (backgroundTimer) clearInterval(backgroundTimer);
  const operationsStopped = stopOperationRunner();
  await backgroundTask;
  await operationsStopped;
  const error = await httpClosed;
  await socketsClosed;
  clearTimeout(terminateSockets);
  await waitForLocksReleased();
  closeDatabase();
  if (error) {
    logger.error("Failed to shut down cleanly", { error: errorMessage(error) });
    process.exitCode = 1;
  } else logger.info("Shutdown complete");
}

process.on("SIGINT", () => void shutdown("SIGINT"));
process.on("SIGTERM", () => void shutdown("SIGTERM"));
