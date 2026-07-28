import { createContext, useContext } from "react";

export interface NavigationContextValue {
  pathname: string;
  navigate: (to: string, options?: { replace?: boolean }) => void;
}

export const NavigationContext =
  createContext<NavigationContextValue | null>(null);

export function useLocation() {
  return { pathname: useNavigation().pathname };
}

export function useNavigate() {
  return useNavigation().navigate;
}

export function useNavigation(): NavigationContextValue {
  const context = useContext(NavigationContext);
  if (!context) {
    throw new Error("Navigation hooks require NavigationProvider");
  }
  return context;
}
