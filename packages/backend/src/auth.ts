import { encodeText } from "./bytes.js";
import { Cookie } from "bun";
import { AppError } from "./errors.js";
import {
  SETUP_CODE_MAX_LENGTH,
  SETUP_CODE_MIN_LENGTH,
  type HistoryActor,
} from "@ludock/shared";
import {
  countUsers,
  createSessionRecord,
  createUser,
  deleteSessionRecord,
  findSessionUser,
  findUserById,
  findUserByUsername,
  keyedCredentialFingerprint,
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
import {
  hashPassword,
  passwordHashNeedsUpgrade,
  verifyPassword,
  withPasswordWork,
} from "./password.js";
export { hashPassword, verifyPassword } from "./password.js";

import { developmentInstance } from "./development-instance.js";
const SESSION_COOKIE = developmentInstance
  ? `ludock_session_${developmentInstance}`
  : "ludock_session";
const SESSION_TTL_MS = 7 * 24 * 60 * 60 * 1000;
export const MIN_API_TOKEN_LENGTH = 32;
export const SETUP_WINDOW_MS = 5 * 60 * 1000;
const API_TOKEN_OPERATION_ACTOR_PREFIX = "api-token:";
const API_TOKEN_OPERATION_ACTOR_PATTERN = /^api-token:[0-9a-f]{64}$/;
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
  private readonly authorizationHash: Uint8Array;
  private pendingGeneratedCode: string | null;

  constructor(
    private readonly now: () => number = Date.now,
    durationMs = SETUP_WINDOW_MS,
    bootstrapCode?: string,
  ) {
    this.expiresAt = now() + durationMs;
    const configured = bootstrapCode ?? process.env.LUDOCK_SETUP_CODE;
    const generated = !configured;
    const code = configured || crypto.getRandomValues(new Uint8Array(32)).toBase64({ alphabet: "base64url", omitPadding: true });
    if (
      code.length < SETUP_CODE_MIN_LENGTH ||
      code.length > SETUP_CODE_MAX_LENGTH
    ) {
      throw new Error(
        `LUDOCK_SETUP_CODE must be between ${SETUP_CODE_MIN_LENGTH} and ${SETUP_CODE_MAX_LENGTH} characters`,
      );
    }
    this.authorizationHash = new Bun.CryptoHasher("sha256").update(code).digest();
    this.pendingGeneratedCode = generated ? code : null;
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

  assertAuthorized(candidate: unknown): void {
    this.assertOpen();
    const value = typeof candidate === "string" ? candidate : "";
    const candidateHash = new Bun.CryptoHasher("sha256").update(value).digest();
    const matches = crypto.timingSafeEqual(candidateHash, this.authorizationHash);
    if (
      !matches ||
      value.length < SETUP_CODE_MIN_LENGTH ||
      value.length > SETUP_CODE_MAX_LENGTH
    ) {
      throw new AuthError(
        "SETUP_AUTHORIZATION_REQUIRED",
        403,
        "Initial setup authorization failed",
      );
    }
  }

  takeGeneratedCode(): string | null {
    const code = this.pendingGeneratedCode;
    this.pendingGeneratedCode = null;
    return code;
  }

  markCompleted(): void {
    this.pendingGeneratedCode = null;
    this.authorizationHash.fill(0);
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

  const generatedCode = setupWindow.takeGeneratedCode();
  if (generatedCode) {
    // This credential belongs in the local process/container console, not the
    // structured logger whose ring buffer is available through the web UI.
    process.stdout.write(
      `Ludock initial setup code: ${generatedCode}\n` +
        "Enter this one-time code in the setup page within five minutes.\n",
    );
  }
  const minutes = Math.round(SETUP_WINDOW_MS / 60_000);
  logger.info("Initial administrator setup is available", {
    durationMinutes: minutes,
    closesAt: new Date(setupWindow.expiresAt).toISOString(),
  });
}

export async function createInitialAdmin(
  input: {
    username: string;
    password: string;
    bootstrapCode: string;
    ipAddress?: string;
  },
  setupWindow: SetupWindow = defaultSetupWindow,
): Promise<SessionUser> {
  setupWindow.assertAuthorized(input.bootstrapCode);

  const now = Date.now();
  const user: SessionUser = {
    id: crypto.randomUUID(),
    username: input.username,
    role: "admin",
  };
  const passwordHash = await withPasswordWork(
    hashToken(`setup-source:${input.ipAddress ?? "unknown"}`),
    "initial-setup",
    () => hashPassword(input.password),
  );
  setupWindow.assertAuthorized(input.bootstrapCode);
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
  setupWindow.markCompleted();
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
  ipAddress?: string,
): { token: string; expiresAt: number } {
  const token = crypto.getRandomValues(new Uint8Array(32)).toBase64({ alphabet: "base64url", omitPadding: true });
  const now = Date.now();
  const expiresAt = now + SESSION_TTL_MS;
  createSessionRecord({
    sessionId: crypto.randomUUID(),
    tokenHash: hashToken(token),
    userId: user.id,
    createdAt: now,
    expiresAt,
    ipAddress,
    userAgent: request.headers.get("user-agent") || undefined,
  });
  return { token, expiresAt };
}

export function setSessionCookie(
  headers: Headers,
  request: Request,
  token: string,
): void {
  const cookie = new Cookie(SESSION_COOKIE, token, {
    httpOnly: true,
    sameSite: "strict",
    secure: isExternalHttpsRequest(request),
    path: "/",
    maxAge: SESSION_TTL_MS / 1000,
  });
  headers.append("Set-Cookie", cookie.toString());
}

export function clearSessionCookie(headers: Headers): void {
  const cookie = new Cookie(SESSION_COOKIE, "", {
    httpOnly: true,
    sameSite: "strict",
    path: "/",
    maxAge: 0,
    expires: new Date(0),
  });
  headers.append("Set-Cookie", cookie.toString());
}

export function getRequestSession(
  request: Pick<Request, "headers">,
): { token: string; tokenHash: string; user: SessionUser } | null {
  const token = cookieValue(request.headers.get("cookie") || "", SESSION_COOKIE);
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
    const candidate = bearerToken(request.headers.get("authorization"));
    if (token && candidate && tokensMatch(candidate, token)) return expected;
  } else {
    const session = getRequestSession(request);
    if (session?.user.id === expected.id) return session.user;
  }
  throw new AuthError("AUTHENTICATION_REQUIRED", 401, "Authentication required");
}

/** Bind durable work to the exact API credential generation that requested it. */
export function operationActorId(
  request: Request,
  expected: SessionUser,
): string {
  const current = assertRequestUser(request, expected);
  if (current.id !== "api-token") return current.id;
  const token = ludockApiToken();
  if (!token) throw new AuthError("AUTHENTICATION_REQUIRED", 401);
  return `${API_TOKEN_OPERATION_ACTOR_PREFIX}${keyedCredentialFingerprint(token)}`;
}

export function isApiTokenOperationActor(actorId: string): boolean {
  return (
    actorId === "api-token" ||
    API_TOKEN_OPERATION_ACTOR_PATTERN.test(actorId)
  );
}

export function isCurrentApiTokenOperationActor(actorId: string): boolean {
  if (
    actorId === "api-token" ||
    !actorId.startsWith(API_TOKEN_OPERATION_ACTOR_PREFIX)
  ) {
    return false;
  }
  const token = ludockApiToken();
  if (!token) return false;
  const current = `${API_TOKEN_OPERATION_ACTOR_PREFIX}${keyedCredentialFingerprint(token)}`;
  return tokensMatch(actorId, current);
}

export function publicOperationActorId(actorId: string): string {
  return actorId === "api-token" || actorId.startsWith(API_TOKEN_OPERATION_ACTOR_PREFIX)
    ? "api-token"
    : actorId;
}

export function publicHistoryActor(id: string | null, name: string | null): HistoryActor | null {
  if (!id && !name) return null;
  const safeId = id === null ? null : publicOperationActorId(id);
  return { id: safeId, name: safeId === "api-token" ? "API token" : name };
}

export function authenticateRequest(
  request: Request,
): { user: SessionUser; sessionTokenHash?: string } | null {
  const session = getRequestSession(request);
  if (session) {
    return { user: session.user, sessionTokenHash: session.tokenHash };
  }

  const apiToken = ludockApiToken();
  const candidate = bearerToken(request.headers.get("authorization"));
  if (apiToken && candidate && tokensMatch(candidate, apiToken)) {
    return { user: { id: "api-token", username: "api-token", role: "admin" } };
  }

  return null;
}

export function authenticateWsRequest(
  request: Request,
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
  if (request.headers.has("origin") && !isSameOriginRequest(request)) {
    logger.debug("WebSocket authentication rejected", {
      reason: "api-token-origin-mismatch",
      ...requestOriginDiagnostic(request),
    });
    return null;
  }
  const candidate = bearerToken(request.headers.get("authorization"));
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
    validate: () => {
      const current = ludockApiToken();
      return current && tokensMatch(candidate, current)
        ? { id: "api-token", username: "api-token", role: "admin" }
        : null;
    },
  };
}

export class AuthError extends AppError {
  constructor(
    code: string,
    statusCode: number,
    message = code,
  ) {
    super(code, statusCode, message);
  }
}

function hashToken(token: string): string {
  return new Bun.CryptoHasher("sha256").update(token).digest("hex");
}

function bearerToken(authorization: string | null): string {
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
  const candidateBuffer = encodeText(candidate);
  const expectedBuffer = encodeText(expected);
  return (
    candidateBuffer.length === expectedBuffer.length &&
    crypto.timingSafeEqual(candidateBuffer, expectedBuffer)
  );
}
