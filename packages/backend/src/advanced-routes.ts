import { Router, type Router as RouterType } from "express";
import { accessRouter } from "./routes/access.js";
import { backupsRouter } from "./routes/backups.js";
import { composeRouter } from "./routes/compose.js";
import { schedulesRouter } from "./routes/schedules.js";
import { statusRouter } from "./routes/status.js";
import { settingsRouter } from "./routes/settings.js";

export const advancedRouter: RouterType = Router();
advancedRouter.use(
  accessRouter,
  backupsRouter,
  composeRouter,
  schedulesRouter,
  statusRouter,
  settingsRouter,
);
