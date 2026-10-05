import { useEffect, useId, useRef, useState } from "react";
import "./action-menu.css";

export interface ActionMenuItem {
  label: string;
  disabled?: boolean;
  danger?: boolean;
  /** A download link instead of a button. */
  href?: string;
  onSelect?: () => void;
}

interface ActionMenuProps {
  /** Accessible name for the trigger, such as "More actions for Valheim". */
  label: string;
  actions: ActionMenuItem[];
  /** Show a compact dots trigger for dense rows. */
  compact?: boolean;
}

// A disclosure with ordinary buttons keeps Tab navigation predictable. It is
// deliberately not an ARIA menu, which would require a different key model.
export default function ActionMenu({ label, actions, compact = false }: ActionMenuProps) {
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

  const close = () => {
    // Return focus before opening a dialog so its own focus cleanup can
    // return to a control that remains visible after dismissal.
    triggerRef.current?.focus();
    setOpen(false);
  };

  return (
    <div
      className="action-menu"
      ref={rootRef}
      onKeyDown={(event) => {
        if (event.key === "Escape" && open) {
          event.preventDefault();
          event.stopPropagation();
          close();
        }
      }}
    >
      <button
        type="button"
        className={`secondary-btn action-menu__trigger${compact ? " action-menu__trigger--compact" : ""}`}
        ref={triggerRef}
        aria-label={label}
        aria-expanded={open}
        aria-controls={contentId}
        onClick={() => setOpen(!open)}
      >
        {compact ? (
          <svg viewBox="0 0 16 16" aria-hidden="true" focusable="false">
            <circle cx="3" cy="8" r="1.5" /><circle cx="8" cy="8" r="1.5" /><circle cx="13" cy="8" r="1.5" />
          </svg>
        ) : <>More <span aria-hidden="true">⌄</span></>}
      </button>
      <div className="action-menu__content" id={contentId} hidden={!open}>
        {actions.map((action) => {
          const className = `action-menu__action${action.danger ? " action-menu__action--danger" : ""}`;
          return action.href ? (
            <a className={className} key={action.label} href={action.href} download onClick={close}>
              {action.label}
            </a>
          ) : (
            <button
              type="button"
              className={className}
              key={action.label}
              disabled={action.disabled}
              onClick={() => {
                close();
                action.onSelect?.();
              }}
            >
              {action.label}
            </button>
          );
        })}
      </div>
    </div>
  );
}
