import { apiJson } from "../api";
import { usePageRead } from "../hooks/usePageRead";
import { NavLink } from "../navigation";
import "./diagnostics.css";

import {
  diagnosticsResponseSchema,
  integrationsResponseSchema,
} from "@ludock/shared";

async function readDiagnostics(signal: AbortSignal) {
  const [system, games] = await Promise.all([
    apiJson("/diagnostics", diagnosticsResponseSchema, { signal }),
    apiJson("/integrations", integrationsResponseSchema, { signal }),
  ]);
  return { ...system, ...games };
}

export default function Diagnostics() {
  const { data, loading, error, refresh } = usePageRead(readDiagnostics, "Unable to load diagnostics.");
  return (
    <div className="page">
      <div className="page__header page__header--actions">
        <div>
          <h1 className="page__title">Diagnostics</h1>
          <p className="page__subtitle">
            Docker connectivity, discovery issues, and integration support.
          </p>
        </div>
        <button
          className="secondary-btn"
          disabled={loading}
          onClick={() => void refresh()}
        >
          Refresh
        </button>
      </div>
      {error && (
        <div className="alert alert--error" role="alert">
          {error}
        </div>
      )}
      {loading ? (
        <p className="muted" role="status">
          Loading diagnostics…
        </p>
      ) : data && !error && (
        <>
          <dl className="metadata-list">
            <dt>Docker Engine</dt>
            <dd>{data.dockerConnected ? "Connected" : "Unavailable"}</dd>
            <dt>Compose updates</dt>
            <dd>{data.composeAvailable ? "Available" : "Not enabled"}</dd>
          </dl>
          {!data.dockerConnected && (
            <section className="help-panel diagnostics-help" aria-labelledby="docker-help-title">
              <h2 id="docker-help-title">Connect Ludock to Docker</h2>
              <p>
                Check that Docker is running on the host where Ludock is installed.
                The Ludock container needs its Docker socket mounted:
              </p>
              <pre><code>/var/run/docker.sock:/var/run/docker.sock:ro</code></pre>
              <p>
                In <code>compose.yaml</code>, change the socket mount’s <code>source</code>
                {" "}to the socket path on the Docker host.
                For rootless Docker, this is usually <code>/run/user/1000/docker.sock</code>;
                replace <code>1000</code> with the Docker user’s ID.
              </p>
              <p>
                Run <code>docker compose up -d --force-recreate ludock</code> from
                Ludock’s Compose folder, then refresh this page. Custom deployments
                must match the mount’s container path to <code>DOCKER_SOCKET</code>.
              </p>
              <NavLink className="text-link" to="/logs">View Ludock logs</NavLink>
            </section>
          )}
          {!data.composeAvailable && (
            <section className="help-panel diagnostics-help" aria-labelledby="compose-help-title">
              <h2 id="compose-help-title">Enable Compose updates when you need them</h2>
              <p>
                Server discovery and ordinary controls work without Compose update
                access. To update through Ludock, use its Linux container image,
                mount the existing Compose sources read-only, and set
                <code> LUDOCK_COMPOSE_ROOTS</code> to those directories.
              </p>
              <NavLink className="text-link" to="/settings">Open update settings</NavLink>
            </section>
          )}
          <section className="settings-section">
            <h2>Discovery issues</h2>
            <div className="table-scroll">
              <table className="data-table">
                <thead>
                  <tr>
                    <th>Container</th>
                    <th>Issue</th>
                    <th>Details</th>
                  </tr>
                </thead>
                <tbody>
                  {data.diagnostics.length === 0 && (
                    <tr>
                      <td colSpan={3} className="muted">
                        {data.dockerConnected
                          ? "No discovery issues reported."
                          : "Discovery cannot be checked until Docker is connected."}
                      </td>
                    </tr>
                  )}
                  {data.diagnostics.map((diagnostic, index) => (
                    <tr
                      key={`${diagnostic.containerId}-${diagnostic.code}-${index}`}
                    >
                      <td>{diagnostic.name || "—"}</td>
                      <td>{diagnostic.code.replaceAll("_", " ")}</td>
                      <td>{diagnostic.message}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </section>
          <section className="settings-section">
            <h2>Supported games</h2>
            <p className="section-note">
              These images are managed automatically and get a built-in console.
              Recognition does not verify an image; other images need
              {" "}<code>ludock.enable=true</code>.
            </p>
            <div className="table-scroll">
              <table className="data-table">
                <thead>
                  <tr>
                    <th>Game</th>
                    <th>Recognized images</th>
                    <th>Console</th>
                  </tr>
                </thead>
                <tbody>
                  {data.integrations.map((game) => (
                    <tr key={game.gameType}>
                      <td>{game.gameType}</td>
                      <td>{game.repositories.join(", ")}</td>
                      <td>{game.console ?? "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </section>
        </>
      )}
    </div>
  );
}
