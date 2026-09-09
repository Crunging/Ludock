import { z } from "zod";
import type { Response } from "express";
import type { ResponseSchema } from "@ludock/shared";
import { writeAuditLog, type SessionUser } from "../database.js";
import { AppError } from "../errors.js";

export const actor = (res: { locals: Record<string, unknown> }): SessionUser =>
  res.locals.user as SessionUser;
const identifier = z.string().uuid();
export const id = (value: unknown): string => identifier.parse(value);
export function audit(
  user: SessionUser,
  action: string,
  targetId?: string,
  details?: Record<string, unknown>,
): void {
  writeAuditLog({
    userId: user.id === "api-token" ? undefined : user.id,
    action,
    targetType: targetId ? "server" : "settings",
    targetId,
    details,
  });
}
export function requestKey(value: string | undefined): string | undefined {
  if (value && !/^[\w.-]{1,128}$/.test(value))
    throw new AppError("INVALID_REQUEST_KEY", 400, "Invalid idempotency key");
  return value;
}

/** Invalid output is a server defect, not an invalid client request. Parsing
 * also keeps internal fields outside the public response contract. */
export function respond<T>(
  res: Response,
  schema: ResponseSchema<T>,
  value: NoInfer<T>,
): void {
  let data: T;
  try {
    data = schema.parse(value);
  } catch {
    throw new AppError(
      "INVALID_RESPONSE",
      500,
      "The server could not produce a valid response",
    );
  }
  res.json(data);
}
