import { useCallback, useEffect, useState, type FormEvent } from "react";
import { apiFetch } from "../api";

interface SessionSummary {
  id: string;
  lastSeenAt: number;
  ipAddress: string | null;
  userAgent: string | null;
  current: boolean;
}

export default function Account() {
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const loadSessions = useCallback(async () => {
    const response = await apiFetch("/api/v1/account/sessions");
    const body = (await response.json().catch(() => ({}))) as {
      sessions?: SessionSummary[];
      error?: string;
    };
    if (!response.ok || !body.sessions) {
      throw new Error(body.error || "Failed to load sessions");
    }
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
      const response = await apiFetch("/api/v1/account/change-password", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ currentPassword, newPassword }),
      });
      const body = (await response.json().catch(() => ({}))) as {
        error?: string;
      };
      if (!response.ok)
        throw new Error(body.error || "Failed to change password");
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
    const response = await apiFetch(`/api/v1/account/sessions/${session.id}`, {
      method: "DELETE",
    });
    const body = (await response.json().catch(() => ({}))) as {
      error?: string;
    };
    if (!response.ok) {
      setError(body.error || "Failed to revoke session");
      return;
    }
    if (session.current) {
      window.location.reload();
      return;
    }
    await loadSessions();
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
              minLength={15}
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
              minLength={15}
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
