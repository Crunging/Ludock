import { okResponseSchema, scheduleResponseSchema, schedulesResponseSchema, } from "@ludock/shared";
import { createSchedule, deleteSchedule, listSchedules } from "../schedules.js";
import { id, requestUser, respond, type ApiRoutes } from "./request.js";

export const schedulesRoutes: ApiRoutes = {
  "/api/v1/servers/:id/schedules": {
    GET: (ctx) => respond(schedulesResponseSchema, {
      schedules: listSchedules(requestUser(ctx), id(ctx.params.id)),
    }),
    POST: (ctx) => respond(scheduleResponseSchema, {
      schedule: createSchedule(requestUser(ctx), id(ctx.params.id), ctx.body),
    }, 201)
  },
  "/api/v1/servers/:id/schedules/:scheduleId": {
    DELETE: (ctx) => {
      deleteSchedule(requestUser(ctx), id(ctx.params.id), id(ctx.params.scheduleId));
      return respond(okResponseSchema, { ok: true });
    }
  }
};
