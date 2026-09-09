import { useEffect, useRef, useState, type FormEvent } from "react";
import { apiJson, jsonBody } from "../api";
import ServerGrants from "../components/ServerGrants";
import { useAuth } from "../auth-context";

import {
  type UserSummary,
  type CreateUserRequest,
  type UserAccessRequest,
  type ResetPasswordRequest,
  PASSWORD_MIN_LENGTH,
  usersResponseSchema,
  userResponseSchema,
  okResponseSchema,
} from "@ludock/shared";

export default function Users() {
  const { user: currentUser } = useAuth();
  const [editingGrants, setEditingGrants] = useState<string | null>(null);
  const [users, setUsers] = useState<UserSummary[]>([]);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<UserSummary["role"]>("operator");
  const [resetPasswords, setResetPasswords] = useState<Record<string, string>>(
    {},
  );
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const [loadState, setLoadState] = useState<"loading" | "ready" | "error">(
    "loading",
  );
  const [loadAttempt, setLoadAttempt] = useState(0);
  const active = useRef(false);
  const mutationPending = useRef(false);

  useEffect(() => {
    const controller = new AbortController();
    active.current = true;
    setLoadState("loading");
    setError(null);
    apiJson("/users", usersResponseSchema, { signal: controller.signal })
      .then((body) => {
        if (controller.signal.aborted) return;
        setUsers(body.users);
        setLoadState("ready");
      })
      .catch((reason: unknown) => {
        if (controller.signal.aborted) return;
        setError(reason instanceof Error ? reason.message : "Failed to load users");
        setLoadState("error");
      });
    return () => {
      active.current = false;
      controller.abort();
    };
  }, [loadAttempt]);

  async function mutate<T,>(action: () => Promise<T>, apply: (result: T) => void) {
    if (mutationPending.current || loadState !== "ready") return;
    mutationPending.current = true;
    setBusy(true);
    setError(null);
    try {
      const result = await action();
      if (active.current) apply(result);
    } catch (reason) {
      if (active.current)
        setError(
          reason instanceof Error ? reason.message : "Unable to save user changes.",
        );
    } finally {
      mutationPending.current = false;
      if (active.current) setBusy(false);
    }
  }

  const createNewUser = async (event: FormEvent) => {
    event.preventDefault();
    await mutate(
      () => apiJson("/users", userResponseSchema, jsonBody("POST", {
        username,
        password,
        role,
      } satisfies CreateUserRequest)),
      ({ user }) => {
        setUsers((current) => [...current, user]);
        setUsername((current) => current === username ? "" : current);
        setPassword((current) => current === password ? "" : current);
      },
    );
  };

  const updateAccess = async (
    user: UserSummary,
    changes: Partial<UserAccessRequest>,
  ) => {
    await mutate(
      () => apiJson(
        `/users/${encodeURIComponent(user.id)}`,
        userResponseSchema,
        jsonBody("PATCH", {
          role: changes.role ?? user.role,
          disabled: changes.disabled ?? user.disabled,
        } satisfies UserAccessRequest),
      ),
      (response) => {
        setUsers((current) => current.map((item) =>
          item.id === response.user.id ? response.user : item,
        ));
      },
    );
  };

  const resetPassword = async (user: UserSummary) => {
    const nextPassword = resetPasswords[user.id] || "";
    if (nextPassword.length < PASSWORD_MIN_LENGTH) {
      setError(
        `Reset passwords must be at least ${PASSWORD_MIN_LENGTH} characters.`,
      );
      return;
    }
    await mutate(
      () => apiJson(
        `/users/${encodeURIComponent(user.id)}/reset-password`,
        okResponseSchema,
        jsonBody("POST", { password: nextPassword } satisfies ResetPasswordRequest),
      ),
      () => {
        setResetPasswords((current) =>
          current[user.id] === nextPassword ? { ...current, [user.id]: "" } : current,
        );
      },
    );
  };

  const removeUser = async (user: UserSummary) => {
    if (
      mutationPending.current ||
      user.id === currentUser?.id ||
      !window.confirm(
        `Delete ${user.username}? This revokes their sessions and cannot be undone.`,
      )
    ) return;
    await mutate(
      () => apiJson(`/users/${encodeURIComponent(user.id)}`, okResponseSchema, {
        method: "DELETE",
      }),
      () => {
        setUsers((current) => current.filter((item) => item.id !== user.id));
        setResetPasswords((current) => {
          const next = { ...current };
          delete next[user.id];
          return next;
        });
      },
    );
  };

  return (
    <div className="page">
      <div className="page__header">
        <h1 className="page__title">Users</h1>
        <p className="page__subtitle">
          Create accounts, then share specific servers and actions. New
          operators and viewers have no server access.
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
              minLength={PASSWORD_MIN_LENGTH}
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
          <button
            className="primary-btn"
            disabled={busy || loadState !== "ready"}
          >
            Create user
          </button>
        </div>
      </form>

      {loadState === "loading" && <p role="status">Loading users…</p>}
      {loadState === "error" && (
        <button
          className="secondary-btn"
          onClick={() => setLoadAttempt((value) => value + 1)}
        >
          Try again
        </button>
      )}
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
                disabled={busy || user.disabled}
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
              disabled={busy}
              onClick={() => updateAccess(user, { disabled: !user.disabled })}
            >
              {user.disabled ? "Enable" : "Disable"}
            </button>
            {user.role !== "admin" && (
              <button
                className="secondary-btn"
                onClick={() =>
                  setEditingGrants(editingGrants === user.id ? null : user.id)
                }
                disabled={busy}
                aria-expanded={editingGrants === user.id}
              >
                Server access
              </button>
            )}
            {user.role === "admin" && (
              <span className="muted user-access-summary">All servers</span>
            )}
            <div className="password-reset">
              <input
                type="password"
                placeholder="New password"
                aria-label={`New password for ${user.username}`}
                autoComplete="new-password"
                minLength={PASSWORD_MIN_LENGTH}
                maxLength={128}
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
                disabled={
                  busy || (resetPasswords[user.id] || "").length < PASSWORD_MIN_LENGTH
                }
              >
                Reset password
              </button>
            </div>
            <button
              className="secondary-btn secondary-btn--danger"
              onClick={() => removeUser(user)}
              disabled={busy || user.id === currentUser?.id}
              title={
                user.id === currentUser?.id
                  ? "You cannot delete your own account"
                  : `Delete ${user.username}`
              }
            >
              Delete
            </button>
            {editingGrants === user.id && user.role !== "admin" && (
              <ServerGrants
                key={`${user.id}-${user.role}`}
                userId={user.id}
                username={user.username}
                role={user.role}
                onClose={() => setEditingGrants(null)}
              />
            )}
          </section>
        ))}
      </div>
    </div>
  );
}
