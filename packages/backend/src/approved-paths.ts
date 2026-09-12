import path from "node:path";
import { constants } from "node:fs";
import { open } from "node:fs/promises";
import { AppError } from "./errors.js";

export function configuredRoots(value: string | undefined): string[] {
  return (value ?? "")
    .split(path.delimiter)
    .map((root) => root.trim())
    .filter(Boolean)
    .map((root) => {
      if (
        !path.isAbsolute(root) ||
        ["/", "/etc", "/proc", "/sys", "/dev", "/run", "/var/run"].includes(
          path.resolve(root),
        )
      )
        throw new AppError(
          "UNSAFE_ROOT",
          400,
          "Use a dedicated absolute data directory as an approved root",
        );
      return path.resolve(root);
    });
}
export function approvedPath(
  candidate: string,
  roots: readonly string[],
): string {
  if (!path.isAbsolute(candidate) || candidate.includes("\0"))
    throw new AppError(
      "UNAPPROVED_PATH",
      400,
      "An absolute path inside an approved root is required",
    );
  const resolved = path.resolve(candidate);
  if (
    !roots.some(
      (root) => resolved === root || resolved.startsWith(`${root}${path.sep}`),
    )
  )
    throw new AppError(
      "UNAPPROVED_PATH",
      400,
      "The path is outside the deployment's approved roots",
    );
  return resolved;
}
/** Linux descriptor traversal pins every parent directory. O_NOFOLLOW on only
 * the final path is insufficient against concurrent parent symlink replacement.
 * Compose execution is supported in Ludock's Linux Docker runtime. */
export async function readApprovedFile(
  candidate: string,
  roots: readonly string[],
  maxBytes = 2 * 1024 * 1024,
): Promise<Buffer> {
  return (await readConfigurationFile(candidate, roots, maxBytes, false))!;
}

/** Only a missing final file is optional; unsafe or inaccessible parents fail. */
export async function readOptionalApprovedFile(
  candidate: string,
  roots: readonly string[],
): Promise<Buffer | undefined> {
  return readConfigurationFile(candidate, roots, 2 * 1024 * 1024, true);
}

async function readConfigurationFile(
  candidate: string,
  roots: readonly string[],
  maxBytes: number,
  optional: boolean,
): Promise<Buffer | undefined> {
  const resolved = approvedPath(candidate, roots);
  if (process.platform !== "linux")
    throw new AppError(
      "COMPOSE_RUNTIME_REQUIRED",
      409,
      "Compose project loading requires the Ludock Linux container runtime",
    );
  let directory = await open(
    "/",
    constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW,
  );
  try {
    const components = resolved.split("/").filter(Boolean);
    for (const component of components.slice(0, -1)) {
      const next = await open(
        `/proc/self/fd/${directory.fd}/${component}`,
        constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW,
      );
      await directory.close();
      directory = next;
    }
    // O_NONBLOCK lets fstat reject FIFOs before an open can wait for a writer.
    const file = await open(
      `/proc/self/fd/${directory.fd}/${components.at(-1)}`,
      constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK,
    ).catch((error: NodeJS.ErrnoException) => {
      if (optional && error.code === "ENOENT") return undefined;
      throw error;
    });
    if (!file) return undefined;
    try {
      const stat = await file.stat();
      if (!stat.isFile() || stat.size > maxBytes)
        throw new AppError(
          "INVALID_CONFIG_FILE",
          400,
          "Configuration inputs must be regular files within the size limit",
        );
      const buffer = Buffer.alloc(maxBytes + 1);
      let offset = 0;
      while (offset < buffer.length) {
        const read = await file.read(
          buffer,
          offset,
          buffer.length - offset,
          null,
        );
        if (!read.bytesRead) break;
        offset += read.bytesRead;
      }
      if (offset > maxBytes)
        throw new AppError(
          "INVALID_CONFIG_FILE",
          400,
          "Configuration input exceeds the size limit",
        );
      return buffer.subarray(0, offset);
    } finally {
      await file.close();
    }
  } catch (error) {
    if (error instanceof AppError) throw error;
    throw new AppError(
      "UNSAFE_CONFIG_PATH",
      400,
      "A configuration file is missing, unreadable, or traverses a symbolic link",
    );
  } finally {
    await directory.close();
  }
}
