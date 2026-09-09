import {
  type ChangeEvent,
  type DragEvent,
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";
import { apiFetch, apiJson, apiResponse } from "../api";
import { useAuth } from "../auth-context";
import { can } from "../permissions";
import { useNavigate } from "../navigation-context";
import type { ManagedContainer } from "../types";

import {
  type FileEntry,
  type FileLocationRequest,
  type CreateDirectoryRequest,
  type RenameFileRequest,
  type UploadFileQuery,
  fileListingSchema,
  serverResponseSchema,
  okResponseSchema,
} from "@ludock/shared";

function joinPath(parent: string, name: string): string {
  return parent ? `${parent}/${name}` : name;
}

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let index = 0;
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024;
    index += 1;
  }
  return `${value < 10 ? value.toFixed(1) : value.toFixed(0)} ${units[index]}`;
}

export default function Files({ containerId }: { containerId: string }) {
  const navigate = useNavigate();
  const { user } = useAuth();
  const fileInput = useRef<HTMLInputElement>(null);
  const [server, setServer] = useState<ManagedContainer | null>(null);
  const canManage = can(user, server, "files.write");
  const [rootId, setRootId] = useState("");
  const [currentPath, setCurrentPath] = useState("");
  const [entries, setEntries] = useState<FileEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  useEffect(() => {
    apiJson(`/servers/${encodeURIComponent(containerId)}`, serverResponseSchema)
      .then(({ server: nextServer }) => {
        setServer(nextServer);
        if (!can(user, nextServer, "files.read"))
          throw new Error("You do not have file access to this server.");
        setRootId(nextServer.fileRoots[0]?.id || "");
      })
      .catch((reason) =>
        setError(
          reason instanceof Error ? reason.message : "Unable to load server",
        ),
      );
  }, [containerId, user]);

  const refresh = useCallback(async () => {
    if (!rootId) {
      setLoading(false);
      return;
    }
    setLoading(true);
    setError(null);
    const query = new URLSearchParams({
      root: rootId,
      path: currentPath,
    } satisfies FileLocationRequest);
    try {
      const listing = await apiJson(
        `/servers/${encodeURIComponent(containerId)}/files?${query}`,
        fileListingSchema,
      );
      setEntries(listing.entries);
    } catch (reason) {
      setError(
        reason instanceof Error ? reason.message : "Unable to list files",
      );
    } finally {
      setLoading(false);
    }
  }, [containerId, currentPath, rootId]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const runMutation = useCallback(
    async (
      request: () => Promise<Response>,
      successMessage: string,
    ): Promise<boolean> => {
      setBusy(true);
      setError(null);
      setNotice(null);
      try {
        const response = await request();
        await apiResponse(response, okResponseSchema);
        setNotice(successMessage);
        await refresh();
        return true;
      } catch (reason) {
        setError(
          reason instanceof Error ? reason.message : "File operation failed",
        );
        return false;
      } finally {
        setBusy(false);
      }
    },
    [refresh],
  );

  const uploadFiles = useCallback(
    async (files: File[]) => {
      if (!canManage || files.length === 0) return;
      setBusy(true);
      setError(null);
      setNotice(null);
      let completed = 0;
      try {
        for (const file of files) {
          const query = new URLSearchParams({
            root: rootId,
            path: currentPath,
            name: file.name,
          } satisfies UploadFileQuery);
          const response = await apiFetch(
            `/api/v1/servers/${encodeURIComponent(containerId)}/files/upload?${query}`,
            {
              method: "PUT",
              headers: { "Content-Type": "application/octet-stream" },
              body: file,
            },
          );
          await apiResponse(response, okResponseSchema);
          completed += 1;
        }
        setNotice(
          `${completed} file${completed === 1 ? "" : "s"} uploaded successfully`,
        );
        await refresh();
      } catch (reason) {
        setError(reason instanceof Error ? reason.message : "Upload failed");
        if (completed > 0) await refresh();
      } finally {
        setBusy(false);
        if (fileInput.current) fileInput.current.value = "";
      }
    },
    [canManage, containerId, currentPath, refresh, rootId],
  );

  const createFolder = async () => {
    const name = window.prompt("New folder name");
    if (!name) return;
    await runMutation(
      () =>
        apiFetch(
          `/api/v1/servers/${encodeURIComponent(containerId)}/files/directory`,
          {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              root: rootId,
              path: currentPath,
              name,
            } satisfies CreateDirectoryRequest),
          },
        ),
      `Created ${name}`,
    );
  };

  const renameEntry = async (entry: FileEntry) => {
    const newName = window.prompt("Rename to", entry.name);
    if (!newName || newName === entry.name) return;
    await runMutation(
      () =>
        apiFetch(
          `/api/v1/servers/${encodeURIComponent(containerId)}/files/rename`,
          {
            method: "PATCH",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              root: rootId,
              path: joinPath(currentPath, entry.name),
              newName,
            } satisfies RenameFileRequest),
          },
        ),
      `Renamed ${entry.name}`,
    );
  };

  const deleteEntry = async (entry: FileEntry) => {
    const description =
      entry.type === "directory"
        ? `"${entry.name}" and everything inside it`
        : `"${entry.name}"`;
    if (!window.confirm(`Permanently delete ${description}?`)) return;
    const query = new URLSearchParams({
      root: rootId,
      path: joinPath(currentPath, entry.name),
    } satisfies FileLocationRequest);
    await runMutation(
      () =>
        apiFetch(
          `/api/v1/servers/${encodeURIComponent(containerId)}/files?${query}`,
          { method: "DELETE" },
        ),
      `Deleted ${entry.name}`,
    );
  };

  const downloadUrl = (entry: FileEntry) => {
    const query = new URLSearchParams({
      root: rootId,
      path: joinPath(currentPath, entry.name),
    } satisfies FileLocationRequest);
    return `/api/v1/servers/${encodeURIComponent(containerId)}/files/download?${query}`;
  };

  const pathParts = currentPath ? currentPath.split("/") : [];
  const selectedRoot = server?.fileRoots.find((root) => root.id === rootId);

  return (
    <div className="page files-page">
      <div className="files-header">
        <div className="files-header__title">
          <button
            className="secondary-btn files-back"
            onClick={() => navigate("/")}
            aria-label="Back to servers"
          >
            ←
          </button>
          <div>
            <div className="files-header__name">
              <h1 className="page__title">
                {server?.displayName || "Server files"}
              </h1>
              {server && (
                <span className={`status-badge status-badge--${server.state}`}>
                  <span className="status-dot" />
                  {server.state}
                </span>
              )}
            </div>
            <p className="page__subtitle">
              Browse worlds, configuration, backups, and mods
            </p>
          </div>
        </div>
        {server && server.fileRoots.length > 1 && (
          <label className="files-root">
            Storage location
            <select
              value={rootId}
              onChange={(event) => {
                setRootId(event.target.value);
                setCurrentPath("");
              }}
              disabled={busy}
            >
              {server.fileRoots.map((root) => (
                <option key={root.id} value={root.id}>
                  {root.name} ({root.path})
                </option>
              ))}
            </select>
          </label>
        )}
      </div>

      {error && (
        <div className="alert alert--error" role="alert">
          <span>{error}</span>
          <button onClick={() => setError(null)} aria-label="Dismiss error">
            ×
          </button>
        </div>
      )}
      {notice && (
        <div className="alert alert--success" role="status">
          <span>{notice}</span>
          <button onClick={() => setNotice(null)} aria-label="Dismiss notice">
            ×
          </button>
        </div>
      )}

      {!rootId && !loading ? (
        <div className="empty-state">
          <div className="empty-state__title">
            File access is not configured
          </div>
          <div className="empty-state__description">
            No approved writable mounts are available. An administrator can
            check the mount restrictions or ludock.files label.
          </div>
        </div>
      ) : (
        <section className="file-manager" aria-busy={busy || loading}>
          <div className="file-toolbar">
            <nav className="file-breadcrumbs" aria-label="Current folder">
              <button onClick={() => setCurrentPath("")}>
                {selectedRoot?.name || "Files"}
              </button>
              {pathParts.map((part, index) => (
                <span key={`${part}-${index}`}>
                  <b>/</b>
                  <button
                    onClick={() =>
                      setCurrentPath(pathParts.slice(0, index + 1).join("/"))
                    }
                  >
                    {part}
                  </button>
                </span>
              ))}
            </nav>
            <div className="file-toolbar__actions">
              <button
                className="secondary-btn"
                onClick={() => void refresh()}
                disabled={busy || loading}
              >
                Refresh
              </button>
              {canManage && (
                <button
                  className="secondary-btn"
                  onClick={() => void createFolder()}
                  disabled={busy}
                >
                  New folder
                </button>
              )}
            </div>
          </div>

          {canManage && (
            <div
              className={`file-dropzone ${dragging ? "file-dropzone--active" : ""}`}
              onDragEnter={(event: DragEvent) => {
                event.preventDefault();
                setDragging(true);
              }}
              onDragOver={(event: DragEvent) => event.preventDefault()}
              onDragLeave={(event: DragEvent) => {
                if (event.currentTarget === event.target) setDragging(false);
              }}
              onDrop={(event: DragEvent) => {
                event.preventDefault();
                setDragging(false);
                void uploadFiles(Array.from(event.dataTransfer.files));
              }}
            >
              <span>Drop files here to upload them to this folder</span>
              <button
                className="primary-btn"
                onClick={() => fileInput.current?.click()}
                disabled={busy}
              >
                {busy ? "Working…" : "Choose files"}
              </button>
              <input
                ref={fileInput}
                type="file"
                multiple
                hidden
                onChange={(event: ChangeEvent<HTMLInputElement>) =>
                  void uploadFiles(Array.from(event.target.files || []))
                }
              />
            </div>
          )}

          <div className="file-list">
            <div className="file-list__header">
              <span>Name</span>
              <span>Size</span>
              <span>Modified</span>
              <span>Actions</span>
            </div>
            {loading ? (
              <div className="loading-spinner">
                <div className="loading-spinner__ring" />
              </div>
            ) : entries.length === 0 ? (
              <div className="file-list__empty">This folder is empty.</div>
            ) : (
              entries.map((entry) => (
                <div className="file-row" key={entry.name}>
                  <div className="file-row__name">
                    <span className="file-row__icon" aria-hidden="true">
                      {entry.type === "directory"
                        ? "DIR"
                        : entry.type === "symlink"
                          ? "LINK"
                          : "FILE"}
                    </span>
                    {entry.type === "directory" ? (
                      <button
                        onClick={() =>
                          setCurrentPath(joinPath(currentPath, entry.name))
                        }
                      >
                        {entry.name}
                      </button>
                    ) : (
                      <span>{entry.name}</span>
                    )}
                    {entry.type === "symlink" && (
                      <small title="Symbolic links are blocked for safety">
                        symbolic link
                      </small>
                    )}
                  </div>
                  <span>
                    {entry.type === "file" ? formatSize(entry.size) : "—"}
                  </span>
                  <span>
                    {entry.modifiedAt
                      ? new Date(entry.modifiedAt).toLocaleString()
                      : "—"}
                  </span>
                  <div className="file-row__actions">
                    {entry.type !== "symlink" && (
                      <a
                        className="secondary-btn"
                        href={downloadUrl(entry)}
                        download
                      >
                        Download
                      </a>
                    )}
                    {canManage && (
                      <>
                        {entry.type !== "symlink" && (
                          <button
                            className="secondary-btn"
                            onClick={() => void renameEntry(entry)}
                            disabled={busy}
                          >
                            Rename
                          </button>
                        )}
                        <button
                          className="secondary-btn secondary-btn--danger"
                          onClick={() => void deleteEntry(entry)}
                          disabled={busy}
                        >
                          Delete
                        </button>
                      </>
                    )}
                  </div>
                </div>
              ))
            )}
          </div>
          {!canManage && (
            <p className="file-manager__note">
              Your account can browse and download files but cannot change them.
            </p>
          )}
        </section>
      )}
    </div>
  );
}
