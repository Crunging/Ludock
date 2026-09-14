import { useCallback, useEffect, useRef, useState } from "react";
import { backupPreflightResponseSchema, type BackupPreflight } from "@ludock/shared";
import { apiJson } from "./api";

export function useBackupPreflight(path: string, enabled: boolean, identity: string) {
  const [preflight, setPreflight] = useState<BackupPreflight | null>(null);
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const request = useRef<AbortController | null>(null);
  const check = useCallback(async () => {
    if (!enabled) return null;
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setPreflight(null);
    setError(null);
    setChecking(true);
    try {
      const response = await apiJson(`${path}/backups/preflight`, backupPreflightResponseSchema, {
        signal: controller.signal,
      });
      if (controller.signal.aborted) return null;
      setPreflight(response.preflight);
      return response.preflight;
    } catch (reason) {
      if (!controller.signal.aborted)
        setError(reason instanceof Error ? reason.message : "Unable to check backup readiness.");
      return null;
    } finally {
      if (!controller.signal.aborted) setChecking(false);
    }
  }, [path, enabled]);
  useEffect(() => {
    setPreflight(null);
    setError(null);
    setChecking(false);
    if (enabled) void check();
    return () => request.current?.abort();
  }, [check, enabled, identity]);
  return { preflight, checking, error, check };
}
