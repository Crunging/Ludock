interface Props {
  serverName: string;
  busy: boolean;
  confirmation: string;
  onConfirmationChange: (value: string) => void;
  onAccept: () => void;
}

export default function BindingReviewPanel(props: Props) {
  const { serverName, busy, confirmation, onConfirmationChange, onAccept } =
    props;
  return (
    <form
      className="danger-panel stack-form"
      onSubmit={(event) => {
        event.preventDefault();
        onAccept();
      }}
    >
      <h3>Review changed server identity</h3>
      <p>
        Verify that this is the same intended server and game data. Accepting
        the new binding re-enables existing user grants. Schedules for changed
        game data stay suspended; review and recreate them separately. Old
        backups remain subject to compatibility checks.
      </p>
      <label>
        <span>
          Type <strong>{serverName}</strong> to accept the changed binding
        </span>
        <input
          value={confirmation}
          onChange={(event) => onConfirmationChange(event.target.value)}
          autoComplete="off"
        />
      </label>
      <button
        className="secondary-btn secondary-btn--danger"
        disabled={busy || confirmation !== serverName}
      >
        Accept binding
      </button>
    </form>
  );
}
