import { inferGameType } from "./server-presets.js";

export const LABEL_ENABLE = "ludock.enable";
export const LABEL_NAME = "ludock.name";
export const LABEL_GAME = "ludock.game";
export const LABEL_COMPOSE_ONEOFF = "com.docker.compose.oneoff";

export type EligibilityReason =
  | "invalid-enable-label"
  | "opted-out"
  | "explicitly-enabled"
  | "compose-oneoff"
  | "recognized-image"
  | "unrecognized-image";

export interface ContainerEligibility {
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

// Do not pass arbitrary ludock.* labels to clients: labels are often used to
// store third-party integration secrets, and are not a safe metadata bag.
const CONFIGURATION_LABELS = new Set([
  LABEL_ENABLE,
  LABEL_NAME,
  LABEL_GAME,
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
        // eslint-disable-next-line no-control-regex
        !/[\u0000-\u001f\u007f]/.test(value),
    ) || !/^[1-9][0-9]*$/.test(compose.containerNumber)
  );
}
