import { useEffect, useState } from "react";
import { apiFetch } from "../api";
import {
  CAPABILITY_LABELS,
  VIEWER_CAPABILITIES,
  toggleGrant,
  type GrantCapability,
} from "../permissions";
import type { ManagedContainer, ServerCapability } from "../types";
import type { UserRole } from "../auth-context";

interface Grant {
  serverId: string;
  capabilities: ServerCapability[];
}
interface Props {
  userId: string;
  username: string;
  role: UserRole;
  onClose: () => void;
}

export default function ServerGrants({
  userId,
  username,
  role,
  onClose,
}: Props) {
  const [servers, setServers] = useState<ManagedContainer[]>([]);
  const [grants, setGrants] = useState<Grant[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    Promise.all([
      apiFetch("/api/v1/servers"),
      apiFetch(`/api/v1/users/${encodeURIComponent(userId)}/server-grants`),
    ])
      .then(async ([serverResponse, grantsResponse]) => {
        if (!serverResponse.ok || !grantsResponse.ok)
          throw new Error("Unable to load server access.");
        const [serverBody, grantsBody] = await Promise.all([
          serverResponse.json(),
          grantsResponse.json(),
        ]);
        if (!cancelled) {
          setServers(serverBody.servers);
          setGrants(grantsBody.grants);
        }
      })
      .catch((reason) => {
        if (!cancelled)
          setError(
            reason instanceof Error
              ? reason.message
              : "Unable to load server access.",
          );
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [userId]);
  function setServerGrant(serverId: string, capabilities: ServerCapability[]) {
    setNotice(null);
    setGrants((current) => [
      ...current.filter((grant) => grant.serverId !== serverId),
      ...(capabilities.length ? [{ serverId, capabilities }] : []),
    ]);
  }
  async function save() {
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      const response = await apiFetch(
        `/api/v1/users/${encodeURIComponent(userId)}/server-grants`,
        {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ grants }),
        },
      );
      const body = await response.json().catch(() => ({}));
      if (!response.ok)
        throw new Error(body.error || "Unable to save server access.");
      setNotice(
        "Server access saved. Revoked permissions apply to open connections and queued work.",
      );
    } catch (reason) {
      setError(
        reason instanceof Error
          ? reason.message
          : "Unable to save server access.",
      );
    } finally {
      setBusy(false);
    }
  }
  const capabilities = (
    Object.keys(CAPABILITY_LABELS) as GrantCapability[]
  ).filter(
    (capability) =>
      role !== "viewer" || VIEWER_CAPABILITIES.includes(capability),
  );
  return (
    <div className="grants-editor">
      <h3>Server access for {username}</h3>
      <p className="muted">
        Choose servers and actions separately. New servers are not shared
        automatically.
      </p>
      {error && (
        <div className="alert alert--error" role="alert">
          {error}
        </div>
      )}
      {notice && (
        <div className="alert alert--success" role="status">
          {notice}
        </div>
      )}
      {loading ? (
        <p role="status">Loading access…</p>
      ) : (
        <>
          {servers.length === 0 && (
            <p className="table-empty">
              No eligible servers are available to assign.
            </p>
          )}
          {servers.map((server) => {
            const selected =
              grants.find((grant) => grant.serverId === server.id)
                ?.capabilities || [];
            return (
              <fieldset
                className="grants-server"
                key={server.id}
                disabled={busy}
              >
                <legend>{server.displayName}</legend>
                <div className="grants-presets">
                  <button
                    type="button"
                    className="text-link"
                    onClick={() => setServerGrant(server.id, ["server.view"])}
                  >
                    Status only
                  </button>
                  {role !== "viewer" && (
                    <button
                      type="button"
                      className="text-link"
                      onClick={() =>
                        setServerGrant(server.id, [
                          "server.view",
                          "server.start",
                          "server.stop",
                        ])
                      }
                    >
                      Start and stop
                    </button>
                  )}
                  <button
                    type="button"
                    className="text-link"
                    onClick={() => setServerGrant(server.id, [])}
                  >
                    Remove access
                  </button>
                </div>
                <div className="grants-capabilities">
                  {capabilities.map((capability) => (
                    <label key={capability}>
                      <input
                        type="checkbox"
                        checked={selected.includes(capability)}
                        onChange={(event) =>
                          setServerGrant(
                            server.id,
                            toggleGrant(
                              selected,
                              capability,
                              event.target.checked,
                            ),
                          )
                        }
                      />
                      {CAPABILITY_LABELS[capability]}
                    </label>
                  ))}
                </div>
                {selected.includes("backups.create") && (
                  <p className="muted">
                    Creating a backup stops a running server for the copy, then
                    starts it again. Archive downloads and restores remain
                    administrator-only.
                  </p>
                )}
              </fieldset>
            );
          })}
          <div className="inline-actions">
            <button
              type="button"
              className="primary-btn"
              onClick={() => void save()}
              disabled={busy}
            >
              {busy ? "Saving…" : "Save server access"}
            </button>
            <button
              type="button"
              className="secondary-btn"
              onClick={onClose}
              disabled={busy}
            >
              Close
            </button>
          </div>
        </>
      )}
    </div>
  );
}
