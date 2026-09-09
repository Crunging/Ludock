import {
  createHash,
  randomBytes,
  randomUUID,
  scrypt,
  timingSafeEqual,
} from "node:crypto";
import type { IncomingMessage } from "node:http";
import type { Request, Response, NextFunction } from "express";
import {
  countUsers,
  createSessionRecord,
  createUser,
  deleteSessionRecord,
  findSessionUser,
  findUserById,
  findUserByUsername,
  upgradeUserPasswordHash,
  writeAuditLog,
  type SessionUser,
} from "./database.js";
import {
  isExternalHttpsRequest,
  isSameOriginRequest,
  requestOriginDiagnostic,
} from "./request-security.js";
import { createLogger } from "./logger.js";

import { developmentInstance } from "./development-instance.js";
const SESSION_COOKIE = developmentInstance
  ? `ludock_session_${developmentInstance}`
  : "ludock_session";
const SESSION_TTL_MS = 7 * 24 * 60 * 60 * 1000;
export const MIN_API_TOKEN_LENGTH = 32;
const SCRYPT_N = 32768;
const SCRYPT_R = 8;
const SCRYPT_P = 3;
const SCRYPT_MAX_MEMORY = 64 * 1024 * 1024;
export const SETUP_WINDOW_MS = 5 * 60 * 1000;
const logger = createLogger("auth");

export interface SetupState {
  required: boolean;
  locked: boolean;
  expiresAt: number | null;
  remainingMs: number | null;
}

export interface WebSocketAuth {
  user: SessionUser;
  sessionTokenHash?: string;
  validate: () => SessionUser | null;
}

let warnedAboutWeakApiToken = "";

/**
 * The API token grants unconditional administrator access and is not subject to
 * the login throttle, so a short token could be brute forced at request rate.
 * Reject anything below the strength floor instead of honouring it.
 */
export function ludockApiToken(): string {
  const configured = process.env.LUDOCK_API_TOKEN?.trim() || "";
  if (!configured) return "";
  if (configured.length < MIN_API_TOKEN_LENGTH) {
    if (warnedAboutWeakApiToken !== configured) {
      warnedAboutWeakApiToken = configured;
      logger.error("LUDOCK_API_TOKEN is too short and has been ignored", {
        minimumLength: MIN_API_TOKEN_LENGTH,
      });
    }
    return "";
  }
  return configured;
}

export class SetupWindow {
  readonly expiresAt: number;

  constructor(
    private readonly now: () => number = Date.now,
    durationMs = SETUP_WINDOW_MS,
  ) {
    this.expiresAt = now() + durationMs;
  }

  getState(): SetupState {
    const required = isSetupRequired();
    const now = this.now();
    return {
      required,
      locked: required && now >= this.expiresAt,
      expiresAt: required ? this.expiresAt : null,
      remainingMs: required ? Math.max(0, this.expiresAt - now) : null,
    };
  }

  assertOpen(): void {
    const state = this.getState();
    if (!state.required) throw new AuthError("SETUP_COMPLETE", 409);
    if (state.locked) throw new AuthError("SETUP_LOCKED", 403);
  }
}

export const defaultSetupWindow = new SetupWindow();

export function isSetupRequired(): boolean {
  return countUsers() === 0;
}

export function logSetupInstructions(
  setupWindow: SetupWindow = defaultSetupWindow,
): void {
  if (!isSetupRequired()) return;

  const minutes = Math.round(SETUP_WINDOW_MS / 60_000);
  logger.info("Initial administrator setup is available", {
    durationMinutes: minutes,
    closesAt: new Date(setupWindow.expiresAt).toISOString(),
  });
}

export async function hashPassword(password: string): Promise<string> {
  const salt = randomBytes(16);
  const key = await derivePassword(password, salt, 64, {
    N: SCRYPT_N,
    r: SCRYPT_R,
    p: SCRYPT_P,
    maxmem: SCRYPT_MAX_MEMORY,
  });

  return `scrypt$${SCRYPT_N}$${SCRYPT_R}$${SCRYPT_P}$${salt.toString("base64url")}$${key.toString("base64url")}`;
}

export async function verifyPassword(
  password: string,
  encoded: string,
): Promise<boolean> {
  const [algorithm, n, r, p, saltValue, keyValue, extra] = encoded.split("$");
  if (
    algorithm !== "scrypt" || !n || !r || !p || !saltValue || !keyValue ||
    extra !== undefined
  ) {
    return false;
  }

  const expected = Buffer.from(keyValue, "base64url");
  const salt = Buffer.from(saltValue, "base64url");
  // Buffer's base64 decoder silently accepts malformed input, including values
  // that decode to an empty key. A zero-length derived key would match every
  // password, so validate the stored encoding before performing verification.
  if (
    expected.length !== 64 || salt.length !== 16 ||
    expected.toString("base64url") !== keyValue ||
    salt.toString("base64url") !== saltValue
  )
    return false;
  const options = {
    N: Number(n),
    r: Number(r),
    p: Number(p),
  };
  if (
    !Number.isInteger(options.N) ||
    !Number.isInteger(options.r) ||
    !Number.isInteger(options.p) ||
    options.N < 2 ||
    options.N > 131072 ||
    (options.N & (options.N - 1)) !== 0 ||
    options.r < 1 ||
    options.r > 16 ||
    options.p < 1 ||
    options.p > 10
  ) {
    return false;
  }
  const actual = await derivePassword(
    password,
    salt,
    expected.length,
    {
      ...options,
      maxmem: Math.max(
        SCRYPT_MAX_MEMORY,
        128 * options.N * options.r + 16 * 1024 * 1024,
      ),
    },
  );

  return actual.length === expected.length && timingSafeEqual(actual, expected);
}

export async function createInitialAdmin(
  input: {
    username: string;
    password: string;
    ipAddress?: string;
  },
  setupWindow: SetupWindow = defaultSetupWindow,
): Promise<SessionUser> {
  setupWindow.assertOpen();

  const now = Date.now();
  const user: SessionUser = {
    id: randomUUID(),
    username: input.username,
    role: "admin",
  };
  const passwordHash = await hashPassword(input.password);
  setupWindow.assertOpen();
  createUser({
    ...user,
    passwordHash,
    disabled: false,
    createdAt: now,
  });
  writeAuditLog({
    userId: user.id,
    action: "auth.setup.completed",
    targetType: "user",
    targetId: user.id,
    ipAddress: input.ipAddress,
  });
  return user;
}

export async function authenticateUser(
  username: string,
  password: string,
): Promise<SessionUser | null> {
  const record = findUserByUsername(username);
  if (!record || record.disabled) {
    await hashPassword(password);
    return null;
  }
  if (!(await verifyPassword(password, record.passwordHash))) return null;
  const upgradedHash = passwordHashNeedsUpgrade(record.passwordHash)
    ? await hashPassword(password)
    : undefined;
  // Password verification and upgrades yield to other requests. A reset or
  // disabled account must invalidate the credentials that were just checked.
  const current = findUserById(record.id);
  if (
    !current || current.disabled || current.passwordHash !== record.passwordHash
  )
    return null;
  if (upgradedHash) upgradeUserPasswordHash(current.id, upgradedHash);

  return { id: current.id, username: current.username, role: current.role };
}

export function createSession(
  user: SessionUser,
  request: Request,
): { token: string; expiresAt: number } {
  const token = randomBytes(32).toString("base64url");
  const now = Date.now();
  const expiresAt = now + SESSION_TTL_MS;
  createSessionRecord({
    sessionId: randomUUID(),
    tokenHash: hashToken(token),
    userId: user.id,
    createdAt: now,
    expiresAt,
    ipAddress: request.ip,
    userAgent: request.get("user-agent"),
  });
  return { token, expiresAt };
}

export function setSessionCookie(
  response: Response,
  request: Request,
  token: string,
): void {
  response.cookie(SESSION_COOKIE, token, {
    httpOnly: true,
    sameSite: "strict",
    secure: isExternalHttpsRequest(request),
    path: "/",
    maxAge: SESSION_TTL_MS,
  });
}

export function clearSessionCookie(response: Response): void {
  response.clearCookie(SESSION_COOKIE, {
    httpOnly: true,
    sameSite: "strict",
    path: "/",
  });
}

export function getRequestSession(
  request: Pick<IncomingMessage, "headers">,
): { token: string; tokenHash: string; user: SessionUser } | null {
  const token = cookieValue(request.headers.cookie || "", SESSION_COOKIE);
  if (!token) return null;
  const tokenHash = hashToken(token);
  const user = findSessionUser(tokenHash, Date.now());
  return user ? { token, tokenHash, user } : null;
}

export function deleteRequestSession(request: Request): void {
  const session = getRequestSession(request);
  if (session) deleteSessionRecord(session.tokenHash);
}

/** Recheck the exact request principal after asynchronous preparation and
 * immediately before dispatching an action. Account state alone cannot detect
 * an explicitly revoked session or a password reset. */
export function assertRequestUser(
  request: Request,
  expected: SessionUser,
): SessionUser {
  if (expected.id === "api-token") {
    const token = ludockApiToken();
    const candidate = bearerToken(request.headers.authorization);
    if (token && candidate && tokensMatch(candidate, token)) return expected;
  } else {
    const session = getRequestSession(request);
    if (session?.user.id === expected.id) return session.user;
  }
  throw new AuthError("AUTHENTICATION_REQUIRED", 401, "Authentication required");
}

export function authMiddleware(
  req: Request,
  res: Response,
  next: NextFunction,
): void {
  const session = getRequestSession(req);
  if (session) {
    res.locals.user = session.user;
    res.locals.sessionTokenHash = session.tokenHash;
    next();
    return;
  }

  const apiToken = ludockApiToken();
  const candidate = bearerToken(req.headers.authorization);
  if (apiToken && candidate && tokensMatch(candidate, apiToken)) {
    res.locals.user = {
      id: "api-token",
      username: "api-token",
      role: "admin",
    } satisfies SessionUser;
    next();
    return;
  }

  res.status(401).json({ error: "Authentication required" });
}

export function requireRole(...roles: SessionUser["role"][]) {
  return (_req: Request, res: Response, next: NextFunction): void => {
    const user = res.locals.user as SessionUser | undefined;
    if (!user || !roles.includes(user.role)) {
      res.status(403).json({ error: "Insufficient permissions" });
      return;
    }
    next();
  };
}

export function authenticateWsRequest(
  request: IncomingMessage,
): WebSocketAuth | null {
  const session = getRequestSession(request);
  if (session) {
    if (!isSameOriginRequest(request)) {
      logger.debug("WebSocket authentication rejected", {
        reason: "session-origin-mismatch",
        ...requestOriginDiagnostic(request),
      });
      return null;
    }
    logger.debug("WebSocket session authenticated", {
      method: "session",
      role: session.user.role,
    });
    return {
      user: session.user,
      sessionTokenHash: session.tokenHash,
      validate: () => findSessionUser(session.tokenHash, Date.now()),
    };
  }

  const apiToken = ludockApiToken();
  if (!apiToken) {
    logger.debug("WebSocket authentication rejected", {
      reason: "no-session-or-api-token",
    });
    return null;
  }
  if (request.headers.origin && !isSameOriginRequest(request)) {
    logger.debug("WebSocket authentication rejected", {
      reason: "api-token-origin-mismatch",
      ...requestOriginDiagnostic(request),
    });
    return null;
  }
  const candidate = bearerToken(request.headers.authorization);
  if (!candidate || !tokensMatch(candidate, apiToken)) {
    logger.debug("WebSocket authentication rejected", {
      reason: candidate ? "invalid-api-token" : "missing-bearer-token",
    });
    return null;
  }
  logger.debug("WebSocket session authenticated", {
    method: "api-token",
    role: "admin",
  });
  return {
    user: { id: "api-token", username: "api-token", role: "admin" },
    validate: () => ({
      id: "api-token",
      username: "api-token",
      role: "admin",
    }),
  };
}

export class AuthError extends Error {
  constructor(
    public readonly code: string,
    public readonly statusCode: number,
    message = code,
  ) {
    super(message);
  }
}

function hashToken(token: string): string {
  return createHash("sha256").update(token).digest("hex");
}

function derivePassword(
  password: string,
  salt: Buffer,
  keyLength: number,
  options: { N: number; r: number; p: number; maxmem: number },
): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    scrypt(password, salt, keyLength, options, (error, derivedKey) => {
      if (error) reject(error);
      else resolve(derivedKey);
    });
  });
}

function passwordHashNeedsUpgrade(encoded: string): boolean {
  const [algorithm, n, r, p] = encoded.split("$");
  return (
    algorithm !== "scrypt" ||
    Number(n) !== SCRYPT_N ||
    Number(r) !== SCRYPT_R ||
    Number(p) !== SCRYPT_P
  );
}

function bearerToken(authorization: string | undefined): string {
  if (!authorization) return "";
  const separator = authorization.indexOf(" ");
  if (
    separator < 1 ||
    authorization.slice(0, separator).toLowerCase() !== "bearer"
  ) {
    return "";
  }
  return authorization.slice(separator + 1);
}

function cookieValue(header: string, name: string): string {
  let value = "";
  for (const rawPart of header.split(";")) {
    const part = rawPart.trim();
    const separator = part.indexOf("=");
    if (separator === -1) continue;

    let cookieName: string;
    try {
      cookieName = decodeURIComponent(part.slice(0, separator));
    } catch {
      continue;
    }
    if (cookieName !== name) continue;

    try {
      value = decodeURIComponent(part.slice(separator + 1));
    } catch {
      value = "";
    }
  }
  return value;
}

function tokensMatch(candidate: string, expected: string): boolean {
  const candidateBuffer = Buffer.from(candidate);
  const expectedBuffer = Buffer.from(expected);
  return (
    candidateBuffer.length === expectedBuffer.length &&
    timingSafeEqual(candidateBuffer, expectedBuffer)
  );
}
