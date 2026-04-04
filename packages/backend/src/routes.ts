import { Router } from "express";
import type { Request, Response, Router as RouterType } from "express";
import {
  listManagedContainers,
  getManagedContainer,
  getContainerStats,
  startContainer,
  stopContainer,
  restartContainer,
} from "./docker.js";

export const router: RouterType = Router();

router.get("/api/servers", async (_req: Request, res: Response) => {
  try {
    const servers = await listManagedContainers();
    res.json({ servers });
  } catch (err) {
    console.error("[API] Failed to list servers:", err);
    res.status(500).json({ error: "Failed to list servers" });
  }
});

router.get("/api/servers/:id", async (req: Request, res: Response) => {
  try {
    const id = req.params.id as string;
    const [server, stats] = await Promise.allSettled([
      getManagedContainer(id),
      getContainerStats(id),
    ]);

    if (server.status === "rejected") {
      res.status(404).json({ error: server.reason?.message || "Not found" });
      return;
    }

    res.json({
      server: server.value,
      stats: stats.status === "fulfilled" ? stats.value : null,
    });
  } catch (err) {
    console.error("[API] Failed to get server:", err);
    res.status(500).json({ error: "Failed to get server details" });
  }
});

router.post("/api/servers/:id/start", async (req: Request, res: Response) => {
  try {
    await startContainer(req.params.id as string);
    res.json({ ok: true });
  } catch (err: any) {
    const status = err?.statusCode === 304 ? 200 : 500;
    res
      .status(status)
      .json({ ok: status === 200, error: err?.message });
  }
});

router.post("/api/servers/:id/stop", async (req: Request, res: Response) => {
  try {
    await stopContainer(req.params.id as string);
    res.json({ ok: true });
  } catch (err: any) {
    const status = err?.statusCode === 304 ? 200 : 500;
    res
      .status(status)
      .json({ ok: status === 200, error: err?.message });
  }
});

router.post(
  "/api/servers/:id/restart",
  async (req: Request, res: Response) => {
    try {
      await restartContainer(req.params.id as string);
      res.json({ ok: true });
    } catch (err: any) {
      console.error("[API] Failed to restart container:", err);
      res.status(500).json({ error: "Failed to restart container" });
    }
  }
);
