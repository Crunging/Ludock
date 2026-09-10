import { constants, createWriteStream } from "node:fs";
import {
  lstat,
  realpath,
  statfs,
  open,
  rename,
  rm,
  type FileHandle,
} from "node:fs/promises";
import path from "node:path";
import { PassThrough, Transform, Writable, type Readable } from "node:stream";
import { pipeline } from "node:stream/promises";
import type Docker from "dockerode";
import * as tar from "tar-stream";
import { backupSettingsSchema, type BackupSettings } from "@ludock/shared";
import { getDockerInstance } from "./docker.js";
import type { ServerContext } from "./servers.js";
import { AppError } from "./errors.js";
import { FILE_HELPER_SCRIPT } from "./file-helper-script.js";
import { RESTORE_EXTRACT_SCRIPT } from "./restore-extract-script.js";
import { createMountProof, assertMountIdentities } from "./mount-proof.js";
import { isSafeWritableDataMount } from "./file-storage.js";
import { DEFAULT_HELPER_IMAGE } from "./runtime-images.js";

export interface BackupRoot {
  id: string;
  path: string;
}
export interface DataHelper {
  container: Docker.Container;
  roots: BackupRoot[];
  cleanup(): Promise<void>;
}
const uuidPattern =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
export const failBackup = (code: string, message: string) =>
  new AppError(code, 409, message);
const within = (value: string, root: string) =>
  value === root || value.startsWith(`${root}${path.sep}`);

export async function validateBackupSettings(
  value: unknown,
): Promise<BackupSettings> {
  const settings = backupSettingsSchema.parse(value);
  const directory = await approvedBackupDirectory(settings.destination);
  const selfId = process.env.LUDOCK_SELF_CONTAINER || process.env.HOSTNAME;
  if (!selfId || !/^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/.test(selfId))
    throw failBackup(
      "BACKUP_MOUNT_UNVERIFIED",
      "Identify Ludock's container with LUDOCK_SELF_CONTAINER before configuring backups.",
    );
  try {
    const self = await getDockerInstance().getContainer(selfId).inspect();
    const mount = (self.Mounts || [])
      .filter(
        (entry) =>
          directory === entry.Destination ||
          directory.startsWith(`${entry.Destination}/`),
      )
      .sort(
        (left, right) => right.Destination.length - left.Destination.length,
      )[0];
    if (!mount?.RW || (mount.Type !== "bind" && mount.Type !== "volume"))
      throw failBackup(
        "BACKUP_MOUNT_UNVERIFIED",
        "The backup destination must be a writable mounted directory in Ludock's container.",
      );
  } catch (error) {
    if (error instanceof AppError) throw error;
    throw failBackup(
      "BACKUP_MOUNT_UNVERIFIED",
      "Ludock could not verify its backup mount. Set LUDOCK_SELF_CONTAINER to its Docker container name or ID.",
    );
  }
  return settings;
}

export async function approvedBackupDirectory(
  directory: string,
): Promise<string> {
  if (!path.isAbsolute(directory) || directory.includes("\0"))
    throw failBackup(
      "BACKUP_DESTINATION",
      "Choose an absolute mounted backup destination inside LUDOCK_BACKUP_ROOTS.",
    );
  const configured = (process.env.LUDOCK_BACKUP_ROOTS || "")
    .split(path.delimiter)
    .filter(Boolean);
  const roots: string[] = [];
  for (const value of configured) {
    if (
      !path.isAbsolute(value) ||
      path.parse(value).root === path.normalize(value)
    )
      continue;
    const canonical = await realpath(value);
    if (path.resolve(value) !== canonical)
      throw failBackup(
        "BACKUP_DESTINATION",
        "Configured backup roots must not traverse symbolic links.",
      );
    roots.push(canonical);
  }
  const resolved = await realpath(directory);
  const root = roots.find((candidate) => within(resolved, candidate));
  if (!root || !within(path.resolve(directory), root))
    throw failBackup(
      "BACKUP_DESTINATION",
      "Backup destination must be an existing mounted directory inside LUDOCK_BACKUP_ROOTS, without symlink traversal.",
    );
  let current = root;
  for (const part of path
    .relative(root, path.resolve(directory))
    .split(path.sep)
    .filter(Boolean)) {
    current = path.join(current, part);
    const info = await lstat(current);
    if (info.isSymbolicLink() || !info.isDirectory())
      throw failBackup(
        "BACKUP_DESTINATION",
        "Backup destination cannot traverse symbolic links.",
      );
  }
  if (
    !(await lstat(root)).isDirectory() ||
    !(await lstat(directory)).isDirectory()
  )
    throw failBackup(
      "BACKUP_DESTINATION",
      "Backup destination must be a directory.",
    );
  return resolved;
}

interface PinnedDirectory {
  descriptor: FileHandle;
  path: string;
}
async function pinBackupDirectory(directory: string): Promise<PinnedDirectory> {
  if (process.platform !== "linux")
    throw failBackup(
      "BACKUP_PLATFORM",
      "Backup storage requires Ludock's Linux Docker runtime.",
    );
  const approved = await approvedBackupDirectory(directory);
  let descriptor = await open(
    "/",
    constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW,
  );
  try {
    for (const component of approved.split("/").filter(Boolean)) {
      const child = await open(
        `/proc/self/fd/${descriptor.fd}/${component}`,
        constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW,
      );
      await descriptor.close();
      descriptor = child;
    }
    return { descriptor, path: `/proc/self/fd/${descriptor.fd}` };
  } catch (error) {
    await descriptor.close();
    throw error;
  }
}
export async function removePartialArchive(
  directory: string,
  id: string,
): Promise<void> {
  if (!uuidPattern.test(id))
    throw failBackup("BACKUP_ID", "Invalid backup identifier.");
  const pinned = await pinBackupDirectory(directory);
  try {
    await rm(`${pinned.path}/${id}.tar.partial`, { force: true });
  } finally {
    await pinned.descriptor.close();
  }
}

export async function backupFilePath(
  directory: string,
  id: string,
  temporary = false,
): Promise<string> {
  if (!uuidPattern.test(id))
    throw failBackup("BACKUP_ID", "Invalid backup identifier.");
  return path.join(
    await approvedBackupDirectory(directory),
    `${id}.tar${temporary ? ".partial" : ""}`,
  );
}

async function destinationBudget(
  directory: string,
  reserve: number,
  additional = 0,
): Promise<bigint> {
  const info = await statfs(directory, { bigint: true });
  const available = info.bavail * info.bsize - BigInt(reserve);
  if (available < BigInt(additional))
    throw failBackup(
      "BACKUP_CAPACITY",
      "The backup destination does not have enough free space above its configured reserve.",
    );
  return available;
}
export async function assertDestinationSpace(
  directory: string,
  reserve: number,
  additional = 0,
): Promise<void> {
  const pinned = await pinBackupDirectory(directory);
  try {
    await destinationBudget(pinned.path, reserve, additional);
  } finally {
    await pinned.descriptor.close();
  }
}

export async function archiveReadStream(
  directory: string,
  id: string,
): Promise<Readable> {
  if (!uuidPattern.test(id))
    throw failBackup("BACKUP_ID", "Invalid backup identifier.");
  const pinned = await pinBackupDirectory(directory);
  let handle: FileHandle | undefined;
  try {
    handle = await open(
      `${pinned.path}/${id}.tar`,
      constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK,
    );
    if (!(await handle.stat()).isFile()) {
      throw failBackup(
        "INVALID_BACKUP",
        "The backup archive is not a regular file.",
      );
    }
    const stream = handle.createReadStream();
    stream.once("close", () => {
      void pinned.descriptor.close();
    });
    return stream;
  } catch (error) {
    await Promise.allSettled([handle?.close(), pinned.descriptor.close()]);
    throw error;
  }
}

type ExtendedHeader = tar.Headers & { pax?: Record<string, string> | null };
export function archiveEntryMetadata(header: tar.Headers): {
  uid: number;
  gid: number;
  mtime: number;
} {
  const pax = (header as ExtendedHeader).pax;
  const uid = Number(pax?.uid ?? header.uid ?? 0),
    gid = Number(pax?.gid ?? header.gid ?? 0);
  const mtime = Number(
    pax?.mtime ?? (header.mtime ? header.mtime.getTime() / 1000 : 0),
  );
  if (
    ![uid, gid].every(
      (value) =>
        Number.isSafeInteger(value) && value >= 0 && value <= 4_294_967_294,
    ) ||
    !Number.isFinite(mtime) ||
    Math.abs(mtime) > 8_640_000_000_000
  )
    throw failBackup(
      "UNSAFE_ARCHIVE",
      "The archive contains invalid ownership or timestamp metadata.",
    );
  return { uid, gid, mtime };
}
export function mappedArchiveHeader(
  header: tar.Headers,
  name: string,
): ExtendedHeader {
  const { uid, gid, mtime } = archiveEntryMetadata(header);
  // Generate fresh, numeric-only PAX metadata. This preserves large Linux IDs
  // and dates beyond 2038 without retaining source path/link overrides.
  return {
    name,
    type: header.type,
    size: header.size,
    mode: header.mode,
    uid,
    gid,
    mtime: new Date(mtime * 1000),
    pax: { uid: String(uid), gid: String(gid), mtime: String(mtime) },
  };
}

export function validateArchiveEntry(
  header: tar.Headers,
  roots: readonly BackupRoot[],
): void {
  const name = header.name.replace(/\/$/, "");
  const parts = name.split("/");
  archiveEntryMetadata(header);
  if (
    header.name.includes("\0") ||
    header.name.includes("\\") ||
    header.name.startsWith("/") ||
    parts.some((part) => part === ".." || part === "." || part === "") ||
    parts.length > 64 ||
    name.length > 4096
  ) {
    throw failBackup("UNSAFE_ARCHIVE", "The archive contains an unsafe path.");
  }
  if (
    name !== "snapshot" &&
    (!name.startsWith("snapshot/") ||
      !roots.some((root) => parts[1] === root.id))
  )
    throw failBackup(
      "UNSAFE_ARCHIVE",
      "The archive contains a path outside its recorded roots.",
    );
  if (!header.type || !["file", "directory"].includes(header.type))
    throw failBackup(
      "UNSAFE_ARCHIVE",
      "Backups support regular files and directories. Remove symbolic links, hard links, and special files from the selected data roots before retrying.",
    );
  if (parts.some((part) => part.startsWith(".ludock-restore-")))
    throw failBackup(
      "RESTORE_RECOVERY_REQUIRED",
      "A previous restore has staging data in this root. Complete its recovery before making a backup.",
    );
  if (!Number.isSafeInteger(header.size ?? 0) || (header.size ?? 0) < 0)
    throw failBackup(
      "UNSAFE_ARCHIVE",
      "The archive has an invalid entry size.",
    );
  if (
    (name === "snapshot" || parts.length === 2) &&
    header.type !== "directory"
  )
    throw failBackup(
      "BACKUP_ROOT_NOT_DIRECTORY",
      "Backup roots must be mounted directories; individual file mounts are not supported.",
    );
}

export function archiveValidator(
  roots: readonly BackupRoot[],
  maxBytes: number,
): Writable {
  const extract = tar.extract();
  const names = new Set<string>();
  const foundRoots = new Set<string>();
  let total = 0;
  extract.on("entry", (header, stream, next) => {
    stream.once("error", (error) => extract.destroy(error));
    try {
      validateArchiveEntry(header, roots);
      const name = header.name.replace(/\/$/, "");
      if (names.has(name) || names.size >= 100_000)
        throw failBackup(
          "UNSAFE_ARCHIVE",
          "The archive has duplicate paths or too many entries.",
        );
      names.add(name);
      total += header.size || 0;
      if (!Number.isSafeInteger(total) || total > maxBytes)
        throw failBackup(
          "BACKUP_CAPACITY",
          "The archive exceeds the configured size limit.",
        );
      if (name.split("/").length === 2) foundRoots.add(name.split("/")[1]);
      stream.resume();
      stream.once("end", next);
    } catch (error) {
      stream.destroy(error as Error);
      extract.destroy(error as Error);
    }
  });
  const validator = new Writable({
    write(chunk: Buffer, _encoding, callback) {
      if (extract.write(chunk)) callback();
      else extract.once("drain", callback);
    },
    final(callback) {
      extract.once("finish", () => {
        callback(
          roots.some((root) => !foundRoots.has(root.id))
            ? failBackup(
                "INVALID_BACKUP",
                "The archive is missing a recorded data root.",
              )
            : undefined,
        );
      });
      extract.end();
    },
    destroy(error, callback) {
      extract.destroy(error || undefined);
      callback(error);
    },
  });
  extract.once("error", (error) => validator.destroy(error));
  return validator;
}

export async function validateArchive(
  directory: string,
  id: string,
  roots: readonly BackupRoot[],
  maxBytes: number,
  checksum: string,
): Promise<void> {
  const input = await archiveReadStream(directory, id);
  const hash = new Bun.CryptoHasher("sha256");
  let bytes = 0;
  const meter = new Transform({
    transform(chunk: Buffer, _encoding, callback) {
      bytes += chunk.length;
      if (bytes > maxBytes)
        return callback(
          failBackup(
            "BACKUP_CAPACITY",
            "The archive exceeds the configured size limit.",
          ),
        );
      hash.update(chunk);
      callback(null, chunk);
    },
  });
  await pipeline(input, meter, archiveValidator(roots, maxBytes));
  if (hash.digest("hex") !== checksum)
    throw failBackup(
      "BACKUP_CHECKSUM",
      "The backup checksum does not match. Restore was stopped before changing game data.",
    );
}

export async function createDataHelper(
  context: ServerContext,
  readOnly: boolean,
  operationId: string,
): Promise<DataHelper> {
  const docker = getDockerInstance();
  const roots = context.container.fileRoots.map(({ id, path: rootPath }) => ({
    id,
    path: rootPath,
  }));
  if (!roots.length)
    throw failBackup(
      "NO_BACKUP_ROOTS",
      "This server has no approved writable data roots.",
    );
  const mounts: NonNullable<Docker.HostConfig["Mounts"]> = [];
  const aliases: string[] = [];
  const physicalRoots: string[] = [];
  for (const root of roots) {
    if (
      context.observation.mounts.some((mount) =>
        mount.destination.startsWith(`${root.path}/`),
      )
    )
      throw failBackup(
        "NESTED_BACKUP_MOUNT",
        "A selected root contains a nested mount. Restrict ludock.files to a data subdirectory without nested mounts before backing up.",
      );
    const source = context.observation.mounts
      .filter(
        (mount) =>
          mount.writable &&
          (mount.type === "bind" || mount.type === "volume") &&
          (root.path === mount.destination ||
            root.path.startsWith(`${mount.destination}/`)),
      )
      .sort(
        (left, right) => right.destination.length - left.destination.length,
      )[0];
    if (!source)
      throw failBackup(
        "BACKUP_ROOT_CHANGED",
        "A selected data root no longer belongs to a writable mount.",
      );
    // Different container paths may expose the same underlying volume/bind
    // subtree. Such aliases would collide during restore staging.
    if (!path.posix.isAbsolute(source.source))
      throw failBackup(
        "BACKUP_ROOT_CHANGED",
        "The physical source of a selected data root could not be verified.",
      );
    const physical = path.posix.normalize(
      `${source.source}${root.path.slice(source.destination.length)}`,
    );
    if (
      physicalRoots.some(
        (other) =>
          physical === other ||
          physical.startsWith(`${other}/`) ||
          other.startsWith(`${physical}/`),
      )
    )
      throw failBackup(
        "OVERLAPPING_BACKUP_ROOTS",
        "Selected data roots overlap on the Docker host. Restrict ludock.files to one path for each distinct data subtree.",
      );
    physicalRoots.push(physical);
    const alias = `/mounts/${root.id}`;
    aliases.push(`${alias}${root.path.slice(source.destination.length)}`);
    mounts.push({
      Type: source.type as "bind" | "volume",
      Source:
        source.type === "volume" ? source.name || source.source : source.source,
      Target: alias,
      ReadOnly: readOnly,
    });
  }
  const checkedMounts = mounts.map((mount) => ({
    Type: mount.Type,
    Source: mount.Source,
    Destination: mount.Target,
    RW: true,
    ...(mount.Type === "volume" ? { Name: mount.Source } : {}),
  }));
  if (checkedMounts.some((mount) => !isSafeWritableDataMount(mount)))
    throw failBackup("UNSAFE_BACKUP_ROOT", "A selected data mount is unsafe.");
  const proof = await createMountProof(checkedMounts, operationId);
  const image = process.env.FILE_HELPER_IMAGE || DEFAULT_HELPER_IMAGE;
  const options: Docker.ContainerCreateOptions = {
    Image: image,
    User: "0",
    Entrypoint: ["bun", "-e"],
    Cmd: ["setInterval(() => {}, 3600000)"],
    Labels: {
      "ludock.enable": "false",
      "ludock.internal": "backup-helper",
      "ludock.operation": operationId,
    },
    HostConfig: {
      Mounts: mounts,
      NetworkMode: "none",
      ReadonlyRootfs: true,
      PidsLimit: 32,
      Memory: 256 * 1024 * 1024,
      NanoCpus: 1_000_000_000,
      CapDrop: ["ALL"],
      CapAdd: readOnly ? ["DAC_OVERRIDE"] : ["DAC_OVERRIDE", "CHOWN", "FOWNER"],
      SecurityOpt: ["no-new-privileges"],
    },
  };
  let container: Docker.Container;
  try {
    container = await docker.createContainer(options);
  } catch (error) {
    if ((error as { statusCode?: number }).statusCode !== 404) {
      await proof.cleanup();
      throw error;
    }
    try {
      const stream = await docker.pull(image);
      await new Promise<void>((resolve, reject) =>
        docker.modem.followProgress(stream, (failure) =>
          failure ? reject(failure) : resolve(),
        ),
      );
      container = await docker.createContainer(options);
    } catch (failure) {
      await proof.cleanup();
      throw failure;
    }
  }
  try {
    await container.start();
    await assertMountIdentities(container, proof.identities);
    // Resolve label-selected subdirectories without following symbolic links.
    for (let index = 0; index < roots.length; index++) {
      await helperExec(container, [
        "bun",
        "-e",
        FILE_HELPER_SCRIPT,
        JSON.stringify({
          operation: "check",
          root: aliases[index],
          path: "",
          blocked: [],
        }),
      ]);
    }
    let cleanupPromise: Promise<void> | undefined;
    const cleanup = () =>
      (cleanupPromise ??= (async () => {
        try {
          await container
            .remove({ force: true })
            .catch((error: { statusCode?: number }) => {
              if (error.statusCode !== 404) throw error;
            });
        } finally {
          await proof.cleanup();
        }
      })());
    return { container, roots, cleanup };
  } catch (error) {
    await container.remove({ force: true }).catch(() => {});
    await proof.cleanup();
    throw error;
  }
}

export function helperRoot(context: ServerContext, root: BackupRoot): string {
  const mount = context.observation.mounts
    .filter(
      (entry) =>
        entry.writable &&
        (root.path === entry.destination ||
          root.path.startsWith(`${entry.destination}/`)),
    )
    .sort((a, b) => b.destination.length - a.destination.length)[0];
  if (!mount)
    throw failBackup("BACKUP_ROOT_CHANGED", "The selected data root changed.");
  return `/mounts/${root.id}${root.path.slice(mount.destination.length)}`;
}

export async function helperExec(
  container: Docker.Container,
  command: string[],
  environment: Record<string, string> = {},
  assertAccess?: () => void,
): Promise<string> {
  const execution = await container.exec({
    Cmd: command,
    Env: Object.entries(environment).map(([key, value]) => `${key}=${value}`),
    AttachStdout: true,
    AttachStderr: true,
  });
  assertAccess?.();
  const stream = await execution.start({ hijack: true, stdin: false });
  const output = new PassThrough(),
    errors = new PassThrough();
  const chunks: Buffer[] = [];
  let size = 0;
  output.on("data", (chunk: Buffer) => {
    size += chunk.length;
    if (size <= 4_194_304) chunks.push(chunk);
    else
      stream.destroy(
        failBackup(
          "HELPER_OUTPUT",
          "A data operation produced too much output.",
        ),
      );
  });
  errors.resume();
  getDockerInstance().modem.demuxStream(stream, output, errors);
  await new Promise<void>((resolve, reject) => {
    const timeout = setTimeout(() => {
      stream.destroy();
      reject(
        failBackup(
          "BACKUP_TIMEOUT",
          "A data operation timed out. Review the operation recovery state.",
        ),
      );
    }, 120_000);
    stream.once("end", () => {
      clearTimeout(timeout);
      resolve();
    });
    stream.once("error", (error) => {
      clearTimeout(timeout);
      reject(error);
    });
  });
  if ((await execution.inspect()).ExitCode !== 0)
    throw failBackup(
      "DATA_OPERATION_FAILED",
      "A data operation failed. The server remains stopped until its data is safe.",
    );
  return Buffer.concat(chunks).toString("utf8");
}

async function helperArchive(
  container: Docker.Container,
  root: string,
  assertAccess?: () => void,
): Promise<Readable> {
  const execution = await container.exec({
    Cmd: [
      "bun",
      "-e",
      FILE_HELPER_SCRIPT,
      JSON.stringify({ operation: "backup", root, path: "", blocked: [] }),
    ],
    AttachStdout: true,
    AttachStderr: true,
  });
  assertAccess?.();
  const stream = await execution.start({ hijack: true, stdin: false });
  const output = new PassThrough(),
    errors = new PassThrough();
  errors.resume();
  const timeout = setTimeout(
    () =>
      output.destroy(
        failBackup(
          "BACKUP_TIMEOUT",
          "The backup exceeded its 30 minute execution limit.",
        ),
      ),
    30 * 60_000,
  );
  output.once("close", () => {
    clearTimeout(timeout);
    stream.destroy();
  });
  stream.once("error", (error) => output.destroy(error));
  stream.once("end", () => {
    void execution
      .inspect()
      .then((result) => {
        if (result.ExitCode !== 0)
          output.destroy(
            failBackup(
              "UNSAFE_ARCHIVE",
              "Archive reading failed. Backups require regular files and directories without symbolic links, hard links, special files, or inaccessible paths.",
            ),
          );
        else output.end();
      })
      .catch(() =>
        output.destroy(
          failBackup(
            "BACKUP_READ_FAILED",
            "The data helper failed to read the selected root.",
          ),
        ),
      );
  });
  getDockerInstance().modem.demuxStream(stream, output, errors);
  return output;
}

/** Repack selected Docker archives to stable root IDs without following links. */
export async function writeSnapshot(
  context: ServerContext,
  helper: DataHelper,
  settings: BackupSettings,
  id: string,
  availableBytes: number,
  assertStopped: () => Promise<void>,
  assertAccess?: () => void,
): Promise<{ size: number; checksum: string }> {
  if (!uuidPattern.test(id))
    throw failBackup("BACKUP_ID", "Invalid backup identifier.");
  const pinned = await pinBackupDirectory(settings.destination);
  let diskBudget: bigint;
  try {
    diskBudget = await destinationBudget(pinned.path, settings.reserveBytes);
  } catch (error) {
    await pinned.descriptor.close();
    throw error;
  }
  const filename = `${pinned.path}/${id}.tar.partial`;
  const output = createWriteStream(filename, { flags: "wx", mode: 0o600 });
  const pack = tar.pack();
  const hash = new Bun.CryptoHasher("sha256");
  let size = 0,
    checked = 0;
  const meter = new Transform({
    transform(chunk: Buffer, _encoding, callback) {
      size += chunk.length;
      if (size > availableBytes)
        return callback(
          failBackup(
            "BACKUP_CAPACITY",
            "The backup exceeds the global byte limit. Remove older backups or increase the limit.",
          ),
        );
      if (BigInt(size) > diskBudget)
        return callback(
          failBackup(
            "BACKUP_CAPACITY",
            "The backup destination does not have enough free space above its configured reserve.",
          ),
        );
      hash.update(chunk);
      if (size - checked > 16_777_216) {
        checked = size;
        void destinationBudget(
          pinned.path,
          settings.reserveBytes,
          chunk.length,
        ).then(() => callback(null, chunk), callback);
      } else callback(null, chunk);
    },
  });
  const copy = new AbortController();
  let outputFailure: Error | undefined;
  const completed = pipeline(pack, meter, output).catch((error: Error) => {
    outputFailure = error;
    // Source extraction may be waiting for pack.entry's callback or drain.
    // Cancel that pipeline too when the destination fails or fills up.
    copy.abort(error);
    throw error;
  });
  // Attach rejection immediately while Docker streams are still being consumed.
  void completed.catch(() => {});
  try {
    await new Promise<void>((resolve, reject) =>
      pack.entry(
        { name: "snapshot/", type: "directory", mode: 0o755 },
        (error) => (error ? reject(error) : resolve()),
      ),
    );
    for (const root of helper.roots) {
      await assertStopped();
      const input = await helperArchive(
        helper.container,
        helperRoot(context, root),
        assertAccess,
      );
      const extract = tar.extract();
      let prefix: string | undefined;
      let entries = 0;
      extract.on("entry", (header, stream, next) => {
        stream.once("error", (error) => extract.destroy(error));
        const name = header.name.replace(/\/$/, "");
        if (
          name.startsWith("/") ||
          name.split("/").some((part) => !part || part === ".." || part === ".")
        ) {
          extract.destroy(
            failBackup(
              "UNSAFE_ARCHIVE",
              "Docker returned an unsafe archive path.",
            ),
          );
          return;
        }
        prefix ??= name.split("/")[0];
        if (name !== prefix && !name.startsWith(`${prefix}/`)) {
          extract.destroy(
            failBackup(
              "UNSAFE_ARCHIVE",
              "Docker returned an archive outside the selected data root.",
            ),
          );
          return;
        }
        // Rebuild only ordinary metadata: source PAX path fields must never
        // override the root mapping when tar-stream writes a fresh header.
        let mapped: tar.Headers;
        try {
          mapped = mappedArchiveHeader(
            header,
            `snapshot/${root.id}${name.slice(prefix.length)}${header.type === "directory" ? "/" : ""}`,
          );
          validateArchiveEntry(mapped, helper.roots);
        } catch (error) {
          stream.destroy(error as Error);
          extract.destroy(error as Error);
          return;
        }
        if (++entries > 100_000) {
          extract.destroy(
            failBackup(
              "UNSAFE_ARCHIVE",
              "The selected root has too many entries.",
            ),
          );
          return;
        }
        try {
          const target = pack.entry(mapped, (error) =>
            error ? extract.destroy(error) : next(),
          );
          target.once("error", (error) => extract.destroy(error));
          stream.pipe(target);
        } catch (error) {
          extract.destroy(outputFailure ?? (error as Error));
        }
        stream.once("error", (error) => extract.destroy(error));
      });
      await pipeline(input, extract, { signal: copy.signal });
      await assertStopped();
    }
    pack.finalize();
    await completed;
    await destinationBudget(pinned.path, settings.reserveBytes);
    await assertStopped();
    const file = await open(
      filename,
      constants.O_RDONLY | constants.O_NOFOLLOW,
    );
    await file.sync();
    await file.close();
    await rename(filename, `${pinned.path}/${id}.tar`);
    await pinned.descriptor.sync();
    return { size, checksum: hash.digest("hex") };
  } catch (error) {
    copy.abort(error);
    pack.destroy(error as Error);
    await completed.catch(() => {});
    await rm(filename, { force: true });
    throw outputFailure ?? error;
  } finally {
    await pinned.descriptor.close();
  }
}

export async function extractRootToStage(
  directory: string,
  id: string,
  helper: Docker.Container,
  root: BackupRoot,
  rootPath: string,
  stageName: string,
  roots: readonly BackupRoot[],
  maxBytes: number,
  checksum: string,
  assertAccess?: () => void,
): Promise<void> {
  const execution = await helper.exec({
    Cmd: [
      "bun",
      "-e",
      RESTORE_EXTRACT_SCRIPT,
      JSON.stringify({ root: rootPath, stage: stageName, maxBytes }),
    ],
    AttachStdin: true,
    AttachStdout: true,
    AttachStderr: true,
  });
  assertAccess?.();
  const socket = await execution.start({ hijack: true, stdin: true });
  const ignored = new PassThrough();
  ignored.resume();
  getDockerInstance().modem.demuxStream(socket, ignored, ignored);
  const timeout = setTimeout(
    () =>
      socket.destroy(
        failBackup(
          "RESTORE_TIMEOUT",
          "Restore extraction exceeded its 30 minute execution limit.",
        ),
      ),
    30 * 60_000,
  );
  const completed = new Promise<void>((resolve, reject) => {
    socket.once("end", () => {
      void execution
        .inspect()
        .then(
          (result) =>
            result.ExitCode === 0
              ? resolve()
              : reject(
                  failBackup(
                    "RESTORE_EXTRACTION",
                    "Restore extraction rejected a changed or unsafe data path.",
                  ),
                ),
          reject,
        );
    });
    socket.once("error", reject);
  });
  void completed.catch(() => {});
  const write = (value: Buffer) =>
    new Promise<void>((resolve, reject) => {
      assertAccess?.();
      socket.write(value, (error?: Error | null) =>
        error ? reject(error) : resolve(),
      );
    });
  const extract = tar.extract();
  const names = new Set<string>();
  const hash = new Bun.CryptoHasher("sha256");
  let total = 0;
  const meter = new Transform({
    transform(chunk: Buffer, _encoding, callback) {
      total += chunk.length;
      if (total > maxBytes)
        return callback(
          failBackup(
            "BACKUP_CAPACITY",
            "The archive exceeds its configured size limit.",
          ),
        );
      hash.update(chunk);
      callback(null, chunk);
    },
  });
  extract.on("entry", (header, stream, next) => {
    stream.once("error", (error) => extract.destroy(error));
    void (async () => {
      validateArchiveEntry(header, roots);
      if (names.has(header.name) || names.size >= 100_000)
        throw failBackup(
          "UNSAFE_ARCHIVE",
          "The archive contains duplicate paths or too many entries.",
        );
      names.add(header.name);
      const name = header.name.replace(/\/$/, ""),
        prefix = `snapshot/${root.id}/`;
      if (!name.startsWith(prefix)) {
        stream.resume();
        stream.once("end", next);
        return;
      }
      await write(
        Buffer.from(
          JSON.stringify({
            name: name.slice(prefix.length),
            type: header.type,
            mode: header.mode || 0,
            ...archiveEntryMetadata(header),
            size: header.size || 0,
          }) + "\n",
        ),
      );
      for await (const chunk of stream)
        await write(
          Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as Uint8Array),
        );
      next();
    })().catch((error: Error) => {
      stream.destroy(error);
      extract.destroy(error);
    });
  });
  try {
    await pipeline(await archiveReadStream(directory, id), meter, extract);
    if (hash.digest("hex") !== checksum)
      throw failBackup(
        "BACKUP_CHECKSUM",
        "The archive changed while it was being staged. No existing game data was replaced.",
      );
    socket.end();
    await completed;
  } catch (error) {
    socket.destroy(error as Error);
    await completed.catch(() => {});
    throw error;
  } finally {
    clearTimeout(timeout);
  }
}

export async function removeArchive(
  directory: string,
  id: string,
  assertAccess?: () => void,
): Promise<void> {
  if (!uuidPattern.test(id))
    throw failBackup("BACKUP_ID", "Invalid backup identifier.");
  const pinned = await pinBackupDirectory(directory);
  try {
    const filename = `${pinned.path}/${id}.tar`;
    const info = await lstat(filename).catch((error: NodeJS.ErrnoException) => {
      if (error.code === "ENOENT") return null;
      throw error;
    });
    if (info?.isSymbolicLink() || (info && !info.isFile()))
      throw failBackup(
        "INVALID_BACKUP",
        "Refusing to remove an archive that is not a regular file.",
      );
    assertAccess?.();
    await rm(filename, { force: true });
    await pinned.descriptor.sync();
  } finally {
    await pinned.descriptor.close();
  }
}

export const newBackupId = () => crypto.randomUUID();
