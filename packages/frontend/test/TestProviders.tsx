import type { ReactNode } from "react";
import type { AuthUser } from "@ludock/shared";
import { AuthContext, type AuthContextValue } from "../src/auth-context";
import { NavigationContext, type NavigationContextValue } from "../src/navigation-context";
import ViewPreferencesProvider from "../src/ViewPreferences";

async function unexpectedAuthAction(): Promise<never> {
  throw new Error("Authentication actions are not configured in TestProviders.");
}

const authDefaults: Omit<AuthContextValue, "user"> = {
  loading: false,
  statusError: false,
  setupRequired: false,
  setupLocked: false,
  authenticated: true,
  refreshStatus: unexpectedAuthAction,
  login: unexpectedAuthAction,
  setup: unexpectedAuthAction,
  logout: unexpectedAuthAction,
};

export default function TestProviders({ children, user, pathname, navigate }: {
  children: ReactNode;
  user: AuthUser;
  pathname: string;
  navigate: NavigationContextValue["navigate"];
}) {
  return (
    <AuthContext.Provider value={{ ...authDefaults, user }}>
      <NavigationContext.Provider value={{ pathname, navigate }}>
        <ViewPreferencesProvider>{children}</ViewPreferencesProvider>
      </NavigationContext.Provider>
    </AuthContext.Provider>
  );
}
