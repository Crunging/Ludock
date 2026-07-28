import { createContext, useContext } from "react";

export type UserRole = "admin" | "operator" | "viewer";

export interface AuthUser {
  id: string;
  username: string;
  role: UserRole;
}

export interface AuthContextValue {
  loading: boolean;
  setupRequired: boolean;
  setupLocked: boolean;
  authenticated: boolean;
  user: AuthUser | null;
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
