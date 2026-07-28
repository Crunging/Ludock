import { useState, type FormEvent } from "react";
import { useAuth } from "../auth-context";

export default function Login() {
  const { login, setup, setupRequired, setupTokenRequired } = useAuth();
  const [setupToken, setSetupToken] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const submitLabel = submitting
    ? setupRequired
      ? "Creating administrator..."
      : "Signing in..."
    : setupRequired
      ? "Create administrator"
      : "Sign in";

  const handleSubmit = async (event: FormEvent) => {
    event.preventDefault();
    if (!username.trim() || !password) return;
    if (setupRequired && password !== confirmation) {
      setError("Passwords do not match.");
      return;
    }

    setSubmitting(true);
    setError(null);
    try {
      const result = setupRequired
        ? await setup(
            username.trim(),
            password,
            setupTokenRequired ? setupToken.trim() : undefined
          )
        : await login(username.trim(), password);
      setError(result);
    } catch {
      setError("Unable to reach the game panel.");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <main className="login-page">
      <form className="login-card" onSubmit={handleSubmit}>
        <div className="sidebar__logo-icon login-card__logo">GP</div>
        <h1 className="login-card__title">
          {setupRequired ? "Set up Game Panel" : "Sign in to Game Panel"}
        </h1>
        <p className="login-card__description">
          {setupRequired
            ? "Create the administrator account you will use to manage this panel."
            : "Sign in with your Game Panel account."}
        </p>
        {setupRequired && setupTokenRequired && (
          <>
            <label className="login-card__label" htmlFor="setup-token">
              Setup code
            </label>
            <input
              id="setup-token"
              className="login-card__input"
              type="password"
              value={setupToken}
              onChange={(event) => setSetupToken(event.target.value)}
              autoComplete="off"
              required
            />
          </>
        )}
        <label className="login-card__label" htmlFor="username">
          Username
        </label>
        <input
          id="username"
          className="login-card__input"
          value={username}
          onChange={(event) => setUsername(event.target.value)}
          autoComplete="username"
          minLength={3}
          maxLength={32}
          required
          autoFocus={!setupRequired}
        />
        <label className="login-card__label" htmlFor="password">
          Password
        </label>
        <input
          id="password"
          className="login-card__input"
          type="password"
          value={password}
          onChange={(event) => setPassword(event.target.value)}
          autoComplete={setupRequired ? "new-password" : "current-password"}
          minLength={12}
          maxLength={128}
          required
        />
        {setupRequired && (
          <div className="login-card__hint">Use at least 12 characters.</div>
        )}
        {setupRequired && (
          <>
            <label className="login-card__label" htmlFor="confirm-password">
              Confirm password
            </label>
            <input
              id="confirm-password"
              className="login-card__input"
              type="password"
              value={confirmation}
              onChange={(event) => setConfirmation(event.target.value)}
              autoComplete="new-password"
              minLength={12}
              maxLength={128}
              required
            />
          </>
        )}
        {error && (
          <div className="login-card__error" role="alert">
            {error}
          </div>
        )}
        <button
          className="login-card__submit"
          type="submit"
          disabled={
            submitting ||
            !username.trim() ||
            !password ||
            (setupRequired && !confirmation) ||
            (setupTokenRequired && !setupToken.trim())
          }
        >
          {submitLabel}
        </button>
      </form>
    </main>
  );
}
