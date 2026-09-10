import { useEffect, useId, useRef } from "react";
import "./lifecycle-confirmation.css";

interface LifecycleConfirmationProps {
  action: "stop" | "restart";
  serverName: string;
  onConfirm: () => void;
  onCancel: () => void;
  fallbackFocusId?: string;
}

export default function LifecycleConfirmation({
  action,
  serverName,
  onConfirm,
  onCancel,
  fallbackFocusId,
}: LifecycleConfirmationProps) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const titleId = useId();
  const descriptionId = useId();
  const label = action === "stop" ? "Stop" : "Restart";

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    const previousFocus = document.activeElement;
    dialog.showModal();
    // Capture the trigger before moving focus; React autoFocus would run
    // during mount, before this effect can remember the original control.
    cancelRef.current?.focus();
    return () => {
      dialog.close();
      if (
        previousFocus instanceof HTMLElement &&
        previousFocus.isConnected &&
        !previousFocus.matches(":disabled")
      ) {
        previousFocus.focus();
      } else if (fallbackFocusId) {
        document.getElementById(fallbackFocusId)?.focus();
      }
    };
  }, [fallbackFocusId]);

  return (
    <dialog
      className="lifecycle-confirmation"
      ref={dialogRef}
      aria-labelledby={titleId}
      aria-describedby={descriptionId}
      onCancel={(event) => {
        event.preventDefault();
        onCancel();
      }}
    >
      <h2 id={titleId}>
        {label} {serverName}?
      </h2>
      <p id={descriptionId}>
        Connected players will be disconnected.
        {action === "stop" &&
          " The server will remain stopped until started again."}
      </p>
      <div className="lifecycle-confirmation__actions">
        <button className="secondary-btn" ref={cancelRef} onClick={onCancel}>
          Cancel
        </button>
        <button
          className="secondary-btn secondary-btn--danger"
          onClick={onConfirm}
        >
          {label} server
        </button>
      </div>
    </dialog>
  );
}
