import { useEffect, useRef, useState } from "react";
import { apiJson, jsonBody } from "../api";
import {
  CAPABILITY_LABELS,
  VIEWER_CAPABILITIES,
  toggleGrant,
  type GrantCapability,
} from "../permissions";
import type { ManagedContainer, ServerCapability } from "../types";
import type { UserRole } from "../auth-context";

import {
  type ServerGrantInput,
  serverGrantsResponseSchema,
  serversResponseSchema,
} from "@ludock/shared";

interface Props {
  userId: string;
  username: string;
  role: UserRole;
  onClose: () => void;
}

export default function ServerGrants(props: Props) {
  return <ServerGrantsEditor key={`${props.userId}-${props.role}`} {...props} />;
}

function ServerGrantsEditor({
  userId,
  username,
  role,
  onClose,
}: Props) {
  const [servers, setServers] = useState<ManagedContainer[]>([]);
  const [grants, setGrants] = useState<ServerGrantInput[]>([]);
  const [loading, setLoading] = useState(true);
  const [loaded, setLoaded] = useState(false);
  const [loadAttempt, setLoadAttempt] = useState(0);
  const saving = useRef(false);
  const active = useRef(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    active.current = true;
    setLoading(true);
    setLoaded(false);
    setError(null);
    Promise.all([
      apiJson("/servers", serversResponseSchema, { signal: controller.signal }),
      apiJson(
        `/users/${encodeURIComponent(userId)}/server-grants`,
        serverGrantsResponseSchema,
        { signal: controller.signal },
      ),
    ])
      .then(([serverBody, grantsBody]) => {
        if (!controller.signal.aborted) {
          setServers(serverBody.servers);
          setGrants(
            grantsBody.grants.map(({ serverId, capabilities }) => ({
              serverId,
              capabilities: capabilities.filter((capability) =>
                capability in CAPABILITY_LABELS &&
                (role !== "viewer" || VIEWER_CAPABILITIES.includes(capability)),
              ),
            })),
          );
          setLoaded(true);
        }
      })
      .catch((reason) => {
        if (!controller.signal.aborted)
          setError(
            reason instanceof Error
              ? reason.message
              : "Unable to load server access.",
          );
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => {
      active.current = false;
      controller.abort();
    };
  }, [userId, role, loadAttempt]);
  function setServerGrant(serverId: string, capabilities: ServerCapability[]) {
    setNotice(null);
    setGrants((current) => [
      ...current.filter((grant) => grant.serverId !== serverId),
      ...(capabilities.length ? [{ serverId, capabilities }] : []),
    ]);
  }
  async function save() {
    if (!loaded || saving.current) return;
    saving.current = true;
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      await apiJson(
        `/users/${encodeURIComponent(userId)}/server-grants`,
        serverGrantsResponseSchema,
        jsonBody("PUT", { grants }),
      );
      if (!active.current) return;
      setNotice(
        "Server access saved. Revoked permissions apply to open connections and queued work.",
      );
    } catch (reason) {
      if (!active.current) return;
      setError(
        reason instanceof Error
          ? reason.message
          : "Unable to save server access.",
      );
    } finally {
      saving.current = false;
      if (active.current) setBusy(false);
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
      ) : !loaded ? (
        <div className="inline-actions">
          <button
            className="secondary-btn"
            onClick={() => setLoadAttempt((value) => value + 1)}
          >
            Try again
          </button>
          <button className="secondary-btn" onClick={onClose}>
            Close
          </button>
        </div>
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
