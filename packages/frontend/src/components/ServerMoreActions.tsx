import { useEffect, useId, useRef, useState } from "react";

interface ServerMoreAction {
  label: string;
  disabled?: boolean;
  onSelect: () => void;
}

interface ServerMoreActionsProps {
  serverName: string;
  actions: ServerMoreAction[];
}

// A disclosure with ordinary buttons keeps Tab navigation predictable. It is
// deliberately not an ARIA menu, which would require a different key model.
export default function ServerMoreActions({
  serverName,
  actions,
}: ServerMoreActionsProps) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const contentId = useId();

  useEffect(() => {
    if (!open) return;
    const closeOutside = (event: Event) => {
      if (
        event.target instanceof Node &&
        !rootRef.current?.contains(event.target)
      ) setOpen(false);
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("focusin", closeOutside);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("focusin", closeOutside);
    };
  }, [open]);

  return (
    <div
      className="server-more"
      ref={rootRef}
      onKeyDown={(event) => {
        if (event.key === "Escape" && open) {
          event.preventDefault();
          event.stopPropagation();
          setOpen(false);
          triggerRef.current?.focus();
        }
      }}
    >
      <button
        className="secondary-btn server-more__trigger"
        ref={triggerRef}
        aria-label={`More actions for ${serverName}`}
        aria-expanded={open}
        aria-controls={contentId}
        onClick={() => setOpen(!open)}
      >
        More <span aria-hidden="true">⌄</span>
      </button>
      <div className="server-more__content" id={contentId} hidden={!open}>
        {actions.map((action) => (
          <button
            className="server-more__action"
            key={action.label}
            disabled={action.disabled}
            onClick={() => {
              // Return focus before opening a dialog so its own focus cleanup
              // can return to a control that remains visible after dismissal.
              triggerRef.current?.focus();
              setOpen(false);
              action.onSelect();
            }}
          >
            {action.label}
          </button>
        ))}
      </div>
    </div>
  );
}
