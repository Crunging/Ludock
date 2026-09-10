import { serve } from "bun";
import { stopEventStream } from "./events.js";
import { logSetupInstructions, ludockApiToken } from "./auth.js";
import { createApp } from "./app.js";
import { closeDatabase } from "./database.js";
import { registerBackgroundJobs } from "./jobs.js";
import { startOperationRunner, stopOperationRunner } from "./operations.js";
import { checkAvailability } from "./monitoring.js";
import { runSchedules } from "./schedules.js";
import { deliverNotifications } from "./notifications.js";
import { refreshServers } from "./servers.js";
import { waitForLocksReleased } from "./operation-locks.js";
import { createWebSocketGateway } from "./websocket-server.js";
import {
  createLogger,
  errorMessage,
  getLogLevelConfiguration,
} from "./logger.js";

const logger = createLogger("server");

export function startServer(options: {
  port?: number;
  hostname?: string;
  frontendDist?: string | false;
} = {}) {
  registerBackgroundJobs();
  const app = createApp({ frontendDist: options.frontendDist });
  const sockets = createWebSocketGateway();
  let backgroundTask: Promise<void> | undefined;
  let shuttingDown = false;
  let shutdownTask: Promise<void> | undefined;
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
    })().finally(() => { backgroundTask = undefined; });
    return backgroundTask;
  }

  const server = serve({
    ...app,
    port: options.port ?? parseInt(process.env.PORT || "3001", 10),
    hostname: options.hostname ?? process.env.HOST,
    websocket: sockets.websocket,
    fetch(request, server) {
      if (request.headers.get("upgrade")?.toLowerCase() === "websocket")
        return sockets.upgrade(request, server);
      return app.fetch(request, server);
    },
    error() {
      logger.error("HTTP request could not be served");
      return Response.json({ error: "Internal server error" }, { status: 500 });
    },
  });

  const logConfiguration = getLogLevelConfiguration();
  logger.info("Ludock listening", {
    address: `http://localhost:${server.port}`,
    logLevel: logConfiguration.level,
  });
  if (logConfiguration.invalidValue) {
    logger.warn("Invalid LOG_LEVEL; using info", {
      configuredValue: logConfiguration.invalidValue,
      supportedValues: "error,warn,info,debug",
    });
  }
  ludockApiToken();
  logSetupInstructions();
  void startOperationRunner().then(() => {
    if (shuttingDown) return;
    void backgroundTick();
    backgroundTimer = setInterval(() => void backgroundTick(), 15_000);
    backgroundTimer.unref();
  }).catch(() => logger.error("Operation recovery requires administrator attention"));

  function shutdown(signal = "shutdown"): Promise<void> {
    if (shutdownTask) return shutdownTask;
    shuttingDown = true;
    logger.info("Shutting down", { signal });
    // Stop new HTTP admission immediately. Native stop() preserves active
    // requests while WebSockets get a close frame and a bounded grace period.
    const httpStopped = server.stop();
    const socketsStopped = sockets.close();
    const eventsStopped = stopEventStream();
    if (backgroundTimer) clearInterval(backgroundTimer);
    const operationsStopped = stopOperationRunner();
    shutdownTask = (async () => {
      const results = await Promise.allSettled([
        backgroundTask, operationsStopped, eventsStopped, httpStopped, socketsStopped,
      ]);
      // A console command or streamed file request can outlive its connection.
      // Its lock and cleanup must settle before the shared database is closed.
      await waitForLocksReleased();
      closeDatabase();
      const failure = results.find((result) => result.status === "rejected");
      if (failure?.status === "rejected") {
        logger.error("Failed to shut down cleanly", { error: errorMessage(failure.reason) });
        process.exitCode = 1;
      } else logger.info("Shutdown complete");
    })();
    return shutdownTask;
  }

  return { server, shutdown };
}

if (import.meta.main) {
  let runtime: ReturnType<typeof startServer> | undefined = undefined;
  let initialSignal: string | undefined;
  const stop = (signal: string) => {
    if (runtime) void runtime.shutdown(signal);
    else initialSignal = signal;
  };
  // Install handlers before listen/readiness output so an immediate container
  // stop follows the same cleanup path as a long-running server.
  process.on("SIGINT", () => stop("SIGINT"));
  process.on("SIGTERM", () => stop("SIGTERM"));
  runtime = startServer();
  if (initialSignal) void runtime.shutdown(initialSignal);
}
