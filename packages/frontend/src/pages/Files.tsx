import {
  type ChangeEvent,
  type DragEvent,
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";
import {
  ApiRequestError,
  apiFetch,
  apiJson,
  apiResponse,
  jsonBody,
} from "../api";
import { useAuth } from "../auth-context";
import { can } from "../permissions";
import { NavLink } from "../navigation";
import type { ManagedContainer } from "../types";
import FileActionDialog, {
  type FileAction,
} from "../components/FileActionDialog";
import {
  type FileEntry,
  type FileLocationRequest,
  type CreateDirectoryRequest,
  type RenameFileRequest,
  type UploadFileQuery,
  fileListingSchema,
  serverResponseSchema,
  okResponseSchema,
  formatByteSize,
} from "@ludock/shared";

interface FileLocation {
  root: string;
  path: string;
}

interface FolderListing {
  location: FileLocation;
  state: "loading" | "ready" | "error";
  entries: FileEntry[];
  error: string | null;
}

interface SelectedAction {
  action: FileAction;
  location: FileLocation;
  folderLabel: string;
  initialName?: string;
}

interface ActionDraft {
  kind: "create" | "rename";
  location: FileLocation;
  sourceName: string | null;
  name: string;
}

interface PendingMutation {
  kind: "action" | "upload";
  controller: AbortController;
}

interface UploadProgress {
  completed: number;
  total: number;
  name: string;
  phase: "uploading" | "canceling" | "refreshing";
}

function joinPath(parent: string, name: string): string {
  return parent ? `${parent}/${name}` : name;
}

function sameLocation(left: FileLocation, right: FileLocation): boolean {
  return left.root === right.root && left.path === right.path;
}

export default function Files({ containerId }: { containerId: string }) {
  // Switching servers must discard that server's folder, dialogs, and requests.
  return <FileBrowser key={containerId} containerId={containerId} />;
}

function FileBrowser({ containerId }: { containerId: string }) {
  const { user } = useAuth();
  const fileInput = useRef<HTMLInputElement>(null);
  const listingRequest = useRef<AbortController | null>(null);
  const mutation = useRef<PendingMutation | null>(null);
  const [server, setServer] = useState<ManagedContainer | null>(null);
  const [serverState, setServerState] = useState<"loading" | "ready" | "error">(
    "loading",
  );
  const [serverError, setServerError] = useState<string | null>(null);
  const [serverReload, setServerReload] = useState(0);
  const [location, setLocation] = useState<FileLocation | null>(null);
  const [listing, setListing] = useState<FolderListing | null>(null);
  const [busy, setBusy] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [selectedAction, setSelectedAction] = useState<SelectedAction | null>(
    null,
  );
  const [dialogError, setDialogError] = useState<string | null>(null);
  const [actionDraft, setActionDraft] = useState<ActionDraft | null>(null);
  const [uploadProgress, setUploadProgress] = useState<UploadProgress | null>(
    null,
  );
  const canRead = can(user, server, "files.read");
  const canManage = can(user, server, "files.write");
  const bindingBlocked = Boolean(server && server.bindingStatus !== "active");
  const currentListing =
    location && listing && sameLocation(location, listing.location)
      ? listing
      : null;
  const loading = Boolean(
    location && (!currentListing || currentListing.state === "loading"),
  );
  const listingReady = currentListing?.state === "ready";
  const entries = listingReady ? currentListing.entries : [];
  const canChangeFiles = canManage && !bindingBlocked && listingReady && !busy;

  useEffect(() => {
    return () => {
      listingRequest.current?.abort();
      listingRequest.current = null;
      mutation.current?.controller.abort();
      mutation.current = null;
    };
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    listingRequest.current?.abort();
    listingRequest.current = null;
    mutation.current?.controller.abort();
    mutation.current = null;
    setServer(null);
    setServerState("loading");
    setServerError(null);
    setLocation(null);
    setListing(null);
    setBusy(false);
    setDragging(false);
    setError(null);
    setNotice(null);
    setSelectedAction(null);
    setActionDraft(null);
    setUploadProgress(null);

    apiJson(
      `/servers/${encodeURIComponent(containerId)}`,
      serverResponseSchema,
      {
        signal: controller.signal,
      },
    )
      .then(({ server: nextServer }) => {
        if (controller.signal.aborted) return;
        setServer(nextServer);
        setServerState("ready");
        if (!can(user, nextServer, "files.read")) {
          setServerError("You do not have file access to this server.");
          return;
        }
        const root = nextServer.fileRoots[0];
        if (root) setLocation({ root: root.id, path: "" });
      })
      .catch((reason) => {
        if (controller.signal.aborted) return;
        setServerState("error");
        setServerError(
          reason instanceof Error ? reason.message : "Unable to load server.",
        );
      });
    return () => controller.abort();
  }, [containerId, serverReload, user]);

  const refresh = useCallback(
    async (target: FileLocation) => {
      listingRequest.current?.abort();
      const controller = new AbortController();
      listingRequest.current = controller;
      setListing({
        location: target,
        state: "loading",
        entries: [],
        error: null,
      });
      const ownsRequest = () =>
        listingRequest.current === controller && !controller.signal.aborted;
      const query = new URLSearchParams({
        ...target,
      } satisfies FileLocationRequest);
      try {
        const next = await apiJson(
          `/servers/${encodeURIComponent(containerId)}/files?${query}`,
          fileListingSchema,
          { signal: controller.signal },
        );
        if (!ownsRequest()) return;
        if (next.root.id !== target.root || next.path !== target.path)
          throw new Error(
            "The folder response did not match the requested location. Try again.",
          );
        setListing({
          location: target,
          state: "ready",
          entries: next.entries,
          error: null,
        });
      } catch (reason) {
        if (!ownsRequest()) return;
        if (reason instanceof ApiRequestError && reason.status === 403) {
          setLocation(null);
          setListing(null);
          setServer(
            (current) =>
              current && {
                ...current,
                fileRoots: [],
                permissions: current.permissions.filter(
                  (permission) =>
                    permission !== "files.read" && permission !== "files.write",
                ),
              },
          );
          setServerState("error");
          setServerError(reason.message);
          return;
        }
        setListing({
          location: target,
          state: "error",
          entries: [],
          error:
            reason instanceof Error ? reason.message : "Unable to list files.",
        });
      } finally {
        if (listingRequest.current === controller)
          listingRequest.current = null;
      }
    },
    [containerId],
  );

  useEffect(() => {
    if (!location) return;
    void refresh(location);
    return () => listingRequest.current?.abort();
  }, [location, refresh]);

  const changeLocation = (target: FileLocation) => {
    if (mutation.current || selectedAction) return;
    setError(null);
    setNotice(null);
    setDragging(false);
    setActionDraft(null);
    setLocation(target);
  };

  const openAction = (action: FileAction) => {
    if (!canChangeFiles || mutation.current || !location) return;
    const root = server?.fileRoots.find((item) => item.id === location.root);
    const initialName =
      actionDraft &&
      sameLocation(actionDraft.location, location) &&
      actionDraft.kind === action.kind &&
      (action.kind === "create" || action.entry.name === actionDraft.sourceName)
        ? actionDraft.name
        : undefined;
    setSelectedAction({
      action,
      location: { ...location },
      folderLabel: joinPath(root?.name || "Files", location.path),
      initialName,
    });
    setActionDraft(null);
    setDialogError(null);
    setError(null);
    setNotice(null);
  };

  const submitAction = async (name: string) => {
    if (
      !selectedAction ||
      !location ||
      !canChangeFiles ||
      mutation.current ||
      !sameLocation(selectedAction.location, location)
    )
      return;
    const { action, location: target } = selectedAction;
    if (action.kind !== "delete" && (!name.trim() || name.length > 255)) return;
    if (action.kind === "rename" && name === action.entry.name) return;
    const owner: PendingMutation = {
      kind: "action",
      controller: new AbortController(),
    };
    mutation.current = owner;
    const ownsMutation = () => mutation.current === owner;
    setBusy(true);
    setDialogError(null);
    const base = `/api/v1/servers/${encodeURIComponent(containerId)}/files`;
    try {
      let response: Response;
      let success: string;
      if (action.kind === "create") {
        response = await apiFetch(`${base}/directory`, {
          ...jsonBody("POST", {
            ...target,
            name,
          } satisfies CreateDirectoryRequest),
          signal: owner.controller.signal,
        });
        success = `Created “${name}”.`;
      } else if (action.kind === "rename") {
        response = await apiFetch(`${base}/rename`, {
          ...jsonBody("PATCH", {
            root: target.root,
            path: joinPath(target.path, action.entry.name),
            newName: name,
          } satisfies RenameFileRequest),
          signal: owner.controller.signal,
        });
        success = `Renamed “${action.entry.name}” to “${name}”.`;
      } else {
        const query = new URLSearchParams({
          root: target.root,
          path: joinPath(target.path, action.entry.name),
        } satisfies FileLocationRequest);
        response = await apiFetch(`${base}?${query}`, {
          method: "DELETE",
          signal: owner.controller.signal,
        });
        success = `Deleted “${action.entry.name}”.`;
      }
      await apiResponse(response, okResponseSchema);
      if (!ownsMutation()) return;
      setNotice(success);
      setActionDraft(null);
      setSelectedAction(null);
      await refresh(target);
    } catch (reason) {
      if (!ownsMutation() || owner.controller.signal.aborted) return;
      if (
        reason instanceof ApiRequestError &&
        reason.status >= 400 &&
        reason.status < 500
      ) {
        setDialogError(reason.message);
      } else {
        // A write may have completed before its response was lost or became
        // invalid. Keep only the name draft, never a sendable old confirmation.
        setActionDraft(
          action.kind === "delete"
            ? null
            : {
                kind: action.kind,
                location: target,
                sourceName:
                  action.kind === "rename" ? action.entry.name : null,
                name,
              },
        );
        setSelectedAction(null);
        setDialogError(null);
        const targetName = action.kind === "create" ? name : action.entry.name;
        setError(
          `The result for “${targetName}” could not be confirmed. The change may have completed. Review the folder before starting another action.`,
        );
        await refresh(target);
      }
    } finally {
      if (ownsMutation()) {
        mutation.current = null;
        setBusy(false);
      }
    }
  };

  const uploadFiles = async (files: File[]) => {
    if (
      !canChangeFiles ||
      !location ||
      selectedAction ||
      mutation.current ||
      files.length === 0
    )
      return;
    const target = { ...location };
    const owner: PendingMutation = {
      kind: "upload",
      controller: new AbortController(),
    };
    mutation.current = owner;
    const ownsMutation = () => mutation.current === owner;
    setBusy(true);
    setError(null);
    setNotice(null);
    let completed = 0;
    let currentName = files[0].name;
    try {
      for (const file of files) {
        owner.controller.signal.throwIfAborted();
        currentName = file.name;
        setUploadProgress({
          completed,
          total: files.length,
          name: currentName,
          phase: "uploading",
        });
        const query = new URLSearchParams({
          ...target,
          name: file.name,
        } satisfies UploadFileQuery);
        const response = await apiFetch(
          `/api/v1/servers/${encodeURIComponent(containerId)}/files/upload?${query}`,
          {
            method: "PUT",
            headers: { "Content-Type": "application/octet-stream" },
            body: file,
            signal: owner.controller.signal,
          },
        );
        await apiResponse(response, okResponseSchema);
        if (!ownsMutation()) return;
        completed += 1;
        owner.controller.signal.throwIfAborted();
      }
      setNotice(`${completed} file${completed === 1 ? "" : "s"} uploaded.`);
    } catch (reason) {
      if (!ownsMutation()) return;
      if (owner.controller.signal.aborted)
        setNotice(
          `Upload canceled. ${completed} of ${files.length} files confirmed complete. Check “${currentName}” before retrying; canceling does not undo data already written.`,
        );
      else
        setError(
          `Uploaded ${completed} of ${files.length} files. Could not upload “${currentName}”: ${reason instanceof Error ? reason.message : "Upload failed."}`,
        );
    } finally {
      if (ownsMutation()) {
        setUploadProgress({
          completed,
          total: files.length,
          name: currentName,
          phase: "refreshing",
        });
        // Listing errors are separate from upload errors, so a partial failure
        // stays visible while the successfully uploaded files are refreshed.
        await refresh(target);
        if (ownsMutation()) {
          mutation.current = null;
          setBusy(false);
          setUploadProgress(null);
          if (fileInput.current) fileInput.current.value = "";
        }
      }
    }
  };

  const downloadUrl = (entry: FileEntry) => {
    const query = new URLSearchParams({
      root: location!.root,
      path: joinPath(location!.path, entry.name),
    } satisfies FileLocationRequest);
    return `/api/v1/servers/${encodeURIComponent(containerId)}/files/download?${query}`;
  };

  const pathParts = location?.path ? location.path.split("/") : [];
  const selectedRoot = server?.fileRoots.find(
    (root) => root.id === location?.root,
  );

  return (
    <div className="page files-page">
      <div className="files-header">
        <div className="files-header__title">
          <NavLink
            className="secondary-btn files-back"
            to={`/servers/${encodeURIComponent(containerId)}`}
            aria-label="Back to server"
            aria-disabled={busy || undefined}
            tabIndex={busy ? -1 : undefined}
            onClick={(event) => {
              if (mutation.current) event.preventDefault();
            }}
          >
            <span aria-hidden="true">←</span>
          </NavLink>
          <div>
            <div className="files-header__name">
              <h1 className="page__title" id="files-page-title" tabIndex={-1}>
                {server?.displayName || "Server files"}
              </h1>
              {server && (
                <span className={`status-badge status-badge--${server.state}`}>
                  <span className="status-dot" aria-hidden="true" />
                  {server.state}
                </span>
              )}
            </div>
            <p className="page__subtitle">
              Worlds, configuration, backups, and mods
            </p>
          </div>
        </div>
        {canRead && server && server.fileRoots.length > 1 && (
          <label className="files-root">
            Storage location
            <select
              value={location?.root || ""}
              onChange={(event) =>
                changeLocation({ root: event.target.value, path: "" })
              }
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

      {serverError && (
        <div className="alert alert--error" role="alert">
          <span>{serverError}</span>
          {serverState === "error" && (
            <button
              className="text-link"
              onClick={() => setServerReload((value) => value + 1)}
            >
              Try again
            </button>
          )}
        </div>
      )}
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
      {serverState === "loading" && (
        <p className="muted" role="status">
          Loading server files…
        </p>
      )}
      {serverState === "ready" && canRead && !location && (
        <div className="empty-state">
          <h2 className="empty-state__title">File access is not configured</h2>
          <p className="empty-state__description">
            No approved mounts are available. An administrator can check the
            mount restrictions or ludock.files label.
          </p>
        </div>
      )}
      {location && canRead && (
        <section className="file-manager" aria-busy={busy || loading}>
          <div className="file-toolbar">
            <nav className="file-breadcrumbs" aria-label="Current folder">
              <button
                disabled={busy}
                onClick={() =>
                  changeLocation({ root: location.root, path: "" })
                }
              >
                {selectedRoot?.name || "Files"}
              </button>
              {pathParts.map((part, index) => (
                <span key={`${part}-${index}`}>
                  <b aria-hidden="true">/</b>
                  <button
                    disabled={busy}
                    onClick={() =>
                      changeLocation({
                        root: location.root,
                        path: pathParts.slice(0, index + 1).join("/"),
                      })
                    }
                    aria-current={
                      index === pathParts.length - 1 ? "page" : undefined
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
                onClick={() => void refresh(location)}
                disabled={busy || loading}
              >
                Refresh
              </button>
              {canManage && (
                <button
                  className="secondary-btn"
                  onClick={() => openAction({ kind: "create" })}
                  disabled={!canChangeFiles}
                >
                  New folder
                </button>
              )}
            </div>
          </div>
          {bindingBlocked && (
            <p className="file-manager__note">
              File changes are unavailable until an administrator reviews this
              server’s identity.
            </p>
          )}
          {canManage && (
            <div
              className={`file-dropzone ${dragging ? "file-dropzone--active" : ""}`}
              aria-disabled={!canChangeFiles}
              onDragEnter={(event: DragEvent) => {
                event.preventDefault();
                if (canChangeFiles) setDragging(true);
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
                disabled={!canChangeFiles}
              >
                Choose files
              </button>
              <input
                ref={fileInput}
                type="file"
                aria-label="Upload files"
                multiple
                hidden
                disabled={!canChangeFiles}
                onChange={(event: ChangeEvent<HTMLInputElement>) =>
                  void uploadFiles(Array.from(event.target.files || []))
                }
              />
            </div>
          )}
          {uploadProgress && (
            <div className="file-upload-progress">
              <span role="status">
                {uploadProgress.phase === "refreshing"
                  ? "Refreshing folder…"
                  : uploadProgress.phase === "canceling"
                    ? "Canceling upload…"
                    : `Uploading ${uploadProgress.completed + 1} of ${uploadProgress.total}: ${uploadProgress.name}`}
              </span>
              {uploadProgress.phase !== "refreshing" && (
                <button
                  className="secondary-btn"
                  disabled={uploadProgress.phase === "canceling"}
                  onClick={() => {
                    if (mutation.current?.kind !== "upload") return;
                    setUploadProgress({
                      ...uploadProgress,
                      phase: "canceling",
                    });
                    mutation.current.controller.abort();
                  }}
                >
                  Cancel upload
                </button>
              )}
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
              <p className="file-list__empty" role="status">
                Loading folder…
              </p>
            ) : currentListing?.state === "error" ? (
              <div className="file-list__error">
                <p role="alert">
                  Unable to load this folder: {currentListing.error}
                </p>
                <button
                  className="secondary-btn"
                  onClick={() => void refresh(location)}
                  disabled={busy}
                >
                  Try again
                </button>
              </div>
            ) : entries.length === 0 ? (
              <p className="file-list__empty">This folder is empty.</p>
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
                        disabled={busy}
                        onClick={() =>
                          changeLocation({
                            root: location.root,
                            path: joinPath(location.path, entry.name),
                          })
                        }
                      >
                        {entry.name}
                      </button>
                    ) : (
                      <span>{entry.name}</span>
                    )}
                    {entry.type === "symlink" && (
                      <small title="Symbolic links cannot be opened">
                        symbolic link
                      </small>
                    )}
                  </div>
                  <span>
                    {entry.type === "file" ? formatByteSize(entry.size) : "—"}
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
                            onClick={() =>
                              openAction({ kind: "rename", entry })
                            }
                            disabled={!canChangeFiles}
                          >
                            Rename
                          </button>
                        )}
                        <button
                          className="secondary-btn secondary-btn--danger"
                          onClick={() => openAction({ kind: "delete", entry })}
                          disabled={!canChangeFiles}
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
      {selectedAction && (
        <FileActionDialog
          action={selectedAction.action}
          serverName={server?.displayName || "Server"}
          folderLabel={selectedAction.folderLabel}
          initialName={selectedAction.initialName}
          busy={busy}
          error={dialogError}
          onSubmit={(name) => void submitAction(name)}
          onCancel={() => {
            if (!mutation.current) setSelectedAction(null);
          }}
        />
      )}
    </div>
  );
}
