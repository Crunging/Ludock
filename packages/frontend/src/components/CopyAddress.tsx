import { useEffect, useState } from "react";
import "./copy-address.css";

async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // The Clipboard API needs HTTPS or localhost; plain-HTTP LAN panels use a selection.
    const field = document.createElement("textarea");
    field.value = text;
    field.setAttribute("readonly", "");
    field.style.position = "fixed";
    field.style.opacity = "0";
    document.body.append(field);
    field.select();
    const copied = document.execCommand("copy");
    field.remove();
    return copied;
  }
}

/** The address players connect to, with a button that copies it. */
export default function CopyAddress({ address, serverName }: { address: string; serverName: string }) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  useEffect(() => {
    if (state === "idle") return;
    const timer = window.setTimeout(() => setState("idle"), 2000);
    return () => window.clearTimeout(timer);
  }, [state]);
  return (
    <span className="copy-address">
      <code>{address}</code>
      <button
        type="button"
        className={`copy-address__button${state === "idle" ? "" : " copy-address__button--done"}`}
        aria-label={`Copy address for ${serverName}`}
        title={state === "copied" ? "Copied" : state === "failed" ? "Copy failed" : "Copy address"}
        onClick={() => void copyText(address).then((copied) => setState(copied ? "copied" : "failed"))}
      >
        {state === "copied" ? (
          <svg viewBox="0 0 16 16" aria-hidden="true" focusable="false"><path d="M3.5 8.5l3 3 6-7" /></svg>
        ) : (
          <svg viewBox="0 0 16 16" aria-hidden="true" focusable="false">
            <rect x="5.5" y="5.5" width="8" height="8" rx="1" /><path d="M10.5 5.5v-2a1 1 0 0 0-1-1h-6a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h2" />
          </svg>
        )}
      </button>
      <span className="sr-only" role="status">
        {state === "copied" ? `Copied ${address}` : state === "failed" ? "Copy failed. Select the address to copy it." : ""}
      </span>
    </span>
  );
}
