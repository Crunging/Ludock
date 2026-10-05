import { useEffect, useRef, useState, type FormEvent } from "react";
import { connectionSettingsSchema } from "@ludock/shared";
import { apiJson, jsonBody } from "../../api";
import { usePageRead } from "../../hooks/usePageRead";

const readSettings = (signal: AbortSignal) =>
  apiJson("/settings/connection", connectionSettingsSchema, { signal });

export default function ConnectionSettingsSection() {
  const page = usePageRead(readSettings, "Unable to load the server address.");
  return (
    <section
      className="settings-section settings-section--divided"
      aria-labelledby="connection-settings-title"
    >
      <h2 id="connection-settings-title" tabIndex={-1}>Server address</h2>
      {page.loading && <p className="muted" role="status">Loading server address…</p>}
      {page.error && (
        <div className="alert alert--error" role="alert">
          <p>{page.error}</p>
          <button className="secondary-btn" onClick={() => void page.refresh()}>
            Retry server address
          </button>
        </div>
      )}
      {page.data && <ConnectionSettingsForm initial={page.data.host} />}
    </section>
  );
}

function ConnectionSettingsForm({ initial }: { initial: string | null }) {
  const [host, setHost] = useState(initial ?? "");
  const mutation = useRef<AbortController | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  useEffect(() => () => mutation.current?.abort(), []);

  async function save(event: FormEvent) {
    event.preventDefault();
    if (mutation.current) return;
    const controller = new AbortController();
    mutation.current = controller;
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      const saved = await apiJson("/settings/connection", connectionSettingsSchema, {
        ...jsonBody("PUT", { host: host.trim() || null }),
        signal: controller.signal,
      });
      if (controller.signal.aborted) return;
      setHost((current) => (current === host ? saved.host ?? "" : current));
      setNotice(saved.host ? "Server address saved." : "Server address cleared.");
    } catch (reason) {
      if (!controller.signal.aborted)
        setError(reason instanceof Error ? reason.message : "Unable to save the server address.");
    } finally {
      mutation.current = null;
      if (!controller.signal.aborted) setBusy(false);
    }
  }

  return (
    <form className="stack-form" onSubmit={save}>
      <p className="section-lede">
        The name or IP address players use to reach this host. Ludock adds each
        server’s port and shows the result with a copy button.
      </p>
      <label className="narrow-field">
        Public address
        <input
          value={host}
          onChange={(event) => setHost(event.target.value)}
          placeholder={window.location.hostname}
          disabled={busy}
          maxLength={253}
          autoCapitalize="none"
          autoComplete="off"
          spellCheck={false}
          aria-describedby="connection-host-help"
        />
      </label>
      <p className="muted" id="connection-host-help">
        For example, play.example.com or 203.0.113.10. Leave it empty to use the
        address you opened Ludock with ({window.location.hostname}).
      </p>
      {error && <div className="alert alert--error" role="alert">{error}</div>}
      {notice && <div className="alert alert--success" role="status">{notice}</div>}
      <button className="primary-btn" disabled={busy}>
        {busy ? "Saving…" : "Save address"}
      </button>
    </form>
  );
}
