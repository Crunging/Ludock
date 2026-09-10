import { useCallback, useMemo, useState, type ReactNode } from "react";
import { ViewPreferencesContext, type DashboardFilters } from "./view-preferences-context";

/** Mounted only for the signed-in account. Navigation choices never reach disk. */
export default function ViewPreferencesProvider({ children }: { children: ReactNode }) {
  const [dashboardFilters, setDashboardFilters] = useState<DashboardFilters>({
    search: "",
    stateFilter: "all",
  });
  const [serverTabs, setServerTabs] = useState<Record<string, string>>({});
  const rememberServerTab = useCallback((serverId: string, tab: string) => {
    setServerTabs((current) => current[serverId] === tab
      ? current
      : { ...current, [serverId]: tab });
  }, []);
  const value = useMemo(() => ({
    dashboardFilters,
    setDashboardFilters,
    serverTabs,
    rememberServerTab,
  }), [dashboardFilters, serverTabs, rememberServerTab]);

  return <ViewPreferencesContext.Provider value={value}>{children}</ViewPreferencesContext.Provider>;
}
