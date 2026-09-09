import { useCallback, useEffect, useState } from "react";
import { apiJson } from "../api";

interface Diagnostic {
  containerId?: string;
  name?: string;
  code: string;
  message: string;
}
interface Capability {
  status: string;
  description: string;
  evidence: string[];
}
interface Integration {
  gameType: string;
  repositories: string[];
  capabilities: Record<string, Capability>;
}
const columns = [
  { id: "recognition", title: "Recognition" },
  { id: "console", title: "Console" },
  { id: "backup", title: "Backups" },
  { id: "readiness", title: "Readiness" },
  { id: "update", title: "Updates" },
  { id: "platforms", title: "Image platforms" },
];

export default function Diagnostics() {
  const [diagnostics, setDiagnostics] = useState<Diagnostic[]>([]);
  const [integrations, setIntegrations] = useState<Integration[]>([]);
  const [dockerConnected, setDockerConnected] = useState(false);
  const [composeAvailable, setComposeAvailable] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState("");
  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [system, games] = await Promise.all([
        apiJson<{
          diagnostics: Diagnostic[];
          dockerConnected: boolean;
          composeAvailable: boolean;
        }>("/diagnostics"),
        apiJson<{ integrations: Integration[] }>("/integrations"),
      ]);
      setDiagnostics(system.diagnostics);
      setDockerConnected(system.dockerConnected);
      setComposeAvailable(system.composeAvailable);
      setIntegrations(games.integrations);
    } catch (reason) {
      setError(
        reason instanceof Error
          ? reason.message
          : "Unable to load diagnostics.",
      );
    } finally {
      setLoading(false);
    }
  }, []);
  useEffect(() => {
    void refresh();
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
      ) : (
        <>
          <dl className="metadata-list">
            <dt>Docker Engine</dt>
            <dd>{dockerConnected ? "Connected" : "Unavailable"}</dd>
            <dt>Docker Compose</dt>
            <dd>{composeAvailable ? "Available" : "Unavailable"}</dd>
          </dl>
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
                        No discovery issues reported.
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
                          {game.capabilities[column.id]?.status || "unverified"}
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
                      {integration.capabilities[column.id]?.description ||
                        "No verified capability information."}
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
