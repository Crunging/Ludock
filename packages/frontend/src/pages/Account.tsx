import { useCallback, useEffect, useState, type FormEvent } from "react";
import { apiJson, jsonBody } from "../api";

import {
  type SessionSummary,
  type ChangePasswordRequest,
  PASSWORD_MIN_LENGTH,
  sessionsResponseSchema,
  okResponseSchema,
} from "@ludock/shared";

export default function Account() {
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const loadSessions = useCallback(async () => {
    const body = await apiJson("/account/sessions", sessionsResponseSchema);
    setSessions(body.sessions);
  }, []);

  useEffect(() => {
    loadSessions().catch((reason: unknown) =>
      setError(
        reason instanceof Error ? reason.message : "Failed to load sessions",
      ),
    );
  }, [loadSessions]);

  const changePassword = async (event: FormEvent) => {
    event.preventDefault();
    setError(null);
    setMessage(null);
    if (newPassword !== confirmPassword) {
      setError("New passwords do not match.");
      return;
    }
    setBusy(true);
    try {
      await apiJson(
        "/account/change-password",
        okResponseSchema,
        jsonBody("POST", {
          currentPassword,
          newPassword,
        } satisfies ChangePasswordRequest),
      );
      setCurrentPassword("");
      setNewPassword("");
      setConfirmPassword("");
      setMessage("Password changed. Other sessions have been signed out.");
      await loadSessions();
    } catch (reason) {
      setError(
        reason instanceof Error ? reason.message : "Failed to change password",
      );
    } finally {
      setBusy(false);
    }
  };

  const revokeSession = async (session: SessionSummary) => {
    setError(null);
    try {
      await apiJson(`/account/sessions/${session.id}`, okResponseSchema, {
        method: "DELETE",
      });
      if (session.current) {
        window.location.reload();
        return;
      }
      await loadSessions();
    } catch (reason) {
      setError(
        reason instanceof Error ? reason.message : "Failed to revoke session",
      );
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

      {error && <div className="alert alert--error">{error}</div>}
      {message && <div className="alert alert--success">{message}</div>}

      <form className="settings-card" onSubmit={changePassword}>
        <h2>Change password</h2>
        <div className="form-grid form-grid--password">
          <label>
            <span>Current password</span>
            <input
              type="password"
              autoComplete="current-password"
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
            {busy ? "Updating..." : "Change password"}
          </button>
        </div>
      </form>

      <section className="settings-section">
        <h2>Active sessions</h2>
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
