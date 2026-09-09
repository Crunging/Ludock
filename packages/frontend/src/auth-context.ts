import { createContext, useContext } from "react";

import type { AuthUser } from "@ludock/shared";
export type { AuthUser, UserRole } from "@ludock/shared";

export interface AuthContextValue {
  loading: boolean;
  statusError: boolean;
  setupRequired: boolean;
  setupLocked: boolean;
  authenticated: boolean;
  user: AuthUser | null;
  refreshStatus: () => Promise<void>;
  login: (username: string, password: string) => Promise<string | null>;
  setup: (username: string, password: string) => Promise<string | null>;
  logout: () => Promise<void>;
}

export const AuthContext = createContext<AuthContextValue | null>(null);

export function useAuth(): AuthContextValue {
  const context = useContext(AuthContext);
  if (!context) throw new Error("useAuth must be used within AuthProvider");
  return context;
}
