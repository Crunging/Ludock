import type { ServerObservation } from "./identity.js";

export function observationSecrets(observation: ServerObservation): string[] {
  const configuration = observation.gameConfiguration ?? {};
  const customPasswordVariable = configuration["ludock.console.password-env"];
  return [
    ...new Set(
      Object.entries(configuration)
        .filter(
          ([key]) =>
            /password|passwd|secret|token|credential|rconpw/i.test(key) ||
            key === `env:${customPasswordVariable}` ||
            key === customPasswordVariable,
        )
        .filter(([key]) => key !== "ludock.console.password-env")
        .map(([, value]) => value)
        .filter(Boolean),
    ),
  ].sort((left, right) => right.length - left.length);
}

/** Keep enough trailing text to redact a credential split across Docker frames.
 * Credentials themselves never reach the browser, even one fragment at a time. */
export class ConsoleOutputRedactor {
  private pending = "";
  private readonly tailLength: number;

  constructor(
    private readonly secrets: readonly string[],
    private readonly output: (value: string) => void,
  ) {
    this.tailLength = Math.max(
      0,
      ...secrets.map((secret) => secret.length - 1),
    );
  }

  push(value: string): void {
    this.pending += value;
    this.emit(false);
  }

  end(): void {
    this.emit(true);
  }

  private emit(final: boolean): void {
    const limit = final
      ? this.pending.length
      : Math.max(0, this.pending.length - this.tailLength);
    if (limit === 0) return;
    // Keep each search result for this chunk. Otherwise a frequent short secret
    // would repeatedly rescan the whole suffix for another, absent credential.
    const positions = this.secrets.map(secret => secret ? this.pending.indexOf(secret) : -1);
    let offset = 0;
    let safe = "";
    while (offset < limit) {
      let next = limit;
      let matched = "";
      // Native string searches skip ordinary log text in one pass. At the same
      // offset, retain the supplied secret order (longest first in production).
      for (let index = 0; index < this.secrets.length; index++) {
        const secret = this.secrets[index];
        let found = positions[index];
        if (found !== -1 && found < offset)
          positions[index] = found = this.pending.indexOf(secret, offset);
        if (found !== -1 && found < next) {
          next = found;
          matched = secret;
          if (next === offset) break;
        }
      }
      safe += this.pending.slice(offset, next);
      offset = next;
      if (!matched) break;
      safe += "[redacted]";
      offset += matched.length;
    }
    this.pending = this.pending.slice(offset);
    if (safe) this.output(safe);
  }
}
