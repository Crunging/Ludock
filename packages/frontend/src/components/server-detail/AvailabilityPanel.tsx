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
      <h2 id="availability-title">Availability monitoring</h2>
      <p role="status">{status}</p>
      {state && <dl className="metadata-list">
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
        <p>
          When enabled, this server is expected to be available 24/7. Ludock uses
          Docker health and running state. Stops initiated
          by Ludock and active operations suppress outage alerts.
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
        <label>
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
          Wait this long before reporting an outage. The default is 120 seconds
          (2 minutes), allowing brief restarts to finish. Use 10–86,400 seconds.
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
          Maintenance mode — pause monitoring
        </label>
        <p className="muted">
          Maintenance pauses alerts until you turn it off and save again; it
          does not stop the server or its schedules.
        </p>
        <p className="muted">
          To receive one notification for an outage and one for recovery,{" "}
          <NavLink className="text-link" to="/settings">configure Discord delivery in Settings</NavLink>.
        </p>
        <button className="primary-btn" disabled={busy}>
          Save monitoring
        </button>
      </form>}
    </section>
  );
}
