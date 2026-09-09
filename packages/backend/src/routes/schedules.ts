import {
  schedulesResponseSchema,
  scheduleResponseSchema,
  okResponseSchema,
} from "@ludock/shared";
import { Router, type Router as RouterType } from "express";
import { createSchedule, deleteSchedule, listSchedules } from "../schedules.js";
import { respond, actor, id } from "./request.js";

export const schedulesRouter: RouterType = Router();

schedulesRouter.get("/api/v1/servers/:id/schedules", (req, res) =>
  respond(res, schedulesResponseSchema, {
    schedules: listSchedules(actor(res), id(req.params.id)),
  }),
);
schedulesRouter.post("/api/v1/servers/:id/schedules", (req, res) =>
  respond(res.status(201), scheduleResponseSchema, {
    schedule: createSchedule(actor(res), id(req.params.id), req.body),
  }),
);
schedulesRouter.delete(
  "/api/v1/servers/:id/schedules/:scheduleId",
  (req, res) => {
    deleteSchedule(actor(res), id(req.params.id), id(req.params.scheduleId));
    respond(res, okResponseSchema, { ok: true });
  },
);
