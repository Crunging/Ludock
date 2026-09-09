interface LudockMarkProps {
  className?: string;
  label?: string;
}

/** Four equal tiles form an L. Omit label when adjacent text names the app. */
export default function LudockMark({ className, label }: LudockMarkProps) {
  return (
    <svg
      className={className}
      viewBox="0 0 64 64"
      fill="currentColor"
      role={label ? "img" : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
      focusable="false"
    >
      <rect x="17" y="9" width="14" height="14" rx="1.5" />
      <rect x="17" y="25" width="14" height="14" rx="1.5" />
      <rect x="17" y="41" width="14" height="14" rx="1.5" />
      <rect x="33" y="41" width="14" height="14" rx="1.5" />
    </svg>
  );
}
