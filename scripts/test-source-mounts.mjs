import { join } from "node:path";

// Production has no installed packages. Only source-test containers receive
// these read-only mounts; the preceding production smoke test receives none.
export function backendSourceMounts(repository) {
  return [
    "package.json", "packages/backend/package.json", "packages/shared/package.json",
    "node_modules", "packages/backend/node_modules", "packages/shared/node_modules",
    "packages/backend/src", "packages/backend/test", "packages/shared/src",
  ].flatMap((directory) => ["-v", `${join(repository, directory)}:/app/${directory}:ro`]);
}
