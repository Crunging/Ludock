import { connectionHostSchema } from "@ludock/shared";
import { inferGameType } from "./server-presets.js";

export const LABEL_ENABLE = "ludock.enable";
export const LABEL_NAME = "ludock.name";
export const LABEL_GAME = "ludock.game";
export const LABEL_ADDRESS = "ludock.address";
const LABEL_COMPOSE_ONEOFF = "com.docker.compose.oneoff";

type EligibilityReason =
  | "invalid-enable-label"
  | "opted-out"
  | "explicitly-enabled"
  | "compose-oneoff"
  | "recognized-image"
  | "unrecognized-image";

interface ContainerEligibility {
  eligible: boolean;
  reason: EligibilityReason;
  recognizedGameType: string;
}

export function parseBooleanLabel(
  value: string | undefined,
): boolean | undefined {
  const normalized = value?.trim().toLowerCase();
  return normalized === "true"
    ? true
    : normalized === "false"
      ? false
      : undefined;
}

/** The same precedence applies to discovery, inspection, and every mutation. */
export function evaluateContainerEligibility(
  image: string,
  labels: Readonly<Record<string, string>> = {},
): ContainerEligibility {
  const recognizedGameType = inferGameType(image);
  const enable = parseBooleanLabel(labels[LABEL_ENABLE]);
  if (Object.hasOwn(labels, LABEL_ENABLE)) {
    if (enable === undefined)
      return {
        eligible: false,
        reason: "invalid-enable-label",
        recognizedGameType,
      };
    if (!enable)
      return { eligible: false, reason: "opted-out", recognizedGameType };
    return { eligible: true, reason: "explicitly-enabled", recognizedGameType };
  }
  if (parseBooleanLabel(labels[LABEL_COMPOSE_ONEOFF]) === true) {
    return { eligible: false, reason: "compose-oneoff", recognizedGameType };
  }
  return recognizedGameType === "unknown"
    ? { eligible: false, reason: "unrecognized-image", recognizedGameType }
    : { eligible: true, reason: "recognized-image", recognizedGameType };
}

/**
 * The exact address players type: a host name or IP address with an optional
 * port, such as `mc.example.com`, `203.0.113.10:30000`, or `[2001:db8::1]:2456`.
 * Returns null for a missing or invalid value, which falls back to detection.
 */
export function parseAddressLabel(
  value: string | undefined,
): { host: string; port: number | null } | null {
  const text = value?.trim() ?? "";
  if (!text || text.length > 300) return null;
  const bracketed = /^\[([^\]]+)\](?::([0-9]+))?$/.exec(text);
  const hostAndPort = /^([^:[\]]+):([0-9]+)$/.exec(text);
  const [host, port] = bracketed
    ? [bracketed[1], bracketed[2]]
    : hostAndPort
      ? [hostAndPort[1], hostAndPort[2]]
      : [text, undefined];
  // Brackets only ever wrap IPv6 addresses.
  if (bracketed && !host.includes(":")) return null;
  const parsedHost = connectionHostSchema.safeParse(host);
  if (!parsedHost.success) return null;
  if (port === undefined) return { host: parsedHost.data, port: null };
  const number = Number(port);
  return Number.isInteger(number) && number >= 1 && number <= 65_535
    ? { host: parsedHost.data, port: number }
    : null;
}

// Do not pass arbitrary ludock.* labels to clients: labels are often used to
// store third-party integration secrets, and are not a safe metadata bag.
const CONFIGURATION_LABELS = new Set([
  LABEL_ENABLE,
  LABEL_NAME,
  LABEL_GAME,
  LABEL_ADDRESS,
  "ludock.files",
  "ludock.console",
  "ludock.console.port",
  "ludock.console.host",
  "ludock.console.password-env",
]);

export function approvedConfigurationLabels(
  labels: Readonly<Record<string, string>>,
): Record<string, string> {
  return Object.fromEntries(
    Object.entries(labels).filter(([key]) => CONFIGURATION_LABELS.has(key)),
  );
}

/** Explicit one-offs have their own standalone name, separate from a service replica. */
export function composeIdentityLabels(
  labels: Readonly<Record<string, string>>,
): { project: string; service: string; containerNumber: string } | undefined {
  if (parseBooleanLabel(labels[LABEL_COMPOSE_ONEOFF]) === true)
    return undefined;
  const project = labels["com.docker.compose.project"];
  const service = labels["com.docker.compose.service"];
  const containerNumber = labels["com.docker.compose.container-number"];
  if (
    project === undefined &&
    service === undefined &&
    containerNumber === undefined
  )
    return undefined;
  return {
    project: project || "",
    service: service || "",
    containerNumber: containerNumber || "",
  };
}

export function hasInvalidComposeIdentity(
  labels: Readonly<Record<string, string>>,
): boolean {
  const compose = composeIdentityLabels(labels);
  if (!compose) return false;
  // Control characters are invalid identity data and must fail closed.
  return (
    ![compose.project, compose.service].every(
      (value) =>
        value.length > 0 &&
        value.length <= 512 &&
        // oxlint-disable-next-line no-control-regex
        !/[\u0000-\u001f\u007f]/.test(value),
    ) || !/^[1-9][0-9]*$/.test(compose.containerNumber)
  );
}
