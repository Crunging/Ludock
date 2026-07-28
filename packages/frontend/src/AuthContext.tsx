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

export function AuthProvider({ children }: { children: ReactNode }) {
  const [loading, setLoading] = useState(true);
  const [setupRequired, setSetupRequired] = useState(false);
  const [setupLocked, setSetupLocked] = useState(false);
  const [setupRemainingMs, setSetupRemainingMs] = useState<number | null>(null);
  const [user, setUser] = useState<AuthUser | null>(null);

  useEffect(() => {
    let active = true;

    fetch("/api/auth/status", { credentials: "same-origin" })
      .then(async (response) => {
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        return (await response.json()) as AuthStatus;
      })
      .then((status) => {
        if (!active) return;
        setSetupRequired(status.setupRequired);
        setSetupLocked(status.setupLocked);
        setSetupRemainingMs(status.setupRemainingMs);
        setUser(status.authenticated ? status.user : null);
      })
      .catch(() => {
        if (!active) return;
        setUser(null);
      })
      .finally(() => {
        if (active) setLoading(false);
      });

    return () => {
      active = false;
    };
  }, []);

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
        setupRequired,
        setupLocked,
        authenticated: user !== null,
        user,
        login,
        setup,
        logout,
      }}
    >
      {children}
    </AuthContext.Provider>
  );
}
