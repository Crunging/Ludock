import { createContext, useContext, type Dispatch, type SetStateAction } from "react";

export interface DashboardFilters {
  search: string;
  stateFilter: string;
}

interface ViewPreferences {
  dashboardFilters: DashboardFilters;
  setDashboardFilters: Dispatch<SetStateAction<DashboardFilters>>;
  serverTabs: Record<string, string>;
  rememberServerTab: (serverId: string, tab: string) => void;
}

export const ViewPreferencesContext = createContext<ViewPreferences | null>(null);

export function useViewPreferences() {
  const value = useContext(ViewPreferencesContext);
  if (!value) throw new Error("View preferences require ViewPreferencesProvider");
  return value;
}
