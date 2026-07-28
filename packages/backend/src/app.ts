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
import { router } from "./routes.js";
import {
  AuthError,
  authMiddleware,
  authenticateUser,
  clearSessionCookie,
  createInitialAdmin,
  createSession,
  deleteRequestSession,
  getRequestSession,
  hashPassword,
  isSetupRequired,
  isSetupTokenRequired,
  verifyPassword,
  requireRole,
  setSessionCookie,
} from "./auth.js";
import { checkDockerConnection } from "./docker.js";
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

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const LOGIN_WINDOW_MS = 15 * 60 * 1000;
const credentialsSchema = z.object({
  username: z
    .string()
    .trim()
    .min(3)
    .max(32)
    .regex(/^[a-zA-Z0-9._-]+$/),
  password: z.string().min(12).max(128),
});
const setupSchema = credentialsSchema.extend({
  setupToken: z.string().max(512).optional(),
});
const newUserSchema = credentialsSchema.extend({
  role: z.enum(["admin", "operator", "viewer"]),
});
const accessSchema = z.object({
  role: z.enum(["admin", "operator", "viewer"]),
  disabled: z.boolean(),
});
const passwordSchema = z.object({
  password: z.string().min(12).max(128),
});
const changePasswordSchema = z.object({
  currentPassword: z.string().min(1).max(128),
  newPassword: z.string().min(12).max(128),
});

interface CreateAppOptions {
  frontendDist?: string | false;
}

function publicUser(user: UserRecord | null) {
  if (!user) return null;
  return {
    id: user.id,
    username: user.username,
    role: user.role,
    disabled: user.disabled,
    createdAt: user.createdAt,
  };
}

export function createApp(options: CreateAppOptions = {}): Express {
  const app = express();

  app.disable("x-powered-by");
  if (process.env.TRUST_PROXY === "true") app.set("trust proxy", 1);
  app.use((req, res, next) => {
    const requestId = randomUUID();
    res.locals.requestId = requestId;
    res.setHeader("X-Request-ID", requestId);
    res.setHeader("X-Content-Type-Options", "nosniff");
    res.setHeader("Referrer-Policy", "no-referrer");
    res.setHeader("X-Frame-Options", "DENY");
    res.setHeader(
      "Permissions-Policy",
      "camera=(), microphone=(), geolocation=(), payment=()"
    );
    res.setHeader(
      "Content-Security-Policy",
      "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self' ws: wss:; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
    );
    if (req.secure) {
      res.setHeader(
        "Strict-Transport-Security",
        "max-age=31536000; includeSubDomains"
      );
    }
    next();
  });
  app.use(express.json({ limit: "64kb" }));
  app.use((req, res, next) => {
    if (!["GET", "HEAD", "OPTIONS"].includes(req.method)) {
      const origin = req.get("origin");
      if (origin) {
        try {
          if (new URL(origin).host !== req.get("host")) {
            res.status(403).json({ error: "Cross-origin request rejected" });
            return;
          }
        } catch {
          res.status(403).json({ error: "Invalid request origin" });
          return;
        }
      }
    }
    next();
  });
  app.get("/api/auth/status", (req, res) => {
    const session = getRequestSession(req);
    res.json({
      setupRequired: isSetupRequired(),
      setupTokenRequired: isSetupRequired() && isSetupTokenRequired(),
      authenticated: session !== null,
      user: session?.user || null,
    });
  });
  app.post("/api/auth/setup", async (req, res) => {
    const parsed = setupSchema.safeParse(req.body);
    if (!parsed.success) {
      res.status(400).json({
        error:
          "Username must be 3-32 letters, numbers, dots, underscores, or hyphens; password must be at least 12 characters.",
      });
      return;
    }

    try {
      const user = await createInitialAdmin({
        ...parsed.data,
        ipAddress: req.ip,
      });
      const session = createSession(user, req);
      setSessionCookie(res, req, session.token);
      res.status(201).json({ user });
    } catch (error) {
      if (error instanceof AuthError) {
        res.status(error.statusCode).json({
          error:
            error.code === "INVALID_SETUP_TOKEN"
              ? "Invalid setup token"
              : "Initial setup has already been completed",
        });
        return;
      }
      throw error;
    }
  });
  app.post("/api/auth/login", async (req, res) => {
    const key = createHash("sha256")
      .update(req.ip || "unknown")
      .digest("hex");
    const now = Date.now();
    const attempt = getLoginThrottle(key, now, LOGIN_WINDOW_MS);
    if (attempt.blockedUntil > now) {
      res.status(429).json({ error: "Too many attempts. Try again later." });
      return;
    }

    const parsed = credentialsSchema.safeParse(req.body);
    if (!parsed.success || isSetupRequired()) {
      res.status(401).json({ error: "Invalid username or password" });
      return;
    }

    const user = await authenticateUser(
      parsed.data.username,
      parsed.data.password
    );
    if (!user) {
      recordLoginFailure(key, now, LOGIN_WINDOW_MS, 5);
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

    clearLoginThrottle(key);
    const session = createSession(user, req);
    setSessionCookie(res, req, session.token);
    writeAuditLog({
      userId: user.id,
      action: "auth.login",
      targetType: "session",
      ipAddress: req.ip,
    });
    res.json({ user });
  });
  app.post("/api/auth/logout", (req, res) => {
    const session = getRequestSession(req);
    deleteRequestSession(req);
    clearSessionCookie(res);
    if (session) {
      writeAuditLog({
        userId: session.user.id,
        action: "auth.logout",
        targetType: "session",
        ipAddress: req.ip,
      });
    }
    res.json({ ok: true });
  });
  app.get("/api/health", async (_req, res) => {
    try {
      checkDatabase();
      await checkDockerConnection();
      res.json({ status: "ok", docker: "connected", database: "connected" });
    } catch {
      res.status(503).json({ status: "degraded" });
    }
  });
  app.use("/api", authMiddleware);
  app.get("/api/auth/me", (_req, res) => {
    res.json({ user: res.locals.user });
  });
  app.post("/api/account/change-password", async (req, res) => {
    const parsed = changePasswordSchema.safeParse(req.body);
    const actor = res.locals.user as SessionUser;
    const record = findUserById(actor.id);
    if (!parsed.success) {
      res.status(400).json({
        error: "New password must be between 12 and 128 characters",
      });
      return;
    }
    if (
      !record ||
      !(await verifyPassword(parsed.data.currentPassword, record.passwordHash))
    ) {
      res.status(400).json({ error: "Current password is incorrect" });
      return;
    }

    await updateUserPassword(
      actor.id,
      await hashPassword(parsed.data.newPassword)
    );
    const session = createSession(actor, req);
    setSessionCookie(res, req, session.token);
    writeAuditLog({
      userId: actor.id,
      action: "auth.password.changed",
      targetType: "user",
      targetId: actor.id,
      ipAddress: req.ip,
    });
    res.json({ ok: true });
  });
  app.get("/api/account/sessions", (_req, res) => {
    const actor = res.locals.user as SessionUser;
    const tokenHash = res.locals.sessionTokenHash as string | undefined;
    res.json({
      sessions: tokenHash ? listUserSessions(actor.id, tokenHash) : [],
    });
  });
  app.delete("/api/account/sessions/:id", (req, res) => {
    const actor = res.locals.user as SessionUser;
    const sessionId = req.params.id as string;
    const removed = deleteUserSessionById(actor.id, sessionId);
    if (!removed) {
      res.status(404).json({ error: "Session not found" });
      return;
    }
    writeAuditLog({
      userId: actor.id,
      action: "auth.session.revoked",
      targetType: "session",
      targetId: sessionId,
      ipAddress: req.ip,
    });
    res.json({ ok: true });
  });
  app.get("/api/users", requireRole("admin"), (_req, res) => {
    res.json({ users: listUsers() });
  });
  app.post("/api/users", requireRole("admin"), async (req, res) => {
    const parsed = newUserSchema.safeParse(req.body);
    if (!parsed.success) {
      res.status(400).json({ error: "Invalid user details" });
      return;
    }

    const now = Date.now();
    const id = randomUUID();
    try {
      createUser({
        id,
        username: parsed.data.username,
        passwordHash: await hashPassword(parsed.data.password),
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

    const actor = res.locals.user as SessionUser;
    writeAuditLog({
      userId: actor.id,
      action: "user.created",
      targetType: "user",
      targetId: id,
      details: { username: parsed.data.username, role: parsed.data.role },
      ipAddress: req.ip,
    });
    res.status(201).json({ user: publicUser(findUserById(id)) });
  });
  app.patch("/api/users/:id", requireRole("admin"), (req, res) => {
    const parsed = accessSchema.safeParse(req.body);
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
      userId: actor.id,
      action: "user.access.updated",
      targetType: "user",
      targetId: target.id,
      details: parsed.data,
      ipAddress: req.ip,
    });
    res.json({ user: publicUser(findUserById(target.id)) });
  });
  app.post(
    "/api/users/:id/reset-password",
    requireRole("admin"),
    async (req, res) => {
      const parsed = passwordSchema.safeParse(req.body);
      const target = findUserById(req.params.id as string);
      if (!parsed.success || !target) {
        res.status(target ? 400 : 404).json({
          error: target
            ? "Password must be at least 12 characters"
            : "User not found",
        });
        return;
      }

      updateUserPassword(target.id, await hashPassword(parsed.data.password));
      const actor = res.locals.user as SessionUser;
      writeAuditLog({
        userId: actor.id,
        action: "user.password.reset",
        targetType: "user",
        targetId: target.id,
        ipAddress: req.ip,
      });
      res.json({ ok: true });
    }
  );
  app.delete("/api/users/:id", requireRole("admin"), (req, res) => {
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
      userId: actor.id,
      action: "user.deleted",
      targetType: "user",
      targetId: target.id,
      details: { username: target.username, role: target.role },
      ipAddress: req.ip,
    });
    res.json({ ok: true });
  });
  app.get("/api/audit", requireRole("admin"), (req, res) => {
    const requested = Number(req.query.limit || 100);
    const limit = Number.isFinite(requested)
      ? Math.max(1, Math.min(250, Math.trunc(requested)))
      : 100;
    res.json({ entries: listAuditLog(limit) });
  });
  app.use(router);
  app.use("/api/{*splat}", (_req, res) => {
    res.status(404).json({ error: "API endpoint not found" });
  });
  app.use(
    (
      error: unknown,
      req: Request,
      res: Response,
      next: NextFunction
    ) => {
      console.error(
        `[API] Unhandled error ${res.locals.requestId || "unknown"}:`,
        error
      );
      if (res.headersSent) {
        next(error);
        return;
      }
      if (req.path.startsWith("/api/")) {
        res.status(500).json({
          error: "Internal server error",
          requestId: res.locals.requestId,
        });
        return;
      }
      next(error);
    }
  );

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

  return app;
}
