import {
  operationsResponseSchema,
  operationResponseSchema,
  availabilityResponseSchema,
} from "@ludock/shared";
import { Router, type Router as RouterType } from "express";
import { requireRole } from "../auth.js";
import { assertServerCapability } from "../authorization.js";
import { configureAvailability, getAvailability } from "../monitoring.js";
import {
  getOperation,
  listOperations,
  publicOperation,
} from "../operations.js";
import { AppError } from "../errors.js";
import { respond, actor, id, audit } from "./request.js";

export const statusRouter: RouterType = Router();

statusRouter.get("/api/v1/servers/:id/operations", (req, res) => {
  const serverId = id(req.params.id);
  assertServerCapability(actor(res), serverId, "server.view");
  respond(res, operationsResponseSchema, {
    operations: listOperations(serverId),
  });
});
statusRouter.get("/api/v1/operations/:id", (req, res) => {
  const operation = getOperation(id(req.params.id));
  if (!operation) throw new AppError("NOT_FOUND", 404, "Operation not found");
  assertServerCapability(actor(res), operation.serverId, "server.view");
  respond(res, operationResponseSchema, {
    operation: publicOperation(operation),
  });
});
statusRouter.get("/api/v1/servers/:id/availability", (req, res) => {
  const serverId = id(req.params.id);
  assertServerCapability(actor(res), serverId, "server.view");
  respond(res, availabilityResponseSchema, getAvailability(serverId));
});
statusRouter.put(
  "/api/v1/servers/:id/availability",
  requireRole("admin"),
  (req, res) => {
    const serverId = id(req.params.id);
    assertServerCapability(actor(res), serverId, "server.view");
    respond(
      res,
      availabilityResponseSchema,
      configureAvailability(serverId, req.body),
    );
    audit(actor(res), "availability.configured", serverId);
  },
);
