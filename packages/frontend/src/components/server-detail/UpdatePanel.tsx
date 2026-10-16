import type { UpdateCapability } from "@ludock/shared";
import { NavLink } from "../../navigation";
export interface UpdateOptions {
  createBackup: boolean;
  forceRecreate: boolean;
  confirmed: boolean;
  skipConfirmation: string;
}

interface Props {
  serverName: string;
  canBackup: boolean;
  canUpdateImage: boolean;
  capability: UpdateCapability | null;
  value: UpdateOptions;
  onChange: (value: UpdateOptions) => void;
  busy: boolean;
  blocked: boolean;
  hasActiveOperation: boolean;
  onRecheck: () => void;
  onSubmit: () => void;
}

export default function UpdatePanel(props: Props) {
  const {
    serverName,
    canBackup,
    canUpdateImage,
    capability,
    value,
    onChange,
    busy,
    blocked,
    hasActiveOperation,
    onRecheck,
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
      {capability?.updatesOnStart && (
        <div className="startup-update-note">
          <h3>Game updates</h3>
          <p className="section-lede">This image downloads the latest game version when it starts, so Restart updates the game.</p>
          <p className="muted">
            Version pins in the image’s settings still apply.
            {canBackup && " To back up first, choose Take a backup first when you restart."}
          </p>
        </div>
      )}
      {!canUpdateImage && capability?.updatesOnStart && (
        <p className="section-note">
          {capability.manager === "compose"
            ? "Pulling a newer image needs an administrator."
            : "Pull a newer image through the tool that created this container."}
        </p>
      )}
      {!canUpdateImage && !capability?.updatesOnStart && (
        <p className="section-note">This image is not known to update the game on startup. Update it through its owning manager.</p>
      )}
      {canUpdateImage && <h3>Update container image</h3>}
      {canUpdateImage && (!capability?.available ? (
        <>
          <p className="section-note">
            {capability?.unavailableReason ||
              "Ludock discovers Compose source files automatically. Check source access in Settings if updates are unavailable."}
          </p>
          <div className="inline-actions">
            <button className="secondary-btn" onClick={onRecheck} disabled={busy}>
              Check again
            </button>
            <NavLink className="text-link" to="/settings">
              Check source access
            </NavLink>
          </div>
        </>
      ) : (
        <form
          className="stack-form"
          onSubmit={(event) => {
            event.preventDefault();
            onSubmit();
          }}
        >
          <p className="section-lede">
            Pull the newest image for this service and recreate it with Docker
            Compose. This updates the image itself, including its system
            packages and startup scripts.
          </p>
          <dl className="metadata-list">
            <dt>Project</dt>
            <dd>{capability.projectName}</dd>
            <dt>Service</dt>
            <dd>{capability.serviceName}</dd>
            <dt>Configured image</dt>
            <dd>{capability.image}</dd>
          </dl>
          <details className="disclosure">
            <summary>What gets replaced</summary>
            <p>
              Ludock follows your Compose files exactly. Changes made to the
              running container that aren’t saved in those files are lost. Only
              this service is recreated; other services and dependencies aren’t
              touched.
            </p>
          </details>
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
          <p className="muted check-help">
            The server stays stopped until it’s recreated. If it was stopped
            before, it stays stopped after.
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
            <p className="muted check-help">
              Use this to pick up a game update that installs on startup.
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
              (!canBackup && createBackup) ||
              blocked ||
              hasActiveOperation ||
              !updateConfirm ||
              (!createBackup && skipConfirmation !== serverName)
            }
          >
            {forceRecreate ? "Recreate service" : capability.actionLabel}
          </button>
        </form>
      ))}
    </>
  );
}
