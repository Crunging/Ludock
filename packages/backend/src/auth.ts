import type { Request, Response, NextFunction } from "express";

// If PANEL_SECRET is set, all requests require a Bearer token (API)
// or ?token= query param (WebSocket). If unset, auth is disabled.

const PANEL_SECRET = process.env.PANEL_SECRET || "";

export function isAuthEnabled(): boolean {
  return PANEL_SECRET.length > 0;
}

export function authMiddleware(
  req: Request,
  res: Response,
  next: NextFunction
): void {
  if (!isAuthEnabled()) {
    next();
    return;
  }

  const authHeader = req.headers.authorization;
  if (!authHeader || !authHeader.startsWith("Bearer ")) {
    res.status(401).json({ error: "Missing or invalid authorization header" });
    return;
  }

  const token = authHeader.slice(7);
  if (token !== PANEL_SECRET) {
    res.status(403).json({ error: "Invalid token" });
    return;
  }

  next();
}

export function validateWsAuth(url: string): boolean {
  if (!isAuthEnabled()) return true;

  try {
    const parsed = new URL(url, "http://localhost");
    return parsed.searchParams.get("token") === PANEL_SECRET;
  } catch {
    return false;
  }
}
