import { attentionResponseSchema } from "@ludock/shared";
import { listAttention } from "../attention.js";
import { requestUser, respond, type ApiRoutes } from "./request.js";

export const attentionRoutes: ApiRoutes = {
  "/api/v1/attention": {
    GET: async (ctx) => respond(attentionResponseSchema, await listAttention(requestUser(ctx))),
  },
};
