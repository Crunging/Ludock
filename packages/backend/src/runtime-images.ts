// Keep the helper on the same Bun major as the application runtime in Dockerfile.
export const DEFAULT_HELPER_IMAGE = "oven/bun:1-alpine@sha256:d888c0ae6c86d7866ff10c5aafdd9077b36aee6455b33dd270fb93c0dd5cef6f";

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

export function getHelperImage(image = process.env.FILE_HELPER_IMAGE || DEFAULT_HELPER_IMAGE): string {
  if (!isPinnedImageReference(image)) {
    throw new Error("FILE_HELPER_IMAGE must identify a trusted Bun helper by its immutable sha256 digest.");
  }
  return image;
}
