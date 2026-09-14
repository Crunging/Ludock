import { createContext, useContext } from "react";

export interface NavigationContextValue {
  pathname: string;
  search?: string;
  navigate: (to: string, options?: { replace?: boolean }) => void;
}

export const NavigationContext =
  createContext<NavigationContextValue | null>(null);

export function useLocation() {
  const { pathname, search = "" } = useNavigation();
  return { pathname, search };
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
