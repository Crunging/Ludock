import type { UpdateCapability } from "@ludock/shared";
export interface UpdateOptions {
  createBackup: boolean;
  forceRecreate: boolean;
  confirmed: boolean;
  skipConfirmation: string;
}

interface Props {
  serverName: string;
  capability: UpdateCapability | null;
  value: UpdateOptions;
  onChange: (value: UpdateOptions) => void;
  busy: boolean;
  blocked: boolean;
  hasActiveOperation: boolean;
  onSubmit: () => void;
}

export default function UpdatePanel(props: Props) {
  const {
    serverName,
    capability,
    value,
    onChange,
    busy,
    blocked,
    hasActiveOperation,
    onSubmit,
  } = props;
  const {
    createBackup,
    forceRecreate,
    confirmed: updateConfirm,
    skipConfirmation,
  } = value;
  return (
    <>
      <h2>{capability?.actionLabel || "Update server"}</h2>
      {!capability?.available ? (
        <p className="section-note">
          {capability?.unavailableReason ||
            "This server’s Compose project is not registered. Update it through its owning manager, or register the trusted project in Settings."}
        </p>
      ) : (
        <form
          className="stack-form"
          onSubmit={(event) => {
            event.preventDefault();
            onSubmit();
          }}
        >
          <p>
            Pull the latest version of this service’s configured image and
            recreate it through Docker Compose. The image’s normal startup
            process may update the game software.
          </p>
          <dl className="metadata-list">
            <dt>Project</dt>
            <dd>{capability.projectName}</dd>
            <dt>Service</dt>
            <dd>{capability.serviceName}</dd>
            <dt>Configured image</dt>
            <dd>{capability.image}</dd>
          </dl>
          <p className="section-note">
            Compose source is authoritative. Redeployment can replace runtime
            changes that were never saved in that source. Other services and
            dependencies are not recreated.
          </p>
          <label className="check-label">
            <input
              type="checkbox"
              checked={createBackup}
              onChange={(event) => {
                onChange({
                  ...value,
                  createBackup: event.target.checked,
                  confirmed: false,
                });
              }}
            />
            Create a stopped-server backup before recreation
          </label>
          <p className="muted">
            The server remains stopped between backup and recreation. A
            previously stopped server stays stopped.
          </p>
          <label className="check-label">
            <input
              type="checkbox"
              checked={forceRecreate}
              onChange={(event) => {
                onChange({
                  ...value,
                  forceRecreate: event.target.checked,
                  confirmed: false,
                });
              }}
            />
            Recreate anyway, even if the configured image is current
          </label>
          {forceRecreate && (
            <p className="muted">
              The service will be replaced even when its image is unchanged.
              Startup-based game updates wait until the server is started.
            </p>
          )}
          {!createBackup && (
            <label>
              <span>
                Type <strong>{serverName}</strong> to skip the backup
              </span>
              <input
                value={skipConfirmation}
                onChange={(event) =>
                  onChange({ ...value, skipConfirmation: event.target.value })
                }
                autoComplete="off"
              />
            </label>
          )}
          <label className="check-label">
            <input
              type="checkbox"
              checked={updateConfirm}
              onChange={(event) =>
                onChange({ ...value, confirmed: event.target.checked })
              }
            />
            I understand that connected players will be disconnected and the
            selected service will be recreated.
          </label>
          <button
            className="primary-btn"
            disabled={
              busy ||
              blocked ||
              hasActiveOperation ||
              !updateConfirm ||
              (!createBackup && skipConfirmation !== serverName)
            }
          >
            {forceRecreate ? "Recreate service" : capability.actionLabel}
          </button>
        </form>
      )}
    </>
  );
}
