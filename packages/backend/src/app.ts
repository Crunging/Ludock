import { createHash, randomUUID } from "node:crypto";
import path from "node:path";
import { fileURLToPath } from "node:url";
import express, {
  type Express,
  type NextFunction,
  type Request,
  type Response,
} from "express";
import { z } from "zod";
import {
  PASSWORD_MIN_LENGTH,
  credentialsRequestSchema,
  setupRequestSchema,
  createUserRequestSchema,
  userAccessRequestSchema,
  resetPasswordRequestSchema,
  changePasswordRequestSchema,
  authStatusSchema,
  authUserResponseSchema,
  usersResponseSchema,
  userResponseSchema,
  sessionsResponseSchema,
  auditResponseSchema,
  applicationLogsResponseSchema,
  okResponseSchema,
} from "@ludock/shared";
import { respond } from "./routes/request.js";
import { router } from "./routes.js";
import { advancedRouter } from "./advanced-routes.js";
import { AppError } from "./errors.js";
import { AuthorizationError, assertAdministrator } from "./authorization.js";
import { ServerBindingError } from "./identity.js";
import {
  AuthError,
  assertRequestUser,
  authMiddleware,
  authenticateUser,
  clearSessionCookie,
  createInitialAdmin,
  createSession,
  defaultSetupWindow,
  deleteRequestSession,
  getRequestSession,
  hashPassword,
  isSetupRequired,
  verifyPassword,
  requireRole,
  setSessionCookie,
  type SetupWindow,
} from "./auth.js";
import { checkDockerConnection } from "./docker.js";
import {
  isExternalHttpsRequest,
  isSameOriginRequest,
} from "./request-security.js";
import {
  countEnabledAdmins,
  checkDatabase,
  clearLoginThrottle,
  createUser,
  deleteUser,
  deleteUserSessionById,
  findUserById,
  getLoginThrottle,
  listAuditLog,
  listUserSessions,
  listUsers,
  recordLoginFailure,
  updateUserAccess,
  updateUserPassword,
  writeAuditLog,
  type SessionUser,
  type UserRecord,
} from "./database.js";
import { createLogger, errorMessage } from "./logger.js";
import { listApplicationLogs } from "./application-logs.js";
import {
  developmentInstance,
  matchesDevelopmentInstance,
} from "./development-instance.js";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const LOGIN_WINDOW_MS = 15 * 60 * 1000;
const logger = createLogger("api");

interface CreateAppOptions {
  frontendDist?: string | false;
  setupWindow?: SetupWindow;
}

function publicUser(user: UserRecord | null) {
  if (!user)
    throw new AppError(
      "INVALID_RESPONSE",
      500,
      "The server could not produce a valid response",
    );
  return {
    id: user.id,
    username: user.username,
    role: user.role,
    disabled: user.disabled,
    createdAt: user.createdAt,
  };
}

function loginThrottleKey(
  scope: "account" | "password-change",
  value: string,
): string {
  return createHash("sha256").update(`${scope}:${value}`).digest("hex");
}

function stringProperty(value: unknown, property: string): string | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const propertyValue = (value as Record<string, unknown>)[property];
  return typeof propertyValue === "string" ? propertyValue : undefined;
}

export function createApp(options: CreateAppOptions = {}): Express {
  const app = express();
  const setupWindow = options.setupWindow || defaultSetupWindow;

  app.disable("x-powered-by");
  app.use((req, res, next) => {
    if (developmentInstance)
      res.setHeader("X-Ludock-Dev-Instance", developmentInstance);
    if (!matchesDevelopmentInstance(req.headers["x-ludock-dev-instance"])) {
      res.status(409).json({
        error: "This request belongs to a different development checkout.",
      });
      return;
    }
    next();
  });
  app.use((req, res, next) => {
    const requestId = randomUUID();
    res.locals.requestId = requestId;
    res.setHeader("X-Request-ID", requestId);
    res.setHeader("X-Content-Type-Options", "nosniff");
    res.setHeader("Referrer-Policy", "no-referrer");
    res.setHeader("X-Frame-Options", "DENY");
    res.setHeader("Cross-Origin-Opener-Policy", "same-origin");
    res.setHeader("Cross-Origin-Resource-Policy", "same-origin");
    res.setHeader("Origin-Agent-Cluster", "?1");
    res.setHeader(
      "Permissions-Policy",
      "camera=(), microphone=(), geolocation=(), payment=()",
    );
    res.setHeader(
      "Content-Security-Policy",
      "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'",
    );
    if (req.path.startsWith("/api/v1/")) {
      res.setHeader("Cache-Control", "no-store");
    }
    if (isExternalHttpsRequest(req)) {
      res.setHeader(
        "Strict-Transport-Security",
        "max-age=31536000; includeSubDomains",
      );
    }
    const startedAt = performance.now();
    res.once("finish", () => {
      if (req.path === "/api/v1/application-logs") return;
      logger.debug("HTTP request completed", {
        requestId,
        method: req.method,
        path: req.path,
        status: res.statusCode,
        durationMs: Math.round(performance.now() - startedAt),
        remoteAddress: req.ip,
      });
    });
    next();
  });
  // Enforce origin checks before parsing request bodies.
  app.use((req, res, next) => {
    if (!["GET", "HEAD", "OPTIONS"].includes(req.method)) {
      if (req.get("sec-fetch-site") === "cross-site") {
        res.status(403).json({ error: "Cross-origin request rejected" });
        return;
      }
      const origin = req.get("origin");
      if (origin && !isSameOriginRequest(req)) {
        res.status(403).json({ error: "Cross-origin request rejected" });
        return;
      }
    }
    next();
  });
  app.use(express.json({ limit: "64kb" }));
  app.get("/api/v1/auth/status", (req, res) => {
    const session = getRequestSession(req);
    const setup = setupWindow.getState();
    const authentication = session
      ? { authenticated: true as const, user: session.user }
      : { authenticated: false as const, user: null };
    respond(res, authStatusSchema, {
      setupRequired: setup.required,
      setupLocked: setup.locked,
      setupExpiresAt: setup.expiresAt,
      setupRemainingMs: setup.remainingMs,
      ...authentication,
    });
  });
  app.post("/api/v1/auth/setup", async (req, res) => {
    const parsed = setupRequestSchema.safeParse(req.body);
    if (!parsed.success) {
      res.status(400).json({
        error: `Username must be 3-32 letters, numbers, dots, underscores, or hyphens; password must be at least ${PASSWORD_MIN_LENGTH} characters.`,
      });
      return;
    }

    try {
      const user = await createInitialAdmin(
        {
          ...parsed.data,
          ipAddress: req.ip,
        },
        setupWindow,
      );
      const session = createSession(user, req);
      setSessionCookie(res, req, session.token);
      respond(res.status(201), authUserResponseSchema, { user });
    } catch (error) {
      if (error instanceof AuthError) {
        res.status(error.statusCode).json({
          error:
            error.code === "SETUP_LOCKED"
              ? "Initial setup has expired. Restart the panel to reopen setup."
              : "Initial setup has already been completed",
        });
        return;
      }
      throw error;
    }
  });
  app.post("/api/v1/auth/login", async (req, res) => {
    const now = Date.now();
    const requestedUsername = (
      stringProperty(req.body as unknown, "username") || ""
    )
      .trim()
      .toLowerCase()
      .slice(0, 32);
    const accountKey = requestedUsername
      ? loginThrottleKey("account", requestedUsername)
      : null;
    const blocked =
      accountKey !== null &&
      getLoginThrottle(accountKey, now, LOGIN_WINDOW_MS).blockedUntil > now;
    if (blocked) {
      res.status(429).json({ error: "Too many attempts. Try again later." });
      return;
    }

    const parsed = credentialsRequestSchema.safeParse(req.body);
    if (!parsed.success || isSetupRequired()) {
      res.status(401).json({ error: "Invalid username or password" });
      return;
    }

    const user = await authenticateUser(
      parsed.data.username,
      parsed.data.password,
    );
    if (!user) {
      if (accountKey) {
        recordLoginFailure(accountKey, now, LOGIN_WINDOW_MS, 5);
      }
      writeAuditLog({
        action: "auth.login.failed",
        targetType: "user",
        details: {
          username:
            typeof parsed.data.username === "string"
              ? parsed.data.username.slice(0, 32)
              : null,
        },
        ipAddress: req.ip,
      });
      res.status(401).json({ error: "Invalid username or password" });
      return;
    }

    if (accountKey) clearLoginThrottle(accountKey);
    const session = createSession(user, req);
    setSessionCookie(res, req, session.token);
    writeAuditLog({
      userId: user.id,
      action: "auth.login",
      targetType: "session",
      ipAddress: req.ip,
    });
    respond(res, authUserResponseSchema, { user });
  });
  app.post("/api/v1/auth/logout", (req, res) => {
    const session = getRequestSession(req);
    deleteRequestSession(req);
    clearSessionCookie(res);
    // Cookie clearing is host-wide, so development checkouts on other ports
    // must retain their sessions. clearSessionCookie removes this one's cookie.
    res.setHeader(
      "Clear-Site-Data",
      developmentInstance
        ? '"cache", "storage"'
        : '"cache", "cookies", "storage"',
    );
    if (session) {
      writeAuditLog({
        userId: session.user.id,
        action: "auth.logout",
        targetType: "session",
        ipAddress: req.ip,
      });
    }
    respond(res, okResponseSchema, { ok: true });
  });
  // Public for container health checks.
  const HEALTH_CACHE_MS = 5_000;
  let healthCache: { checkedAt: number; healthy: boolean } | null = null;
  let healthProbe: Promise<boolean> | null = null;

  const probeHealth = async (): Promise<boolean> => {
    const now = Date.now();
    if (healthCache && now - healthCache.checkedAt < HEALTH_CACHE_MS) {
      return healthCache.healthy;
    }
    healthProbe ||= (async () => {
      try {
        checkDatabase();
        await checkDockerConnection();
        return true;
      } catch {
        return false;
      }
    })().then((healthy) => {
      healthCache = { checkedAt: Date.now(), healthy };
      healthProbe = null;
      return healthy;
    });
    return healthProbe;
  };

  app.get("/api/v1/health", async (_req, res) => {
    if (await probeHealth()) {
      res.json({ status: "ok", docker: "connected", database: "connected" });
      return;
    }
    res.status(503).json({ status: "degraded" });
  });
  app.use("/api/v1", authMiddleware);
  app.get("/api/v1/auth/me", (_req, res) => {
    respond(res, authUserResponseSchema, {
      user: res.locals.user as SessionUser,
    });
  });
  app.post("/api/v1/account/change-password", async (req, res) => {
    const parsed = changePasswordRequestSchema.safeParse(req.body);
    const actor = res.locals.user as SessionUser;
    if (!parsed.success) {
      res.status(400).json({
        error: `New password must be between ${PASSWORD_MIN_LENGTH} and 128 characters`,
      });
      return;
    }

    // Throttle current-password guesses so a stolen session cannot be brute
    // forced into a permanent account takeover.
    const now = Date.now();
    const throttleKey = loginThrottleKey("password-change", actor.id);
    if (
      getLoginThrottle(throttleKey, now, LOGIN_WINDOW_MS).blockedUntil > now
    ) {
      res.status(429).json({ error: "Too many attempts. Try again later." });
      return;
    }

    const record = findUserById(actor.id);
    const verified = record !== null &&
      await verifyPassword(parsed.data.currentPassword, record.passwordHash);
    assertRequestUser(req, actor);
    if (!record || !verified) {
      recordLoginFailure(throttleKey, now, LOGIN_WINDOW_MS, 5);
      writeAuditLog({
        userId: actor.id === "api-token" ? undefined : actor.id,
        action: "auth.password.change-failed",
        targetType: "user",
        targetId: actor.id,
        ipAddress: req.ip,
      });
      res.status(400).json({ error: "Current password is incorrect" });
      return;
    }

    const passwordHash = await hashPassword(parsed.data.newPassword);
    const current = assertRequestUser(req, actor);
    if (findUserById(actor.id)?.passwordHash !== record.passwordHash) {
      throw new AppError(
        "PASSWORD_CHANGED", 409, "Password changed; sign in again",
      );
    }
    clearLoginThrottle(throttleKey);
    updateUserPassword(actor.id, passwordHash);
    const session = createSession(current, req);
    setSessionCookie(res, req, session.token);
    writeAuditLog({
      userId: actor.id === "api-token" ? undefined : actor.id,
      action: "auth.password.changed",
      targetType: "user",
      targetId: actor.id,
      ipAddress: req.ip,
    });
    respond(res, okResponseSchema, { ok: true });
  });
  app.get("/api/v1/account/sessions", (_req, res) => {
    const actor = res.locals.user as SessionUser;
    const tokenHash = res.locals.sessionTokenHash as string | undefined;
    respond(res, sessionsResponseSchema, {
      sessions: tokenHash ? listUserSessions(actor.id, tokenHash) : [],
    });
  });
  app.delete("/api/v1/account/sessions/:id", (req, res) => {
    const actor = res.locals.user as SessionUser;
    const sessionId = req.params.id;
    const removed = deleteUserSessionById(actor.id, sessionId);
    if (!removed) {
      res.status(404).json({ error: "Session not found" });
      return;
    }
    writeAuditLog({
      userId: actor.id === "api-token" ? undefined : actor.id,
      action: "auth.session.revoked",
      targetType: "session",
      targetId: sessionId,
      ipAddress: req.ip,
    });
    respond(res, okResponseSchema, { ok: true });
  });
  app.get("/api/v1/users", requireRole("admin"), (_req, res) => {
    respond(res, usersResponseSchema, { users: listUsers() });
  });
  app.post("/api/v1/users", requireRole("admin"), async (req, res) => {
    const parsed = createUserRequestSchema.safeParse(req.body);
    if (!parsed.success) {
      res.status(400).json({ error: "Invalid user details" });
      return;
    }

    const now = Date.now();
    const id = randomUUID();
    const actor = res.locals.user as SessionUser;
    try {
      const passwordHash = await hashPassword(parsed.data.password);
      assertAdministrator(assertRequestUser(req, actor));
      createUser({
        id,
        username: parsed.data.username,
        passwordHash,
        role: parsed.data.role,
        disabled: false,
        createdAt: now,
      });
    } catch (error) {
      if (String(error).includes("UNIQUE constraint failed")) {
        res.status(409).json({ error: "Username is already in use" });
        return;
      }
      throw error;
    }

    writeAuditLog({
      userId: actor.id === "api-token" ? undefined : actor.id,
      action: "user.created",
      targetType: "user",
      targetId: id,
      details: { username: parsed.data.username, role: parsed.data.role },
      ipAddress: req.ip,
    });
    respond(res.status(201), userResponseSchema, {
      user: publicUser(findUserById(id)),
    });
  });
  app.patch("/api/v1/users/:id", requireRole("admin"), (req, res) => {
    const parsed = userAccessRequestSchema.safeParse(req.body);
    const target = findUserById(req.params.id as string);
    if (!parsed.success || !target) {
      res.status(target ? 400 : 404).json({
        error: target ? "Invalid access settings" : "User not found",
      });
      return;
    }

    const removesAdmin =
      target.role === "admin" &&
      !target.disabled &&
      (parsed.data.role !== "admin" || parsed.data.disabled);
    if (removesAdmin && countEnabledAdmins() <= 1) {
      res.status(409).json({ error: "At least one active admin is required" });
      return;
    }

    updateUserAccess(target.id, parsed.data.role, parsed.data.disabled);
    const actor = res.locals.user as SessionUser;
    writeAuditLog({
      userId: actor.id === "api-token" ? undefined : actor.id,
      action: "user.access.updated",
      targetType: "user",
      targetId: target.id,
      details: parsed.data,
      ipAddress: req.ip,
    });
    respond(res, userResponseSchema, {
      user: publicUser(findUserById(target.id)),
    });
  });
  app.post(
    "/api/v1/users/:id/reset-password",
    requireRole("admin"),
    async (req, res) => {
      const parsed = resetPasswordRequestSchema.safeParse(req.body);
      const target = findUserById(req.params.id as string);
      if (!parsed.success || !target) {
        res.status(target ? 400 : 404).json({
          error: target
            ? `Password must be at least ${PASSWORD_MIN_LENGTH} characters`
            : "User not found",
        });
        return;
      }

      const actor = res.locals.user as SessionUser;
      const passwordHash = await hashPassword(parsed.data.password);
      assertAdministrator(assertRequestUser(req, actor));
      if (!findUserById(target.id)) {
        throw new AppError("USER_NOT_FOUND", 404, "User not found");
      }
      updateUserPassword(target.id, passwordHash);
      writeAuditLog({
        userId: actor.id === "api-token" ? undefined : actor.id,
        action: "user.password.reset",
        targetType: "user",
        targetId: target.id,
        ipAddress: req.ip,
      });
      respond(res, okResponseSchema, { ok: true });
    },
  );
  app.delete("/api/v1/users/:id", requireRole("admin"), (req, res) => {
    const target = findUserById(req.params.id as string);
    const actor = res.locals.user as SessionUser;
    if (!target) {
      res.status(404).json({ error: "User not found" });
      return;
    }
    if (target.id === actor.id) {
      res.status(409).json({ error: "You cannot delete your own account" });
      return;
    }
    if (
      target.role === "admin" &&
      !target.disabled &&
      countEnabledAdmins() <= 1
    ) {
      res.status(409).json({ error: "At least one active admin is required" });
      return;
    }

    deleteUser(target.id);
    writeAuditLog({
      userId: actor.id === "api-token" ? undefined : actor.id,
      action: "user.deleted",
      targetType: "user",
      targetId: target.id,
      details: { username: target.username, role: target.role },
      ipAddress: req.ip,
    });
    respond(res, okResponseSchema, { ok: true });
  });
  app.get("/api/v1/audit", requireRole("admin"), (req, res) => {
    const requested = Number(req.query.limit || 100);
    const limit = Number.isFinite(requested)
      ? Math.max(1, Math.min(250, Math.trunc(requested)))
      : 100;
    respond(res, auditResponseSchema, { entries: listAuditLog(limit) });
  });
  app.get("/api/v1/application-logs", requireRole("admin"), (req, res) => {
    const requestedLimit = Number(req.query.limit || 250);
    const requestedAfter = Number(req.query.after || 0);
    const limit = Number.isFinite(requestedLimit)
      ? Math.max(1, Math.min(1_000, Math.trunc(requestedLimit)))
      : 250;
    const after =
      Number.isSafeInteger(requestedAfter) && requestedAfter >= 0
        ? requestedAfter
        : 0;
    const generation =
      typeof req.query.generation === "string"
        ? req.query.generation.slice(0, 64)
        : undefined;
    respond(
      res,
      applicationLogsResponseSchema,
      listApplicationLogs({ after, limit, generation }),
    );
  });
  app.use(router);
  app.use(advancedRouter);
  app.use("/api/{*splat}", (_req, res) => {
    res.status(404).json({ error: "API endpoint not found" });
  });

  const frontendDist =
    options.frontendDist === undefined
      ? path.resolve(__dirname, "../../frontend/dist")
      : options.frontendDist;

  if (frontendDist !== false) {
    app.use(express.static(frontendDist));
    app.get("/{*splat}", (_req, res) => {
      res.sendFile(path.join(frontendDist, "index.html"));
    });
  }

  // Registered last so it also catches failures from the static and SPA
  // handlers, which would otherwise fall through to Express' default handler
  // and leak a stack trace.
  app.use((error: unknown, req: Request, res: Response, next: NextFunction) => {
    if (error instanceof z.ZodError) {
      res
        .status(400)
        .json({ error: "Invalid request", code: "INVALID_REQUEST" });
      return;
    }
    if (
      error instanceof AppError ||
      error instanceof AuthError ||
      error instanceof AuthorizationError ||
      error instanceof ServerBindingError
    ) {
      res
        .status(error.statusCode)
        .json({ error: error.message, code: error.code });
      return;
    }
    const requestId =
      typeof res.locals.requestId === "string"
        ? res.locals.requestId
        : "unknown";
    logger.error("Unhandled request error", {
      requestId,
      method: req.method,
      path: req.path,
      error: errorMessage(error),
    });
    if (res.headersSent) {
      next(error);
      return;
    }
    if (req.path.startsWith("/api/v1/")) {
      res.status(500).json({
        error: "Internal server error",
        requestId,
      });
      return;
    }
    res.status(500).type("text/plain").send("Internal server error");
  });

  return app;
}
