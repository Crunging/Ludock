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
  findUserByUsername,
  upgradeUserPasswordHash,
  writeAuditLog,
  type SessionUser,
} from "./database.js";
import {
  isExternalHttpsRequest,
  isSameOriginRequest,
} from "./request-security.js";

const SESSION_COOKIE = "ludock_session";
const SESSION_TTL_MS = 7 * 24 * 60 * 60 * 1000;
export const MIN_API_TOKEN_LENGTH = 32;
const SCRYPT_N = 32768;
const SCRYPT_R = 8;
const SCRYPT_P = 3;
const SCRYPT_MAX_MEMORY = 64 * 1024 * 1024;
export const SETUP_WINDOW_MS = 5 * 60 * 1000;

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
      console.error(
        `LUDOCK_API_TOKEN is shorter than ${MIN_API_TOKEN_LENGTH} characters and has been ignored. Generate one with: openssl rand -hex 32`
      );
    }
    return "";
  }
  return configured;
}

export class SetupWindow {
  readonly expiresAt: number;

  constructor(
    private readonly now: () => number = Date.now,
    durationMs = SETUP_WINDOW_MS
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
  setupWindow: SetupWindow = defaultSetupWindow
): void {
  if (!isSetupRequired()) return;

  const minutes = Math.round(SETUP_WINDOW_MS / 60_000);
  console.log(
    `Initial administrator setup is available for ${minutes} minutes.`
  );
  console.log(
    `Setup closes at ${new Date(setupWindow.expiresAt).toISOString()}; restart the panel to reopen it.`
  );
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
  encoded: string
): Promise<boolean> {
  const [algorithm, n, r, p, saltValue, keyValue] = encoded.split("$");
  if (
    algorithm !== "scrypt" ||
    !n ||
    !r ||
    !p ||
    !saltValue ||
    !keyValue
  ) {
    return false;
  }

  const expected = Buffer.from(keyValue, "base64url");
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
    options.r < 1 ||
    options.r > 16 ||
    options.p < 1 ||
    options.p > 10
  ) {
    return false;
  }
  const actual = await derivePassword(
    password,
    Buffer.from(saltValue, "base64url"),
    expected.length,
    {
      ...options,
      maxmem: Math.max(
        SCRYPT_MAX_MEMORY,
        128 * options.N * options.r + 16 * 1024 * 1024
      ),
    }
  );

  return actual.length === expected.length && timingSafeEqual(actual, expected);
}

export async function createInitialAdmin(
  input: {
    username: string;
    password: string;
    ipAddress?: string;
  },
  setupWindow: SetupWindow = defaultSetupWindow
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
  password: string
): Promise<SessionUser | null> {
  const record = findUserByUsername(username);
  if (!record || record.disabled) {
    await hashPassword(password);
    return null;
  }
  if (!(await verifyPassword(password, record.passwordHash))) return null;
  if (passwordHashNeedsUpgrade(record.passwordHash)) {
    upgradeUserPasswordHash(record.id, await hashPassword(password));
  }

  return { id: record.id, username: record.username, role: record.role };
}

export function createSession(
  user: SessionUser,
  request: Request
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
  token: string
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
  request: Pick<IncomingMessage, "headers">
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

export function authMiddleware(
  req: Request,
  res: Response,
  next: NextFunction
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
  request: IncomingMessage
): WebSocketAuth | null {
  const session = getRequestSession(request);
  if (session) {
    if (!isSameOriginRequest(request)) return null;
    return {
      user: session.user,
      sessionTokenHash: session.tokenHash,
      validate: () => findSessionUser(session.tokenHash, Date.now()),
    };
  }

  const apiToken = ludockApiToken();
  if (!apiToken) return null;
  if (request.headers.origin && !isSameOriginRequest(request)) {
    return null;
  }
  const candidate = bearerToken(request.headers.authorization);
  if (!candidate || !tokensMatch(candidate, apiToken)) return null;
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
    public readonly statusCode: number
  ) {
    super(code);
  }
}

function hashToken(token: string): string {
  return createHash("sha256").update(token).digest("hex");
}

function derivePassword(
  password: string,
  salt: Buffer,
  keyLength: number,
  options: { N: number; r: number; p: number; maxmem: number }
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
