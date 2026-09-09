import { z } from "zod";
import { roleSchema } from "./access.js";

export const authUserSchema = z.object({
  id: z.union([z.string().uuid(), z.literal("api-token")]),
  username: z.string(),
  role: roleSchema,
});
export type AuthUser = z.infer<typeof authUserSchema>;
export const authStatusSchema = z
  .object({
    setupRequired: z.boolean(),
    setupLocked: z.boolean(),
    setupExpiresAt: z.number().int().nonnegative().nullable(),
    setupRemainingMs: z.number().nonnegative().nullable(),
  })
  .and(
    z.discriminatedUnion("authenticated", [
      z.object({ authenticated: z.literal(true), user: authUserSchema }),
      z.object({ authenticated: z.literal(false), user: z.null() }),
    ]),
  );
export type AuthStatus = z.infer<typeof authStatusSchema>;
export const authUserResponseSchema = z.object({ user: authUserSchema });
export const userSummarySchema = authUserSchema.extend({
  id: z.string().uuid(),
  disabled: z.boolean(),
  createdAt: z.number().int().nonnegative(),
});
export type UserSummary = z.infer<typeof userSummarySchema>;
export const usersResponseSchema = z.object({
  users: z.array(userSummarySchema),
});
export const userResponseSchema = z.object({ user: userSummarySchema });
export const sessionSummarySchema = z.object({
  id: z.string().uuid(),
  createdAt: z.number().int().nonnegative(),
  expiresAt: z.number().int().nonnegative(),
  lastSeenAt: z.number().int().nonnegative(),
  ipAddress: z.string().nullable(),
  userAgent: z.string().nullable(),
  current: z.boolean(),
});
export type SessionSummary = z.infer<typeof sessionSummarySchema>;
export const sessionsResponseSchema = z.object({
  sessions: z.array(sessionSummarySchema),
});

export const PASSWORD_MIN_LENGTH = 15;

export const credentialsRequestSchema = z.object({
  username: z
    .string()
    .trim()
    .min(3)
    .max(32)
    .regex(/^[a-zA-Z0-9._-]+$/),
  password: z.string().min(PASSWORD_MIN_LENGTH).max(128),
});
export const setupRequestSchema = credentialsRequestSchema;
export const createUserRequestSchema = credentialsRequestSchema.extend({
  role: roleSchema,
});
export const userAccessRequestSchema = z.object({
  role: roleSchema,
  disabled: z.boolean(),
});
export const resetPasswordRequestSchema = z.object({
  password: z.string().min(PASSWORD_MIN_LENGTH).max(128),
});
export const changePasswordRequestSchema = z.object({
  currentPassword: z.string().min(1).max(128),
  newPassword: z.string().min(PASSWORD_MIN_LENGTH).max(128),
});

export type CredentialsRequest = z.input<typeof credentialsRequestSchema>;
export type SetupRequest = z.input<typeof setupRequestSchema>;
export type CreateUserRequest = z.input<typeof createUserRequestSchema>;
export type UserAccessRequest = z.input<typeof userAccessRequestSchema>;
export type ResetPasswordRequest = z.input<typeof resetPasswordRequestSchema>;
export type ChangePasswordRequest = z.input<typeof changePasswordRequestSchema>;
