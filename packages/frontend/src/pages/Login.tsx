import { useRef, useState, type FormEvent } from "react";
import { PASSWORD_MIN_LENGTH } from "@ludock/shared";
import { useAuth } from "../auth-context";
import LudockMark from "../components/LudockMark";
import "./login.css";

export default function Login() {
  const { login, setup, setupRequired, setupLocked, refreshStatus } = useAuth();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [showPassword, setShowPassword] = useState(false);
  const [confirmationError, setConfirmationError] = useState(false);
  const confirmationInput = useRef<HTMLInputElement>(null);
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
      setConfirmationError(true);
      confirmationInput.current?.focus();
      return;
    }

    setSubmitting(true);
    setError(null);
    try {
      const result = setupRequired
        ? await setup(username.trim(), password)
        : await login(username.trim(), password);
      setError(result);
    } catch {
      setError("Unable to reach Ludock.");
    } finally {
      setSubmitting(false);
    }
  };

  if (setupRequired && setupLocked) {
    return (
      <main className="login-page">
        <section className="login-card" aria-labelledby="setup-expired-title">
          <LudockMark className="sidebar__logo-icon login-card__logo" />
          <h1 className="login-card__title" id="setup-expired-title">
            Setup window expired
          </h1>
          <p className="login-card__description">
            Restart the Ludock container, then return here within five
            minutes to create the administrator account.
          </p>
          <button
            className="login-card__submit"
            type="button"
            disabled={submitting}
            onClick={async () => {
              setSubmitting(true);
              try {
                await refreshStatus();
              } finally {
                setSubmitting(false);
              }
            }}
          >
            {submitting ? "Checking…" : "Check again"}
          </button>
        </section>
      </main>
    );
  }

  return (
    <main className="login-page">
      <form className="login-card" onSubmit={handleSubmit}>
        <LudockMark className="sidebar__logo-icon login-card__logo" />
        <h1 className="login-card__title">
          {setupRequired ? "Set up Ludock" : "Sign in to Ludock"}
        </h1>
        <p className="login-card__description">
          {setupRequired
            ? "Create your administrator account to manage existing game servers. Setup stays open for five minutes after Ludock starts."
            : "Sign in with your Ludock account."}
        </p>
        <label className="login-card__label" htmlFor="username">
          Username
        </label>
        <input
          id="username"
          className="login-card__input"
          value={username}
          onChange={(event) => setUsername(event.target.value)}
          autoComplete="username"
          autoCapitalize="none"
          spellCheck={false}
          minLength={3}
          maxLength={32}
          pattern={setupRequired ? "[a-zA-Z0-9._\\-]+" : undefined}
          aria-describedby={setupRequired ? "username-hint" : undefined}
          required
          autoFocus={!setupRequired}
        />
        {setupRequired && (
          <p className="login-card__hint" id="username-hint">
            3–32 characters: letters, numbers, periods, hyphens, or underscores.
          </p>
        )}
        <label className="login-card__label" htmlFor="password">
          Password
        </label>
        <input
          id="password"
          className="login-card__input"
          type={showPassword ? "text" : "password"}
          value={password}
          onChange={(event) => {
            setPassword(event.target.value);
            if (confirmationError) {
              setConfirmationError(false);
              setError(null);
            }
          }}
          autoComplete={setupRequired ? "new-password" : "current-password"}
          minLength={PASSWORD_MIN_LENGTH}
          maxLength={128}
          aria-describedby={setupRequired ? "password-hint" : undefined}
          required
        />
        {setupRequired && (
          <p className="login-card__hint" id="password-hint">
            Use at least {PASSWORD_MIN_LENGTH} characters. A few words work well.
          </p>
        )}
        {setupRequired && (
          <>
            <label className="login-card__label" htmlFor="confirm-password">
              Confirm password
            </label>
            <input
              id="confirm-password"
              ref={confirmationInput}
              className="login-card__input"
              type={showPassword ? "text" : "password"}
              value={confirmation}
              onChange={(event) => {
                setConfirmation(event.target.value);
                if (confirmationError) {
                  setConfirmationError(false);
                  setError(null);
                }
              }}
              autoComplete="new-password"
              minLength={PASSWORD_MIN_LENGTH}
              maxLength={128}
              aria-invalid={confirmationError || undefined}
              aria-describedby={confirmationError ? "login-error" : undefined}
              required
            />
          </>
        )}
        <label className="login-card__visibility">
          <input
            type="checkbox"
            checked={showPassword}
            onChange={(event) => setShowPassword(event.target.checked)}
          />
          {setupRequired ? "Show passwords" : "Show password"}
        </label>
        {error && (
          <div className="login-card__error" id="login-error" role="alert">
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
            (setupRequired && !confirmation)
          }
        >
          {submitLabel}
        </button>
        {!setupRequired && (
          <p className="login-card__account-help">
            Need an account or a password reset? Ask the person who runs this Ludock panel.
          </p>
        )}
      </form>
    </main>
  );
}
