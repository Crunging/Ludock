import { bindingReviewRequestSchema, bindingReviewResponseSchema, serverGrantsResponseSchema, serverGrantsSchema, } from "@ludock/shared";
import { assertRequestUser } from "../auth.js";
import { assertAdministrator, listUserServerGrants, setUserServerGrants } from "../authorization.js";
import { findUserById } from "../database.js";
import { AppError } from "../errors.js";
import { getLogicalServer, reviewServerBinding } from "../identity.js";
import { withLocks } from "../operation-locks.js";
import { getServer, refreshServers } from "../servers.js";
import { administrator, audit, id, requestUser, respond, type ApiRoutes } from "./request.js";

export const accessRoutes: ApiRoutes = {
  "/api/v1/users/:userId/server-grants": {
    GET: administrator((ctx) => {
      if (!findUserById(id(ctx.params.userId)))
        throw new AppError("NOT_FOUND", 404, "User not found");
      return respond(serverGrantsResponseSchema, {
        grants: listUserServerGrants(ctx.params.userId),
      });
    }),
    PUT: administrator(async (ctx) => {
      const input = serverGrantsSchema.parse(ctx.body);
      await refreshServers();
      return respond(serverGrantsResponseSchema, {
        grants: setUserServerGrants(id(ctx.params.userId), input.grants, assertRequestUser(ctx.request, requestUser(ctx))),
      });
    })
  },
  "/api/v1/servers/:id/binding-review": {
    POST: administrator(async (ctx) => {
      const serverId = id(ctx.params.id);
      await refreshServers();
      const server = getLogicalServer(serverId);
      const input = bindingReviewRequestSchema.parse(ctx.body);
      if (!server || !server.pendingFingerprint)
        throw new AppError("NO_REVIEW", 409, "This server has no pending binding review");
      if (input.confirmation !== server.displayName)
        throw new AppError("CONFIRMATION_REQUIRED", 400, "Type the server name to accept the changed binding");
      await withLocks([`server:${serverId}`], () => {
        assertAdministrator(assertRequestUser(ctx.request, requestUser(ctx)));
        reviewServerBinding(serverId, server.pendingFingerprint!);
      });
      audit(ctx, "server.binding.reviewed", serverId);
      return respond(bindingReviewResponseSchema, {
        server: await getServer(requestUser(ctx), serverId),
      });
    })
  }
};
