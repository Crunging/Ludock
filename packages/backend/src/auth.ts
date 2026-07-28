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
  writeAuditLog,
  type SessionUser,
} from "./database.js";

const SESSION_COOKIE = "dgm_session";
const SESSION_TTL_MS = 7 * 24 * 60 * 60 * 1000;

export function isSetupRequired(): boolean {
  return countUsers() === 0;
}

export function isSetupTokenRequired(): boolean {
  return getSetupToken() !== null;
}

function getSetupToken(): string | null {
  return process.env.PANEL_SETUP_TOKEN || process.env.PANEL_SECRET || null;
}

export function logSetupInstructions(): void {
  if (!isSetupRequired()) return;

  console.log("Initial administrator setup is available in the web interface.");
  if (isSetupTokenRequired()) {
    console.log("The configured PANEL_SETUP_TOKEN is required.");
  }
}

export async function hashPassword(password: string): Promise<string> {
  const salt = randomBytes(16);
  const key = await derivePassword(password, salt, 64, {
    N: 32768,
    r: 8,
    p: 1,
    maxmem: 64 * 1024 * 1024,
  });

  return `scrypt$32768$8$1$${salt.toString("base64url")}$${key.toString("base64url")}`;
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
  const actual = await derivePassword(
    password,
    Buffer.from(saltValue, "base64url"),
    expected.length,
    {
      N: Number(n),
      r: Number(r),
      p: Number(p),
      maxmem: 64 * 1024 * 1024,
    }
  );

  return actual.length === expected.length && timingSafeEqual(actual, expected);
}

export async function createInitialAdmin(input: {
  setupToken?: string;
  username: string;
  password: string;
  ipAddress?: string;
}): Promise<SessionUser> {
  if (!isSetupRequired()) throw new AuthError("SETUP_COMPLETE", 409);
  const expectedToken = getSetupToken();
  if (
    expectedToken !== null &&
    !tokensMatch(input.setupToken || "", expectedToken)
  ) {
    throw new AuthError("INVALID_SETUP_TOKEN", 401);
  }

  const now = Date.now();
  const user: SessionUser = {
    id: randomUUID(),
    username: input.username,
    role: "admin",
  };
  const passwordHash = await hashPassword(input.password);
  if (!isSetupRequired()) throw new AuthError("SETUP_COMPLETE", 409);
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
  const secure = request.secure || process.env.COOKIE_SECURE === "true";
  response.cookie(SESSION_COOKIE, token, {
    httpOnly: true,
    sameSite: "strict",
    secure,
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
  const token = parseCookies(request.headers.cookie || "")[SESSION_COOKIE];
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

  const apiToken =
    process.env.PANEL_API_TOKEN || process.env.PANEL_SECRET || "";
  const authHeader = req.headers.authorization;
  if (
    apiToken &&
    authHeader?.startsWith("Bearer ") &&
    tokensMatch(authHeader.slice(7), apiToken)
  ) {
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
): SessionUser | null {
  const session = getRequestSession(request);
  if (session) return session.user;

  const apiToken =
    process.env.PANEL_API_TOKEN || process.env.PANEL_SECRET || "";
  if (!apiToken) return null;

  try {
    const parsed = new URL(request.url || "", "http://localhost");
    const candidate = parsed.searchParams.get("token");
    return candidate !== null && tokensMatch(candidate, apiToken)
      ? { id: "api-token", username: "api-token", role: "admin" }
      : null;
  } catch {
    return null;
  }
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

function parseCookies(header: string): Record<string, string> {
  return Object.fromEntries(
    header
      .split(";")
      .map((part) => part.trim())
      .filter(Boolean)
      .map((part) => {
        const separator = part.indexOf("=");
        return separator === -1
          ? [part, ""]
          : [
              decodeURIComponent(part.slice(0, separator)),
              decodeURIComponent(part.slice(separator + 1)),
            ];
      })
  );
}

function tokensMatch(candidate: string, expected: string): boolean {
  const candidateBuffer = Buffer.from(candidate);
  const expectedBuffer = Buffer.from(expected);
  return (
    candidateBuffer.length === expectedBuffer.length &&
    timingSafeEqual(candidateBuffer, expectedBuffer)
  );
}
