export type PipTone = "ok" | "failed" | "attention" | "active" | "idle";

/** Decorative state marker; the adjacent text always names the state. */
export default function StatusPip({ tone }: { tone: PipTone }) {
  return <span className={`status-pip status-pip--${tone}`} aria-hidden="true" />;
}
