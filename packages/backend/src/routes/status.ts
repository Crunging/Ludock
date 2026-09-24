import { availabilityResponseSchema, operationHistoryQuerySchema, operationResponseSchema, operationsResponseSchema, } from "@ludock/shared";
import { assertServerCapability } from "../authorization.js";
import { AppError } from "../errors.js";
import { configureAvailability, getAvailability } from "../monitoring.js";
import { getOperation, publicOperation, } from "../operations.js";
import { listOperationHistory } from "../history.js";
import { administrator, audit, id, requestUser, respond, type ApiRoutes } from "./request.js";

export const statusRoutes: ApiRoutes = {
  "/api/v1/operations": {
    GET: (ctx) => respond(operationsResponseSchema, listOperationHistory(
      requestUser(ctx), operationHistoryQuerySchema.parse(Object.fromEntries(ctx.url.searchParams)),
    )),
  },
  "/api/v1/servers/:id/operations": {
    GET: (ctx) => {
      const serverId = id(ctx.params.id);
      assertServerCapability(requestUser(ctx), serverId, "server.view");
      const query = operationHistoryQuerySchema.parse(Object.fromEntries(ctx.url.searchParams));
      if (query.serverId && query.serverId !== serverId)
        throw new AppError("INVALID_HISTORY_FILTER", 400, "The server filter must match this server");
      return respond(operationsResponseSchema, listOperationHistory(requestUser(ctx), { ...query, serverId }));
    }
  },
  "/api/v1/operations/:id": {
    GET: (ctx) => {
      const operation = getOperation(id(ctx.params.id));
      if (!operation)
        throw new AppError("NOT_FOUND", 404, "Operation not found");
      assertServerCapability(requestUser(ctx), operation.serverId, "server.view");
      return respond(operationResponseSchema, {
        operation: publicOperation(operation),
      });
    }
  },
  "/api/v1/servers/:id/availability": {
    GET: (ctx) => {
      const serverId = id(ctx.params.id);
      assertServerCapability(requestUser(ctx), serverId, "server.view");
      return respond(availabilityResponseSchema, getAvailability(serverId));
    },
    PUT: administrator((ctx) => {
      const serverId = id(ctx.params.id);
      assertServerCapability(requestUser(ctx), serverId, "server.view");
      const availability = configureAvailability(serverId, ctx.body);
      audit(ctx, "availability.configured", serverId);
      return respond(availabilityResponseSchema, availability);
    })
  }
};
