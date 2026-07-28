import { useCallback, useEffect, useState, type FormEvent } from "react";
import { apiFetch } from "../api";
import { useAuth, type UserRole } from "../auth-context";

interface UserSummary {
  id: string;
  username: string;
  role: UserRole;
  disabled: boolean;
  createdAt: number;
}

export default function Users() {
  const { user: currentUser } = useAuth();
  const [users, setUsers] = useState<UserSummary[]>([]);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<UserSummary["role"]>("operator");
  const [resetPasswords, setResetPasswords] = useState<Record<string, string>>(
    {}
  );
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const loadUsers = useCallback(async () => {
    const response = await apiFetch("/api/users");
    const body = (await response.json().catch(() => ({}))) as {
      users?: UserSummary[];
      error?: string;
    };
    if (!response.ok || !body.users) {
      throw new Error(body.error || "Failed to load users");
    }
    setUsers(body.users);
  }, []);

  useEffect(() => {
    loadUsers().catch((reason: unknown) => {
      setError(reason instanceof Error ? reason.message : "Failed to load users");
    });
  }, [loadUsers]);

  const createNewUser = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const response = await apiFetch("/api/users", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password, role }),
      });
      const body = (await response.json().catch(() => ({}))) as {
        error?: string;
      };
      if (!response.ok) throw new Error(body.error || "Failed to create user");
      setUsername("");
      setPassword("");
      await loadUsers();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "Failed to create user");
    } finally {
      setBusy(false);
    }
  };

  const updateAccess = async (
    user: UserSummary,
    changes: Partial<Pick<UserSummary, "role" | "disabled">>
  ) => {
    setError(null);
    try {
      const response = await apiFetch(`/api/users/${user.id}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          role: changes.role ?? user.role,
          disabled: changes.disabled ?? user.disabled,
        }),
      });
      const body = (await response.json().catch(() => ({}))) as {
        error?: string;
      };
      if (!response.ok) throw new Error(body.error || "Failed to update user");
      await loadUsers();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "Failed to update user");
    }
  };

  const resetPassword = async (user: UserSummary) => {
    const nextPassword = resetPasswords[user.id] || "";
    if (nextPassword.length < 12) {
      setError("Reset passwords must be at least 12 characters.");
      return;
    }

    setError(null);
    try {
      const response = await apiFetch(`/api/users/${user.id}/reset-password`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password: nextPassword }),
      });
      const body = (await response.json().catch(() => ({}))) as {
        error?: string;
      };
      if (!response.ok) throw new Error(body.error || "Failed to reset password");
      setResetPasswords((current) => ({ ...current, [user.id]: "" }));
    } catch (reason) {
      setError(
        reason instanceof Error ? reason.message : "Failed to reset password"
      );
    }
  };

  const removeUser = async (user: UserSummary) => {
    if (
      !window.confirm(
        `Delete ${user.username}? This revokes their sessions and cannot be undone.`
      )
    ) {
      return;
    }
    setError(null);
    const response = await apiFetch(`/api/users/${user.id}`, {
      method: "DELETE",
    });
    const body = (await response.json().catch(() => ({}))) as { error?: string };
    if (!response.ok) {
      setError(body.error || "Failed to delete user");
      return;
    }
    await loadUsers();
  };

  return (
    <div className="page">
      <div className="page__header">
        <h1 className="page__title">Users</h1>
        <p className="page__subtitle">
          Manage accounts and permissions for this panel.
        </p>
      </div>

      {error && (
        <div className="alert alert--error" role="alert">
          <span>{error}</span>
          <button onClick={() => setError(null)} aria-label="Dismiss error">
            ×
          </button>
        </div>
      )}

      <form className="settings-card" onSubmit={createNewUser}>
        <h2>Create user</h2>
        <div className="form-grid">
          <label>
            <span>Username</span>
            <input
              value={username}
              onChange={(event) => setUsername(event.target.value)}
              minLength={3}
              maxLength={32}
              required
            />
          </label>
          <label>
            <span>Temporary password</span>
            <input
              type="password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              minLength={12}
              maxLength={128}
              required
            />
          </label>
          <label>
            <span>Role</span>
            <select
              value={role}
              onChange={(event) =>
                setRole(event.target.value as UserSummary["role"])
              }
            >
              <option value="admin">Admin</option>
              <option value="operator">Operator</option>
              <option value="viewer">Viewer</option>
            </select>
          </label>
          <button className="primary-btn" disabled={busy}>
            {busy ? "Creating..." : "Create user"}
          </button>
        </div>
      </form>

      <div className="settings-list">
        {users.map((user) => (
          <section className="settings-card user-row" key={user.id}>
            <div className="user-row__identity">
              <strong>{user.username}</strong>
              <span>
                Created {new Date(user.createdAt).toLocaleDateString()}
                {user.disabled ? " · Disabled" : ""}
              </span>
            </div>
            <label>
              <span>Role</span>
              <select
                value={user.role}
                disabled={user.disabled}
                onChange={(event) =>
                  updateAccess(user, {
                    role: event.target.value as UserSummary["role"],
                  })
                }
              >
                <option value="admin">Admin</option>
                <option value="operator">Operator</option>
                <option value="viewer">Viewer</option>
              </select>
            </label>
            <button
              className="secondary-btn"
              onClick={() =>
                updateAccess(user, { disabled: !user.disabled })
              }
            >
              {user.disabled ? "Enable" : "Disable"}
            </button>
            <div className="password-reset">
              <input
                type="password"
                placeholder="New password"
                aria-label={`New password for ${user.username}`}
                value={resetPasswords[user.id] || ""}
                onChange={(event) =>
                  setResetPasswords((current) => ({
                    ...current,
                    [user.id]: event.target.value,
                  }))
                }
              />
              <button
                className="secondary-btn"
                onClick={() => resetPassword(user)}
                disabled={(resetPasswords[user.id] || "").length < 12}
              >
                Reset password
              </button>
            </div>
            <button
              className="secondary-btn secondary-btn--danger"
              onClick={() => removeUser(user)}
              disabled={user.id === currentUser?.id}
              title={
                user.id === currentUser?.id
                  ? "You cannot delete your own account"
                  : `Delete ${user.username}`
              }
            >
              Delete
            </button>
          </section>
        ))}
      </div>
    </div>
  );
}
