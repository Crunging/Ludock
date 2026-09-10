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
    let offset = 0;
    let safe = "";
    while (offset < limit) {
      const matched = this.secrets.find((secret) =>
        this.pending.startsWith(secret, offset),
      );
      if (matched) {
        safe += "[redacted]";
        offset += matched.length;
      } else {
        safe += this.pending[offset];
        offset += 1;
      }
    }
    this.pending = this.pending.slice(offset);
    if (safe) this.output(safe);
  }
}
