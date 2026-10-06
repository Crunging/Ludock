import { useEffect, useState } from "react";
import type { AvailabilityPolicy, AvailabilityState } from "@ludock/shared";
import { NavLink } from "../../navigation";

export interface AvailabilityDraft extends Omit<AvailabilityPolicy, "graceSeconds"> {
  graceSeconds: string;
}

interface Props {
  policy: AvailabilityPolicy;
  state: AvailabilityState | null;
  admin: boolean;
  monitoringPaused?: boolean;
  value: AvailabilityDraft;
  onChange: (value: AvailabilityDraft) => void;
  busy: boolean;
  onSave: () => void;
}

export default function AvailabilityPanel(props: Props) {
  const { policy, state, admin, monitoringPaused = false, value: availability, onChange, busy, onSave } = props;
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    const interval = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(interval);
  }, []);
  const suppressed = state !== null && state.suppressedUntil > now;
  const monitoring = policy.enabled && !policy.maintenance && !monitoringPaused && !state?.intentionallyStopped && !suppressed;
  const outageStartedAt = monitoring ? state?.outageStartedAt ?? null : null;
  let status = "No outage detected.";
  if (!state) status = "Availability status unavailable.";
  else if (!policy.enabled) status = "Monitoring disabled.";
  else if (policy.maintenance) status = "Monitoring paused for maintenance.";
  else if (monitoringPaused) status = "Monitoring paused while a server operation is active.";
  else if (state.intentionallyStopped) status = "Intentionally stopped. Outage alerts are paused until the server is running again.";
  else if (suppressed) status = "Waiting for monitoring to resume after a server action.";
  else if (outageStartedAt !== null) {
    status = now - outageStartedAt < policy.graceSeconds * 1000
      ? "Availability problem detected. Waiting for the failure grace period."
      : "Availability problem detected.";
  } else if (state.lastState === null) status = "Waiting for the first availability observation.";
  return (
    <section className="stack-form" aria-labelledby="availability-title">
      <h2 id="availability-title">Monitoring</h2>
      <p role="status">{status}</p>
      {state && policy.enabled && <dl className="metadata-list">
        {outageStartedAt !== null && <>
          <dt>Outage since</dt>
          <dd><time dateTime={new Date(outageStartedAt).toISOString()}>{new Date(outageStartedAt).toLocaleString()}</time></dd>
        </>}
        <dt>Last observed state</dt>
        <dd className="capitalize">{state.lastState?.replaceAll("_", " ") ?? "Not observed yet"}</dd>
        {suppressed && policy.enabled && !policy.maintenance && !monitoringPaused && !state.intentionallyStopped && <>
          <dt>Monitoring resumes</dt>
          <dd><time dateTime={new Date(state.suppressedUntil).toISOString()}>{new Date(state.suppressedUntil).toLocaleString()}</time></dd>
        </>}
      </dl>}
      {outageStartedAt !== null && <p className="section-note">
        {state?.lastState === "docker_unavailable"
          ? <>Ludock cannot reach Docker to verify this server. {admin && <NavLink className="text-link" to="/diagnostics">Check Docker connectivity in Diagnostics</NavLink>}</>
          : "Review the container’s state and health in its owning manager. A running container can still be unhealthy or starting."}
      </p>}
      {!admin && <p className="muted">Ask an administrator to investigate availability problems or change monitoring settings.</p>}
      {admin && <form
        className="stack-form"
        onSubmit={(event) => {
          event.preventDefault();
          onSave();
        }}
      >
        <p className="muted">
          Alert when this server is down. Ludock checks Docker’s running state
          and health check, and ignores stops made through Ludock.
        </p>
        <label className="check-label">
          <input
            type="checkbox"
            disabled={busy}
            checked={availability.enabled}
            onChange={(event) =>
              onChange({
                ...availability,
                enabled: event.target.checked,
              })
            }
          />
          Monitor this server
        </label>
        {availability.enabled && <>
        <label className="narrow-field">
          Failure grace period (seconds)
          <input
            type="number"
            disabled={busy}
            min={10}
            max={86400}
            inputMode="numeric"
            aria-describedby="monitoring-grace-help"
            required
            value={availability.graceSeconds}
            onChange={(event) =>
              onChange({
                ...availability,
                graceSeconds: event.target.value,
              })
            }
          />
        </label>
        <p className="muted" id="monitoring-grace-help">
          How long to wait before reporting an outage, so quick restarts don’t
          alert. Default 120 seconds.
        </p>
        <label className="check-label">
          <input
            type="checkbox"
            disabled={busy}
            checked={availability.maintenance}
            onChange={(event) =>
              onChange({
                ...availability,
                maintenance: event.target.checked,
              })
            }
          />
          Maintenance mode
        </label>
        <p className="muted check-help">
          Pauses alerts until you turn it off. The server and its schedules keep running.
        </p>
        </>}
        <p className="muted">
          Alerts go to Discord.{" "}
          <NavLink className="text-link" to="/settings">
            Set up Discord in Settings
          </NavLink>
        </p>
        <button className="primary-btn" disabled={busy}>
          Save monitoring
        </button>
      </form>}
    </section>
  );
}
