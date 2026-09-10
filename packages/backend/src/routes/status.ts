import { availabilityResponseSchema, operationResponseSchema, operationsResponseSchema, } from "@ludock/shared";
import { assertServerCapability } from "../authorization.js";
import { AppError } from "../errors.js";
import { configureAvailability, getAvailability } from "../monitoring.js";
import { getOperation, listOperations, publicOperation, } from "../operations.js";
import { administrator, audit, id, requestUser, respond, type ApiRoutes } from "./request.js";

export const statusRoutes: ApiRoutes = {
  "/api/v1/servers/:id/operations": {
    GET: (ctx) => {
      const serverId = id(ctx.params.id);
      assertServerCapability(requestUser(ctx), serverId, "server.view");
      return respond(operationsResponseSchema, {
        operations: listOperations(serverId),
      });
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
      audit(requestUser(ctx), "availability.configured", serverId);
      return respond(availabilityResponseSchema, availability);
    })
  }
};
