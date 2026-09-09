import {
  serverGrantsResponseSchema,
  bindingReviewResponseSchema,
  bindingReviewRequestSchema,
  serverGrantsSchema,
} from "@ludock/shared";
import { Router, type Router as RouterType } from "express";
import { requireRole } from "../auth.js";
import { findUserById } from "../database.js";
import { listUserServerGrants, setUserServerGrants } from "../authorization.js";
import { getLogicalServer, reviewServerBinding } from "../identity.js";
import { getServer, refreshServers } from "../servers.js";
import { withLocks } from "../operation-locks.js";
import { AppError } from "../errors.js";
import { respond, actor, id, audit } from "./request.js";

export const accessRouter: RouterType = Router();

accessRouter.get(
  "/api/v1/users/:userId/server-grants",
  requireRole("admin"),
  (req, res) => {
    if (!findUserById(id(req.params.userId)))
      throw new AppError("NOT_FOUND", 404, "User not found");
    respond(res, serverGrantsResponseSchema, {
      grants: listUserServerGrants(req.params.userId as string),
    });
  },
);
accessRouter.put(
  "/api/v1/users/:userId/server-grants",
  requireRole("admin"),
  async (req, res) => {
    const input = serverGrantsSchema.parse(req.body);
    await refreshServers();
    respond(res, serverGrantsResponseSchema, {
      grants: setUserServerGrants(
        id(req.params.userId),
        input.grants,
        actor(res),
      ),
    });
  },
);
accessRouter.post(
  "/api/v1/servers/:id/binding-review",
  requireRole("admin"),
  async (req, res) => {
    const serverId = id(req.params.id);
    await refreshServers();
    const server = getLogicalServer(serverId);
    const input = bindingReviewRequestSchema.parse(req.body);
    if (!server || !server.pendingFingerprint)
      throw new AppError(
        "NO_REVIEW",
        409,
        "This server has no pending binding review",
      );
    if (input.confirmation !== server.displayName)
      throw new AppError(
        "CONFIRMATION_REQUIRED",
        400,
        "Type the server name to accept the changed binding",
      );
    await withLocks([`server:${serverId}`], () => {
      reviewServerBinding(serverId, server.pendingFingerprint!);
    });
    audit(actor(res), "server.binding.reviewed", serverId);
    respond(res, bindingReviewResponseSchema, {
      server: await getServer(actor(res), serverId),
    });
  },
);
