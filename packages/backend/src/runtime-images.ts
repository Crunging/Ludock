import { docker } from "./docker-client.js";
import { AppError } from "./errors.js";

// Reviewed multi-platform Ludock runtime for native runs and custom hostnames.
export const FALLBACK_HELPER_IMAGE = "ghcr.io/crunging/ludock@sha256:87806cbb9082cae6f2ce55d6c26e35f1cd28d97a6a2acd92da483f96ffdeeb67";

let selfImage: { container: string; result: Promise<string> } | undefined;

/** Reuse this deployment's exact, patched runtime without another registry pull. */
export async function resolveHelperImage(): Promise<string> {
  if (process.env.FILE_HELPER_IMAGE) return validateHelperImage(process.env.FILE_HELPER_IMAGE);
  const container = process.env.LUDOCK_SELF_CONTAINER || process.env.HOSTNAME;
  if (!container) return FALLBACK_HELPER_IMAGE;
  const unavailable = () => new AppError("HELPER_IMAGE_UNAVAILABLE", 503,
    "Ludock could not identify its runtime image. Check Docker connectivity and any LUDOCK_SELF_CONTAINER setting.");
  if (selfImage?.container === container) return selfImage.result;
  const result = Promise.resolve().then(() => docker.getContainer(container).inspect()).then((inspection) => {
    // Config.Image may be a moving tag. Only Docker's content-addressed ID is safe.
    if (!/^sha256:[a-f0-9]{64}$/.test(inspection.Image)) throw unavailable();
    return inspection.Image;
  }).catch((error: unknown) => {
    // A native hostname or custom Docker hostname need not name a container.
    // Other daemon failures remain retryable errors, not silent fallbacks.
    if ((error as { statusCode?: number })?.statusCode === 404) return FALLBACK_HELPER_IMAGE;
    if (selfImage?.result === result) selfImage = undefined;
    throw unavailable();
  });
  selfImage = { container, result };
  return result;
}

// Docker's reference grammar and familiar-name rules:
// https://github.com/distribution/reference/tree/v0.6.0
// This validates syntax, not registry trust or image contents.
const REGISTRY = /^(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*|\[[A-Fa-f0-9:]+\])(?::[0-9]+)?$/;
const REPOSITORY_COMPONENT = /^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$/;
const TAG = /^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$/;

function isPinnedImageReference(image: string): boolean {
  if (/\s/.test(image)) return false;
  const reference = /^([^@]+)@sha256:[a-f0-9]{64}$/.exec(image);
  if (!reference) return false;
  let repository = reference[1];
  const tagSeparator = repository.lastIndexOf(":");
  if (tagSeparator > repository.lastIndexOf("/")) {
    if (!TAG.test(repository.slice(tagSeparator + 1))) return false;
    repository = repository.slice(0, tagSeparator);
  }

  let registry = "docker.io";
  const firstSlash = repository.indexOf("/");
  const firstComponent = repository.slice(0, firstSlash);
  if (firstSlash !== -1 && (
    firstComponent === "localhost" || /[.:A-Z]/.test(firstComponent)
  )) {
    registry = firstComponent;
    repository = repository.slice(firstSlash + 1);
  }
  if (!REGISTRY.test(registry)) return false;
  if (registry === "index.docker.io") registry = "docker.io";
  // Docker adds this namespace before applying its 255-character path limit.
  if (registry === "docker.io" && !repository.includes("/"))
    repository = `library/${repository}`;
  return repository.length <= 255 &&
    repository.split("/").every((component) => REPOSITORY_COMPONENT.test(component));
}

export function validateHelperImage(image: string): string {
  if (!isPinnedImageReference(image)) {
    throw new Error("FILE_HELPER_IMAGE must identify a trusted Bun helper by its immutable sha256 digest.");
  }
  return image;
}
