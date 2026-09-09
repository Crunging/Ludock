const byteUnits: Record<string, bigint> = {
  b: 1n,
  kb: 1000n,
  mb: 1000n ** 2n,
  gb: 1000n ** 3n,
  tb: 1000n ** 4n,
  kib: 1024n,
  mib: 1024n ** 2n,
  gib: 1024n ** 3n,
  tib: 1024n ** 4n,
};
const binaryUnits = ["B", "KiB", "MiB", "GiB", "TiB"];

/** Parse plain decimal sizes without rounding away fractions of a byte. */
export function parseByteSize(value: string): number | null {
  const match = /^(\d+(?:\.\d*)?|\.\d+)\s*(B|[KMGT]i?B)?$/i.exec(value.trim());
  if (!match) return null;
  const [whole, fraction = ""] = match[1].split(".");
  const numerator = BigInt(whole + fraction) * byteUnits[(match[2] || "b").toLowerCase()];
  const denominator = 10n ** BigInt(fraction.length);
  if (numerator % denominator !== 0n) return null;
  const bytes = numerator / denominator;
  return bytes <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(bytes) : null;
}

/** Rounded display text; use the original byte count when saving settings. */
export function formatByteSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "—";
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < binaryUnits.length - 1) {
    value /= 1024;
    unit++;
  }
  let rounded = Number(value.toFixed(2));
  if (rounded === 1024 && unit < binaryUnits.length - 1) {
    rounded = 1;
    unit++;
  }
  return `${rounded} ${binaryUnits[unit]}`;
}
