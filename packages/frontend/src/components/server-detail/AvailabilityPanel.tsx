import type { AvailabilityPolicy } from "@ludock/shared";

interface Props {
  value: AvailabilityPolicy;
  onChange: (value: AvailabilityPolicy) => void;
  busy: boolean;
  onSave: () => void;
}

export default function AvailabilityPanel(props: Props) {
  const { value: availability, onChange, busy, onSave } = props;
  return (
    <form
      className="stack-form"
      onSubmit={(event) => {
        event.preventDefault();
        onSave();
      }}
    >
      <h2>Availability monitoring</h2>
      <p>
        When enabled, this server is expected to be available 24/7. Ludock uses
        a supported game probe, Docker health, or running state. Stops initiated
        by Ludock and active operations suppress outage alerts.
      </p>
      <label className="check-label">
        <input
          type="checkbox"
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
          min={10}
          max={86400}
          required
          value={availability.graceSeconds}
          onChange={(event) =>
            onChange({
              ...availability,
              graceSeconds: Number(event.target.value),
            })
          }
        />
      </label>
      <label className="check-label">
        <input
          type="checkbox"
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
        One notification is sent for an outage and one for recovery. Configure
        Discord delivery in Settings.
      </p>
      <button className="primary-btn" disabled={busy}>
        Save monitoring
      </button>
    </form>
  );
}
