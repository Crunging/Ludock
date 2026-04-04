import { createServer } from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";
import express from "express";
import cors from "cors";
import { WebSocketServer } from "ws";
import { router } from "./routes.js";
import { handleConsoleConnection } from "./console.js";
import { addEventClient } from "./events.js";
import { authMiddleware, validateWsAuth, isAuthEnabled } from "./auth.js";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PORT = parseInt(process.env.PORT || "3001", 10);

const app = express();
app.use(cors());
app.use(express.json());
app.use(authMiddleware);
app.use(router);

const frontendDist = path.resolve(__dirname, "../../frontend/dist");
app.use(express.static(frontendDist));
app.get("/{*splat}", (_req, res) => {
  res.sendFile(path.join(frontendDist, "index.html"));
});

const server = createServer(app);
const wss = new WebSocketServer({ noServer: true });

server.on("upgrade", (req, socket, head) => {
  if (!validateWsAuth(req.url || "")) {
    socket.write("HTTP/1.1 401 Unauthorized\r\n\r\n");
    socket.destroy();
    return;
  }

  const pathname = new URL(req.url || "", `http://${req.headers.host}`).pathname;

  if (pathname.startsWith("/ws/console/")) {
    wss.handleUpgrade(req, socket, head, (ws) => {
      handleConsoleConnection(ws, req);
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
  if (isAuthEnabled()) console.log("Auth enabled via PANEL_SECRET");
});
