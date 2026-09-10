import path from "node:path";
import { PassThrough, type Readable } from "node:stream";
import type Docker from "dockerode";
import { docker } from "./docker-client.js";
import type { ManagedContainer } from "./docker.js";
import { createLogger } from "./logger.js";
import { FILE_HELPER_SCRIPT } from "./file-helper-script.js";
import { evaluateContainerEligibility } from "./discovery.js";
import { createMountProof, assertMountIdentities } from "./mount-proof.js";
import { DEFAULT_HELPER_IMAGE } from "./runtime-images.js";

export const LABEL_FILES = "ludock.files";
const logger = createLogger("files");
const MAX_HELPER_OUTPUT = 4 * 1024 * 1024;
const normalizedMountPath = (value: string) =>
  path.posix.normalize(value).replace(/\/$/, "") || "/";
const within = (value: string, root: string) =>
  value === root || value.startsWith(`${root}/`);

import type { FileRoot, FileEntry } from "@ludock/shared";
export type { FileRoot, FileEntry } from "@ludock/shared";

export interface ContainerFileMount {
  Type: string;
  Source: string;
  Destination: string;
  RW: boolean;
  Name?: string;
}
export interface FileContainerAccess {
  container: Docker.Container;
  blockedPaths: string[];
  cleanup: () => Promise<void>;
  assertAccess?: () => void;
}
interface FileRequest {
  operation:
    | "check"
    | "list"
    | "stat"
    | "mkdir"
    | "delete"
    | "rename"
    | "upload"
    | "upload-cleanup"
    | "download";
  root: string;
  path: string;
  blocked: string[];
  destination?: string;
  size?: number;
  uploadId?: string;
}

export function getFileRoots(
  server: Pick<ManagedContainer, "gameType" | "image" | "labels">,
  mounts: readonly ContainerFileMount[] = [],
): FileRoot[] {
  const safe = mounts.filter(isSafeWritableDataMount);
  const configured = server.labels[LABEL_FILES];
  const candidates =
    configured !== undefined
      ? configured.split(",").map((value) => value.trim())
      : safe.map((mount) => mount.Destination);
  const paths = candidates
    .filter(
      (value) =>
        value.startsWith("/") &&
        !value.includes("\0") &&
        !value.split("/").includes(".."),
    )
    .map((value) => path.posix.normalize(value).replace(/\/$/, ""))
    .filter(
      (value, index, values) =>
        value &&
        value.length <= 512 &&
        values.indexOf(value) === index &&
        !isSensitiveSystemPath(value),
    )
    .filter((value) => {
      // The deepest actual mount owns this path. A label cannot bypass a
      // read-only or sensitive nested mount merely by naming its parent.
      const owner = mounts
        .filter((mount) =>
          within(value, normalizedMountPath(mount.Destination)),
        )
        .sort(
          (left, right) => right.Destination.length - left.Destination.length,
        )[0];
      return owner && isSafeWritableDataMount(owner);
    });
  return paths
    .filter(
      (candidate) =>
        !paths.some((other) => other !== candidate && within(candidate, other)),
    )
    .slice(0, 8)
    .map((rootPath, index) => ({
      id: `root-${index}`,
      name: rootName(server.gameType, rootPath),
      path: rootPath,
    }));
}

export function isSafeWritableDataMount(mount: ContainerFileMount): boolean {
  if (!mount.RW || (mount.Type !== "bind" && mount.Type !== "volume"))
    return false;
  const destination = normalizedMountPath(mount.Destination);
  if (
    !destination.startsWith("/") ||
    destination.length > 512 ||
    mount.Destination.includes("\0") ||
    isSensitiveSystemPath(destination) ||
    /\.sock$/i.test(destination)
  )
    return false;
  if (mount.Type === "bind") {
    const source = path.posix.normalize(mount.Source);
    if (
      !source.startsWith("/") ||
      mount.Source.includes("\0") ||
      isSensitiveSystemPath(source) ||
      /\.sock$/i.test(source)
    )
      return false;
  }
  return true;
}

function isSensitiveSystemPath(value: string): boolean {
  const configured = (
    process.env.LUDOCK_SENSITIVE_PATHS ||
    process.env.FILE_SENSITIVE_PATHS ||
    ""
  )
    .split(path.delimiter)
    .filter((part) => part.startsWith("/"))
    .map((part) => path.posix.normalize(part));
  const protectedPaths = [
    "/proc",
    "/sys",
    "/dev",
    "/run",
    "/var/run",
    "/var/lib/docker",
    "/var/lib/containerd",
    "/etc",
    "/boot",
    "/bin",
    "/sbin",
    "/lib",
    "/lib64",
    "/usr",
    "/root/.ssh",
    "/root/.aws",
    "/root/.docker",
    ...configured,
  ];
  return (
    [
      "/",
      "/home",
      "/root",
      "/srv",
      "/opt",
      "/mnt",
      "/media",
      "/Users",
    ].includes(value) ||
    protectedPaths.some((root) => within(value, root) || within(root, value))
  );
}

export async function listFiles(
  server: ManagedContainer,
  rootId: string,
  relativePath: string,
  assertAccess?: () => void,
): Promise<{ root: FileRoot; path: string; entries: FileEntry[] }> {
  const target = resolveTarget(server, rootId, relativePath);
  const access = await acquireFileContainer(server, target.root, {
    readOnly: true,
    assertAccess,
  });
  try {
    const result = await helperRequest(access, {
      operation: "list",
      root: target.root.path,
      path: target.relativePath,
    });
    access.assertAccess?.();
    const entries = JSON.parse(result.stdout) as FileEntry[];
    entries.sort((a, b) =>
      a.type === "directory" && b.type !== "directory"
        ? -1
        : a.type !== "directory" && b.type === "directory"
          ? 1
          : a.name.localeCompare(b.name, undefined, { sensitivity: "base" }),
    );
    return { root: target.root, path: target.relativePath, entries };
  } finally {
    await access.cleanup();
  }
}

export async function createDirectory(
  server: ManagedContainer,
  rootId: string,
  relativeParent: string,
  name: string,
  assertAccess?: () => void,
): Promise<void> {
  validateName(name);
  const target = resolveTarget(
    server,
    rootId,
    joinRelative(relativeParent, name),
  );
  const access = await acquireFileContainer(server, target.root, { assertAccess });
  try {
    await helperRequest(access, {
      operation: "mkdir",
      root: target.root.path,
      path: target.relativePath,
    });
  } finally {
    await access.cleanup();
  }
}

export async function deleteFileEntry(
  server: ManagedContainer,
  rootId: string,
  relativePath: string,
  assertAccess?: () => void,
): Promise<void> {
  const target = resolveTarget(server, rootId, relativePath);
  if (!target.relativePath) throw new FileStorageError("ROOT_MUTATION", 400);
  const access = await acquireFileContainer(server, target.root, { assertAccess });
  try {
    await helperRequest(access, {
      operation: "delete",
      root: target.root.path,
      path: target.relativePath,
    });
  } finally {
    await access.cleanup();
  }
}

export async function renameFileEntry(
  server: ManagedContainer,
  rootId: string,
  relativePath: string,
  newName: string,
  assertAccess?: () => void,
): Promise<void> {
  validateName(newName);
  const target = resolveTarget(server, rootId, relativePath);
  if (!target.relativePath) throw new FileStorageError("ROOT_MUTATION", 400);
  const access = await acquireFileContainer(server, target.root, { assertAccess });
  try {
    await helperRequest(access, {
      operation: "rename",
      root: target.root.path,
      path: target.relativePath,
      destination: joinRelative(
        path.posix.dirname(target.relativePath),
        newName,
      ),
    });
  } finally {
    await access.cleanup();
  }
}

export async function uploadFile(
  server: ManagedContainer,
  rootId: string,
  relativeParent: string,
  name: string,
  size: number,
  source: Readable,
  assertAccess?: () => void,
): Promise<void> {
  validateName(name);
  if (!Number.isSafeInteger(size) || size < 0)
    throw new FileStorageError("INVALID_UPLOAD_SIZE", 400);
  const target = resolveTarget(
    server,
    rootId,
    joinRelative(relativeParent, name),
  );
  // The HTTP body may be aborted while Docker is still preparing its helper.
  const ignoreInputError = () => {};
  source.on("error", ignoreInputError);
  try {
    const access = await acquireFileContainer(server, target.root, { assertAccess });
    const uploadId = crypto.randomUUID();
    try {
      await helperRequest(
        access,
        {
          operation: "upload",
          root: target.root.path,
          path: target.relativePath,
          size,
          uploadId,
        },
        source,
        Buffer.from(uploadId),
      );
    } finally {
      try {
        // Cleanup has no user-controlled operation or filename. It must remain
        // available after cancellation/revocation so staging files are removed.
        await helperRequest({ ...access, assertAccess: undefined }, {
          operation: "upload-cleanup",
          root: target.root.path,
          path: target.relativePath,
          uploadId,
        });
      } catch {
        logger.warn("Failed to clean up an upload temporary file", {
          container: server.id.slice(0, 12),
        });
      } finally {
        await access.cleanup();
      }
    }
  } finally {
    source.off("error", ignoreInputError);
  }
}

export async function openDownload(
  server: ManagedContainer,
  rootId: string,
  relativePath: string,
  assertAccess?: () => void,
): Promise<{
  name: string;
  type: "file" | "directory";
  size: number;
  stream: NodeJS.ReadableStream;
  completed: Promise<void>;
}> {
  const target = resolveTarget(server, rootId, relativePath);
  if (!target.relativePath) throw new FileStorageError("ROOT_DOWNLOAD", 400);
  const access = await acquireFileContainer(server, target.root, {
    readOnly: true,
    assertAccess,
  });
  try {
    const result = await helperRequest(access, {
      operation: "stat",
      root: target.root.path,
      path: target.relativePath,
    });
    const info = JSON.parse(result.stdout) as {
      type: "file" | "directory";
      size: number;
    };
    const execution = await access.container.exec(
      helperOptions({
        operation: "download",
        root: target.root.path,
        path: target.relativePath,
        blocked: access.blockedPaths,
      }),
    );
    access.assertAccess?.();
    const stream = await execution.start({ hijack: true, stdin: false });
    const output = new PassThrough();
    output.once("error", () => {});
    let finishCleanup!: () => void;
    const completed = new Promise<void>((resolve) => {
      finishCleanup = resolve;
    });
    let cleaning = false;
    const timeout = setTimeout(() => {
      output.destroy(new FileStorageError("FILE_OPERATION_TIMEOUT", 409));
    }, 30 * 60_000);
    const cleanup = () => {
      if (cleaning) return;
      cleaning = true;
      clearTimeout(timeout);
      stream.destroy();
      void access
        .cleanup()
        .catch(() =>
          logger.warn("Failed to remove download helper", {
            container: server.id.slice(0, 12),
          }),
        )
        .finally(finishCleanup);
    };
    output.once("close", cleanup);
    output.once("error", cleanup);
    output.once("end", cleanup);
    void pumpDockerDownload(stream, output)
      .then(async () => {
        const status = await execution.inspect();
        if (status.ExitCode !== 0 || status.Running)
          output.destroy(
            new FileStorageError("UNSAFE_OR_CHANGED_FILE_PATH", 409),
          );
        else output.end();
      })
      .catch(() =>
        output.destroy(new FileStorageError("FILE_DOWNLOAD_FAILED", 409)),
      );
    const name = path.posix.basename(target.relativePath);
    return {
      name: info.type === "directory" ? `${name}.tar` : name,
      type: info.type,
      size: info.size,
      stream: output,
      completed,
    };
  } catch (error) {
    await access.cleanup();
    throw error;
  }
}

/** Honor the HTTP consumer's backpressure while decoding Docker's non-TTY
 * frames. docker-modem's event-based demux ignores writable backpressure. */
export async function pumpDockerDownload(
  source: Readable,
  output: PassThrough,
): Promise<void> {
  let header = Buffer.alloc(0);
  let remaining = 0;
  let channel = 0;
  for await (const value of source) {
    const chunk = value as Buffer;
    let offset = 0;
    while (offset < chunk.length) {
      if (!remaining) {
        const count = Math.min(8 - header.length, chunk.length - offset);
        header = Buffer.concat([
          header,
          chunk.subarray(offset, offset + count),
        ]);
        offset += count;
        if (header.length !== 8) continue;
        channel = header[0];
        remaining = header.readUInt32BE(4);
        if (
          ![0, 1, 2].includes(channel) ||
          header[1] ||
          header[2] ||
          header[3] ||
          remaining > 64 * 1024 * 1024
        )
          throw new FileStorageError("INVALID_DOWNLOAD_STREAM", 409);
        header = Buffer.alloc(0);
        if (!remaining) continue;
      }
      const count = Math.min(remaining, chunk.length - offset);
      if (channel === 1 && count)
        await new Promise<void>((resolve, reject) => {
          const closed = () =>
            finish(new FileStorageError("FILE_DOWNLOAD_CLOSED", 409));
          const finish = (error?: Error | null) => {
            output.off("error", finish);
            output.off("close", closed);
            if (error) reject(error);
            else resolve();
          };
          if (output.destroyed) {
            closed();
            return;
          }
          output.once("error", finish);
          output.once("close", closed);
          output.write(chunk.subarray(offset, offset + count), finish);
        });
      offset += count;
      remaining -= count;
    }
  }
  if (header.length || remaining)
    throw new FileStorageError("INCOMPLETE_DOWNLOAD_STREAM", 409);
}

function helperOptions(request: FileRequest): Docker.ExecCreateOptions {
  return {
    Cmd: ["bun", "-e", FILE_HELPER_SCRIPT, JSON.stringify(request)],
    AttachStdout: true,
    AttachStderr: true,
    AttachStdin: request.operation === "upload",
    Tty: false,
  };
}

function fileTargetSignature(inspection: Docker.ContainerInspectInfo): string {
  return JSON.stringify({
    id: inspection.Id,
    name: inspection.Name,
    image: inspection.Config.Image,
    labels: Object.entries(inspection.Config.Labels || {}).sort(
      ([left], [right]) => left.localeCompare(right),
    ),
    mounts: (inspection.Mounts || [])
      .map((mount) => [
        mount.Type,
        mount.Source,
        mount.Name,
        mount.Destination,
        mount.RW,
      ])
      .sort((left, right) => String(left[3]).localeCompare(String(right[3]))),
  });
}
async function helperRequest(
  access: FileContainerAccess,
  request: Omit<FileRequest, "blocked">,
  input?: Readable,
  inputTrailer?: Buffer,
): Promise<{ stdout: string }> {
  return runExec(
    access.container,
    helperOptions({ ...request, blocked: access.blockedPaths }),
    input,
    access.assertAccess,
    inputTrailer,
  );
}

export async function acquireFileContainer(
  server: ManagedContainer,
  root: FileRoot,
  options: { readOnly?: boolean; assertAccess?: () => void } = {},
): Promise<FileContainerAccess> {
  const inspection = await docker.getContainer(server.id).inspect();
  if (
    inspection.Id !== server.id ||
    inspection.Name.replace(/^\//, "") !== server.name ||
    !evaluateContainerEligibility(
      inspection.Config.Image,
      inspection.Config.Labels || {},
    ).eligible
  )
    throw new FileStorageError("FILE_TARGET_CHANGED", 409);
  const mounts = (inspection.Mounts || []) as ContainerFileMount[];
  const freshRoots = getFileRoots(
    { ...server, labels: inspection.Config.Labels || {} },
    mounts,
  );
  if (!freshRoots.some((candidate) => candidate.path === root.path))
    throw new FileStorageError("FILE_ROOT_CHANGED", 409);
  const owner = mounts
    .filter((mount) =>
      within(root.path, normalizedMountPath(mount.Destination)),
    )
    .sort(
      (left, right) => right.Destination.length - left.Destination.length,
    )[0];
  if (!owner || !isSafeWritableDataMount(owner))
    throw new FileStorageError("FILE_ROOT_CHANGED", 409);
  const children = mounts.filter(
    (mount) =>
      mount !== owner &&
      within(normalizedMountPath(mount.Destination), root.path),
  );
  const blockedPaths = children
    .filter((mount) => !isSafeWritableDataMount(mount))
    .map((mount) => normalizedMountPath(mount.Destination));
  const selected = [
    owner,
    ...children.filter(
      (mount) =>
        isSafeWritableDataMount(mount) &&
        !blockedPaths.some((blocked) =>
          within(normalizedMountPath(mount.Destination), blocked),
        ),
    ),
  ];
  const projections = selected.map((mount) => {
    const source =
      mount.Type === "volume" ? mount.Name || mount.Source : mount.Source;
    if (mount.Type === "volume" && source.includes("/"))
      throw new FileStorageError("UNIDENTIFIED_VOLUME", 409);
    return {
      Type: mount.Type as "bind" | "volume",
      Source: source,
      Target: normalizedMountPath(mount.Destination),
      ReadOnly: Boolean(options.readOnly),
      ...(mount.Type === "bind"
        ? {
            BindOptions: {
              Propagation: "rprivate" as const,
              NonRecursive: true,
            },
          }
        : {}),
    };
  });
  const proof = await createMountProof(selected);
  const image = process.env.FILE_HELPER_IMAGE || DEFAULT_HELPER_IMAGE;
  const containerOptions: Docker.ContainerCreateOptions = {
    Image: image,
    Entrypoint: ["bun", "-e"],
    Cmd: [
      "setInterval(() => {}, 3600000); setTimeout(() => process.exit(0), 2100000)",
    ],
    User: "0",
    Labels: { "ludock.enable": "false", "ludock.internal": "file-helper" },
    HostConfig: {
      Mounts: projections,
      NetworkMode: "none",
      ReadonlyRootfs: true,
      AutoRemove: true,
      CapDrop: ["ALL"],
      CapAdd: options.readOnly ? ["DAC_OVERRIDE"] : ["DAC_OVERRIDE", "CHOWN"],
      SecurityOpt: ["no-new-privileges"],
      PidsLimit: 32,
      Memory: 256 * 1024 * 1024,
      NanoCpus: 1_000_000_000,
    },
  };
  let helper: Docker.Container;
  try {
    helper = await docker.createContainer(containerOptions);
  } catch (error) {
    try {
      if ((error as { statusCode?: number }).statusCode !== 404) throw error;
      const stream = await docker.pull(image);
      await new Promise<void>((resolve, reject) =>
        docker.modem.followProgress(stream, (error) =>
          error ? reject(error) : resolve(),
        ),
      );
      helper = await docker.createContainer(containerOptions);
    } catch (failure) {
      await proof.cleanup();
      throw failure;
    }
  }
  let removal: Promise<void> | undefined;
  const cleanup = () => {
    removal ??= (async () => {
      try {
        await helper.remove({ force: true });
      } catch (error) {
        if ((error as { statusCode?: number }).statusCode !== 404) {
          try {
            await helper.stop({ t: 0 });
            await helper.remove({ force: true });
          } catch {
            logger.warn("Failed to remove file helper", {
              container: server.id.slice(0, 12),
            });
          }
        }
      } finally {
        await proof.cleanup();
      }
    })();
    return removal;
  };
  try {
    await helper.start();
    await assertMountIdentities(helper, proof.identities);
    const access = { container: helper, blockedPaths, cleanup, assertAccess: options.assertAccess };
    await helperRequest(access, {
      operation: "check",
      root: root.path,
      path: "",
    });
    // Pulling or starting a helper can take long enough for an external manager
    // to replace the game and reuse its volume names. Once helper mounts are
    // pinned, prove the original game still owns the exact captured binding.
    let fresh: Docker.ContainerInspectInfo;
    try {
      fresh = await docker.getContainer(server.id).inspect();
    } catch {
      throw new FileStorageError("FILE_TARGET_CHANGED", 409);
    }
    if (fileTargetSignature(fresh) !== fileTargetSignature(inspection))
      throw new FileStorageError("FILE_TARGET_CHANGED", 409);
    return access;
  } catch (error) {
    await cleanup();
    throw error;
  }
}

async function runExec(
  container: Docker.Container,
  options: Docker.ExecCreateOptions,
  input?: Readable,
  assertAccess?: () => void,
  inputTrailer?: Buffer,
): Promise<{ stdout: string }> {
  assertAccess?.();
  if (input?.destroyed && !input.readableEnded)
    throw new FileStorageError("FILE_UPLOAD_FAILED", 400);
  const execution = await container.exec(options);
  assertAccess?.();
  const stream = await execution.start({
    hijack: true,
    stdin: Boolean(input),
  });
  const stdout = new PassThrough();
  const stderr = new PassThrough();
  const chunks: Buffer[] = [];
  let size = 0;
  stdout.on("data", (chunk: Buffer) => {
    size += chunk.length;
    if (size > MAX_HELPER_OUTPUT)
      stream.destroy(new FileStorageError("FILE_OUTPUT_LIMIT", 409));
    else chunks.push(chunk);
  });
  stderr.resume();
  await new Promise<void>((resolve, reject) => {
    let settled = false;
    let inputEnded = false;
    let inputFailure: Error | undefined;
    let cancellationTimeout: ReturnType<typeof setTimeout> | undefined;
    const timeout = setTimeout(
      () => {
        stream.destroy();
        finish(new FileStorageError("FILE_OPERATION_TIMEOUT", 409));
      },
      input ? 30 * 60_000 : 60_000,
    );
    const finish = (error?: Error) => {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      clearTimeout(cancellationTimeout);
      input?.unpipe(stream);
      input?.off("end", endInput);
      input?.off("error", errorInput);
      input?.off("close", closeInput);
      const failure = inputFailure || error;
      if (failure) reject(failure);
      else resolve();
    };
    const failInput = (error?: Error) => {
      if (settled || inputFailure) return;
      inputFailure = error || new FileStorageError("FILE_UPLOAD_FAILED", 400);
      input?.unpipe(stream);
      // Missing commit trailer makes even a full-length cancelled body fail.
      // Keep the read side alive until the helper finishes its own cleanup.
      stream.end();
      cancellationTimeout = setTimeout(() => {
        stream.destroy();
        finish(inputFailure);
      }, 5_000);
    };
    const endInput = () => {
      inputEnded = true;
      if (settled || inputFailure) return;
      try { assertAccess?.(); }
      catch (error) { failInput(error as Error); return; }
      stream.end(inputTrailer);
    };
    const closeInput = () => { if (!inputEnded) failInput(); };
    const errorInput = () => failInput();
    stream.once("end", () => finish());
    stream.once("close", () => finish(new FileStorageError("FILE_OPERATION_FAILED", 409)));
    stream.once("error", () =>
      finish(new FileStorageError("FILE_OPERATION_FAILED", 409)),
    );
    docker.modem.demuxStream(stream, stdout, stderr);
    if (input) {
      input.once("error", errorInput);
      input.once("end", endInput);
      input.once("close", closeInput);
      if (input.destroyed && !input.readableEnded) failInput();
      else if (input.readableEnded) endInput();
      else input.pipe(stream, { end: false });
    }
  });
  const status = await execution.inspect();
  if (status.ExitCode !== 0 || status.Running)
    throw new FileStorageError("UNSAFE_OR_CHANGED_FILE_PATH", 409);
  return { stdout: Buffer.concat(chunks).toString("utf8") };
}

function rootName(gameType: string, rootPath: string): string {
  if (gameType.toLowerCase() === "minecraft" && rootPath === "/data") {
    return "Minecraft data";
  }
  if (gameType.toLowerCase() === "valheim" && rootPath === "/config") {
    return "Valheim config";
  }
  if (
    gameType.toLowerCase() === "terraria" &&
    rootPath === "/root/.local/share/Terraria/Worlds"
  ) {
    return "Terraria worlds";
  }
  return path.posix.basename(rootPath) || rootPath;
}

function resolveTarget(
  server: ManagedContainer,
  rootId: string,
  relativePath: string,
): { root: FileRoot; relativePath: string } {
  const root = server.fileRoots.find((candidate) => candidate.id === rootId);
  if (!root) throw new FileStorageError("ROOT_NOT_FOUND", 404);
  const normalized = normalizeRelativePath(relativePath);
  return {
    root,
    relativePath: normalized,
  };
}

export function normalizeRelativePath(value: string): string {
  if (
    value.includes("\0") ||
    value.startsWith("/") ||
    value.split("/").some((part) => part === "..")
  ) {
    throw new FileStorageError("INVALID_PATH", 400);
  }
  const normalized = path.posix.normalize(value || ".").replace(/^\.$/, "");
  if (normalized === ".." || normalized.startsWith("../")) {
    throw new FileStorageError("INVALID_PATH", 400);
  }
  return normalized;
}

function joinRelative(parent: string, name: string): string {
  return parent ? path.posix.join(parent, name) : name;
}

function validateName(name: string): void {
  if (
    !name ||
    name.length > 255 ||
    name === "." ||
    name === ".." ||
    name.includes("/") ||
    name.trim() !== name ||
    // Rejecting control characters is the point of this check: they are
    // illegal in paths and can forge terminal output in logs.
    // eslint-disable-next-line no-control-regex
    /[\x00-\x1f\x7f]/.test(name)
  ) {
    throw new FileStorageError("INVALID_NAME", 400);
  }
}

export class FileStorageError extends Error {
  constructor(
    public readonly code: string,
    public readonly statusCode: number,
  ) {
    super(
      code === "UNSAFE_OR_CHANGED_FILE_PATH"
        ? "The path changed, is unsafe, or cannot be accessed"
        : code,
    );
  }
}
