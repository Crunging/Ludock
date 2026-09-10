import { useEffect, useId, useRef, type ReactNode } from "react";

interface Props {
  label: string;
  tabs: { id: string; label: string }[];
  activeId: string;
  onChange: (id: string) => void;
  children: ReactNode;
}

/** These panels are already loaded, so arrow keys can select them immediately. */
export default function SectionTabs({
  label,
  tabs,
  activeId,
  onChange,
  children,
}: Props) {
  const id = useId();
  const buttons = useRef(new Map<string, HTMLButtonElement>());
  const panels = useRef(new Map<string, HTMLElement>());
  const previousActiveId = useRef(activeId);

  useEffect(() => {
    // Actions such as "Recreate anyway" remove their own focused control when
    // opening another panel. Keep keyboard users in the new panel in that case.
    if (
      previousActiveId.current !== activeId &&
      document.activeElement === document.body
    ) {
      panels.current.get(activeId)?.focus();
    }
    previousActiveId.current = activeId;
  }, [activeId]);

  return (
    <>
      <div className="section-tabs" role="tablist" aria-label={label}>
        {tabs.map((tab, index) => (
          <button
            type="button"
            role="tab"
            key={tab.id}
            id={`${id}-tab-${tab.id}`}
            aria-controls={`${id}-panel-${tab.id}`}
            aria-selected={activeId === tab.id}
            tabIndex={activeId === tab.id ? 0 : -1}
            ref={(element) => {
              if (element) buttons.current.set(tab.id, element);
              else buttons.current.delete(tab.id);
            }}
            onClick={() => onChange(tab.id)}
            onKeyDown={(event) => {
              let nextIndex: number;
              switch (event.key) {
                case "ArrowRight":
                  nextIndex = (index + 1) % tabs.length;
                  break;
                case "ArrowLeft":
                  nextIndex = (index + tabs.length - 1) % tabs.length;
                  break;
                case "Home":
                  nextIndex = 0;
                  break;
                case "End":
                  nextIndex = tabs.length - 1;
                  break;
                default:
                  return;
              }
              event.preventDefault();
              const next = tabs[nextIndex];
              onChange(next.id);
              buttons.current.get(next.id)?.focus();
            }}
          >
            {tab.label}
          </button>
        ))}
      </div>
      {tabs.map((tab) => (
        <section
          key={tab.id}
          role="tabpanel"
          id={`${id}-panel-${tab.id}`}
          aria-labelledby={`${id}-tab-${tab.id}`}
          hidden={activeId !== tab.id}
          tabIndex={0}
          className="section-tab-panel"
          ref={(element) => {
            if (element) panels.current.set(tab.id, element);
            else panels.current.delete(tab.id);
          }}
        >
          {activeId === tab.id ? children : null}
        </section>
      ))}
    </>
  );
}
