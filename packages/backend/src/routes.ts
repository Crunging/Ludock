import { Router, type Router as RouterType } from "express";
import { serversRouter } from "./routes/servers.js";
import { filesRouter } from "./routes/files.js";

export const router: RouterType = Router();
router.use(serversRouter, filesRouter);
