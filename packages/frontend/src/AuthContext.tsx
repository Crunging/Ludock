import { useCallback, useEffect, useState, type ReactNode } from "react";
import { AUTH_REQUIRED_EVENT } from "./api";
import { AuthContext, type AuthUser } from "./auth-context";

interface AuthStatus {
  setupRequired: boolean;
  setupLocked: boolean;
  setupRemainingMs: number | null;
  authenticated: boolean;
  user: AuthUser | null;
}

const STATUS_RETRY_DELAYS_MS = [250, 500, 1_000];

function retryDelay(milliseconds: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, milliseconds));
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [loading, setLoading] = useState(true);
  const [statusError, setStatusError] = useState(false);
  const [setupRequired, setSetupRequired] = useState(false);
  const [setupLocked, setSetupLocked] = useState(false);
  const [setupRemainingMs, setSetupRemainingMs] = useState<number | null>(null);
  const [user, setUser] = useState<AuthUser | null>(null);

  const refreshStatus = useCallback(async () => {
    setLoading(true);
    setStatusError(false);

    for (let attempt = 0; attempt <= STATUS_RETRY_DELAYS_MS.length; attempt += 1) {
      try {
        const response = await fetch("/api/auth/status", {
          credentials: "same-origin",
        });
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        const status = (await response.json()) as AuthStatus;
        setSetupRequired(status.setupRequired);
        setSetupLocked(status.setupLocked);
        setSetupRemainingMs(status.setupRemainingMs);
        setUser(status.authenticated ? status.user : null);
        setLoading(false);
        return;
      } catch {
        const delay = STATUS_RETRY_DELAYS_MS[attempt];
        if (delay === undefined) {
          setStatusError(true);
          setLoading(false);
          return;
        }
        await retryDelay(delay);
      }
    }
  }, []);

  useEffect(() => {
    void refreshStatus();
  }, [refreshStatus]);

  useEffect(() => {
    if (!setupRequired || setupLocked || setupRemainingMs === null) return;
    const timer = window.setTimeout(
      () => setSetupLocked(true),
      Math.max(0, setupRemainingMs)
    );
    return () => window.clearTimeout(timer);
  }, [setupLocked, setupRemainingMs, setupRequired]);

  useEffect(() => {
    const requireAuth = () => setUser(null);
    window.addEventListener(AUTH_REQUIRED_EVENT, requireAuth);
    return () => window.removeEventListener(AUTH_REQUIRED_EVENT, requireAuth);
  }, []);

  const login = useCallback(
    async (username: string, password: string): Promise<string | null> => {
      const response = await fetch("/api/auth/login", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      const body = (await response.json().catch(() => ({}))) as {
        user?: AuthUser;
        error?: string;
      };
      if (!response.ok || !body.user) {
        return body.error || `Unable to sign in (HTTP ${response.status}).`;
      }

      setUser(body.user);
      return null;
    },
    []
  );

  const setup = useCallback(
    async (username: string, password: string): Promise<string | null> => {
      const response = await fetch("/api/auth/setup", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      const body = (await response.json().catch(() => ({}))) as {
        user?: AuthUser;
        error?: string;
      };
      if (!response.ok || !body.user) {
        if (response.status === 403) setSetupLocked(true);
        return body.error || `Unable to complete setup (HTTP ${response.status}).`;
      }

      setSetupRequired(false);
      setSetupLocked(false);
      setSetupRemainingMs(null);
      setUser(body.user);
      return null;
    },
    []
  );

  const logout = useCallback(async () => {
    try {
      await fetch("/api/auth/logout", {
        method: "POST",
        credentials: "same-origin",
      });
    } finally {
      setUser(null);
    }
  }, []);

  return (
    <AuthContext.Provider
      value={{
        loading,
        statusError,
        setupRequired,
        setupLocked,
        authenticated: user !== null,
        user,
        refreshStatus,
        login,
        setup,
        logout,
      }}
    >
      {children}
    </AuthContext.Provider>
  );
}
