import { useEffect, useRef, useState, type FormEvent } from "react";
import { apiJson, jsonBody } from "../api";
import { usePageRead } from "../hooks/usePageRead";

import {
  type SessionSummary,
  type ChangePasswordRequest,
  PASSWORD_MIN_LENGTH,
  sessionsResponseSchema,
  okResponseSchema,
} from "@ludock/shared";

const readSessions = (signal: AbortSignal) => apiJson("/account/sessions", sessionsResponseSchema, { signal });

export default function Account() {
  const { data, loading: sessionsLoading, error: sessionsError, refresh: loadSessions } = usePageRead(readSessions, "Failed to load sessions");
  const sessions = data?.sessions ?? [];
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const mutation = useRef<AbortController | null>(null);
  useEffect(() => () => mutation.current?.abort(), []);

  const changePassword = async (event: FormEvent) => {
    event.preventDefault();
    if (mutation.current) return;
    setError(null);
    setMessage(null);
    if (newPassword !== confirmPassword) {
      setError("New passwords do not match.");
      return;
    }
    const controller = new AbortController();
    mutation.current = controller;
    setBusy(true);
    try {
      await apiJson(
        "/account/change-password",
        okResponseSchema,
        {
          ...jsonBody("POST", { currentPassword, newPassword } satisfies ChangePasswordRequest),
          signal: controller.signal,
        },
      );
      if (controller.signal.aborted) return;
      setCurrentPassword((value) => value === currentPassword ? "" : value);
      setNewPassword((value) => value === newPassword ? "" : value);
      setConfirmPassword((value) => value === confirmPassword ? "" : value);
      setMessage("Password changed. Other sessions have been signed out.");
      await loadSessions();
    } catch (reason) {
      if (controller.signal.aborted) return;
      setError(
        reason instanceof Error ? reason.message : "Failed to change password",
      );
    } finally {
      if (mutation.current === controller) {
        mutation.current = null;
        if (!controller.signal.aborted) setBusy(false);
      }
    }
  };

  const revokeSession = async (session: SessionSummary) => {
    if (mutation.current || sessionsLoading || sessionsError) return;
    const controller = new AbortController();
    mutation.current = controller;
    setBusy(true);
    setError(null);
    setMessage(null);
    try {
      await apiJson(`/account/sessions/${session.id}`, okResponseSchema, {
        method: "DELETE",
        signal: controller.signal,
      });
      if (controller.signal.aborted) return;
      if (session.current) {
        window.location.reload();
        return;
      }
      setMessage("Session revoked.");
      await loadSessions();
    } catch (reason) {
      if (controller.signal.aborted) return;
      setError(
        reason instanceof Error ? reason.message : "Failed to revoke session",
      );
    } finally {
      if (mutation.current === controller) {
        mutation.current = null;
        if (!controller.signal.aborted) setBusy(false);
      }
    }
  };

  return (
    <div className="page">
      <div className="page__header">
        <h1 className="page__title">Account</h1>
        <p className="page__subtitle">
          Change your password and review signed-in devices.
        </p>
      </div>

      {error && <div className="alert alert--error" role="alert">{error}</div>}
      {message && <div className="alert alert--success" role="status">{message}</div>}

      <form className="settings-card" onSubmit={changePassword}>
        <h2>Change password</h2>
        <div className="form-grid form-grid--password">
          <label>
            <span>Current password</span>
            <input
              type="password"
              autoComplete="current-password"
              maxLength={128}
              value={currentPassword}
              onChange={(event) => setCurrentPassword(event.target.value)}
              required
            />
          </label>
          <label>
            <span>New password</span>
            <input
              type="password"
              autoComplete="new-password"
              minLength={PASSWORD_MIN_LENGTH}
              maxLength={128}
              value={newPassword}
              onChange={(event) => setNewPassword(event.target.value)}
              required
            />
          </label>
          <label>
            <span>Confirm new password</span>
            <input
              type="password"
              autoComplete="new-password"
              minLength={PASSWORD_MIN_LENGTH}
              maxLength={128}
              value={confirmPassword}
              onChange={(event) => setConfirmPassword(event.target.value)}
              required
            />
          </label>
          <button className="primary-btn" disabled={busy}>
            {busy ? "Working…" : "Change password"}
          </button>
        </div>
      </form>

      <section className="settings-section">
        <div className="section-heading">
          <h2>Active sessions</h2>
          <button className="secondary-btn" disabled={sessionsLoading || busy} onClick={() => void loadSessions()}>
            Refresh sessions
          </button>
        </div>
        {sessionsLoading && <p className="muted" role="status">Loading sessions…</p>}
        {sessionsError && <div className="alert alert--error" role="alert">Unable to load sessions: {sessionsError}</div>}
        {!sessionsLoading && !sessionsError && sessions.length === 0 && <p className="muted">No active sessions.</p>}
        <div className="settings-list">
          {sessions.map((session) => (
            <article className="settings-card session-row" key={session.id}>
              <div>
                <strong>
                  {session.current
                    ? "This device"
                    : session.ipAddress || "Unknown address"}
                </strong>
                <span>{session.userAgent || "Unknown browser"}</span>
                <span>
                  Last active {new Date(session.lastSeenAt).toLocaleString()}
                </span>
              </div>
              <button
                className="secondary-btn secondary-btn--danger"
                disabled={busy || sessionsLoading}
                onClick={() => revokeSession(session)}
              >
                {session.current ? "Sign out" : "Revoke"}
              </button>
            </article>
          ))}
        </div>
      </section>
    </div>
  );
}
