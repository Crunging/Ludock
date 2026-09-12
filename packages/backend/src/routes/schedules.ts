import { okResponseSchema, scheduleResponseSchema, schedulesResponseSchema, } from "@ludock/shared";
import {
  createSchedule,
  deleteSchedule,
  listSchedules,
  setScheduleEnabled,
  updateSchedule,
} from "../schedules.js";
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
    PUT: (ctx) => respond(scheduleResponseSchema, {
      schedule: updateSchedule(
        requestUser(ctx), id(ctx.params.id), id(ctx.params.scheduleId), ctx.body,
      ),
    }),
    PATCH: (ctx) => respond(scheduleResponseSchema, {
      schedule: setScheduleEnabled(
        requestUser(ctx), id(ctx.params.id), id(ctx.params.scheduleId), ctx.body,
      ),
    }),
    DELETE: (ctx) => {
      deleteSchedule(requestUser(ctx), id(ctx.params.id), id(ctx.params.scheduleId));
      return respond(okResponseSchema, { ok: true });
    }
  }
};
