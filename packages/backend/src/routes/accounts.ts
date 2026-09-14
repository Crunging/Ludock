import {
  PASSWORD_MIN_LENGTH,
  applicationLogsResponseSchema,
  auditResponseSchema,
  auditHistoryQuerySchema,
  authStatusSchema,
  authUserResponseSchema,
  changePasswordRequestSchema,
  createUserRequestSchema,
  credentialsRequestSchema,
  okResponseSchema,
  resetPasswordRequestSchema,
  sessionsResponseSchema,
  setupRequestSchema,
  userAccessRequestSchema,
  userResponseSchema,
  usersResponseSchema,
} from "@ludock/shared";
import { listApplicationLogs } from "../application-logs.js";
import {
  AuthError, assertRequestUser, authenticateUser, clearSessionCookie, createInitialAdmin,
  createSession, deleteRequestSession, getRequestSession, hashPassword, isSetupRequired,
  setSessionCookie,
  verifyPassword,
  type SetupWindow,
} from "../auth.js";
import { assertAdministrator } from "../authorization.js";
import {
  clearLoginThrottle,
  countEnabledAdmins,
  createUser, deleteUser, deleteUserSessionById,
  findUserById, getLoginThrottle, listUserSessions, listUsers, recordLoginFailure,
  updateUserAccess, updateUserPassword, writeAuditLog,
  type UserRecord
} from "../database.js";
import { developmentInstance } from "../development-instance.js";
import { AppError } from "../errors.js";
import { listAuditHistory } from "../history.js";
import {
  PasswordWorkBusyError,
  withPasswordWork,
} from "../password.js";
import { administrator, requestUser, respond, type ApiRoutes } from "./request.js";
const LOGIN_WINDOW_MS = 15 * 60 * 1000;
const LOGIN_ACCOUNT_SOURCE_MAX_FAILURES = 5;
const LOGIN_SOURCE_MAX_FAILURES = 20;
const LOGIN_ACCOUNT_COOLDOWN_THRESHOLD = 20;
const LOGIN_ACCOUNT_COOLDOWN_BASE_MS = 250;
const LOGIN_ACCOUNT_COOLDOWN_MAX_MS = 5_000;
const SETUP_SOURCE_MAX_FAILURES = 20;
const setupThrottleScopes = new WeakMap<SetupWindow, string>();
function publicUser(user: UserRecord | null) {
  if (!user)
    throw new AppError("INVALID_RESPONSE", 500, "The server could not produce a valid response");
  return {
    id: user.id,
    username: user.username,
    role: user.role,
    disabled: user.disabled,
    createdAt: user.createdAt,
  };
}
function loginThrottleKey(scope: string, value: string): string {
  return new Bun.CryptoHasher("sha256").update(`${scope}:${value}`).digest("hex");
}
function passwordWorkSourceKey(ipAddress: string | undefined): string {
  return loginThrottleKey("password-work-source", ipAddress ?? "unknown");
}
function tooManyAttempts(
  keys: Array<{ key: string; windowMs: number }>,
  now: number,
): boolean {
  return keys.some(
    ({ key, windowMs }) =>
      getLoginThrottle(key, now, windowMs).blockedUntil > now,
  );
}
function busyResponse(retryAfterMs?: number): Response {
  return Response.json(
    { error: "Too many attempts. Try again later." },
    {
      status: 429,
      ...(retryAfterMs === undefined ? {} : {
        headers: { "Retry-After": String(Math.max(1, Math.ceil(retryAfterMs / 1_000))) },
      }),
    },
  );
}
function stringProperty(value: unknown, property: string): string | undefined {
  if (typeof value !== "object" || value === null)
    return undefined;
  const propertyValue = (value as Record<string, unknown>)[property];
  return typeof propertyValue === "string" ? propertyValue : undefined;
}
export function accountRoutes(setupWindow: SetupWindow): ApiRoutes {
  // Rebuilding routes for the same window must not reset its source limits;
  // a fresh startup/window must not inherit a previous setup lockout.
  let setupScope = setupThrottleScopes.get(setupWindow);
  if (!setupScope) {
    setupScope = crypto.randomUUID();
    setupThrottleScopes.set(setupWindow, setupScope);
  }
  return {
    "/api/v1/auth/status": {
      GET: (ctx) => {
        const session = getRequestSession(ctx.request);
        const setup = setupWindow.getState();
        const authentication = session
          ? { authenticated: true as const, user: session.user }
          : { authenticated: false as const, user: null };
        return respond(authStatusSchema, {
          setupRequired: setup.required,
          setupLocked: setup.locked,
          setupExpiresAt: setup.expiresAt,
          setupRemainingMs: setup.remainingMs,
          ...authentication,
        });
      }
    },
    "/api/v1/auth/setup": {
      POST: async (ctx) => {
        const sourceKey = loginThrottleKey(
          "setup-source",
          `${setupScope}:${ctx.ipAddress ?? "unknown"}`,
        );
        try {
          // Completed/expired setup stays authoritative even for a blocked
          // source. Invalid code attempts never reach password work.
          setupWindow.assertOpen();
          if (tooManyAttempts([
            { key: sourceKey, windowMs: LOGIN_WINDOW_MS },
          ], Date.now())) return busyResponse();
          // Authorize before schema validation or password hashing. Missing and
          // incorrect codes intentionally receive the same response.
          setupWindow.assertAuthorized(stringProperty(ctx.body, "bootstrapCode"));
        } catch (error) {
          if (error instanceof AuthError) {
            if (error.code === "SETUP_AUTHORIZATION_REQUIRED") {
              recordLoginFailure(
                sourceKey,
                Date.now(),
                LOGIN_WINDOW_MS,
                SETUP_SOURCE_MAX_FAILURES,
              );
            }
            return Response.json({
              error: error.code === "SETUP_LOCKED"
                ? "Initial setup has expired. Restart the panel to reopen setup."
                : error.code === "SETUP_COMPLETE"
                  ? "Initial setup has already been completed"
                  : "Initial setup authorization failed",
            }, { status: error.statusCode });
          }
          throw error;
        }
        clearLoginThrottle(sourceKey);
        const parsed = setupRequestSchema.safeParse(ctx.body);
        if (!parsed.success) {
          return Response.json({
            error: `Username must be 3-32 letters, numbers, dots, underscores, or hyphens; password must be at least ${PASSWORD_MIN_LENGTH} characters.`,
          }, { status: 400 });
        }
        try {
          const user = await createInitialAdmin({
            ...parsed.data,
            ipAddress: ctx.ipAddress,
          }, setupWindow);
          const session = createSession(user, ctx.request, ctx.ipAddress);
          setSessionCookie(ctx.headers, ctx.request, session.token);
          return respond(authUserResponseSchema, { user }, 201);
        }
        catch (error) {
          if (error instanceof PasswordWorkBusyError) return busyResponse();
          if (error instanceof AuthError) {
            return Response.json({
              error: error.code === "SETUP_LOCKED"
                ? "Initial setup has expired. Restart the panel to reopen setup."
                : "Initial setup has already been completed",
            }, { status: error.statusCode });
          }
          throw error;
        }
      }
    },
    "/api/v1/auth/login": {
      POST: async (ctx) => {
        const now = Date.now();
        const source = ctx.ipAddress ?? "unknown";
        const requestedUsername = (stringProperty(ctx.body, "username") || "")
          .trim()
          .toLowerCase()
          .slice(0, 32);
        const sourceKey = loginThrottleKey("login-source", source);
        const accountSourceKey = requestedUsername
          ? loginThrottleKey(
              "login-account-source",
              `${source}\0${requestedUsername}`,
            )
          : null;
        if (tooManyAttempts([
          { key: sourceKey, windowMs: LOGIN_WINDOW_MS },
          ...(accountSourceKey
            ? [{ key: accountSourceKey, windowMs: LOGIN_WINDOW_MS }]
            : []),
        ], now)) return busyResponse();
        const parsed = credentialsRequestSchema.safeParse(ctx.body);
        if (!parsed.success || isSetupRequired()) {
          return Response.json({ error: "Invalid username or password" }, { status: 401 });
        }
        const accountKey = loginThrottleKey("login-account-failures", requestedUsername);
        const accountCooldown = getLoginThrottle(
          accountKey,
          now,
          LOGIN_WINDOW_MS,
        );
        if (accountCooldown.blockedUntil > now)
          return busyResponse(accountCooldown.blockedUntil - now);
        let user;
        try {
          user = await withPasswordWork(
            passwordWorkSourceKey(ctx.ipAddress),
            loginThrottleKey(
              "password-work-login-subject",
              parsed.data.username.toLowerCase(),
            ),
            () => authenticateUser(parsed.data.username, parsed.data.password),
          );
        } catch (error) {
          if (error instanceof PasswordWorkBusyError) return busyResponse();
          throw error;
        }
        if (!user) {
          const failedAt = Date.now();
          recordLoginFailure(
            sourceKey,
            failedAt,
            LOGIN_WINDOW_MS,
            LOGIN_SOURCE_MAX_FAILURES,
          );
          if (accountSourceKey)
            recordLoginFailure(
              accountSourceKey,
              failedAt,
              LOGIN_WINDOW_MS,
              LOGIN_ACCOUNT_SOURCE_MAX_FAILURES,
            );
          // A rotating source cannot avoid this account-wide slowdown. Keep
          // the cooldown short: the old five-guess account lockout let anyone
          // deny the owner access for fifteen minutes. Rejected retries do not
          // extend this cooldown, and successful authentication clears it.
          const accountFailures = getLoginThrottle(accountKey, failedAt, LOGIN_WINDOW_MS).failures + 1;
          const step = Math.max(0, Math.min(5, Math.floor(
            (accountFailures - LOGIN_ACCOUNT_COOLDOWN_THRESHOLD) / 5,
          )));
          const cooldownMs = Math.min(
            LOGIN_ACCOUNT_COOLDOWN_MAX_MS,
            LOGIN_ACCOUNT_COOLDOWN_BASE_MS * 2 ** step,
          );
          recordLoginFailure(
            accountKey,
            failedAt,
            LOGIN_WINDOW_MS,
            LOGIN_ACCOUNT_COOLDOWN_THRESHOLD,
            cooldownMs,
          );
          writeAuditLog({
            action: "auth.login.failed",
            targetType: "user",
            details: {
              username: typeof parsed.data.username === "string"
                ? parsed.data.username.slice(0, 32)
                : null,
            },
            ipAddress: ctx.ipAddress,
          });
          return Response.json({ error: "Invalid username or password" }, { status: 401 });
        }
        if (accountSourceKey) clearLoginThrottle(accountSourceKey);
        clearLoginThrottle(accountKey);
        const session = createSession(user, ctx.request, ctx.ipAddress);
        setSessionCookie(ctx.headers, ctx.request, session.token);
        writeAuditLog({
          userId: user.id,
          action: "auth.login",
          targetType: "session",
          ipAddress: ctx.ipAddress,
        });
        return respond(authUserResponseSchema, { user });
      }
    },
    "/api/v1/auth/logout": {
      POST: (ctx) => {
        const session = getRequestSession(ctx.request);
        deleteRequestSession(ctx.request);
        clearSessionCookie(ctx.headers);
        // Cookie clearing is host-wide, so development checkouts on other ports
        // must retain their sessions. clearSessionCookie removes this one's cookie.
        ctx.headers.set("Clear-Site-Data", developmentInstance
          ? '"cache", "storage"'
          : '"cache", "cookies", "storage"');
        if (session) {
          writeAuditLog({
            userId: session.user.id,
            action: "auth.logout",
            targetType: "session",
            ipAddress: ctx.ipAddress,
          });
        }
        return respond(okResponseSchema, { ok: true });
      }
    },
    "/api/v1/auth/me": {
      GET: (ctx) => {
        return respond(authUserResponseSchema, {
          user: requestUser(ctx),
        });
      }
    },
    "/api/v1/account/change-password": {
      POST: async (ctx) => {
        const parsed = changePasswordRequestSchema.safeParse(ctx.body);
        const actor = requestUser(ctx);
        if (!parsed.success) {
          return Response.json({
            error: `New password must be between ${PASSWORD_MIN_LENGTH} and 128 characters`,
          }, { status: 400 });
        }
        // Throttle current-password guesses so a stolen session cannot be brute
        // forced into a permanent account takeover. The credential-specific
        // key prevents one stolen session from locking every session for a user.
        const now = Date.now();
        const source = ctx.ipAddress ?? "unknown";
        const credential = ctx.sessionTokenHash ?? `principal:${actor.id}`;
        const sourceThrottleKey = loginThrottleKey(
          "password-change-source",
          source,
        );
        const credentialThrottleKey = loginThrottleKey(
          "password-change-credential",
          credential,
        );
        if (tooManyAttempts([
          { key: sourceThrottleKey, windowMs: LOGIN_WINDOW_MS },
          { key: credentialThrottleKey, windowMs: LOGIN_WINDOW_MS },
        ], now)) return busyResponse();

        let work;
        try {
          work = await withPasswordWork(
            passwordWorkSourceKey(ctx.ipAddress),
            loginThrottleKey("password-work-change-subject", credential),
            async () => {
              const record = findUserById(actor.id);
              const verified = record !== null &&
                await verifyPassword(
                  parsed.data.currentPassword,
                  record.passwordHash,
                );
              assertRequestUser(ctx.request, actor);
              return {
                record,
                verified,
                passwordHash: verified
                  ? await hashPassword(parsed.data.newPassword)
                  : null,
              };
            },
          );
        } catch (error) {
          if (error instanceof PasswordWorkBusyError) return busyResponse();
          throw error;
        }
        const { record, verified, passwordHash } = work;
        if (!record || !verified || !passwordHash) {
          const failedAt = Date.now();
          recordLoginFailure(
            sourceThrottleKey,
            failedAt,
            LOGIN_WINDOW_MS,
            LOGIN_SOURCE_MAX_FAILURES,
          );
          recordLoginFailure(
            credentialThrottleKey,
            failedAt,
            LOGIN_WINDOW_MS,
            LOGIN_ACCOUNT_SOURCE_MAX_FAILURES,
          );
          writeAuditLog({
            userId: actor.id === "api-token" ? undefined : actor.id,
            action: "auth.password.change-failed",
            targetType: "user",
            targetId: actor.id,
            ipAddress: ctx.ipAddress,
          });
          return Response.json({ error: "Current password is incorrect" }, { status: 400 });
        }
        const current = assertRequestUser(ctx.request, actor);
        if (findUserById(actor.id)?.passwordHash !== record.passwordHash) {
          throw new AppError("PASSWORD_CHANGED", 409, "Password changed; sign in again");
        }
        clearLoginThrottle(credentialThrottleKey);
        updateUserPassword(actor.id, passwordHash);
        const session = createSession(current, ctx.request, ctx.ipAddress);
        setSessionCookie(ctx.headers, ctx.request, session.token);
        writeAuditLog({
          userId: actor.id === "api-token" ? undefined : actor.id,
          action: "auth.password.changed",
          targetType: "user",
          targetId: actor.id,
          ipAddress: ctx.ipAddress,
        });
        return respond(okResponseSchema, { ok: true });
      }
    },
    "/api/v1/account/sessions": {
      GET: (ctx) => {
        const actor = requestUser(ctx);
        const tokenHash = ctx.sessionTokenHash;
        return respond(sessionsResponseSchema, {
          sessions: tokenHash ? listUserSessions(actor.id, tokenHash) : [],
        });
      }
    },
    "/api/v1/account/sessions/:id": {
      DELETE: (ctx) => {
        const actor = requestUser(ctx);
        const sessionId = ctx.params.id;
        const removed = deleteUserSessionById(actor.id, sessionId);
        if (!removed) {
          return Response.json({ error: "Session not found" }, { status: 404 });
        }
        writeAuditLog({
          userId: actor.id === "api-token" ? undefined : actor.id,
          action: "auth.session.revoked",
          targetType: "session",
          targetId: sessionId,
          ipAddress: ctx.ipAddress,
        });
        return respond(okResponseSchema, { ok: true });
      }
    },
    "/api/v1/users": {
      GET: administrator(() => {
        return respond(usersResponseSchema, { users: listUsers() });
      }),
      POST: administrator(async (ctx) => {
        const parsed = createUserRequestSchema.safeParse(ctx.body);
        if (!parsed.success) {
          return Response.json({ error: "Invalid user details" }, { status: 400 });
        }
        const now = Date.now();
        const id = crypto.randomUUID();
        const actor = requestUser(ctx);
        try {
          const passwordHash = await withPasswordWork(
            passwordWorkSourceKey(ctx.ipAddress),
            loginThrottleKey(
              "password-work-admin-subject",
              ctx.sessionTokenHash ?? actor.id,
            ),
            () => hashPassword(parsed.data.password),
          );
          assertAdministrator(assertRequestUser(ctx.request, actor));
          createUser({
            id,
            username: parsed.data.username,
            passwordHash,
            role: parsed.data.role,
            disabled: false,
            createdAt: now,
          });
        }
        catch (error) {
          if (error instanceof PasswordWorkBusyError) return busyResponse();
          if (String(error).includes("UNIQUE constraint failed")) {
            return Response.json({ error: "Username is already in use" }, { status: 409 });
          }
          throw error;
        }
        writeAuditLog({
          userId: actor.id === "api-token" ? undefined : actor.id,
          action: "user.created",
          targetType: "user",
          targetId: id,
          details: { username: parsed.data.username, role: parsed.data.role },
          ipAddress: ctx.ipAddress,
        });
        return respond(userResponseSchema, {
          user: publicUser(findUserById(id)),
        }, 201);
      })
    },
    "/api/v1/users/:id": {
      PATCH: administrator((ctx) => {
        const parsed = userAccessRequestSchema.safeParse(ctx.body);
        const target = findUserById(ctx.params.id);
        if (!parsed.success || !target) {
          return Response.json({
            error: target ? "Invalid access settings" : "User not found",
          }, { status: target ? 400 : 404 });
        }
        const removesAdmin = target.role === "admin" &&
          !target.disabled &&
          (parsed.data.role !== "admin" || parsed.data.disabled);
        if (removesAdmin && countEnabledAdmins() <= 1) {
          return Response.json({ error: "At least one active admin is required" }, { status: 409 });
        }
        updateUserAccess(target.id, parsed.data.role, parsed.data.disabled);
        const actor = requestUser(ctx);
        writeAuditLog({
          userId: actor.id === "api-token" ? undefined : actor.id,
          action: "user.access.updated",
          targetType: "user",
          targetId: target.id,
          details: parsed.data,
          ipAddress: ctx.ipAddress,
        });
        return respond(userResponseSchema, {
          user: publicUser(findUserById(target.id)),
        });
      }),
      DELETE: administrator((ctx) => {
        const target = findUserById(ctx.params.id);
        const actor = requestUser(ctx);
        if (!target) {
          return Response.json({ error: "User not found" }, { status: 404 });
        }
        if (target.id === actor.id) {
          return Response.json({ error: "You cannot delete your own account" }, { status: 409 });
        }
        if (target.role === "admin" &&
          !target.disabled &&
          countEnabledAdmins() <= 1) {
          return Response.json({ error: "At least one active admin is required" }, { status: 409 });
        }
        deleteUser(target.id);
        writeAuditLog({
          userId: actor.id === "api-token" ? undefined : actor.id,
          action: "user.deleted",
          targetType: "user",
          targetId: target.id,
          details: { username: target.username, role: target.role },
          ipAddress: ctx.ipAddress,
        });
        return respond(okResponseSchema, { ok: true });
      })
    },
    "/api/v1/users/:id/reset-password": {
      POST: administrator(async (ctx) => {
        const parsed = resetPasswordRequestSchema.safeParse(ctx.body);
        const target = findUserById(ctx.params.id);
        if (!parsed.success || !target) {
          return Response.json({
            error: target
              ? `Password must be at least ${PASSWORD_MIN_LENGTH} characters`
              : "User not found",
          }, { status: target ? 400 : 404 });
        }
        const actor = requestUser(ctx);
        let passwordHash;
        try {
          passwordHash = await withPasswordWork(
            passwordWorkSourceKey(ctx.ipAddress),
            loginThrottleKey(
              "password-work-admin-subject",
              ctx.sessionTokenHash ?? actor.id,
            ),
            () => hashPassword(parsed.data.password),
          );
        } catch (error) {
          if (error instanceof PasswordWorkBusyError) return busyResponse();
          throw error;
        }
        assertAdministrator(assertRequestUser(ctx.request, actor));
        if (!findUserById(target.id)) {
          throw new AppError("USER_NOT_FOUND", 404, "User not found");
        }
        updateUserPassword(target.id, passwordHash);
        writeAuditLog({
          userId: actor.id === "api-token" ? undefined : actor.id,
          action: "user.password.reset",
          targetType: "user",
          targetId: target.id,
          ipAddress: ctx.ipAddress,
        });
        return respond(okResponseSchema, { ok: true });
      })
    },
    "/api/v1/audit": {
      GET: administrator((ctx) => {
        return respond(auditResponseSchema, listAuditHistory(
          auditHistoryQuerySchema.parse(Object.fromEntries(ctx.url.searchParams)),
        ));
      })
    },
    "/api/v1/application-logs": {
      GET: administrator((ctx) => {
        const requestedLimit = Number(ctx.url.searchParams.get("limit") || 250);
        const requestedAfter = Number(ctx.url.searchParams.get("after") || 0);
        const limit = Number.isFinite(requestedLimit)
          ? Math.max(1, Math.min(1000, Math.trunc(requestedLimit)))
          : 250;
        const after = Number.isSafeInteger(requestedAfter) && requestedAfter >= 0
          ? requestedAfter
          : 0;
        const generation = ctx.url.searchParams.get("generation")?.slice(0, 64);
        return respond(applicationLogsResponseSchema, listApplicationLogs({ after, limit, generation }));
      })
    }
  };
}
