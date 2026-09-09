import { useCallback, useEffect, useRef, useState } from "react";
import { apiJson } from "../api";
import { NavLink } from "../navigation";
import "./diagnostics.css";

import {
  diagnosticsResponseSchema,
  integrationsResponseSchema,
  type DiscoveryDiagnostic,
  type GameIntegration,
} from "@ludock/shared";

const columns = [
  { id: "recognition", title: "Recognition" },
  { id: "console", title: "Console" },
  { id: "backup", title: "Backups" },
  { id: "readiness", title: "Readiness" },
  { id: "update", title: "Updates" },
  { id: "platforms", title: "Image platforms" },
] as const;

export default function Diagnostics() {
  const [diagnostics, setDiagnostics] = useState<DiscoveryDiagnostic[]>([]);
  const [integrations, setIntegrations] = useState<GameIntegration[]>([]);
  const [dockerConnected, setDockerConnected] = useState(false);
  const [composeAvailable, setComposeAvailable] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState("");
  const request = useRef<AbortController | null>(null);
  const refresh = useCallback(async () => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setLoading(true);
    setError(null);
    try {
      const [system, games] = await Promise.all([
        apiJson("/diagnostics", diagnosticsResponseSchema, { signal: controller.signal }),
        apiJson("/integrations", integrationsResponseSchema, { signal: controller.signal }),
      ]);
      if (controller.signal.aborted || request.current !== controller) return;
      setDiagnostics(system.diagnostics);
      setDockerConnected(system.dockerConnected);
      setComposeAvailable(system.composeAvailable);
      setIntegrations(games.integrations);
    } catch (reason) {
      if (controller.signal.aborted || request.current !== controller) return;
      setError(
        reason instanceof Error
          ? reason.message
          : "Unable to load diagnostics.",
      );
    } finally {
      if (request.current === controller && !controller.signal.aborted) {
        request.current = null;
        setLoading(false);
      }
    }
  }, []);
  useEffect(() => {
    void refresh();
    return () => request.current?.abort();
  }, [refresh]);
  const integration = integrations.find((item) => item.gameType === selected);
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
      ) : !error && (
        <>
          <dl className="metadata-list">
            <dt>Docker Engine</dt>
            <dd>{dockerConnected ? "Connected" : "Unavailable"}</dd>
            <dt>Compose updates</dt>
            <dd>{composeAvailable ? "Available" : "Not enabled"}</dd>
          </dl>
          {!dockerConnected && (
            <section className="help-panel diagnostics-help" aria-labelledby="docker-help-title">
              <h2 id="docker-help-title">Connect Ludock to Docker</h2>
              <p>
                Check that Docker is running on the host where Ludock is installed.
                The Ludock container needs its Docker socket mounted:
              </p>
              <pre><code>/var/run/docker.sock:/var/run/docker.sock:ro</code></pre>
              <p>
                If you use a different socket, its mount must match the
                <code> DOCKER_SOCKET</code> setting inside Ludock. Recreate Ludock
                after changing the mount, then refresh this page.
              </p>
              <NavLink className="text-link" to="/logs">View Ludock logs</NavLink>
            </section>
          )}
          {!composeAvailable && (
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
                  {diagnostics.length === 0 && (
                    <tr>
                      <td colSpan={3} className="muted">
                        {dockerConnected
                          ? "No discovery issues reported."
                          : "Discovery cannot be checked until Docker is connected."}
                      </td>
                    </tr>
                  )}
                  {diagnostics.map((diagnostic, index) => (
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
            <h2>Game capabilities</h2>
            <p className="section-note">
              Image recognition is separate from console, backup, readiness, and
              platform support. Select a game for prerequisites and limitations.
            </p>
            <div className="table-scroll">
              <table className="data-table">
                <thead>
                  <tr>
                    <th>Game</th>
                    {columns.map((column) => (
                      <th key={column.id}>{column.title}</th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {integrations.map((game) => (
                    <tr key={game.gameType}>
                      <td>
                        <button
                          className="text-link"
                          onClick={() => setSelected(game.gameType)}
                          aria-expanded={selected === game.gameType}
                        >
                          {game.gameType}
                        </button>
                      </td>
                      {columns.map((column) => (
                        <td key={column.id}>
                          {game.capabilities[column.id].status}
                        </td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {integration && (
              <div className="help-panel integration-detail">
                <h3>{integration.gameType}</h3>
                <p className="muted">
                  Recognized repositories: {integration.repositories.join(", ")}
                </p>
                {columns.map((column) => (
                  <div key={column.id}>
                    <h4>{column.title}</h4>
                    <p>
                      {integration.capabilities[column.id].description}
                    </p>
                  </div>
                ))}
              </div>
            )}
          </section>
        </>
      )}
    </div>
  );
}
