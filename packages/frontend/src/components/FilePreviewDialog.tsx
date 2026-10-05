import { useEffect, useId, useRef, useState } from "react";
import { apiErrorSchema, formatByteSize, type FileEntry } from "@ludock/shared";
import { apiFetch } from "../api";
import { formatDateTime } from "../format";
import "./file-action-dialog.css";

/** Larger or binary files are downloaded instead of shown. */
const PREVIEW_LIMIT = 512 * 1024;

type Preview =
  | { state: "loading" }
  | { state: "ready"; text: string }
  | { state: "binary" }
  | { state: "large" }
  | { state: "error"; message: string };

interface Props {
  entry: FileEntry;
  path: string;
  downloadUrl: string;
  onClose: () => void;
}

export default function FilePreviewDialog({ entry, path, downloadUrl, onClose }: Props) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const closeRef = useRef<HTMLButtonElement>(null);
  const id = useId();
  const tooLarge = entry.size > PREVIEW_LIMIT;
  const [preview, setPreview] = useState<Preview>({ state: tooLarge ? "large" : "loading" });

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    const previousFocus = document.activeElement;
    dialog.showModal();
    closeRef.current?.focus();
    return () => {
      dialog.close();
      if (previousFocus instanceof HTMLElement && previousFocus.isConnected) previousFocus.focus();
      else document.getElementById("files-page-title")?.focus();
    };
  }, []);

  useEffect(() => {
    if (tooLarge) return;
    const controller = new AbortController();
    void (async () => {
      try {
        const response = await apiFetch(downloadUrl, { signal: controller.signal });
        if (!response.ok) {
          const error = apiErrorSchema.safeParse(await response.json().catch(() => undefined));
          throw new Error(error.success ? error.data.error : `Unable to open this file (HTTP ${response.status}).`);
        }
        const bytes = new Uint8Array(await response.arrayBuffer());
        if (controller.signal.aborted) return;
        if (bytes.byteLength > PREVIEW_LIMIT || bytes.includes(0)) {
          setPreview({ state: "binary" });
          return;
        }
        try {
          setPreview({ state: "ready", text: new TextDecoder("utf-8", { fatal: true }).decode(bytes) });
        } catch {
          setPreview({ state: "binary" });
        }
      } catch (reason) {
        if (controller.signal.aborted) return;
        setPreview({
          state: "error",
          message: reason instanceof Error ? reason.message : "Unable to open this file.",
        });
      }
    })();
    return () => controller.abort();
  }, [downloadUrl, tooLarge]);

  return (
    <dialog
      className="file-action-dialog file-preview"
      ref={dialogRef}
      aria-labelledby={`${id}-title`}
      aria-describedby={`${id}-description`}
      onCancel={(event) => {
        event.preventDefault();
        onClose();
      }}
    >
      <div className="file-preview__header">
        <div>
          <h2 id={`${id}-title`}>{entry.name}</h2>
          <p id={`${id}-description`} className="file-preview__meta">
            <code>{path}</code>
            <span>{formatByteSize(entry.size)}</span>
            {entry.modifiedAt ? <span>Modified {formatDateTime(entry.modifiedAt)}</span> : null}
          </p>
        </div>
      </div>
      <div className="file-preview__body" aria-busy={preview.state === "loading"}>
        {preview.state === "loading" && <p className="muted" role="status">Opening file…</p>}
        {preview.state === "ready" && (
          preview.text
            ? <pre tabIndex={0} aria-label={`Contents of ${entry.name}`}>{preview.text}</pre>
            : <p className="muted">This file is empty.</p>
        )}
        {preview.state === "large" && (
          <p className="muted">Files over {formatByteSize(PREVIEW_LIMIT)} aren’t shown here. Download it to open it.</p>
        )}
        {preview.state === "binary" && (
          <p className="muted">This file can’t be shown as text. Download it to open it.</p>
        )}
        {preview.state === "error" && (
          <div className="alert alert--error" role="alert">{preview.message}</div>
        )}
      </div>
      <div className="file-action-dialog__actions">
        <a className="secondary-btn" href={downloadUrl} download>Download</a>
        <button type="button" className="primary-btn" ref={closeRef} onClick={onClose}>
          Close
        </button>
      </div>
    </dialog>
  );
}
