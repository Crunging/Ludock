import type { Response } from "express";
import { authStatusSchema, okResponseSchema } from "@ludock/shared";
import { respond } from "../../src/routes/request.js";
import { serverAction } from "../../src/routes/server-action.js";

// Compile-only checks ensure that runtime parsing complements producer types
// and every new direct server action explicitly declares its capability.
export function verifyRouteContracts(response: Response): void {
  respond(response, okResponseSchema, { ok: true });
  // @ts-expect-error A response cannot widen the schema's literal true to boolean.
  respond(response, okResponseSchema, { ok: false });
  // @ts-expect-error A response cannot widen the schema's field type.
  respond(response, okResponseSchema, { ok: "true" });
  // @ts-expect-error Authenticated responses require an actual user.
  respond(response, authStatusSchema, {
    setupRequired: false,
    setupLocked: false,
    setupExpiresAt: null,
    setupRemainingMs: null,
    authenticated: true,
    user: null,
  });
  serverAction("files.read", async () => {});
  // @ts-expect-error A new direct action cannot omit its capability.
  serverAction(async () => {});
  // @ts-expect-error Capabilities must be registered in the shared contract.
  serverAction("server.everything", async () => {});
}
