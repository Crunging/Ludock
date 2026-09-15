import { useEffect, useRef, useState, type FormEvent } from "react";
import { notificationSettingsResponseSchema } from "@ludock/shared";
import { apiJson, jsonBody } from "../../api";
import { usePageRead } from "../../hooks/usePageRead";
import NotificationDeliveries from "../NotificationDeliveries";

const readSettings = (signal: AbortSignal) =>
  apiJson("/notifications", notificationSettingsResponseSchema, { signal });

export default function NotificationSettingsSection() {
  const page = usePageRead(
    readSettings,
    "Unable to load notification settings.",
  );
  return (
    <section
      className="settings-section settings-section--divided"
      aria-labelledby="notification-settings-title"
    >
      <h2 id="notification-settings-title">Discord notifications</h2>
      {page.loading && (
        <p className="muted" role="status">
          Loading notification settings…
        </p>
      )}
      {page.error && (
        <div className="alert alert--error" role="alert">
          <p>{page.error}</p>
          <button className="secondary-btn" onClick={() => void page.refresh()}>
            Retry notification settings
          </button>
        </div>
      )}
      {page.data && <NotificationSettingsForm initial={page.data} />}
    </section>
  );
}

function NotificationSettingsForm({
  initial,
}: {
  initial: { configured: boolean; enabled: boolean };
}) {
  const [webhookUrl, setWebhookUrl] = useState("");
  const [notificationConfigured, setNotificationConfigured] = useState(
    initial.configured,
  );
  const [notificationEnabled, setNotificationEnabled] = useState(
    initial.enabled,
  );
  const [savedNotificationEnabled, setSavedNotificationEnabled] = useState(
    initial.enabled,
  );
  const mutation = useRef<AbortController | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState(false);
  useEffect(() => () => mutation.current?.abort(), []);

  async function saveNotifications(event: FormEvent) {
    event.preventDefault();
    if (mutation.current) return;
    const controller = new AbortController();
    mutation.current = controller;
    setBusy(true);
    setError(null);
    setNotice(false);
    try {
      const response = await apiJson(
        "/notifications",
        notificationSettingsResponseSchema,
        {
          ...jsonBody("PUT", {
            enabled: notificationEnabled,
            ...(webhookUrl.trim() ? { webhookUrl: webhookUrl.trim() } : {}),
          }),
          signal: controller.signal,
        },
      );
      if (controller.signal.aborted) return;
      setNotificationConfigured(response.configured);
      setSavedNotificationEnabled(response.enabled);
      setNotificationEnabled((current) =>
        current === notificationEnabled ? response.enabled : current,
      );
      setWebhookUrl((current) => (current === webhookUrl ? "" : current));
      setNotice(true);
    } catch (reason) {
      if (!controller.signal.aborted)
        setError(
          reason instanceof Error
            ? reason.message
            : "Unable to save notification settings.",
        );
    } finally {
      mutation.current = null;
      if (!controller.signal.aborted) setBusy(false);
    }
  }
  return (
    <>
      <form className="stack-form" onSubmit={saveNotifications}>
        <p>
          Receive outage and recovery alerts, backup or schedule failures, and
          restore and update results. Enable availability monitoring separately
          for each server.
        </p>
        <label className="check-label">
          <input
            type="checkbox"
            checked={notificationEnabled}
            onChange={(event) => setNotificationEnabled(event.target.checked)}
          />
          Enable Discord delivery
        </label>
        <label>
          {notificationConfigured ? "Replace webhook URL" : "Webhook URL"}
          <input
            type="password"
            value={webhookUrl}
            onChange={(event) => setWebhookUrl(event.target.value)}
            autoComplete="new-password"
            placeholder={
              notificationConfigured
                ? "Saved — leave blank to keep existing URL"
                : "https://discord.com/api/webhooks/…"
            }
          />
        </label>
        <p className="muted">
          The saved webhook is write-only. It is never returned to the browser.
        </p>
        {error && (
          <div className="alert alert--error" role="alert">
            {error}
          </div>
        )}
        {notice && (
          <div className="alert alert--success" role="status">
            Notification settings saved.
          </div>
        )}
        <button
          className="primary-btn"
          disabled={
            busy ||
            (notificationEnabled &&
              !notificationConfigured &&
              !webhookUrl.trim())
          }
        >
          Save notifications
        </button>
      </form>
      <NotificationDeliveries
        configured={notificationConfigured}
        enabled={savedNotificationEnabled}
        unsavedChanges={
          Boolean(webhookUrl.trim()) ||
          notificationEnabled !== savedNotificationEnabled
        }
        saving={busy}
      />
    </>
  );
}
