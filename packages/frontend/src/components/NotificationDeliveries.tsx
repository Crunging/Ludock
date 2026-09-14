import { useCallback, useEffect, useRef, useState } from "react";
import {
  notificationDeliveriesResponseSchema,
  notificationDeliveryResponseSchema,
  type NotificationDelivery,
} from "@ludock/shared";
import { apiJson } from "../api";

function deliveryStatus(delivery: NotificationDelivery, enabled: boolean) {
  if (delivery.state === "delivered") return "Delivered";
  if (delivery.state === "failed") return "Failed";
  if (!enabled) return "Paused";
  return delivery.attempts > 0 ? "Pending retry" : "Queued";
}

function timestamp(value: number) {
  return <time dateTime={new Date(value).toISOString()}>{new Date(value).toLocaleString()}</time>;
}

export default function NotificationDeliveries({
  configured,
  enabled,
  unsavedChanges,
  saving,
}: {
  configured: boolean;
  enabled: boolean;
  unsavedChanges: boolean;
  saving: boolean;
}) {
  const [deliveries, setDeliveries] = useState<NotificationDelivery[] | null>(null);
  const [refreshing, setRefreshing] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [pendingAction, setPendingAction] = useState<string | null>(null);
  const readRequest = useRef<AbortController | null>(null);
  const actionRequest = useRef<AbortController | null>(null);
  const canSend = configured && enabled && !unsavedChanges && !saving;

  const refresh = useCallback(async () => {
    if (actionRequest.current) return;
    readRequest.current?.abort();
    const controller = new AbortController();
    readRequest.current = controller;
    const ownsRequest = () => readRequest.current === controller && !controller.signal.aborted;
    setRefreshing(true);
    setLoadError(null);
    try {
      const response = await apiJson("/notifications/deliveries", notificationDeliveriesResponseSchema, {
        signal: controller.signal,
      });
      if (ownsRequest()) setDeliveries(response.deliveries);
    } catch (reason) {
      if (ownsRequest()) setLoadError(reason instanceof Error ? reason.message : "Unable to load notification deliveries.");
    } finally {
      if (ownsRequest()) setRefreshing(false);
      if (readRequest.current === controller) readRequest.current = null;
    }
  }, []);

  useEffect(() => () => {
    readRequest.current?.abort();
    readRequest.current = null;
    actionRequest.current?.abort();
    actionRequest.current = null;
  }, []);

  useEffect(() => {
    void refresh();
  }, [configured, enabled, refresh]);

  const hasQueued = deliveries?.some((delivery) => delivery.state === "queued") ?? false;
  useEffect(() => {
    if (!hasQueued || !enabled || !configured) return;
    const timer = window.setInterval(() => {
      if (!readRequest.current) void refresh();
    }, 5_000);
    return () => window.clearInterval(timer);
  }, [hasQueued, enabled, configured, refresh]);

  async function queue(delivery?: NotificationDelivery) {
    if (!canSend || actionRequest.current || (delivery && !delivery.retryable)) return;
    // A history read started before this mutation must not replace its result.
    readRequest.current?.abort();
    readRequest.current = null;
    setRefreshing(false);
    const controller = new AbortController();
    actionRequest.current = controller;
    setPendingAction(delivery?.id ?? "test");
    setActionError(null);
    setNotice(null);
    try {
      const response = await apiJson(
        delivery ? `/notifications/deliveries/${delivery.id}/retry` : "/notifications/test",
        notificationDeliveryResponseSchema,
        { method: "POST", signal: controller.signal },
      );
      if (controller.signal.aborted) return;
      setDeliveries((current) => [
        response.delivery,
        ...(current ?? []).filter((item) => item.id !== response.delivery.id),
      ].sort((left, right) => right.createdAt - left.createdAt || right.id.localeCompare(left.id)).slice(0, 50));
      setNotice(delivery
        ? "Notification queued for retry. Delivery status will update below."
        : "Test notification queued. Delivery status will update below.");
    } catch (reason) {
      if (!controller.signal.aborted)
        setActionError(reason instanceof Error ? reason.message : "Unable to queue notification.");
    } finally {
      if (actionRequest.current === controller) {
        actionRequest.current = null;
        setPendingAction(null);
      }
    }
  }

  return (
    <div className="notification-deliveries">
      <div className="notification-deliveries__test">
        <button
          type="button"
          className="secondary-btn"
          disabled={!canSend || pendingAction !== null}
          aria-describedby="notification-test-help"
          onClick={() => { void queue(); }}
        >
          {pendingAction === "test" ? "Queuing test…" : "Send test notification"}
        </button>
        <p className="muted" id="notification-test-help">
          {unsavedChanges
            ? "Save notification changes before sending a test or retrying a delivery."
            : !configured || !enabled
              ? "Save a webhook URL and enable Discord delivery before sending a test or retrying a delivery."
              : "Send a test to the saved Discord webhook. Delivery may take a few seconds."}
        </p>
      </div>
      {actionError && <p className="alert alert--error" role="alert">{actionError}</p>}
      {notice && <p className="alert alert--success" role="status">{notice}</p>}
      <div className="section-heading notification-deliveries__heading">
        <h3>Recent deliveries</h3>
        <button
          type="button"
          className="secondary-btn"
          disabled={refreshing || pendingAction !== null}
          onClick={() => { void refresh(); }}
        >Refresh deliveries</button>
      </div>
      <p className="muted notification-deliveries__help">
        The latest 50 notifications. {enabled
          ? "Pending deliveries refresh every five seconds."
          : "Discord delivery is disabled. Queued notifications resume when delivery is enabled."}{" "}
        Failed deliveries can be retried after fixing and saving the configuration.
      </p>
      {loadError && <p className="alert alert--error" role="alert">{loadError}{deliveries && " Showing the last loaded deliveries."}</p>}
      {deliveries === null ? (
        refreshing && <p className="muted" role="status">Loading notification deliveries…</p>
      ) : (
        <div className="table-scroll">
          {/* Keep table semantics when the mobile layout changes display. */}
          <table className="data-table notification-deliveries-table" aria-label="Recent notification deliveries" role="table">
            <thead role="rowgroup"><tr role="row"><th scope="col" role="columnheader">Notification</th><th scope="col" role="columnheader">Delivery</th><th scope="col" role="columnheader">Action</th></tr></thead>
            <tbody role="rowgroup">
              {deliveries.length === 0 && <tr role="row"><td role="cell" colSpan={3} className="muted">No notifications have been queued yet. Send a test to check delivery.</td></tr>}
              {deliveries.map((delivery) => (
                <tr key={delivery.id} role="row">
                  <td role="cell">
                    {delivery.kind === "test" ? "Test notification" : "Server notification"}
                    <span className="table-detail">Queued {timestamp(delivery.createdAt)}</span>
                  </td>
                  <td role="cell">
                    <strong className={`notification-delivery-status notification-delivery-status--${delivery.state}`}>{deliveryStatus(delivery, enabled)}</strong>
                    <span className="table-detail">{delivery.attempts} {delivery.attempts === 1 ? "attempt" : "attempts"}</span>
                    {delivery.deliveredAt !== null && <span className="table-detail">Delivered {timestamp(delivery.deliveredAt)}</span>}
                    {delivery.lastAttemptAt !== null && delivery.deliveredAt === null && <span className="table-detail">Last attempt {timestamp(delivery.lastAttemptAt)}</span>}
                    {delivery.nextAttemptAt !== null && enabled && <span className="table-detail">{delivery.attempts > 0 ? "Next retry" : "Next attempt"} {timestamp(delivery.nextAttemptAt)}</span>}
                    {delivery.lastFailure && <p className="notification-delivery-failure">{delivery.lastFailure}</p>}
                  </td>
                  <td role="cell">
                    {delivery.retryable && (
                      <button
                        type="button"
                        className="secondary-btn"
                        disabled={!canSend || pendingAction !== null}
                        aria-label={`Retry notification from ${new Date(delivery.createdAt).toLocaleString()}`}
                        aria-describedby="notification-test-help"
                        onClick={() => { void queue(delivery); }}
                      >{pendingAction === delivery.id ? "Queuing…" : "Retry"}</button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
