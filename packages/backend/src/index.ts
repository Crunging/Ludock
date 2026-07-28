import { createServer } from "node:http";
import { WebSocketServer } from "ws";
import { handleConsoleConnection } from "./console.js";
import { addEventClient, stopEventStream } from "./events.js";
import { authenticateWsRequest, logSetupInstructions } from "./auth.js";
import { createApp } from "./app.js";
import { closeDatabase } from "./database.js";

const PORT = parseInt(process.env.PORT || "3001", 10);

const app = createApp();
const server = createServer(app);
const wss = new WebSocketServer({ noServer: true, maxPayload: 64 * 1024 });

server.on("upgrade", (req, socket, head) => {
  const user = authenticateWsRequest(req);
  if (!user) {
    socket.write("HTTP/1.1 401 Unauthorized\r\n\r\n");
    socket.destroy();
    return;
  }

  const pathname = new URL(req.url || "", `http://${req.headers.host}`).pathname;

  if (
    pathname.startsWith("/ws/game-console/") ||
    pathname.startsWith("/ws/console/")
  ) {
    wss.handleUpgrade(req, socket, head, (ws) => {
      void handleConsoleConnection(ws, req, user, "game");
    });
  } else if (pathname.startsWith("/ws/shell/")) {
    if (user.role !== "admin") {
      socket.write("HTTP/1.1 403 Forbidden\r\n\r\n");
      socket.destroy();
      return;
    }
    wss.handleUpgrade(req, socket, head, (ws) => {
      void handleConsoleConnection(ws, req, user, "shell");
    });
  } else if (pathname === "/ws/events") {
    wss.handleUpgrade(req, socket, head, (ws) => {
      addEventClient(ws);
    });
  } else {
    socket.write("HTTP/1.1 404 Not Found\r\n\r\n");
    socket.destroy();
  }
});

server.listen(PORT, () => {
  console.log(`Docker Game Manager listening on http://localhost:${PORT}`);
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
