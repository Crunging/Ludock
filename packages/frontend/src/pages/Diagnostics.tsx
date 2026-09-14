import { useState } from "react";
import { apiJson } from "../api";
import { usePageRead } from "../hooks/usePageRead";
import { NavLink } from "../navigation";
import "./diagnostics.css";

import {
  diagnosticsResponseSchema,
  integrationsResponseSchema,
} from "@ludock/shared";

const columns = [
  { id: "recognition", title: "Recognition" },
  { id: "console", title: "Console" },
  { id: "backup", title: "Backups" },
  { id: "readiness", title: "Readiness" },
  { id: "update", title: "Updates" },
  { id: "platforms", title: "Image platforms" },
] as const;

async function readDiagnostics(signal: AbortSignal) {
  const [system, games] = await Promise.all([
    apiJson("/diagnostics", diagnosticsResponseSchema, { signal }),
    apiJson("/integrations", integrationsResponseSchema, { signal }),
  ]);
  return { ...system, ...games };
}

export default function Diagnostics() {
  const { data, loading, error, refresh } = usePageRead(readDiagnostics, "Unable to load diagnostics.");
  const [selected, setSelected] = useState("");
  const integration = data?.integrations.find((item) => item.gameType === selected);
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
                If you use a different socket, its mount must match the
                <code> DOCKER_SOCKET</code> setting inside Ludock. Recreate Ludock
                after changing the mount, then refresh this page.
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
                  {data.integrations.map((game) => (
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
