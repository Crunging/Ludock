import type { DeploymentSettings } from "@ludock/shared";

interface Props {
  section: "backups" | "compose";
  deployment: DeploymentSettings;
  onUseBackupRoot?: (root: string) => void;
}

export default function DeploymentGuidance({ section, deployment, onUseBackupRoot }: Props) {
  if (section === "backups") {
    return (
      <div className="admin-setup-help">
        {deployment.backupRoots.length > 0 ? (
          <>
            <p>Choose a configured backup root, or enter a folder inside one. Ludock checks that the destination is mounted and writable when you save.</p>
            <div className="admin-root-options">
              {deployment.backupRoots.map((root) => (
                <button type="button" className="secondary-btn" key={root} onClick={() => onUseBackupRoot?.(root)}>
                  Use <code>{root}</code>
                </button>
              ))}
            </div>
          </>
        ) : (
          <>
            <h3>Mount a backup folder first</h3>
            <ol>
              <li>Create a separate backup folder on the Docker host and mount it writable into Ludock, for example <code>/srv/ludock-backups:/backups</code>.</li>
              <li>Set <code>LUDOCK_BACKUP_ROOTS=/backups</code> in Ludock’s <code>.env</code>, then recreate Ludock to apply the mount and setting.</li>
              <li>Enter that mounted path below, choose the limits, and save. You can then create a backup from a server’s Backups tab.</li>
            </ol>
          </>
        )}
      </div>
    );
  }
  return (
    <div className="admin-setup-help admin-compose-guidance">
      {!deployment.composeAvailable && deployment.composeRoots.length > 0 && (
        <p>Compose updates are not available yet. Verify that Ludock is running on Linux with the Docker Compose plugin and that these source roots are configured correctly. The bundled Ludock image includes the plugin.</p>
      )}
      {deployment.composeRoots.length > 0 ? (
        <>
          <p>Configured Compose source roots:</p>
          <ul className="admin-root-list">{deployment.composeRoots.map((root) => <li key={root}><code>{root}</code></li>)}</ul>
          <p>Ludock discovers project files inside these roots automatically and validates them when you update a server.</p>
        </>
      ) : (
        <>
          <h3>Enable Compose updates</h3>
          <ol>
            <li>Mount the project folder read-only into Ludock at the same absolute path as on the Docker host, for example <code>/srv/game-stacks:/srv/game-stacks:ro</code>.</li>
            <li>Set <code>LUDOCK_COMPOSE_ROOTS=/srv/game-stacks</code> in Ludock’s <code>.env</code>, then recreate Ludock.</li>
            <li>Open the server’s Update tab and click Check again. Project registration is not needed.</li>
          </ol>
        </>
      )}
    </div>
  );
}
