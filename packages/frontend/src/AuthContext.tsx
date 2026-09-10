import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { AUTH_REQUIRED_EVENT, ApiRequestError, apiJson, jsonBody } from "./api";
import {
  type AuthStatus,
  type CredentialsRequest,
  type SetupRequest,
  authStatusSchema,
  authUserResponseSchema,
  okResponseSchema,
} from "@ludock/shared";
import { AuthContext, type AuthUser } from "./auth-context";

const STATUS_RETRY_DELAYS_MS = [250, 500, 1_000];

function retryDelay(milliseconds: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const finish = () => {
      window.clearTimeout(timer);
      signal.removeEventListener("abort", finish);
      resolve();
    };
    const timer = window.setTimeout(finish, milliseconds);
    signal.addEventListener("abort", finish, { once: true });
    if (signal.aborted) finish();
  });
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [loading, setLoading] = useState(true);
  const [statusError, setStatusError] = useState(false);
  const [setupRequired, setSetupRequired] = useState(false);
  const [setupLocked, setSetupLocked] = useState(false);
  const [setupRemainingMs, setSetupRemainingMs] = useState<number | null>(null);
  const [user, setUser] = useState<AuthUser | null>(null);
  const revision = useRef(0);
  const statusRequest = useRef<AbortController | null>(null);
  const authRequest = useRef<AbortController | null>(null);

  const applyStatus = useCallback((status: AuthStatus) => {
    setSetupRequired(status.setupRequired);
    setSetupLocked(status.setupLocked);
    setSetupRemainingMs(status.setupRemainingMs);
    setUser(status.authenticated ? status.user : null);
  }, []);

  const invalidateStatus = useCallback(() => {
    revision.current += 1;
    statusRequest.current?.abort();
    statusRequest.current = null;
    return revision.current;
  }, []);

  const refreshStatus = useCallback(async () => {
    if (authRequest.current) return;
    const owner = invalidateStatus();
    const controller = new AbortController();
    statusRequest.current = controller;
    setLoading(true);
    setStatusError(false);

    for (let attempt = 0; attempt <= STATUS_RETRY_DELAYS_MS.length; attempt += 1) {
      if (controller.signal.aborted || revision.current !== owner) return;
      try {
        const status = await apiJson("/auth/status", authStatusSchema, {
          signal: controller.signal,
        });
        if (controller.signal.aborted || revision.current !== owner) return;
        applyStatus(status);
        statusRequest.current = null;
        setLoading(false);
        return;
      } catch {
        if (controller.signal.aborted || revision.current !== owner) return;
        const delay = STATUS_RETRY_DELAYS_MS[attempt];
        if (delay === undefined) {
          statusRequest.current = null;
          setUser(null);
          setStatusError(true);
          setLoading(false);
          return;
        }
        await retryDelay(delay, controller.signal);
      }
    }
  }, [applyStatus, invalidateStatus]);

  useEffect(() => {
    const requireAuth = () => {
      invalidateStatus();
      setUser(null);
      setStatusError(false);
      setLoading(false);
    };
    window.addEventListener(AUTH_REQUIRED_EVENT, requireAuth);
    void refreshStatus();
    return () => {
      window.removeEventListener(AUTH_REQUIRED_EVENT, requireAuth);
      invalidateStatus();
      authRequest.current?.abort();
      authRequest.current = null;
    };
  }, [invalidateStatus, refreshStatus]);

  useEffect(() => {
    if (!setupRequired || setupLocked || setupRemainingMs === null) return;
    const timer = window.setTimeout(
      () => setSetupLocked(true),
      Math.max(0, setupRemainingMs),
    );
    return () => window.clearTimeout(timer);
  }, [setupLocked, setupRemainingMs, setupRequired]);

  const authenticate = useCallback(
    async (
      path: "/auth/login" | "/auth/setup",
      username: string,
      password: string,
      bootstrapCode?: string,
    ): Promise<string | null> => {
      // Serialize cookie-changing requests; aborting a request cannot undo a Set-Cookie response.
      if (authRequest.current)
        return "Another sign-in request is still in progress.";
      const owner = invalidateStatus();
      const controller = new AbortController();
      authRequest.current = controller;
      try {
        const body: CredentialsRequest | SetupRequest = path === "/auth/setup"
          ? { username, password, bootstrapCode: bootstrapCode ?? "" }
          : { username, password };
        const { user } = await apiJson(path, authUserResponseSchema, {
          ...jsonBody("POST", body),
          signal: controller.signal,
        });
        if (controller.signal.aborted || revision.current !== owner)
          return "Your session changed. Please sign in again.";
        setSetupRequired(false);
        setSetupLocked(false);
        setSetupRemainingMs(null);
        setStatusError(false);
        setLoading(false);
        setUser(user);
        return null;
      } catch (error) {
        if (
          !controller.signal.aborted && revision.current === owner &&
          path === "/auth/setup" && error instanceof ApiRequestError &&
          error.status === 403
        ) {
          // A 403 also covers an invalid bootstrap code. Ask the server whether
          // setup actually expired before replacing the form, and leave its
          // local draft mounted when the status check is unavailable.
          try {
            const status = await apiJson("/auth/status", authStatusSchema, {
              signal: controller.signal,
            });
            if (!controller.signal.aborted && revision.current === owner) {
              applyStatus(status);
              setStatusError(false);
            }
          } catch {
            // The setup error remains actionable; a failed probe is not proof
            // that the time-limited setup window is locked.
          }
        }
        return error instanceof Error ? error.message : "Unable to sign in.";
      } finally {
        if (authRequest.current === controller) authRequest.current = null;
      }
    },
    [applyStatus, invalidateStatus],
  );

  const login = useCallback(
    (username: string, password: string) => authenticate("/auth/login", username, password),
    [authenticate],
  );
  const setup = useCallback(
    (username: string, password: string, bootstrapCode: string) =>
      authenticate("/auth/setup", username, password, bootstrapCode),
    [authenticate],
  );

  const logout = useCallback(async () => {
    if (authRequest.current) return;
    const owner = invalidateStatus();
    const controller = new AbortController();
    authRequest.current = controller;
    setUser(null);
    setLoading(true);
    setStatusError(false);
    try {
      await apiJson("/auth/logout", okResponseSchema, {
        method: "POST",
        signal: controller.signal,
      });
    } catch {
      if (!controller.signal.aborted && revision.current === owner) setStatusError(true);
    } finally {
      if (authRequest.current === controller) authRequest.current = null;
      if (!controller.signal.aborted && revision.current === owner) setLoading(false);
    }
  }, [invalidateStatus]);

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
