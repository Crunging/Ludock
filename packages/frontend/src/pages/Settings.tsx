import { deploymentSettingsResponseSchema } from "@ludock/shared";
import { apiJson } from "../api";
import { usePageRead } from "../hooks/usePageRead";
import DeploymentGuidance from "../components/DeploymentGuidance";
import BackupSettings from "../components/settings/BackupSettings";
import NotificationSettings from "../components/settings/NotificationSettings";
import "../styles/admin-setup.css";

const readDeployment = (signal: AbortSignal) =>
  apiJson("/settings/deployment", deploymentSettingsResponseSchema, { signal });

export default function Settings() {
  const deployment = usePageRead(
    readDeployment,
    "Unable to load deployment guidance.",
  );
  return (
    <div className="page settings-page">
      <div className="page__header">
        <h1 className="page__title">Settings</h1>
        <p className="page__subtitle">
          Storage, Compose source access, and notification delivery.
        </p>
      </div>
      {deployment.loading && (
        <p className="muted" role="status">
          Loading setup guidance…
        </p>
      )}
      {deployment.error && (
        <div className="admin-setup-help">
          <p>
            Setup guidance is unavailable: {deployment.error} Your settings can
            still be edited below.
          </p>
          <button
            className="secondary-btn"
            onClick={() => void deployment.refresh()}
          >
            Reload setup guidance
          </button>
        </div>
      )}
      <BackupSettings deployment={deployment.data} />
      <section
        className="settings-section settings-section--divided"
        aria-labelledby="compose-settings-title"
      >
        <h2 id="compose-settings-title">Compose updates</h2>
        <p className="section-note">
          Update servers from their Update tab. Ludock discovers and validates
          source files from Docker. Your existing manager continues to own and
          edit those files.
        </p>
        {deployment.data && (
          <DeploymentGuidance section="compose" deployment={deployment.data} />
        )}
      </section>
      <NotificationSettings />
    </div>
  );
}
