import { type AttentionItem, type NextRunUnavailableReason } from "@ludock/shared";
import { useEffect, useRef, useState } from "react";
import { useAuth } from "../auth-context";
import { useAttention } from "../hooks/useAttention";
import { NavLink } from "../navigation";

const scheduleReasons: Record<NextRunUnavailableReason, string> = {
  owner_missing: "The schedule owner no longer exists.",
  owner_disabled: "The schedule owner’s account is disabled.",
  owner_access_removed: "The owner no longer has schedule access.",
  action_access_removed: "The owner no longer has access to this action.",
  binding_changed: "Recreate the schedule after the server binding is reviewed.",
  binding_unavailable: "The server binding needs administrator attention.",
  unavailable: "Review the schedule settings and server state.",
};
const capitalize = (value: string) => value.charAt(0).toUpperCase() + value.slice(1).replaceAll("_", " ");

function describe(item: AttentionItem): { title: string; detail: string; link: string; action: string } {
  const path = `/servers/${encodeURIComponent(item.serverId)}`;
  switch (item.kind) {
    case "operation":
      return {
        title: `${capitalize(item.operationKind)} ${item.status}`,
        detail: new Date(item.updatedAt).toLocaleString(),
        link: `${path}?tab=activity&operation=${encodeURIComponent(item.operationId)}`,
        action: "Review activity",
      };
    case "schedule":
      return {
        title: `${capitalize(item.action)} schedule suspended`,
        detail: scheduleReasons[item.reason],
        link: `${path}?tab=schedules&schedule=${encodeURIComponent(item.scheduleId)}`,
        action: "Review schedule",
      };
    case "binding":
      return {
        title: item.bindingStatus === "review_required" ? "Binding review required"
          : item.bindingStatus === "ambiguous" ? "Ambiguous server binding" : "Server container missing",
        detail: item.bindingStatus === "review_required" ? "Review the changed server identity before resuming controls."
          : "Check the container in Docker or its owning manager, then refresh.",
        link: path,
        action: "Review server",
      };
    case "availability":
      return {
        title: item.state === "docker_unavailable" ? "Availability cannot be verified" : "Server unavailable",
        detail: item.state === "docker_unavailable" ? "Ludock cannot reach Docker."
          : `Monitoring detected a problem since ${new Date(item.outageStartedAt).toLocaleString()}.`,
        link: `${path}?tab=availability`,
        action: "Check availability",
      };
  }
}

/** The caller keys this component by account and unmounts it on access denial. */
export default function NeedsAttention({ refreshKey }: { refreshKey: string }) {
  const { user } = useAuth();
  const { data, loading, error, refresh } = useAttention();
  const previousRefreshKey = useRef(refreshKey);
  const [expanded, setExpanded] = useState(false);
  useEffect(() => {
    if (previousRefreshKey.current === refreshKey) return;
    previousRefreshKey.current = refreshKey;
    void refresh();
  }, [refreshKey, refresh]);
  // The API applies current server grants and schedule ownership. Keep role
  // ceilings here too, so a malformed response cannot offer an admin workflow.
  const items = (data?.items ?? []).filter((item) =>
    item.kind === "binding" ? user?.role === "admin"
      : item.kind === "schedule" ? user?.role === "admin" || user?.role === "operator"
        : Boolean(user),
  );
  const visible = expanded ? items : items.slice(0, 10);

  return (
    <section className="needs-attention" aria-labelledby="needs-attention-title" aria-busy={loading}>
      <div className="needs-attention__heading">
        <h2 id="needs-attention-title">Needs attention</h2>
        {data && items.length > 0 && <span className="muted">{items.length} {items.length === 1 ? "item" : "items"}</span>}
        {data && loading && <span className="muted" aria-live="polite">Checking…</span>}
      </div>
      {loading && !data && <p className="muted" aria-live="polite">Checking for issues…</p>}
      {error && (
        <div className="needs-attention__error" role="alert">
          <p>Unable to load attention items: {error}</p>
          <button className="secondary-btn" onClick={() => void refresh()}>Retry attention check</button>
        </div>
      )}
      {data?.discoveryUnavailable && (
        <p className="needs-attention__warning">
          Docker is unavailable. These items use saved state; current server availability cannot be verified.
          {user?.role === "admin" && <> <NavLink to="/diagnostics" className="text-link">Check diagnostics</NavLink></>}
        </p>
      )}
      {data && items.length === 0 && (
        <p className="muted">{data.discoveryUnavailable
          ? "No attention items in the saved state you can access."
          : "No issues need attention in the servers and schedules you can access."}</p>
      )}
      {visible.length > 0 && (
        <ul className="needs-attention__list">
          {visible.map((item) => {
            const info = describe(item);
            return (
              <li key={item.id}>
                <div className="needs-attention__description">
                  <span className="needs-attention__server">{item.serverName}</span>
                  <strong>{info.title}</strong>
                  <span className="muted">{info.detail}</span>
                </div>
                <NavLink to={info.link} className="text-link needs-attention__action"
                  aria-label={`${info.action}: ${item.serverName} — ${info.title}`}>
                  {info.action}<span aria-hidden="true"> →</span>
                </NavLink>
              </li>
            );
          })}
        </ul>
      )}
      {items.length > 10 && (
        <button className="secondary-btn needs-attention__expand" aria-expanded={expanded}
          onClick={() => setExpanded((current) => !current)}>
          {expanded ? "Show fewer items" : `Show all ${items.length} items`}
        </button>
      )}
      {items.some((item) => item.kind === "operation") && <p className="muted needs-attention__note">Includes failures from each server’s most recent 100 operations.</p>}
    </section>
  );
}
