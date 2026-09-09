import { useEffect, useId, useRef, useState } from "react";
import type { FileEntry } from "@ludock/shared";
import "./file-action-dialog.css";

export type FileAction =
  | { kind: "create" }
  | { kind: "rename" | "delete"; entry: FileEntry };

interface Props {
  action: FileAction;
  serverName: string;
  folderLabel: string;
  initialName?: string;
  busy: boolean;
  error: string | null;
  onSubmit: (name: string) => void;
  onCancel: () => void;
}

export default function FileActionDialog({
  action,
  serverName,
  folderLabel,
  initialName,
  busy,
  error,
  onSubmit,
  onCancel,
}: Props) {
  const [name, setName] = useState(
    initialName ?? (action.kind === "rename" ? action.entry.name : ""),
  );
  const dialogRef = useRef<HTMLDialogElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const id = useId();
  const deleting = action.kind === "delete";
  const title =
    action.kind === "create"
      ? "New folder"
      : `${deleting ? "Delete" : "Rename"} “${action.entry.name}”${deleting ? "?" : ""}`;
  const submitLabel =
    action.kind === "create"
      ? "Create folder"
      : action.kind === "rename"
        ? "Rename"
        : action.entry.type === "directory"
          ? "Delete folder"
          : action.entry.type === "symlink"
            ? "Delete link"
            : "Delete file";

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    const previousFocus = document.activeElement;
    dialog.showModal();
    if (deleting) cancelRef.current?.focus();
    else {
      inputRef.current?.focus();
      inputRef.current?.select();
    }
    return () => {
      dialog.close();
      if (
        previousFocus instanceof HTMLElement &&
        previousFocus.isConnected &&
        !previousFocus.matches(":disabled")
      ) {
        previousFocus.focus();
      } else {
        document.getElementById("files-page-title")?.focus();
      }
    };
  }, [deleting]);

  return (
    <dialog
      className="file-action-dialog"
      ref={dialogRef}
      aria-labelledby={`${id}-title`}
      aria-describedby={`${id}-description`}
      onCancel={(event) => {
        event.preventDefault();
        if (!busy) onCancel();
      }}
    >
      <form
        onSubmit={(event) => {
          event.preventDefault();
          if (!busy) onSubmit(name);
        }}
        aria-busy={busy}
      >
        <h2 id={`${id}-title`}>{title}</h2>
        <div
          id={`${id}-description`}
          className="file-action-dialog__description"
        >
          <p>
            {serverName} · {folderLabel}
          </p>
          {deleting && (
            <p>
              {action.entry.type === "directory"
                ? "This permanently deletes the folder and everything inside it."
                : action.entry.type === "symlink"
                  ? "This permanently deletes the symbolic link. Its target is not changed."
                  : "This permanently deletes the file."}
            </p>
          )}
        </div>
        {!deleting && (
          <label className="file-action-dialog__field">
            {action.kind === "create" ? "Folder name" : "New name"}
            <input
              ref={inputRef}
              value={name}
              onChange={(event) => setName(event.target.value)}
              required
              maxLength={255}
              disabled={busy}
              aria-describedby={error ? `${id}-error` : undefined}
              autoComplete="off"
              spellCheck={false}
            />
          </label>
        )}
        {error && (
          <div className="alert alert--error" role="alert" id={`${id}-error`}>
            {error}
          </div>
        )}
        {busy && (
          <p className="file-action-dialog__status" role="status">
            Working…
          </p>
        )}
        <div className="file-action-dialog__actions">
          <button
            type="button"
            className="secondary-btn"
            ref={cancelRef}
            onClick={onCancel}
            disabled={busy}
          >
            Cancel
          </button>
          <button
            type="submit"
            className={
              deleting ? "secondary-btn secondary-btn--danger" : "primary-btn"
            }
            disabled={
              busy ||
              (!deleting && !name.trim()) ||
              (action.kind === "rename" && name === action.entry.name)
            }
          >
            {submitLabel}
          </button>
        </div>
      </form>
    </dialog>
  );
}
