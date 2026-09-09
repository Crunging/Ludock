import { useCallback, useEffect, useState, type ReactNode } from "react";
import { AUTH_REQUIRED_EVENT, ApiRequestError, apiJson, jsonBody } from "./api";
import {
  type CredentialsRequest,
  type SetupRequest,
  authStatusSchema,
  authUserResponseSchema,
  okResponseSchema,
} from "@ludock/shared";
import { AuthContext, type AuthUser } from "./auth-context";

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

    for (
      let attempt = 0;
      attempt <= STATUS_RETRY_DELAYS_MS.length;
      attempt += 1
    ) {
      try {
        const status = await apiJson("/auth/status", authStatusSchema);
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
      Math.max(0, setupRemainingMs),
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
      try {
        const { user } = await apiJson(
          "/auth/login",
          authUserResponseSchema,
          jsonBody("POST", {
            username,
            password,
          } satisfies CredentialsRequest),
        );
        setUser(user);
        return null;
      } catch (error) {
        return error instanceof Error ? error.message : "Unable to sign in.";
      }
    },
    [],
  );

  const setup = useCallback(
    async (username: string, password: string): Promise<string | null> => {
      try {
        const { user } = await apiJson(
          "/auth/setup",
          authUserResponseSchema,
          jsonBody("POST", { username, password } satisfies SetupRequest),
        );
        setSetupRequired(false);
        setSetupLocked(false);
        setSetupRemainingMs(null);
        setUser(user);
        return null;
      } catch (error) {
        if (error instanceof ApiRequestError && error.status === 403)
          setSetupLocked(true);
        return error instanceof Error
          ? error.message
          : "Unable to complete setup.";
      }
    },
    [],
  );

  const logout = useCallback(async () => {
    try {
      await apiJson("/auth/logout", okResponseSchema, { method: "POST" });
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
